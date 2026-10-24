package manager

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/oklog/ulid/v2"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	"github.com/j75689/cadre/internal/storage"
	"github.com/j75689/cadre/internal/task"
)

// A step-mode run is driven a step at a time by whichever worker takes the
// step: the Task is the queue. Its conversation lives in the transcript, a
// log stream per run, <ns>/<task>/transcript/<runId>, with one committed
// entry per step at SeqKey(step). A worker appends the step's entry,
// create-only, before it commits the step with Advance; a worker that takes
// a step whose entry is there already (its previous worker died before
// committing) adopts that entry and commits it without doing the step
// again. A failed attempt leaves a diagnostic entry at
// SeqKey(step)+"-attempt-"+k, which sorts after the step's own key and is
// never taken for it: a step's entry is found by its exact key only.

// StepKind is what a step does.
type StepKind string

// Step kinds.
const (
	StepStart     StepKind = "start"      // step 0: set up the conversation
	StepLLMTurn   StepKind = "llm_turn"   // one model response
	StepExecTools StepKind = "exec_tools" // the calls the last turn asked for
	StepFinish    StepKind = "finish"     // the last turn asked for none: finish the run
)

// StepCall is a tool call a model turn asked for, with its input as the
// model wrote it.
type StepCall struct {
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input,omitempty"`
}

// TranscriptEntry is one committed step. Messages and Request are the
// provider's own JSON, kept as it is, so what the model returned (thinking
// blocks and their signatures) goes back unchanged.
type TranscriptEntry struct {
	Step int64    `json:"step"`
	Kind StepKind `json:"kind"`
	// Step 0: who the conversation is with, and how it starts.
	Provider string          `json:"provider,omitempty"`
	Model    string          `json:"model,omitempty"`
	SDK      string          `json:"sdk,omitempty"` // module@version
	Effort   string          `json:"effort,omitempty"`
	Request  json.RawMessage `json:"request,omitempty"`
	// Later steps: what the step added to the conversation. An llm_turn
	// records the model's messages as its provider sends them back, its
	// settings when the turn changed them, its calls (none means the run's
	// last turn), its text and how full the context was; an exec_tools
	// step, the calls' results. Settings and Results are the worker's own.
	Messages      []json.RawMessage `json:"messages,omitempty"`
	Settings      json.RawMessage   `json:"settings,omitempty"`
	Calls         []StepCall        `json:"calls,omitempty"`
	Text          string            `json:"text,omitempty"`
	Stop          string            `json:"stop,omitempty"`
	Usage         json.RawMessage   `json:"usage,omitempty"`
	ContextTokens int64             `json:"contextTokens,omitempty"`
	Results       json.RawMessage   `json:"results,omitempty"`
	At            time.Time         `json:"at"`
}

// Attempt is a failed attempt at a step, kept for diagnosis.
type Attempt struct {
	Step    int64     `json:"step"`
	Attempt int32     `json:"attempt"`
	Error   string    `json:"error"`
	At      time.Time `json:"at"`
}

// TranscriptStream is the log stream of a run's transcript.
func TranscriptStream(key storage.Key, runID string) string {
	return key.Namespace + "/" + key.Name + "/transcript/" + runID
}

func attemptKey(step int64, attempt int32) string {
	return storage.SeqKey(step) + "-attempt-" + strconv.Itoa(int(attempt))
}

// transcriptEntry reads step's entry, by its exact key, or nil.
func (m *Manager) transcriptEntry(ctx context.Context, key storage.Key, runID string, step int64) (*TranscriptEntry, error) {
	if step < 0 {
		return nil, nil
	}
	data, ok, err := m.entryAt(ctx, TranscriptStream(key, runID), step)
	if err != nil || !ok {
		return nil, err
	}
	var e TranscriptEntry
	if err := json.Unmarshal(data, &e); err != nil {
		return nil, fmt.Errorf("transcript step %d: %w", step, err)
	}
	return &e, nil
}

// stepKind is what step does, from the entry before it.
func (m *Manager) stepKind(ctx context.Context, key storage.Key, runID string, step int64) (StepKind, error) {
	if step == 0 {
		return StepStart, nil
	}
	prev, err := m.transcriptEntry(ctx, key, runID, step-1)
	if err != nil {
		return "", err
	}
	if prev == nil {
		return "", fmt.Errorf("transcript of run %s has no step %d: %w", runID, step-1, storage.ErrNotFound)
	}
	return NextKind(prev), nil
}

// StopToolUse is an llm_turn's stop when the model asked for calls, even
// ones its provider answered itself, so the turn has none to make.
const StopToolUse = "tool_use"

