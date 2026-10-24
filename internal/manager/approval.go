package manager

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	"github.com/j75689/cadre/internal/storage"
	"github.com/j75689/cadre/internal/task"
)

// ToolCall is a forwarded tool call that must ask a human first, as
// recorded on the thread entries about it.
type ToolCall struct {
	// ID is minted when the call is asked; every entry about the call
	// carries it.
	ID string `json:"id"`
	// Name is the tool as the agent called it, <toolserver>__<tool>; Tool
	// is the ToolServer's own name for it.
	Name       string          `json:"name,omitempty"`
	ToolServer string          `json:"toolServer,omitempty"`
	Tool       string          `json:"tool,omitempty"`
	Arguments  json.RawMessage `json:"arguments,omitempty"`
	// IsError marks a ToolResult the call failed with, or one recorded
	// without making the call.
	IsError bool `json:"isError,omitempty"`
}

// ToolResult is what an approved call returned, as text for the agent.
type ToolResult struct {
	Body    string
	IsError bool
}

// ToolCaller makes an approved call on its ToolServer, which it reads
// fresh. It never fails: a call it cannot or may not make comes back as an
// error result saying why. It bounds the call itself, by CallTimeout.
type ToolCaller interface {
	CallTool(ctx context.Context, namespace string, call ToolCall) ToolResult
	// CallTimeout is the longest a call may take.
	CallTimeout() time.Duration
}

// MaxToolArguments bounds the arguments of a call that asks first.
const MaxToolArguments = 1 << 20

// ErrEntryPending means a task waits on an entry (a question, a tool call)
// that is committed but not written to the thread yet; it is, a moment
// later.
var ErrEntryPending = errors.New("the entry the task waits on is still being recorded; try again in a moment")

// ErrInProgress means another request is doing what was asked: an approved
// call that may still be running. It comes as a *CallInProgress.
var ErrInProgress = errors.New("in progress")

// CallInProgress is an approved call that may still be running. If it has
// no result by RepairAt, approving again records its outcome as unknown.
type CallInProgress struct {
	RepairAt time.Time
}

func (e *CallInProgress) Error() string {
	return "the approved call may still be running; if it records no result, approve again after " + e.RepairAt.Format(time.RFC3339)
}

// Is makes a CallInProgress match ErrInProgress.
func (e *CallInProgress) Is(target error) bool { return target == ErrInProgress }

// repairMargin is how much longer than its ToolCaller's CallTimeout an
// approved call without a result is waited for before it is taken to have
// died with its approver.
const repairMargin = time.Minute

// repairAfter is how long after its approval a call without a result is
// written off as unknown.
func (m *Manager) repairAfter() time.Duration {
	if m.cfg.Tools == nil {
		return repairMargin
	}
	return m.cfg.Tools.CallTimeout() + repairMargin
}

// systemAuthor writes the results of approved calls.
var systemAuthor = Author{Kind: AuthorSystem, Name: "cadre"}

// msgUnknownOutcome is the result recorded for an approved call whose
// approver died before recording what it returned.
const msgUnknownOutcome = "The outcome of this call is unknown: it was approved, but no result was recorded, so it may or may not have happened. It was not repeated. Check whether it took effect before calling it again."

// RunToolCall records a call that must ask a human first and ends the run
// as Waiting{ToolApproval}, in one fenced write, as RunAsk does: the Finish
// carries the ToolCall entry's outbox item, and the wait names it until the
// dispatcher has written it. When it returns without error the run is
// over; a retry after the Finish landed fails as the run is gone, and the
// one ToolCall entry is the first attempt's. A run that lost its task can
// land no entry.
func (m *Manager) RunToolCall(ctx context.Context, key storage.Key, runID string, call ToolCall) (*ThreadEntry, error) {
	switch {
	case call.Name == "" || call.ToolServer == "" || call.Tool == "":
		return nil, fmt.Errorf("a tool call needs its name, ToolServer and tool: %w", ErrInvalid)
	case len(call.Arguments) > MaxToolArguments:
		return nil, fmt.Errorf("arguments are %d bytes, the limit is %d: %w", len(call.Arguments), MaxToolArguments, ErrInvalid)
	}
	args, err := canonical(call.Arguments)
	if err != nil {
		return nil, err
	}
	call.Arguments = args
	t, err := m.activeRun(ctx, key, runID)
	if err != nil {
		return nil, err
	}
	item, err := m.callItem(ctx, key, t, runID, EntryToolCall, "Calls "+call.Name, &call)
	if err != nil {
		return nil, err
	}
	step, attempt := stepFence(t)
	return m.finishWaitingOn(ctx, key, item, FinishRequest{
		RunID: runID, Step: step, Attempt: attempt, Outcome: task.OutcomeWaiting, Reason: v1alpha1.WaitingToolApproval,
		WaitingItem: item.ID, Outbox: []v1alpha1.OutboxItem{item},
	})
}

// canonical is arguments as the thread stores them: checked to be JSON and
// compacted.
func canonical(args json.RawMessage) (json.RawMessage, error) {
	if len(args) == 0 {
		return nil, nil
	}
	if !json.Valid(args) {
		return nil, fmt.Errorf("arguments are not JSON: %w", ErrInvalid)
	}
	return json.Marshal(args)
}

// Approval is the outcome of approving or denying a call.
type Approval struct {
	Task *v1alpha1.Task
	// Decision is the Approved or Denied entry that decided the call.
	Decision *ThreadEntry
	// Result is the call's ToolResult entry; nil for a denied call.
	Result *ThreadEntry
}

