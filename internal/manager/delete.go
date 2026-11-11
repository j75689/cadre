package manager

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	"github.com/j75689/cadre/internal/storage"
	"github.com/j75689/cadre/internal/task"
)

// deleteBatch bounds the Tasks one tick deletes in one Team, so a Team
// that turns deleting on with a long history drains over a few ticks.
const deleteBatch = 20

// DeleteFinished deletes, with their log streams, the finished Tasks of
// every Team with spec.archiveAfter or spec.deleteFinishedAfter, once they
// have been finished that long: oldest first, up to deleteBatch per Team, a
// parent only once its subtasks are gone. With archiveAfter, which wins
// over deleteFinishedAfter, each is archived: a snapshot of it is written
// first. A finished Task without status.finishedAt, from before it was set,
// is stamped now instead, so it goes one full period later. A Task marked
// deleting, by a delete that stopped half way here or on another replica,
// is deleted whatever its age. A Task whose delete landed but that a
// finalizer holds is left alone. Then the snapshots of every Team past its
// spec.archiveRetention, or DefaultArchiveRetention, are removed, whether
// it archives or not; a failure there holds up no delete.
func (m *Manager) DeleteFinished(ctx context.Context) error {
	teams, err := m.store.ListTeams(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, team := range teams {
		var err error
		switch spec := team.Spec; {
		case spec.ArchiveAfter != nil:
			err = m.deleteFinished(ctx, team.Namespace, spec.ArchiveAfter.Duration, true)
		case spec.DeleteFinishedAfter != nil:
			err = m.deleteFinished(ctx, team.Namespace, spec.DeleteFinishedAfter.Duration, false)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", team.Namespace, err))
		}
		if err := m.expireSnapshots(ctx, team.Namespace, archiveRetention(team.Spec)); err != nil {
			errs = append(errs, fmt.Errorf("%s: expire snapshots: %w", team.Namespace, err))
		}
	}
	return errors.Join(errs...)
}

// deleteFinished is DeleteFinished for the Team of ns, whose finished
// Tasks go d after they finished, archived if archive.
func (m *Manager) deleteFinished(ctx context.Context, ns string, d time.Duration, archive bool) error {
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
		case t.DeletionTimestamp != nil:
			// Held by a finalizer: deleted already, it goes when that clears.
		case t.Status.DeletingSince != nil:
			report(t.Name, m.deleteTask(ctx, key, d, archive))
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
		report(t.Name, m.deleteTask(ctx, storage.Key{Namespace: ns, Name: t.Name}, d, archive))
	}
	return errors.Join(errs...)
}