// NextKind is the kind of the step after prev, a committed entry: what
// a step does follows from the one before it.
func NextKind(prev *TranscriptEntry) StepKind {
	switch {
	case prev.Kind == StepLLMTurn && len(prev.Calls) == 0 && prev.Stop != StopToolUse:
		return StepFinish
	case prev.Kind == StepLLMTurn:
		return StepExecTools
	}
	return StepLLMTurn
}

// takeStepIn hands the worker a free step of a step-mode run in ns.
func (m *Manager) takeStepIn(ctx context.Context, ns string, req ClaimRequest) (*Claimed, error) {
	labels := req.Labels
	if req.Scope != nil {
		labels = req.Scope[ns]
	}
	running, err := m.store.ListTasks(ctx, storage.Query{Namespace: ns, Phases: []v1alpha1.Phase{v1alpha1.PhaseRunning}})
	if err != nil {
		return nil, err
	}
	now := m.now()
	for _, cand := range running {
		r := cand.Status.Run
		// Pre-filters only: TakeStep decides on the fresh read.
		if r == nil || r.Mode != v1alpha1.RunModeStep || (!r.StepFree && !now.After(r.LeaseUntil.Add(m.cfg.Heartbeat))) ||
			cand.Spec.Assignee == nil || cand.Spec.Assignee.Kind != v1alpha1.AssigneeAgent {
			continue
		}
		cached, err := m.agents.CachedAgent(ctx, ns, cand.Spec.Assignee.Name)
		if errors.Is(err, storage.ErrNotFound) || (err == nil && (!claimableBy(cached, labels) || !inMode(cached, req.Mode))) {
			continue
		}
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
		key := storage.Key{Namespace: ns, Name: cand.Name}
		// What the step does follows from the committed entry before it,
		// which never changes: read it before taking the step, so a failed
		// read takes nothing.
		kind, err := m.stepKind(ctx, key, r.ID, r.Step)
		if err != nil {
			return nil, err
		}
		token, tokenHash, err := mintToken(key)
		if err != nil {
			return nil, err
		}
		// TakeStep is fenced on the step read here, so the kind is that
		// step's.
		t, err := m.transition(ctx, key, task.TakeStep{
			RunID: r.ID, Step: r.Step, Runner: req.Identity, RunnerName: req.Runner,
			Lease: m.cfg.Lease, Grace: m.cfg.Heartbeat, MCPTokenHash: tokenHash,
		})
		if errors.Is(err, task.ErrStepTaken) || errors.Is(err, task.ErrStepExhausted) || errors.Is(err, task.ErrStepMismatch) ||
			errors.Is(err, task.ErrRunMismatch) || isLostCandidate(err) {
			continue // someone else took it, it moved on, or it ended
		}
		if err != nil {
			return nil, err
		}
		run := t.Status.Run
		return &Claimed{
			Task: t, RunID: run.ID, Mode: v1alpha1.RunModeStep, LeaseUntil: run.LeaseUntil.Time,
			Agent: agent, ModelProvider: model, MCPToken: token,
			Step: run.Step, Attempt: run.Attempt, Kind: kind,
		}, nil
	}
	return nil, nil
}

// heldStep reads the task fresh and checks that req's worker holds its
// step, at its attempt: the attempt fences a step as the run id fences the
// run.
func (m *Manager) heldStep(ctx context.Context, key storage.Key, req StepRequest) error {
	t, err := m.activeRun(ctx, key, req.RunID)
	if err != nil {
		return err
	}
	run := t.Status.Run
	switch {
	case run.Mode != v1alpha1.RunModeStep:
		return fmt.Errorf("run %s is not in step mode: %w", req.RunID, task.ErrInvalidTransition)
	case run.Step != req.Step:
		return fmt.Errorf("step %d, run is at %d: %w", req.Step, run.Step, task.ErrStepMismatch)
	case run.StepFree || run.Attempt != req.Attempt:
		return fmt.Errorf("attempt %d at step %d: %w", req.Attempt, req.Step, task.ErrAttemptMismatch)
	case !task.HeldBy(run.Runner, run.RunnerName, req.Runner):
		return task.ErrForbidden
	}
	return nil
}

// MaxTranscriptEntry is the largest transcript entry, in bytes: an
// exec_tools entry carries every result of the batch.
const MaxTranscriptEntry = 8 << 20

