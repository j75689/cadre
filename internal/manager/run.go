package manager

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync/atomic"
	"time"

	"github.com/oklog/ulid/v2"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	"github.com/j75689/cadre/internal/storage"
	"github.com/j75689/cadre/internal/task"
)

// ErrInvalid means a request is malformed.
var ErrInvalid = errors.New("invalid request")

// ClaimRequest asks for one task to work on.
type ClaimRequest struct {
	Runner     string
	Namespaces []string          // empty means every Team namespace
	Labels     map[string]string // matched against Agent selectors from M1; ignored in M0
}

// Claimed is a task handed to a runner.
type Claimed struct {
	Task       *v1alpha1.Task
	RunID      string
	Mode       v1alpha1.RunMode
	LeaseUntil time.Time
}

// rotation spreads claims across namespaces, so one busy team cannot starve
// the others.
var rotation atomic.Uint64

// Claim hands the most urgent claimable task to the runner, or returns nil
// when there is none. A task is claimable when it is Todo and assigned to an
// Agent, and its Team has a free slot.
//
// The index only proposes candidates. Each one is taken with two conditional
// writes: a Team slot first, then the Task claim; if the claim loses, the
// slot is given back and the next candidate is tried.
func (m *Manager) Claim(ctx context.Context, req ClaimRequest) (*Claimed, error) {
	if req.Runner == "" {
		return nil, fmt.Errorf("runner is required: %w", ErrInvalid)
	}
	namespaces, err := m.namespaces(ctx, req.Namespaces)
	if err != nil {
		return nil, err
	}
	if len(namespaces) == 0 {
		return nil, nil
	}
	start := int(rotation.Add(1) % uint64(len(namespaces)))
	for i := range namespaces {
		ns := namespaces[(start+i)%len(namespaces)]
		claimed, err := m.claimIn(ctx, ns, req.Runner)
		if claimed != nil || err != nil {
			return claimed, err
		}
	}
	return nil, nil
}

// namespaces returns the requested namespaces that have a Team, or all of
// them.
func (m *Manager) namespaces(ctx context.Context, want []string) ([]string, error) {
	teams, err := m.store.ListTeams(ctx)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, t := range teams {
		if len(want) == 0 || slices.Contains(want, t.Namespace) {
			out = append(out, t.Namespace)
		}
	}
	return out, nil
}

func (m *Manager) claimIn(ctx context.Context, ns, runner string) (*Claimed, error) {
	todo, err := m.store.ListTasks(ctx, storage.Query{Namespace: ns, Phases: []v1alpha1.Phase{v1alpha1.PhaseTodo}})
	if err != nil {
		return nil, err
	}
	for _, cand := range todo {
		if cand.Spec.Assignee == nil || cand.Spec.Assignee.Kind != v1alpha1.AssigneeAgent {
			continue
		}
		err := m.writeTeam(ctx, ns, task.AcquireSlot)
		if errors.Is(err, task.ErrNoSlot) || errors.Is(err, storage.ErrNotFound) {
			return nil, nil // the team is full or gone: try the next namespace
		}
		if err != nil {
			return nil, err
		}
		runID := m.newID()
		t, err := m.transition(ctx, storage.Key{Namespace: ns, Name: cand.Name}, task.Claim{
			RunID: runID, Mode: v1alpha1.RunModeRuntime, Runner: runner, Lease: m.cfg.Lease,
		})
		if err == nil {
			return &Claimed{Task: t, RunID: runID, Mode: v1alpha1.RunModeRuntime, LeaseUntil: t.Status.Run.LeaseUntil.Time}, nil
		}
		// Lost the task (someone else claimed it, it was cancelled or
		// deleted, the index was stale): give the slot back and move on.
		_ = m.writeTeam(ctx, ns, func(team *v1alpha1.Team) (v1alpha1.TeamStatus, error) {
			return task.ReleaseSlot(team), nil
		})
		if !isLostCandidate(err) {
			return nil, err
		}
	}
	return nil, nil
}

func isLostCandidate(err error) bool {
	return errors.Is(err, task.ErrInvalidTransition) || errors.Is(err, task.ErrUnassigned) || errors.Is(err, storage.ErrNotFound)
}

