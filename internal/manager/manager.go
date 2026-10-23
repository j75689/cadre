// Package manager is the Task Manager: the only place that changes task
// state. Every method reads the task fresh, runs one transition of the pure
// state machine in internal/task and writes the result with one conditional
// write. It is stateless, so any number of instances can run at once.
package manager

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/oklog/ulid/v2"
	"k8s.io/apimachinery/pkg/api/equality"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	"github.com/j75689/cadre/internal/storage"
	"github.com/j75689/cadre/internal/task"
)

// Config holds the Task Manager's timings and the source of its Agents.
type Config struct {
	// Agents decides which tasks a runner may claim. Without it no task is
	// claimable.
	Agents Agents

	// Lease is how long a claim or heartbeat keeps a run alive.
	Lease time.Duration
	// Heartbeat is how often runners are told to renew the lease.
	Heartbeat time.Duration
}

// DefaultConfig is the configuration used when a field is zero.
var DefaultConfig = Config{Lease: 30 * time.Second, Heartbeat: 10 * time.Second}

// Manager is a Task Manager instance.
type Manager struct {
	store  storage.Driver
	cfg    Config
	agents Agents
	now    func() time.Time
	newID  func() string
}

// Option configures a Manager.
type Option func(*Manager)

// WithClock replaces the wall clock, for tests.
func WithClock(now func() time.Time) Option { return func(m *Manager) { m.now = now } }

// WithIDs replaces the ULID generator, for tests.
func WithIDs(newID func() string) Option { return func(m *Manager) { m.newID = newID } }

// New returns a Manager on store.
func New(store storage.Driver, cfg Config, opts ...Option) *Manager {
	if cfg.Lease <= 0 {
		cfg.Lease = DefaultConfig.Lease
	}
	if cfg.Heartbeat <= 0 {
		cfg.Heartbeat = DefaultConfig.Heartbeat
	}
	if cfg.Agents == nil {
		cfg.Agents = StaticAgents{}
	}
	m := &Manager{store: store, cfg: cfg, agents: cfg.Agents, now: time.Now, newID: func() string { return ulid.Make().String() }}
	for _, o := range opts {
		o(m)
	}
	return m
}

// maxConflicts bounds the read-apply-write retries of one transition. Each
// retry means another writer advanced the object; giving up surfaces a
// retriable error instead of spinning.
const maxConflicts = 20

var (
	// ErrBusy means a transition kept losing conditional writes. It is
	// retriable.
	ErrBusy = errors.New("object is busy; retry")
	// ErrUncertain means a write failed in a way that leaves open whether
	// it landed, and a re-read could not settle it. Callers must not undo
	// anything (such as releasing a Team slot) on this error.
	ErrUncertain = errors.New("write outcome unknown")
)

// transition reads the task, applies ev and writes the result, retrying on
// conflicts. On success it returns the written task. A transition that ends
// a run gives the run's Team slot back after the Task write, which is the
// commit point. If that release is lost, the dispatcher repeats it when it
// applies the run's RunEnded item; releases are idempotent.
//
// A write that fails without a conflict, a timeout say, may still have been
// committed. The task is then read again: if it holds exactly the status
// that was written, the write landed and the transition succeeded, so its
// slot release is not lost; if it holds something else, the write failed.
func (m *Manager) transition(ctx context.Context, key storage.Key, ev task.Event) (*v1alpha1.Task, error) {
	for range maxConflicts {
		t, err := m.store.GetTask(ctx, key)
		if err != nil {
			return nil, err
		}
		var runID string
		if t.Status.Run != nil {
			runID = t.Status.Run.ID
		}
		res, err := task.Apply(t, ev, m.now())
		if err != nil {
			return t, err
		}
		if !res.ReleasesSlot && equality.Semantic.DeepEqual(&t.Status, &res.Status) {
			// Nothing changed (a thread revision already past this entry,
			// say): no write, and no watch event telling anyone otherwise.
			return t, nil
		}
		t.Status = res.Status
		err = m.store.UpdateTaskStatus(ctx, t)
		if errors.Is(err, storage.ErrConflict) {
			continue
		}
		if err != nil {
			cur, landed, rerr := m.landed(ctx, key, ev, &res.Status)
			if rerr != nil {
				return nil, fmt.Errorf("%s %s: %w: %w", task.KindOf(ev), key, ErrUncertain, err)
			}
			if !landed {
				return nil, err
			}
			t = cur
		}
		if res.ReleasesSlot {
			// The dispatcher repeats a release lost here.
			_ = m.releaseSlot(ctx, key.Namespace, runID)
		}
		return t, nil
	}
	return nil, fmt.Errorf("%s %s: %w", task.KindOf(ev), key, ErrBusy)
}

