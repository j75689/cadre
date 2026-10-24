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
	// Runner is the name the runner gives itself. Identity is who it
	// authenticated as, and holds the run; on a server that does not
	// authenticate runners it is empty, and so is status.run.runner.
	Runner     string
	Identity   string
	Namespaces []string          // empty means every Team namespace
	Labels     map[string]string // decide which agents' tasks the runner may take (see claimableBy)
	// Scope, when set, limits the claim to its namespaces, each with its
	// own labels in place of Labels.
	Scope map[string]map[string]string
	// Mode is runtime (the default) or step: a step-mode claim takes one
	// step of a builtin agent's step-mode run, a runtime claim a whole run
	// of any other.
	Mode v1alpha1.RunMode
}

// Claimed is a task handed to a runner.
type Claimed struct {
	Task       *v1alpha1.Task
	RunID      string
	Mode       v1alpha1.RunMode
	LeaseUntil time.Time
	// Agent is the task's assignee, as read for the claim decision, and
	// ModelProvider its model, when it exists.
	Agent         *v1alpha1.Agent
	ModelProvider *v1alpha1.ModelProvider
	// MCPToken is the run's per-run MCP token. It exists only here: the
	// task stores its hash. In step mode it is the step's, and dies when
	// the step is committed or released.
	MCPToken string
	// Step, Attempt and Kind, in step mode: the step taken, how many
	// workers took it (this one included), and what it is, as the server
	// reads it from the transcript.
	Step    int64
	Attempt int32
	Kind    StepKind
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
	teams, err := m.store.ListTeams(ctx)
	if err != nil {
		return nil, err
	}
	scoped := func(ns string) bool {
		if req.Scope == nil {
			return true
		}
		_, ok := req.Scope[ns]
		return ok
	}
	namespaces := slices.DeleteFunc(teamNamespaces(teams, req.Namespaces, true), func(ns string) bool { return !scoped(ns) })
	if req.Mode == v1alpha1.RunModeStep {
		// A step of a run already going comes before a new run: finish
		// what was started. Those need no room in the Team.
		going := slices.DeleteFunc(teamNamespaces(teams, req.Namespaces, false), func(ns string) bool { return !scoped(ns) })
		if len(going) > 0 {
			start := int(rotation.Add(1) % uint64(len(going)))
			for i := range going {
				if claimed, err := m.takeStepIn(ctx, going[(start+i)%len(going)], req); claimed != nil || err != nil {
					return claimed, err
				}
			}
		}
	}
	if len(namespaces) == 0 {
		return nil, nil
	}
	start := int(rotation.Add(1) % uint64(len(namespaces)))
	for i := range namespaces {
		ns := namespaces[(start+i)%len(namespaces)]
		claimed, err := m.claimIn(ctx, ns, req)
		if claimed != nil || err != nil {
			return claimed, err
		}
	}
	return nil, nil
}

// teamNamespaces returns the requested namespaces that have a Team, or
// all of them. With withRoom, namespaces whose Team the index shows full are left
// out, so a full team costs no API reads; the conditional slot write still
// decides.
func teamNamespaces(teams []v1alpha1.Team, want []string, withRoom bool) []string {
	var out []string
	for _, t := range teams {
		if withRoom && len(t.Status.Slots) >= int(max(t.Spec.MaxConcurrentRuns, 1)) {
			continue
		}
		if len(want) == 0 || slices.Contains(want, t.Namespace) {
			out = append(out, t.Namespace)
		}
	}
	return out
}