// Subtask is a subtask a run asks for.
type Subtask struct {
	ID   string // optional ULID; minted when empty
	Spec v1alpha1.TaskSpec
}

// FinishRequest ends a run.
type FinishRequest struct {
	RunID     string
	Step      int64 // step mode only
	Outcome   task.Outcome
	Reason    v1alpha1.WaitingReason
	ThreadSeq int64
	Subtasks  []Subtask
}

// Finish ends the run. If this same finish already landed (a retry), it
// reports alreadyFinished instead of a run mismatch.
func (m *Manager) Finish(ctx context.Context, key storage.Key, req FinishRequest) (phase v1alpha1.Phase, alreadyFinished bool, err error) {
	items := make([]v1alpha1.OutboxItem, 0, len(req.Subtasks))
	for _, st := range req.Subtasks {
		id := st.ID
		if id == "" {
			id = m.newID()
		} else if _, err := ulid.ParseStrict(id); err != nil {
			return "", false, fmt.Errorf("subtask id %q: %w", id, ErrInvalid)
		}
		items = append(items, v1alpha1.OutboxItem{ID: id, Kind: v1alpha1.OutboxCreateSubtask, Spec: st.Spec.DeepCopy()})
	}
	t, err := m.transition(ctx, key, task.Finish{
		RunID: req.RunID, Step: req.Step, Outcome: req.Outcome,
		Reason: req.Reason, ThreadSeq: req.ThreadSeq, Outbox: items,
	})
	if errors.Is(err, task.ErrRunMismatch) && t != nil {
		rec, lookupErr := m.endedRun(ctx, t, req.RunID)
		if lookupErr != nil {
			return "", false, lookupErr
		}
		if rec != nil && rec.Reason == v1alpha1.RunFinished && rec.Outcome == req.Outcome {
			return t.Status.Phase, true, nil
		}
	}
	if err != nil {
		return "", false, err
	}
	return t.Status.Phase, false, nil
}

// RunsStream is the log stream holding a task's run history.
func RunsStream(key storage.Key) string { return key.Namespace + "/" + key.Name + "/runs" }

// EventsStream is the log stream holding one run's progress events.
func EventsStream(key storage.Key, runID string) string {
	return RunsStream(key) + "/" + runID + "/events"
}

// endedRun finds how runID ended: in the task's outbox if the dispatcher has
// not applied it yet, otherwise in the run history.
func (m *Manager) endedRun(ctx context.Context, t *v1alpha1.Task, runID string) (*v1alpha1.RunRecord, error) {
	for _, it := range t.Status.Outbox {
		if it.Kind == v1alpha1.OutboxRunEnded && it.ID == runID {
			return it.Run, nil
		}
	}
	entries, err := m.store.Read(ctx, RunsStream(storage.Key{Namespace: t.Namespace, Name: t.Name}), "", 0)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if e.Key != runID {
			continue
		}
		rec := &v1alpha1.RunRecord{}
		if err := json.Unmarshal(e.Data, rec); err != nil {
			return nil, fmt.Errorf("run history entry %s: %w", runID, err)
		}
		return rec, nil
	}
	return nil, nil
}

// Event is a runner's progress report.
type Event struct {
	RunID         string `json:"runId"`
	Seq           int64  `json:"seq"`
	InputTokens   int64  `json:"inputTokens,omitempty"`
	OutputTokens  int64  `json:"outputTokens,omitempty"`
	ContextTokens int64  `json:"contextTokens,omitempty"`
	Progress      string `json:"progress,omitempty"`
}

// RecordEvent appends a progress event to the run's event log. A duplicate
// sequence number is accepted, so retries are harmless. It never writes the
// Task.
func (m *Manager) RecordEvent(ctx context.Context, key storage.Key, ev Event) error {
	if ev.Seq < 1 {
		return fmt.Errorf("seq %d: must be at least 1: %w", ev.Seq, ErrInvalid)
	}
	t, err := m.store.GetTask(ctx, key)
	if err != nil {
		return err
	}
	if t.Status.Run == nil || t.Status.Run.ID != ev.RunID {
		return task.ErrRunMismatch
	}
	data, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	err = m.store.Append(ctx, EventsStream(key, ev.RunID), storage.SeqKey(ev.Seq), data)
	if errors.Is(err, storage.ErrAlreadyExists) {
		return nil
	}
	return err
}