// AppendStep appends the step's transcript entry, create-only, for the
// worker holding the step, and returns the step's entry as stored. Its
// step and kind are the server's: a kind other than the one the claim
// said is refused. If the step's entry is there already, written by a
// worker that took the step before, that entry is returned, with adopted
// true: the worker commits it instead of its own.
func (m *Manager) AppendStep(ctx context.Context, key storage.Key, req StepRequest, e TranscriptEntry) (json.RawMessage, bool, error) {
	if err := requireRunID(req.RunID); err != nil {
		return nil, false, err
	}
	if err := m.heldStep(ctx, key, req); err != nil {
		return nil, false, err
	}
	kind, err := m.stepKind(ctx, key, req.RunID, req.Step)
	if err != nil {
		return nil, false, err
	}
	if e.Kind != kind {
		return nil, false, fmt.Errorf("step %d is %s, not %q: %w", req.Step, kind, e.Kind, ErrInvalid)
	}
	e.Step, e.At = req.Step, m.now().UTC()
	data, err := marshalEntry(e)
	if err != nil {
		return nil, false, err
	}
	if len(data) > MaxTranscriptEntry {
		return nil, false, fmt.Errorf("transcript entry is %d bytes, the limit is %d: %w", len(data), MaxTranscriptEntry, ErrInvalid)
	}
	stream := TranscriptStream(key, req.RunID)
	err = m.store.Append(ctx, stream, storage.SeqKey(req.Step), data)
	if err == nil {
		return data, false, nil
	}
	if !errors.Is(err, storage.ErrAlreadyExists) {
		return nil, false, err
	}
	there, ok, err := m.entryAt(ctx, stream, req.Step)
	if err != nil {
		return nil, false, err
	}
	if !ok {
		return nil, false, fmt.Errorf("transcript step %d is taken but unreadable: %w", req.Step, ErrBusy)
	}
	return there, true, nil
}

// marshalEntry encodes an entry without escaping HTML, so the provider's
// JSON is stored as close to as it came as JSON allows.
func marshalEntry(e TranscriptEntry) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(e); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// transcript returns a run's committed steps as stored, in order;
// diagnostic attempt entries are left out.
func (m *Manager) transcript(ctx context.Context, key storage.Key, runID string) ([]json.RawMessage, error) {
	entries, err := m.store.Read(ctx, TranscriptStream(key, runID), "", 0)
	if err != nil {
		return nil, err
	}
	out := []json.RawMessage{}
	for _, en := range entries {
		if _, err := strconv.ParseInt(en.Key, 10, 64); err != nil {
			continue // an attempt entry
		}
		out = append(out, en.Data)
	}
	return out, nil
}

// Transcript returns a run's committed steps, in order.
func (m *Manager) Transcript(ctx context.Context, key storage.Key, runID string) ([]TranscriptEntry, error) {
	raw, err := m.transcript(ctx, key, runID)
	if err != nil {
		return nil, err
	}
	out := make([]TranscriptEntry, 0, len(raw))
	for i, data := range raw {
		var e TranscriptEntry
		if err := json.Unmarshal(data, &e); err != nil {
			return nil, fmt.Errorf("transcript entry %d: %w", i, err)
		}
		out = append(out, e)
	}
	return out, nil
}

// StepRequest names a step of a step-mode run, the worker's attempt at it
// and the worker; for a runtime-mode run, the run alone.
type StepRequest struct {
	RunID   string
	Step    int64
	Attempt int32
	Runner  string // the worker's identity; empty on a server that does not authenticate runners
}

// Advance commits the held step, whose transcript entry is written, and
// frees the next for any worker. Subtasks are created as with Finish.
func (m *Manager) Advance(ctx context.Context, key storage.Key, req StepRequest, subtasks []Subtask) (*v1alpha1.Task, error) {
	if err := requireRunID(req.RunID); err != nil {
		return nil, err
	}
	if err := m.heldStep(ctx, key, req); err != nil {
		return nil, err
	}
	if _, ok, err := m.entryAt(ctx, TranscriptStream(key, req.RunID), req.Step); err != nil || !ok {
		if err == nil {
			err = fmt.Errorf("step %d has no transcript entry; append it before committing the step: %w", req.Step, ErrInvalid)
		}
		return nil, err
	}
	items, err := m.subtaskItems(subtasks)
	if err != nil {
		return nil, err
	}
	return m.transition(ctx, key, task.Advance{
		RunID: req.RunID, Step: req.Step, Attempt: req.Attempt, Runner: req.Runner, Idle: m.cfg.StepIdle, Outbox: items,
	})
}