func (m *Manager) claimIn(ctx context.Context, ns string, req ClaimRequest) (*Claimed, error) {
	// The run is held by the identity; claimed without one, by nobody in
	// particular, and runnerName alone says who.
	labels, holder := req.Labels, req.Identity
	if req.Scope != nil {
		labels = req.Scope[ns]
	}
	todo, err := m.store.ListTasks(ctx, storage.Query{Namespace: ns, Phases: []v1alpha1.Phase{v1alpha1.PhaseTodo}})
	if err != nil {
		return nil, err
	}
	for _, cand := range todo {
		// Pre-filters only: the Claim transition decides on a fresh read of
		// the task, and the agent is read fresh below.
		if cand.Spec.Assignee == nil || cand.Spec.Assignee.Kind != v1alpha1.AssigneeAgent {
			continue
		}
		// The cache may say no; anything else it says (an error, such as
		// a cache still syncing) is settled by the fresh read below.
		cached, err := m.agents.CachedAgent(ctx, ns, cand.Spec.Assignee.Name)
		if errors.Is(err, storage.ErrNotFound) || (err == nil && (!claimableBy(cached, labels) || !inMode(cached, req.Mode))) {
			continue
		}
		// The decision: fresh reads of the agent and its model. A missing
		// or unready agent, one this runner may not run, or a missing
		// model makes the task unclaimable for now; the claim moves on.
		agent, err := m.agents.Agent(ctx, ns, cand.Spec.Assignee.Name)
		if errors.Is(err, storage.ErrNotFound) || (err == nil && (!claimableBy(agent, labels) || !inMode(agent, req.Mode))) {
			continue
		}
		if err != nil {
			return nil, err
		}
		model, err := m.agents.ModelProvider(ctx, ns, agent.Spec.ModelRef)
		if errors.Is(err, storage.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		// The run id is minted first, so the slot is the run's from the start
		// and every release names it.
		runID := m.newID()
		key := storage.Key{Namespace: ns, Name: cand.Name}
		token, tokenHash, err := mintToken(key)
		if err != nil {
			return nil, err
		}
		err = m.writeTeam(ctx, ns, func(team *v1alpha1.Team) (v1alpha1.TeamStatus, error) {
			return task.AcquireSlot(team, runID, cand.Name, m.now())
		})
		if errors.Is(err, task.ErrNoSlot) || errors.Is(err, storage.ErrNotFound) {
			return nil, nil // the team is full or gone: try the next namespace
		}
		if err != nil {
			return nil, err
		}
		t, err := m.transition(ctx, key, task.Claim{
			RunID: runID, Mode: runMode(req.Mode), Runner: holder, RunnerName: req.Runner, Lease: m.cfg.Lease,
			Agent: agent.Name, MCPTokenHash: tokenHash,
		})
		if err == nil && t.Status.Run != nil && t.Status.Run.ID == runID {
			c := &Claimed{
				Task: t, RunID: runID, Mode: t.Status.Run.Mode, LeaseUntil: t.Status.Run.LeaseUntil.Time,
				Agent: agent, ModelProvider: model, MCPToken: token,
			}
			if c.Mode == v1alpha1.RunModeStep {
				c.Attempt, c.Kind = t.Status.Run.Attempt, StepStart
			}
			return c, nil
		}
		if err == nil {
			// The claim landed, but the run was ended (cancelled, say)
			// before it could be handed out. That transition released the
			// slot; releasing it again below is a no-op.
			err = fmt.Errorf("claim of %s ended before it was handed out: %w", cand.Name, task.ErrInvalidTransition)
		}
		if errors.Is(err, ErrUncertain) {
			// The claim may have landed: keep the slot. If it did not, the
			// reconciler gives the slot back once it is old enough.
			return nil, err
		}
		// Lost the task (someone else claimed it, it was cancelled,
		// reassigned or deleted, the index was stale) or the write failed:
		// give the slot back. If the claim did land after all and was then
		// ended by a cancel or reclaim, that transition releases the same
		// run id, which is a no-op.
		_ = m.releaseSlot(ctx, ns, runID)
		if !isLostCandidate(err) {
			return nil, err
		}
	}
	return nil, nil
}

// inMode reports whether agent's runs are driven in mode: a step-mode
// claim takes builtin agents set to step execution only, a runtime claim
// every other agent.
func inMode(agent *v1alpha1.Agent, mode v1alpha1.RunMode) bool {
	return agent.Spec.Runtime.StepMode() == (mode == v1alpha1.RunModeStep)
}

func runMode(mode v1alpha1.RunMode) v1alpha1.RunMode {
	if mode == v1alpha1.RunModeStep {
		return mode
	}
	return v1alpha1.RunModeRuntime
}

func isLostCandidate(err error) bool {
	return errors.Is(err, task.ErrInvalidTransition) || errors.Is(err, task.ErrNotClaimable) || errors.Is(err, storage.ErrNotFound)
}

// subtaskItems are the outbox items creating subtasks.
func (m *Manager) subtaskItems(subtasks []Subtask) ([]v1alpha1.OutboxItem, error) {
	items := make([]v1alpha1.OutboxItem, 0, len(subtasks))
	for _, st := range subtasks {
		id := st.ID
		if id == "" {
			id = m.newID()
		} else if _, err := ulid.ParseStrict(id); err != nil {
			return nil, fmt.Errorf("subtask id %q: %w", id, ErrInvalid)
		}
		items = append(items, v1alpha1.OutboxItem{ID: id, Kind: v1alpha1.OutboxCreateSubtask, Spec: st.Spec.DeepCopy()})
	}
	return items, nil
}

// Subtask is a subtask a run asks for.
type Subtask struct {
	ID   string // optional ULID; minted when empty
	Spec v1alpha1.TaskSpec
}

// FinishRequest ends a run.
type FinishRequest struct {
	RunID string
	// Runner is the authenticated identity sending the finish; empty on a
	// server that does not authenticate runners.
	Runner    string
	Step      int64 // step mode only: the held step
	Attempt   int32 // step mode only: the attempt at it
	Outcome   task.Outcome
	Reason    v1alpha1.WaitingReason
	ThreadSeq int64
	Subtasks  []Subtask
	// WaitingItem and Outbox, for a finish that waits on an entry it
	// writes: the entry's fenced outbox item, and the item's id.
	WaitingItem string
	Outbox      []v1alpha1.OutboxItem
}

// Finish ends the run. If this same finish already landed (a retry), it
// reports alreadyFinished instead of a run mismatch.
func (m *Manager) Finish(ctx context.Context, key storage.Key, req FinishRequest) (phase v1alpha1.Phase, alreadyFinished bool, err error) {
	if err := requireRunID(req.RunID); err != nil {
		return "", false, err
	}
	items, err := m.subtaskItems(req.Subtasks)
	if err != nil {
		return "", false, err
	}
	t, err := m.transition(ctx, key, task.Finish{
		RunID: req.RunID, Runner: req.Runner, Step: req.Step, Attempt: req.Attempt, Outcome: req.Outcome,
		Reason: req.Reason, ThreadSeq: req.ThreadSeq, WaitingItem: req.WaitingItem, Outbox: append(items, req.Outbox...),
	})
	if errors.Is(err, task.ErrRunMismatch) && t != nil {
		rec, lookupErr := m.endedRun(ctx, t, req.RunID)
		if lookupErr != nil {
			return "", false, lookupErr
		}
		if rec != nil && !task.HeldBy(rec.Runner, rec.RunnerName, req.Runner) {
			return "", false, task.ErrForbidden
		}
		if rec != nil && rec.Reason == v1alpha1.RunFinished && rec.Outcome == req.Outcome {
			// The task may have moved on since (a reply, a new claim), so
			// report the phase this finish produced, not the current one.
			return outcomePhase[req.Outcome], true, nil
		}
	}
	if err != nil {
		return "", false, err
	}
	return t.Status.Phase, false, nil
}

// outcomePhase is the phase each finish outcome leads to.
var outcomePhase = map[task.Outcome]v1alpha1.Phase{
	task.OutcomeDone:    v1alpha1.PhaseDone,
	task.OutcomeReview:  v1alpha1.PhaseReview,
	task.OutcomeFailed:  v1alpha1.PhaseFailed,
	task.OutcomeWaiting: v1alpha1.PhaseWaiting,
}

// RunsStream is the log stream holding a task's run history.
func RunsStream(key storage.Key) string { return key.Namespace + "/" + key.Name + "/runs" }

// EventsStream is the log stream holding one run's progress events. It is
// not nested under RunsStream, whose entry keys are run ids: a stream must
// never sit under a path that is an entry of another (see storage.Log).
func EventsStream(key storage.Key, runID string) string {
	return key.Namespace + "/" + key.Name + "/events/" + runID
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
	RunID        string `json:"runId"`
	Seq          int64  `json:"seq"`
	InputTokens  int64  `json:"inputTokens,omitempty"`
	OutputTokens int64  `json:"outputTokens,omitempty"`
	// Prompt-cache tokens, apart from InputTokens.
	CacheReadInputTokens     int64 `json:"cacheReadInputTokens,omitempty"`
	CacheCreationInputTokens int64 `json:"cacheCreationInputTokens,omitempty"`
	ContextTokens            int64 `json:"contextTokens,omitempty"`
	// Runner is the authenticated identity sending the event; it is
	// checked, not stored.
	Runner   string `json:"-"`
	Progress string `json:"progress,omitempty"`
}

// RecordEvent appends a progress event to the run's event log. A duplicate
// sequence number is accepted, so retries are harmless. It never writes the
// Task.
func (m *Manager) RecordEvent(ctx context.Context, key storage.Key, ev Event) error {
	if err := requireRunID(ev.RunID); err != nil {
		return err
	}
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
	if !task.HeldBy(t.Status.Run.Runner, t.Status.Run.RunnerName, ev.Runner) {
		return task.ErrForbidden
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
