package manager

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	"github.com/j75689/cadre/internal/storage"
	"github.com/j75689/cadre/internal/task"
)

// The background work below finds candidates in the index and decides on a
// fresh read, so any number of instances may run it at once: losers of a
// conditional write simply find nothing left to do.

// forEachTeam calls fn for every Team namespace, collecting errors so one
// broken namespace does not stop the others.
func (m *Manager) forEachTeam(ctx context.Context, fn func(ns string) error) error {
	teams, err := m.store.ListTeams(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, t := range teams {
		if err := fn(t.Namespace); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", t.Namespace, err))
		}
	}
	return errors.Join(errs...)
}

// NumberNew gives every unnumbered task a number from its Team's counter and
// moves it to Todo. Tasks applied with kubectl and subtasks created by the
// dispatcher both arrive here. A number claimed for a task that another
// instance numbers first is skipped: numbers are unique, not dense.
func (m *Manager) NumberNew(ctx context.Context) error {
	return m.forEachTeam(ctx, func(ns string) error {
		unborn, err := m.store.ListTasks(ctx, storage.Query{Namespace: ns, Phases: []v1alpha1.Phase{""}})
		if err != nil {
			return err
		}
		// One failing task must not hold up the ones after it.
		var errs []error
		for _, t := range unborn {
			var n int64
			if err := m.writeTeam(ctx, ns, func(team *v1alpha1.Team) (v1alpha1.TeamStatus, error) {
				num, next := task.ClaimNumber(team)
				n = num
				return next, nil
			}); err != nil {
				return err // the Team itself is failing; the rest would too
			}
			_, err := m.transition(ctx, storage.Key{Namespace: ns, Name: t.Name}, task.Create{Number: n})
			if err != nil && !isLostCandidate(err) {
				errs = append(errs, fmt.Errorf("%s: %w", t.Name, err))
			}
		}
		return errors.Join(errs...)
	})
}

// Dispatch applies every pending outbox item and acks the applied ones.
// Items are idempotent, so an item applied by an instance that died before
// acking is applied again without effect.
func (m *Manager) Dispatch(ctx context.Context) error {
	return m.forEachTeam(ctx, func(ns string) error {
		tasks, err := m.store.ListTasks(ctx, storage.Query{Namespace: ns})
		if err != nil {
			return err
		}
		var errs []error
		for _, t := range tasks {
			if len(t.Status.Outbox) == 0 {
				continue
			}
			if err := m.dispatchTask(ctx, storage.Key{Namespace: ns, Name: t.Name}); err != nil {
				errs = append(errs, err)
			}
		}
		return errors.Join(errs...)
	})
}