// ReleaseStep gives a step back after a failed attempt, recording why in
// the transcript; the step's attempts stay counted.
func (m *Manager) ReleaseStep(ctx context.Context, key storage.Key, req StepRequest, why string) error {
	if err := requireRunID(req.RunID); err != nil {
		return err
	}
	if err := m.heldStep(ctx, key, req); err != nil {
		return err
	}
	data, err := json.Marshal(Attempt{Step: req.Step, Attempt: req.Attempt, Error: why, At: m.now().UTC()})
	if err != nil {
		return err
	}
	err = m.store.Append(ctx, TranscriptStream(key, req.RunID), attemptKey(req.Step, req.Attempt), data)
	if err != nil && !errors.Is(err, storage.ErrAlreadyExists) {
		return err
	}
	_, err = m.transition(ctx, key, task.ReleaseStep{RunID: req.RunID, Step: req.Step, Attempt: req.Attempt, Runner: req.Runner, Idle: m.cfg.StepIdle})
	return err
}

type callIDKey struct{}

// WithCallID says which of the model's tool calls a run tool is serving:
// in step mode, a re-executed step makes the same call again, and the
// items it records get the same ids, so they are recorded and written
// once.
func WithCallID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, callIDKey{}, id)
}

// itemID is the id of the outbox item a run tool records. In step mode it
// derives from the run, its step and the tool call the worker serves, so a
// step done again records the same item: the call id is then required, and
// must be one the step's model turn asked for. Each item a call records in
// turn gets its own, as the n-th of the call. A fresh ULID otherwise.
func (m *Manager) itemID(ctx context.Context, key storage.Key, t *v1alpha1.Task) (string, error) {
	run := t.Status.Run
	if run == nil || run.Mode != v1alpha1.RunModeStep {
		return m.newID(), nil
	}
	call, _ := ctx.Value(callIDKey{}).(string)
	if call == "" {
		return "", fmt.Errorf("a step-mode tool call needs its call id in _meta: %w", ErrInvalid)
	}
	turn, err := m.transcriptEntry(ctx, key, run.ID, run.Step-1)
	if err != nil {
		return "", err
	}
	if turn == nil || turn.Kind != StepLLMTurn || !slices.ContainsFunc(turn.Calls, func(c StepCall) bool { return c.ID == call }) {
		return "", fmt.Errorf("step %d's model turn asked for no call %q: %w", run.Step-1, call, ErrInvalid)
	}
	return derivedID(run.ID, run.Step, call, callCounter(ctx)), nil
}

// derivedID is the item id of the n-th item a step's call records.
func derivedID(runID string, step int64, call string, n int) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%d\x00%s\x00%d", runID, step, call, n)))
	var id ulid.ULID
	copy(id[:], sum[:16])
	return id.String()
}

type counterKey struct{}

// WithCallCounter numbers the items one tool call records; the MCP
// endpoint sets it per call.
func WithCallCounter(ctx context.Context) context.Context {
	return context.WithValue(ctx, counterKey{}, new(int))
}

func callCounter(ctx context.Context) int {
	c, ok := ctx.Value(counterKey{}).(*int)
	if !ok {
		return 0
	}
	*c++
	return *c
}

// StepTranscript returns the run's committed steps as stored, for the
// worker holding its current step.
func (m *Manager) StepTranscript(ctx context.Context, key storage.Key, req StepRequest) ([]json.RawMessage, error) {
	if err := requireRunID(req.RunID); err != nil {
		return nil, err
	}
	if err := m.heldStep(ctx, key, req); err != nil {
		return nil, err
	}
	return m.transcript(ctx, key, req.RunID)
}

// record is the Record of a run tool's item, fenced on the fresh task's
// run, and in step mode its step and attempt.
func record(runID string, t *v1alpha1.Task, item v1alpha1.OutboxItem) task.Record {
	step, attempt := stepFence(t)
	return task.Record{RunID: runID, Step: step, Attempt: attempt, Outbox: []v1alpha1.OutboxItem{item}}
}

// stepFence is the step and attempt a run tool's write is fenced on: the
// fresh task's, as the step's MCP token proves its caller holds it.
func stepFence(t *v1alpha1.Task) (int64, int32) {
	if r := t.Status.Run; r != nil && r.Mode == v1alpha1.RunModeStep {
		return r.Step, r.Attempt
	}
	return 0, 0
}

// recordedAfter is where the entry of item id is looked for: where the
// step first recorded it, when a step done again recorded it again.
func recordedAfter(t *v1alpha1.Task, id string, after int64) int64 {
	if t.Status.Run != nil {
		for _, it := range t.Status.Run.StepItems {
			if it.ID == id {
				return it.After
			}
		}
	}
	return after
}