// landed reads the task again after an ambiguous write of ev and reports
// whether the write took effect, returning what it read.
//
// A claim is recognised by its run id, which only that claim can have
// written: comparing whole statuses would mistake a live claim for a lost
// one as soon as anything else touched the task, such as the dispatcher
// acking an outbox item, and its slot would be given back. Other events
// compare the whole status; a wrong "not landed" there only costs the
// caller a retry.
func (m *Manager) landed(ctx context.Context, key storage.Key, ev task.Event, status *v1alpha1.TaskStatus) (*v1alpha1.Task, bool, error) {
	cur, err := m.store.GetTask(ctx, key)
	if err != nil {
		return nil, false, err
	}
	if c, ok := ev.(task.Claim); ok {
		return cur, cur.Status.Run != nil && cur.Status.Run.ID == c.RunID, nil
	}
	return cur, equality.Semantic.DeepEqual(&cur.Status, status), nil
}

// releaseSlot gives runID's slot back to the namespace's Team. It is
// idempotent. A failure is repaired later by the dispatcher or the
// reconciler, so it is only reported, never acted on.
func (m *Manager) releaseSlot(ctx context.Context, namespace, runID string) error {
	err := m.writeTeam(ctx, namespace, func(team *v1alpha1.Team) (v1alpha1.TeamStatus, error) {
		return task.ReleaseSlot(team, runID), nil
	})
	if errors.Is(err, storage.ErrNotFound) {
		return nil // no Team, no slot
	}
	return err
}

// writeTeam reads the namespace's Team, applies change and writes it,
// retrying on conflicts.
func (m *Manager) writeTeam(ctx context.Context, namespace string, change func(*v1alpha1.Team) (v1alpha1.TeamStatus, error)) error {
	for range maxConflicts {
		team, err := m.store.GetTeam(ctx, namespace)
		if err != nil {
			return err
		}
		next, err := change(team)
		if err != nil {
			return err
		}
		if equality.Semantic.DeepEqual(&team.Status, &next) {
			return nil // nothing to write: the change was already in place
		}
		team.Status = next
		err = m.store.UpdateTeamStatus(ctx, team)
		if !errors.Is(err, storage.ErrConflict) {
			return err
		}
	}
	return fmt.Errorf("team %s: %w", namespace, ErrBusy)
}

// Reply is a human answer or approval: a Waiting task goes back to Todo.
func (m *Manager) Reply(ctx context.Context, key storage.Key) error {
	_, err := m.transition(ctx, key, task.Reply{})
	return err
}

// Cancel cancels a task in any phase and returns it. An in-flight run learns
// on its next heartbeat.
func (m *Manager) Cancel(ctx context.Context, key storage.Key) (*v1alpha1.Task, error) {
	return m.transition(ctx, key, task.Cancel{})
}

// requireRunID rejects an empty run id as a malformed request rather than a
// run mismatch.
func requireRunID(runID string) error {
	if runID == "" {
		return fmt.Errorf("runId is required: %w", ErrInvalid)
	}
	return nil
}

// Heartbeat renews the lease of runID on the task.
func (m *Manager) Heartbeat(ctx context.Context, key storage.Key, runID string) (time.Time, error) {
	if err := requireRunID(runID); err != nil {
		return time.Time{}, err
	}
	t, err := m.transition(ctx, key, task.Heartbeat{RunID: runID, Lease: m.cfg.Lease})
	if err != nil {
		return time.Time{}, err
	}
	return t.Status.Run.LeaseUntil.Time, nil
}

// Ready reports whether the store can serve the Task Manager: the index
// answers a cheap query.
func (m *Manager) Ready(ctx context.Context) error {
	_, err := m.store.ListTeams(ctx)
	return err
}

// Config returns the manager's effective configuration.
func (m *Manager) Config() Config { return m.cfg }
