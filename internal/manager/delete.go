package manager

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	"github.com/j75689/cadre/internal/storage"
	"github.com/j75689/cadre/internal/task"
)

// deleteBatch bounds the Tasks one tick deletes in one Team, so a Team
// that turns deleting on with a long history drains over a few ticks.
const deleteBatch = 20

// DeleteFinished deletes, with their log streams, the finished Tasks of
// every Team with spec.deleteFinishedAfter, once they have been finished
// that long: oldest first, up to deleteBatch per Team, a parent only once
// its subtasks are gone. A finished Task without status.finishedAt, from
// before it was set, is stamped now instead, so it goes one full
// deleteFinishedAfter later. A Task marked deleting, by a delete that
// stopped half way here or on another replica, is deleted whatever its age.
func (m *Manager) DeleteFinished(ctx context.Context) error {
	teams, err := m.store.ListTeams(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, team := range teams {
		if team.Spec.DeleteFinishedAfter == nil {
			continue
		}
		if err := m.deleteFinished(ctx, team.Namespace, team.Spec.DeleteFinishedAfter.Duration); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", team.Namespace, err))
		}
	}
	return errors.Join(errs...)
}

// deleteFinished is DeleteFinished for the Team of ns, whose finished
// Tasks go d after they finished.
func (m *Manager) deleteFinished(ctx context.Context, ns string, d time.Duration) error {
	finished, err := m.store.ListTasks(ctx, storage.Query{Namespace: ns, Phases: []v1alpha1.Phase{v1alpha1.PhaseDone, v1alpha1.PhaseCancelled, v1alpha1.PhaseFailed}})
	if err != nil {
		return err
	}
	now := m.now()
	var errs []error
	report := func(name string, err error) {
		if err != nil && !isLostCandidate(err) {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
		}
	}
	var due []v1alpha1.Task
	for _, t := range finished {
		key := storage.Key{Namespace: ns, Name: t.Name}
		switch {
		case t.Status.DeletingSince != nil:
			report(t.Name, m.deleteTask(ctx, key, d))
		case t.Status.FinishedAt == nil:
			_, err := m.transition(ctx, key, task.StampFinished{})
			report(t.Name, err)
		case now.Sub(t.Status.FinishedAt.Time) >= d:
			due = append(due, t)
		}
	}
	slices.SortFunc(due, func(a, b v1alpha1.Task) int {
		return a.Status.FinishedAt.Compare(b.Status.FinishedAt.Time)
	})
	deleted := 0
	for _, t := range due {
		if deleted == deleteBatch {
			break
		}
		if len(t.Status.Outbox) > 0 {
			continue // refused until it is applied; uses none of the batch
		}
		subtasks, err := m.store.ListTasks(ctx, storage.Query{Namespace: ns, Parent: t.Name, Limit: 1})
		if err != nil {
			report(t.Name, err)
			continue
		}
		if len(subtasks) > 0 {
			continue // goes after its subtasks
		}
		deleted++
		report(t.Name, m.deleteTask(ctx, storage.Key{Namespace: ns, Name: t.Name}, d))
	}
	return errors.Join(errs...)
}

// deleteTask deletes a finished task and its log streams, in an order that
// a crash or another replica can pick up at any step: mark the task
// deleting, which no event passes after, so nothing writes to it again;
// let Config.Snapshot save it; delete its streams; delete the object, at
// the version marked, so a task made again under the name is not deleted.
// A task whose outbox is not applied yet stays for a later tick. The
// TaskDeleted Event is recorded only by the replica whose delete landed.
func (m *Manager) deleteTask(ctx context.Context, key storage.Key, d time.Duration) error {
	t, err := m.transition(ctx, key, task.Delete{})
	switch {
	case errors.Is(err, task.ErrDeleting):
		// Marked before: finish that delete.
	case errors.Is(err, task.ErrInvalidTransition):
		return nil
	case err != nil:
		return err
	}
	if m.cfg.Snapshot != nil {
		if err := m.cfg.Snapshot(ctx, t); err != nil {
			return fmt.Errorf("snapshot: %w", err)
		}
	}
	if err := m.store.DeleteStream(ctx, key.Namespace+"/"+key.Name); err != nil {
		return err
	}
	for range maxConflicts {
		err := m.store.DeleteTask(ctx, t)
		switch {
		case err == nil:
			m.recordDeleted(t, d)
			return nil
		case errors.Is(err, storage.ErrNotFound):
			return nil // another replica's delete landed
		case !errors.Is(err, storage.ErrConflict):
			return err
		}
		if t, err = m.store.GetTask(ctx, key); err != nil {
			if errors.Is(err, storage.ErrNotFound) {
				return nil
			}
			return err
		}
		if t.Status.DeletingSince == nil {
			return nil // made again under the name, after a delete landed
		}
	}
	return fmt.Errorf("delete task %s: %w", key, ErrBusy)
}

// recordDeleted records the TaskDeleted Event of t, as read before its
// delete. The recorder queues it, so a refused Event fails no delete.
func (m *Manager) recordDeleted(t *v1alpha1.Task, d time.Duration) {
	if m.cfg.Events == nil {
		return
	}
	m.cfg.Events.Eventf(t, nil, corev1.EventTypeNormal, "TaskDeleted", "Delete",
		"Task #%d %s finished %s (%s), deleted %s later by Team.spec.deleteFinishedAfter",
		t.Status.Number, t.Name, t.Status.FinishedAt.UTC().Format(time.RFC3339), t.Status.Phase, shortDuration(d))
}