// deleteTask deletes a finished task and its log streams, in an order that
// a crash or another replica can pick up at any step: mark the task
// deleting, which no event passes after, so nothing writes to it again,
// and if archive with the mark saying whether a snapshot is written, as
// its logs measured just before; if so, write it, and stop there if that
// fails, nothing deleted; delete its streams; delete the object, at the
// version marked, so a task made again under the name is not deleted. A
// delete picked up on a mark with a snapshot writes it, archived as
// marked, even if it does not archive itself, or d after it finished if
// the mark's message does not say how long. An archive picked up on a
// mark without a snapshot writes none: the mark said so, or a delete that
// did not archive made it, and may have deleted part of the logs. A task whose outbox is not applied yet stays for a
// later tick. The task's Event is recorded only by the replica whose
// delete landed.
func (m *Manager) deleteTask(ctx context.Context, key storage.Key, d time.Duration, archive bool) error {
	mark := task.Delete{Archive: archive, ArchiveAfter: d}
	if archive {
		skipped, err := m.overCap(ctx, key)
		if err != nil {
			return fmt.Errorf("snapshot: %w", err)
		}
		mark.Skipped = skipped
	}
	t, err := m.transition(ctx, key, mark)
	switch {
	case errors.Is(err, task.ErrDeleting):
		// Marked before: finish that delete.
	case errors.Is(err, task.ErrInvalidTransition):
		return nil
	case err != nil:
		return err
	}
	if t.DeletionTimestamp != nil {
		return nil // a delete landed, read before the list saw it
	}
	var skipped string
	switch c := meta.FindStatusCondition(t.Status.Conditions, task.ConditionSnapshot); {
	case c != nil && c.Status == metav1.ConditionTrue:
		// Even a delete that does not archive: another replica may be
		// writing the snapshot, from the logs this one would delete.
		// A mark it cannot read still gets its snapshot, under the d of
		// this delete, which only the archiveAfter recorded gets wrong.
		if marked, err := task.MarkedArchiveAfter(c); err != nil {
			m.log.Warn("cannot read the archiveAfter of a Snapshot mark: the snapshot is written under this delete's", "task", key.String(), "archiveAfter", shortDuration(d), "err", err)
		} else {
			d = marked
		}
		archive = true
		if err := m.snapshot(ctx, t, d); err != nil {
			return fmt.Errorf("snapshot: %w", err)
		}
	case !archive:
	case c == nil:
		skipped = "its delete began under Team.spec.deleteFinishedAfter, so part of its logs may be gone"
	default:
		skipped = c.Message
	}
	if err := m.store.DeleteStream(ctx, key.Namespace+"/"+key.Name); err != nil {
		return err
	}
	for range maxConflicts {
		err := m.store.DeleteTask(ctx, t)
		switch {
		case err == nil:
			m.recordDeleted(t, d, archive, skipped, m.retention(ctx, key.Namespace, archive && skipped == ""))
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
		if t.DeletionTimestamp != nil {
			return nil // another replica's delete landed, held by a finalizer
		}
	}
	return fmt.Errorf("delete task %s: %w", key, ErrBusy)
}

// archiveRetention is how long the Team of spec keeps a snapshot.
func archiveRetention(spec v1alpha1.TeamSpec) time.Duration {
	if r := spec.ArchiveRetention; r != nil {
		return r.Duration
	}
	return DefaultArchiveRetention
}

// retention is archiveRetention of the Team of ns, for an Event's note,
// if needed: read from the index, or DefaultArchiveRetention if it cannot
// be.
func (m *Manager) retention(ctx context.Context, ns string, needed bool) time.Duration {
	if !needed {
		return 0
	}
	team, err := m.Team(ctx, ns)
	if err != nil {
		m.log.Warn("cannot read the Team: its TaskArchived Event names the default retention", "namespace", ns, "err", err)
		return DefaultArchiveRetention
	}
	return archiveRetention(team.Spec)
}

// recordDeleted records the Event of t, as read before its delete:
// TaskDeleted, or if archive TaskArchived, with the snapshot's retention,
// or SnapshotSkipped when the snapshot was skipped for the reason skipped.
// The recorder queues it, so a refused Event fails no delete.
func (m *Manager) recordDeleted(t *v1alpha1.Task, d time.Duration, archive bool, skipped string, retention time.Duration) {
	if m.cfg.Events == nil {
		return
	}
	finished := t.Status.FinishedAt.UTC().Format(time.RFC3339)
	switch {
	case !archive:
		m.cfg.Events.Eventf(t, nil, corev1.EventTypeNormal, "TaskDeleted", "Delete",
			"Task #%d %s finished %s (%s), deleted %s later by Team.spec.deleteFinishedAfter",
			t.Status.Number, t.Name, finished, t.Status.Phase, shortDuration(d))
	case skipped != "":
		m.cfg.Events.Eventf(t, nil, corev1.EventTypeWarning, "SnapshotSkipped", "Archive",
			"Task #%d %s finished %s (%s), deleted %s later by Team.spec.archiveAfter without a snapshot: %s",
			t.Status.Number, t.Name, finished, t.Status.Phase, shortDuration(d), skipped)
	default:
		m.cfg.Events.Eventf(t, nil, corev1.EventTypeNormal, "TaskArchived", "Archive",
			"Task #%d %s finished %s (%s), archived %s later by Team.spec.archiveAfter; its snapshot is kept for %s",
			t.Status.Number, t.Name, finished, t.Status.Phase, shortDuration(d), shortDuration(retention))
	}
}
