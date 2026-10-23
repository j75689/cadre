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
		// The transition that ended the run released its slot already,
		// unless that release was lost; releasing again is a no-op.
		if err := m.releaseSlot(ctx, parent.Namespace, it.ID); err != nil {
			return err
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

// ReconcileTeams repairs the Teams' slot sets. Like every transition, it
// decides only on fresh reads of the Task a slot names, never on the index:
// the index only proposes candidates, and an eventually consistent count can
// never tell a claim in flight from a lost release.
//
//   - A slot whose task does not hold its run is given back once it is older
//     than twice the lease. Its claim never landed, or the run ended and both
//     of its releases were lost. A younger slot may belong to a claim in
//     flight, so it is left alone.
//   - A run that is Running without a slot is added back, past the cap if
//     need be, so the count never hides a run that is visibly running.
func (m *Manager) ReconcileTeams(ctx context.Context) error {
	return m.forEachTeam(ctx, func(ns string) error {
		team, err := m.store.GetTeam(ctx, ns)
		if err != nil {
			return err
		}
		now := m.now()
		var errs []error
		for _, sl := range team.Status.Slots {
			if now.Sub(sl.AcquiredAt.Time) <= 2*m.cfg.Lease {
				continue
			}
			t, err := m.store.GetTask(ctx, storage.Key{Namespace: ns, Name: sl.Task})
			switch {
			case errors.Is(err, storage.ErrNotFound):
			case err != nil:
				errs = append(errs, err)
				continue
			case t.Status.Run != nil && t.Status.Run.ID == sl.RunID:
				continue // the run is live
			}
			if err := m.releaseSlot(ctx, ns, sl.RunID); err != nil {
				errs = append(errs, err)
			}
		}

		running, err := m.store.ListTasks(ctx, storage.Query{Namespace: ns, Phases: []v1alpha1.Phase{v1alpha1.PhaseRunning}})
		if err != nil {
			return errors.Join(append(errs, err)...)
		}
		for _, cand := range running {
			if cand.Status.Run == nil || task.HoldsSlot(team, cand.Status.Run.ID) {
				continue
			}
			t, err := m.store.GetTask(ctx, storage.Key{Namespace: ns, Name: cand.Name})
			if err != nil {
				if !errors.Is(err, storage.ErrNotFound) {
					errs = append(errs, err)
				}
				continue
			}
			if t.Status.Phase != v1alpha1.PhaseRunning || t.Status.Run == nil {
				continue
			}
			runID := t.Status.Run.ID
			if err := m.writeTeam(ctx, ns, func(team *v1alpha1.Team) (v1alpha1.TeamStatus, error) {
				return task.AddSlot(team, runID, t.Name, now), nil
			}); err != nil {
				errs = append(errs, err)
			}
		}
		return errors.Join(errs...)
	})
}

// LoopConfig sets how often the background loops run.
type LoopConfig struct {
	Reclaim  time.Duration // how often expired leases are reclaimed
	Dispatch time.Duration // how often the outbox is applied, new tasks numbered and Team counts reconciled
}

// DefaultLoopConfig is used for any zero field of a LoopConfig.
var DefaultLoopConfig = LoopConfig{Reclaim: 5 * time.Second, Dispatch: 10 * time.Second}

// Run runs the background loops until ctx is done. A nil log logs to
// slog.Default().
func (m *Manager) Run(ctx context.Context, cfg LoopConfig, log *slog.Logger) {
	if log == nil {
		log = slog.Default()
	}
	if cfg.Reclaim <= 0 {
		cfg.Reclaim = DefaultLoopConfig.Reclaim
	}
	if cfg.Dispatch <= 0 {
		cfg.Dispatch = DefaultLoopConfig.Dispatch
	}
	reclaim := time.NewTicker(cfg.Reclaim)
	dispatch := time.NewTicker(cfg.Dispatch)
	defer reclaim.Stop()
	defer dispatch.Stop()
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
			if err := m.ReconcileTeams(ctx); err != nil {
				log.Warn("reconcile teams", "err", err)
			}
		}
	}
}