// Approve decides the call a task waits on, then sends the task back to
// Todo for the agent to continue. Approving makes the call, Cadre-side.
//
// The thread is append-only and every decision is a create-only append, so
// the earliest decision on a call wins. A request makes the call only when
// the winning Approved entry is its own, so two approvers cannot make it
// twice. A request that loses to the other decision fails with
// ErrInvalidTransition; one that loses to the same decision finishes the
// wait. A call is never repeated: when its approver died before recording
// the result, approving again once the ToolCaller's CallTimeout and a
// minute have passed records the outcome as unknown, and before that fails
// with ErrInProgress. Only the earliest decision on a call counts; a later
// one, from an approver who lost a race, stays in the thread and means
// nothing.
//
// The result and the Reply are written even if ctx ends once the call has
// been made. A Reply that finds the wait already ended by another approver
// of the same decision succeeds.
func (m *Manager) Approve(ctx context.Context, key storage.Key, author Author, approve bool, note string) (*Approval, error) {
	if author.Name == "" {
		return nil, fmt.Errorf("author is required: %w", ErrInvalid)
	}
	t, err := m.store.GetTask(ctx, key)
	if err != nil {
		return nil, err
	}
	w := t.Status.Waiting
	if t.Status.Phase != v1alpha1.PhaseWaiting || w == nil || w.Reason != v1alpha1.WaitingToolApproval {
		return nil, fmt.Errorf("approve a task that is not waiting for a tool approval: %w", task.ErrInvalidTransition)
	}
	if w.ItemID != "" && w.ThreadSeq == 0 {
		// The ToolCall is committed but not in the thread yet.
		return nil, ErrEntryPending
	}
	since, err := m.callEntries(ctx, key, w.ThreadSeq)
	if err != nil {
		return nil, err
	}
	if len(since) == 0 || since[0].Seq != w.ThreadSeq || since[0].Kind != EntryToolCall || since[0].Call == nil {
		return nil, fmt.Errorf("thread entry %d is not the call the task waits on: %w", w.ThreadSeq, ErrInvalid)
	}
	call := *since[0].Call
	want := EntryDenied
	if approve {
		want = EntryApproved
	}

	decision := firstAbout(since, call.ID, EntryApproved, EntryDenied)
	var mine *ThreadEntry
	if decision == nil {
		if note == "" {
			note = string(want)
		}
		if mine, err = m.appendEntry(ctx, key, &ThreadEntry{Author: author, Kind: want, Body: note, Call: &ToolCall{ID: call.ID}}); err != nil {
			return nil, err
		}
		// Another approver may have decided first.
		if since, err = m.callEntries(ctx, key, w.ThreadSeq); err != nil {
			return nil, err
		}
		decision = firstAbout(since, call.ID, EntryApproved, EntryDenied)
	}
	if decision == nil || decision.Kind != want {
		return nil, fmt.Errorf("the call was already decided otherwise: %w", task.ErrInvalidTransition)
	}
	out := &Approval{Decision: decision, Result: firstAbout(since, call.ID, EntryToolResult)}

	if approve && out.Result == nil {
		var result ToolResult
		switch {
		case mine != nil && mine.Seq == decision.Seq:
			result = m.callTool(ctx, key.Namespace, call)
		case m.now().Sub(decision.At) < m.repairAfter():
			return nil, &CallInProgress{RepairAt: decision.At.Add(m.repairAfter())}
		default:
			result = ToolResult{Body: msgUnknownOutcome, IsError: true}
		}
		// The call has been made (or written off): its result must not
		// be lost with the caller.
		ctx = context.WithoutCancel(ctx)
		if result.Body == "" {
			result.Body = "(no output)"
		}
		out.Result, err = m.appendEntry(ctx, key, &ThreadEntry{
			Author: systemAuthor, Kind: EntryToolResult, Body: result.Body, Call: &ToolCall{ID: call.ID, IsError: result.IsError},
		})
		if err != nil {
			return nil, err
		}
	}
	if out.Task, err = m.replyTo(ctx, key, w.ThreadSeq); err != nil {
		return nil, err
	}
	return out, nil
}

// callEntries reads the thread from the entry a wait names. The task is
// known to exist.
func (m *Manager) callEntries(ctx context.Context, key storage.Key, seq int64) ([]ThreadEntry, error) {
	return readStream[ThreadEntry](ctx, m.store, ThreadStream(key), storage.SeqKey(seq-1), 0)
}

// replyTo ends the wait on thread entry seq. A wait another request already
// ended counts as ended: that request acted on the same decision.
func (m *Manager) replyTo(ctx context.Context, key storage.Key, seq int64) (*v1alpha1.Task, error) {
	t, err := m.transition(ctx, key, task.Reply{ThreadSeq: seq})
	if !errors.Is(err, task.ErrInvalidTransition) {
		return t, err
	}
	if t, rerr := m.store.GetTask(ctx, key); rerr == nil {
		if w := t.Status.Waiting; t.Status.Phase != v1alpha1.PhaseWaiting || w == nil || w.ThreadSeq != seq {
			return t, nil
		}
	}
	return nil, err
}

// callTool makes an approved call, bounded by the ToolCaller but not by
// ctx: a caller that hangs up does not cut a call that may be taking effect.
func (m *Manager) callTool(ctx context.Context, namespace string, call ToolCall) ToolResult {
	if m.cfg.Tools == nil {
		return ToolResult{Body: "The call was not made: this server forwards no ToolServers.", IsError: true}
	}
	return m.cfg.Tools.CallTool(context.WithoutCancel(ctx), namespace, call)
}

// firstAbout returns the earliest entry about call id of one of kinds.
func firstAbout(entries []ThreadEntry, id string, kinds ...EntryKind) *ThreadEntry {
	for i, e := range entries {
		if e.Call != nil && e.Call.ID == id && slices.Contains(kinds, e.Kind) {
			return &entries[i]
		}
	}
	return nil
}