func (m *Manager) dispatchTask(ctx context.Context, key storage.Key) error {
	t, err := m.store.GetTask(ctx, key)
	if errors.Is(err, storage.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	var applied []string
	var errs []error
	for _, it := range t.Status.Outbox {
		if err := m.applyItem(ctx, t, it); err != nil {
			errs = append(errs, fmt.Errorf("outbox item %s: %w", it.ID, err))
			continue
		}
		applied = append(applied, it.ID)
	}
	if len(applied) > 0 {
		if _, err := m.transition(ctx, key, task.AckOutbox{IDs: applied}); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (m *Manager) applyItem(ctx context.Context, parent *v1alpha1.Task, it v1alpha1.OutboxItem) error {
	switch it.Kind {
	case v1alpha1.OutboxCreateSubtask:
		if it.Spec == nil {
			return fmt.Errorf("no spec: %w", ErrInvalid)
		}
		sub := &v1alpha1.Task{
			ObjectMeta: metav1.ObjectMeta{Name: task.SubtaskName(it), Namespace: parent.Namespace},
			Spec:       *it.Spec.DeepCopy(),
		}
		sub.Spec.ParentRef = &v1alpha1.TaskReference{Name: parent.Name}
		err := m.store.CreateTask(ctx, sub)
		if !errors.Is(err, storage.ErrAlreadyExists) {
			return err
		}
		// Applied before, unless the name was taken by something else, such
		// as a runner reusing a ULID under another parent. Then the item
		// stays in the outbox, visibly failing, instead of being dropped.
		existing, err := m.store.GetTask(ctx, storage.Key{Namespace: sub.Namespace, Name: sub.Name})
		if err != nil {
			return err
		}
		if existing.Spec.ParentRef == nil || existing.Spec.ParentRef.Name != parent.Name {
			return fmt.Errorf("task %s exists but is not a subtask of %s: %w", sub.Name, parent.Name, storage.ErrAlreadyExists)
		}
		return nil
	case v1alpha1.OutboxRunEnded:
		if it.Run == nil {
			return fmt.Errorf("no run: %w", ErrInvalid)
		}
		data, err := json.Marshal(it.Run)
		if err != nil {
			return err
		}
		err = m.store.Append(ctx, RunsStream(storage.Key{Namespace: parent.Namespace, Name: parent.Name}), it.ID, data)
		if errors.Is(err, storage.ErrAlreadyExists) {
			return nil
		}
		return err
	default:
		return fmt.Errorf("unknown kind %q: %w", it.Kind, ErrInvalid)
	}
}

// ReclaimExpired takes back every run whose lease passed more than one
// heartbeat interval ago.
func (m *Manager) ReclaimExpired(ctx context.Context) error {
	grace := m.cfg.Heartbeat
	return m.forEachTeam(ctx, func(ns string) error {
		running, err := m.store.ListTasks(ctx, storage.Query{Namespace: ns, Phases: []v1alpha1.Phase{v1alpha1.PhaseRunning}})
		if err != nil {
			return err
		}
		now := m.now()
		var errs []error
		for _, t := range running {
			if t.Status.Run != nil && !now.After(t.Status.Run.LeaseUntil.Add(grace)) {
				continue // still live as far as the index knows
			}
			_, err := m.transition(ctx, storage.Key{Namespace: ns, Name: t.Name}, task.Reclaim{Grace: grace})
			if err != nil && !errors.Is(err, task.ErrLeaseNotExpired) && !isLostCandidate(err) {
				errs = append(errs, fmt.Errorf("%s: %w", t.Name, err))
			}
		}
		return errors.Join(errs...)
	})
}

// teamReconciler corrects a Team's running count from the index. A lost
// release leaves the count too high, which only costs capacity; correcting
// it too eagerly could let a claim overshoot, because a claim holds its slot
// before the index shows the task Running. So a count is only lowered once
// two scans in a row saw the same excess.
type teamReconciler struct {
	excess map[string]int32
}

func (r *teamReconciler) reconcile(ctx context.Context, m *Manager) error {
	if r.excess == nil {
		r.excess = map[string]int32{}
	}
	return m.forEachTeam(ctx, func(ns string) error {
		running, err := m.store.ListTasks(ctx, storage.Query{Namespace: ns, Phases: []v1alpha1.Phase{v1alpha1.PhaseRunning}})
		if err != nil {
			return err
		}
		count := int32(len(running))
		team, err := m.store.GetTeam(ctx, ns)
		if err != nil {
			return err
		}
		excess := team.Status.Running - count
		prev := r.excess[ns]
		r.excess[ns] = excess
		if excess <= 0 || prev <= 0 {
			return nil
		}
		fix := min(excess, prev)
		if err := m.writeTeam(ctx, ns, func(team *v1alpha1.Team) (v1alpha1.TeamStatus, error) {
			s := team.Status
			s.Running = max(s.Running-fix, 0)
			return s, nil
		}); err != nil {
			return err
		}
		// The fixed part is gone; only what is left over counts as seen
		// once. Otherwise the next scan would treat a claim in flight as a
		// second sighting and take its slot.
		r.excess[ns] = excess - fix
		return nil
	})
}

// LoopConfig sets how often the background loops run.
type LoopConfig struct {
	Reclaim  time.Duration // default 5s
	Dispatch time.Duration // default 10s; also numbers new tasks and reconciles Team counts
}

// Run runs the background loops until ctx is done. A nil log logs to
// slog.Default().
func (m *Manager) Run(ctx context.Context, cfg LoopConfig, log *slog.Logger) {
	if log == nil {
		log = slog.Default()
	}
	if cfg.Reclaim <= 0 {
		cfg.Reclaim = 5 * time.Second
	}
	if cfg.Dispatch <= 0 {
		cfg.Dispatch = 10 * time.Second
	}
	reclaim := time.NewTicker(cfg.Reclaim)
	dispatch := time.NewTicker(cfg.Dispatch)
	defer reclaim.Stop()
	defer dispatch.Stop()
	var teams teamReconciler
	for {
		select {
		case <-ctx.Done():
			return
		case <-reclaim.C:
			if err := m.ReclaimExpired(ctx); err != nil {
				log.Warn("reclaim", "err", err)
			}
		case <-dispatch.C:
			if err := m.NumberNew(ctx); err != nil {
				log.Warn("number new tasks", "err", err)
			}
			if err := m.Dispatch(ctx); err != nil {
				log.Warn("dispatch outbox", "err", err)
			}
			if err := teams.reconcile(ctx, m); err != nil {
				log.Warn("reconcile teams", "err", err)
			}
		}
	}
}
