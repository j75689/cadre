package manager

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	"github.com/j75689/cadre/internal/storage"
	"github.com/j75689/cadre/internal/task"
)

// caller is a ToolCaller that counts its calls.
type caller struct{ calls atomic.Int32 }

func (c *caller) CallTool(_ context.Context, _ string, call ToolCall) ToolResult {
	c.calls.Add(1)
	return ToolResult{Body: "opened " + string(call.Arguments)}
}

var (
	bob    = Author{Kind: AuthorUser, Name: "bob"}
	prCall = ToolCall{Name: "github__create_pull_request", ToolServer: "github", Tool: "create_pull_request", Arguments: []byte(`{"title":"fix"}`)}
)

// withTools returns a Task Manager on store that makes approved calls
// through tools.
func (e *env) withTools(store storage.Driver, tools ToolCaller) *Manager {
	return New(store, Config{Lease: e.m.cfg.Lease, Heartbeat: e.m.cfg.Heartbeat, Agents: testAgents, Tools: tools}, WithClock(e.clock.now))
}

// asked claims the task and has the run ask for prCall.
func (e *env) asked(t *testing.T, m *Manager, key storage.Key) *ThreadEntry {
	t.Helper()
	c := e.claim(t)
	entry, err := m.RunToolCall(ctx, key, c.RunID, prCall)
	if err != nil {
		t.Fatal(err)
	}
	return entry
}

// A call whose ending of the run failed is retried: the thread keeps one
// ToolCall, and the retry ends the run.
func TestRunToolCallWritesOneCall(t *testing.T) {
	e := newEnv(t, 1)
	key := e.addTask(t, "t", v1alpha1.AssigneeAgent)
	c := e.claim(t)
	if _, err := e.manager(&failSecondWrite{Driver: e.store}).RunToolCall(ctx, key, c.RunID, prCall); err == nil {
		t.Fatal("first call: want the injected failure")
	}
	entry, err := e.m.RunToolCall(ctx, key, c.RunID, prCall)
	if err != nil || entry.Seq != 1 || entry.Call.ID == "" || entry.Kind != EntryToolCall {
		t.Fatalf("retried call: %+v, %v", entry, err)
	}
	if thread, _ := e.m.ReadThread(ctx, key, 0, 0); len(thread) != 1 {
		t.Fatalf("%d entries, want one ToolCall: %+v", len(thread), thread)
	}
	w := e.get(t, key).Status.Waiting
	if w == nil || w.Reason != v1alpha1.WaitingToolApproval || w.ThreadSeq != 1 {
		t.Fatalf("waiting %+v, want ToolApproval on entry 1", w)
	}
	if _, err := e.m.RunToolCall(ctx, key, c.RunID, prCall); !errors.Is(err, task.ErrRunMismatch) {
		t.Fatalf("a call by the ended run: %v, want ErrRunMismatch", err)
	}
}

func TestApproveMakesTheCallOnce(t *testing.T) {
	e := newEnv(t, 1)
	tools := &caller{}
	m := e.withTools(e.store, tools)
	key := e.addTask(t, "t", v1alpha1.AssigneeAgent)

	if _, _, err := m.Answer(ctx, key, alice, "yes"); !errors.Is(err, task.ErrInvalidTransition) {
		t.Fatalf("answer to a task not waiting: %v", err)
	}
	e.asked(t, m, key)
	if _, _, err := m.Answer(ctx, key, alice, "yes"); !errors.Is(err, task.ErrInvalidTransition) {
		t.Fatalf("an answer to a tool approval: %v, want ErrInvalidTransition", err)
	}
	if _, err := m.Approve(ctx, key, Author{Kind: AuthorUser}, true, ""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("an approval without an author: %v, want ErrInvalid", err)
	}
	a, err := m.Approve(ctx, key, alice, true, "")
	if err != nil {
		t.Fatal(err)
	}
	if a.Decision.Kind != EntryApproved || a.Decision.Body != "Approved" || a.Result.Body != `opened {"title":"fix"}` ||
		a.Result.Author != systemAuthor || a.Task.Status.Phase != v1alpha1.PhaseTodo || tools.calls.Load() != 1 {
		t.Fatalf("approval: %+v %+v %+v, %d calls", a.Decision, a.Result, a.Task.Status, tools.calls.Load())
	}
	if _, err := m.Approve(ctx, key, alice, true, ""); !errors.Is(err, task.ErrInvalidTransition) || tools.calls.Load() != 1 {
		t.Fatalf("approving again: %v, %d calls", err, tools.calls.Load())
	}
}

// Another approver's decision that is already in the thread decides the
// call: the same decision finishes the wait without a second call, and a
// different one is refused.
func TestEarliestDecisionWins(t *testing.T) {
	e := newEnv(t, 1)
	tools := &caller{}
	m := e.withTools(e.store, tools)
	key := e.addTask(t, "t", v1alpha1.AssigneeAgent)

	call := e.asked(t, m, key)
	if _, err := m.appendEntry(ctx, key, &ThreadEntry{Author: bob, Kind: EntryDenied, Body: "no", Call: &ToolCall{ID: call.Call.ID}}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Approve(ctx, key, alice, true, ""); !errors.Is(err, task.ErrInvalidTransition) {
		t.Fatalf("approving a denied call: %v, want ErrInvalidTransition", err)
	}
	a, err := m.Approve(ctx, key, alice, false, "")
	if err != nil || a.Decision.Author != bob || a.Result != nil || a.Task.Status.Phase != v1alpha1.PhaseTodo {
		t.Fatalf("denying a denied call: %+v, %v", a, err)
	}
	if n := tools.calls.Load(); n != 0 {
		t.Fatalf("%d calls, want none", n)
	}
}

// raceDecision lets another approver's Denied take the sequence number of
// the first Approved entry appended.
type raceDecision struct {
	storage.Driver
	once   sync.Once
	before func()
}

func (r *raceDecision) Append(c context.Context, stream, key string, data []byte) error {
	if strings.HasSuffix(stream, "/thread") && strings.Contains(string(data), `"kind":"Approved"`) {
		r.once.Do(r.before)
	}
	return r.Driver.Append(c, stream, key, data)
}

// Two approvers at once: the one whose decision lands first wins, even
// when the other's append came in a moment later, and the call is not made.
func TestConcurrentDecisions(t *testing.T) {
	e := newEnv(t, 1)
	tools := &caller{}
	key := e.addTask(t, "t", v1alpha1.AssigneeAgent)
	call := e.asked(t, e.m, key)
	race := &raceDecision{Driver: e.store, before: func() {
		if _, err := e.m.appendEntry(ctx, key, &ThreadEntry{Author: bob, Kind: EntryDenied, Body: "no", Call: &ToolCall{ID: call.Call.ID}}); err != nil {
			t.Error(err)
		}
	}}
	if _, err := e.withTools(race, tools).Approve(ctx, key, alice, true, ""); !errors.Is(err, task.ErrInvalidTransition) {
		t.Fatalf("an approval that lost the race: %v, want ErrInvalidTransition", err)
	}
	if n := tools.calls.Load(); n != 0 {
		t.Fatalf("%d calls, want none", n)
	}
}

// An approved call whose approver died before recording a result is never
// repeated: until RepairAfter another approval is told it is in progress,
// after it the outcome is recorded as unknown.
func TestApprovedCallIsNeverRepeated(t *testing.T) {
	e := newEnv(t, 1)
	tools := &caller{}
	m := e.withTools(e.store, tools)
	key := e.addTask(t, "t", v1alpha1.AssigneeAgent)
	call := e.asked(t, m, key)
	if _, err := m.appendEntry(ctx, key, &ThreadEntry{Author: bob, Kind: EntryApproved, Body: "ok", Call: &ToolCall{ID: call.Call.ID}}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Approve(ctx, key, alice, true, ""); !errors.Is(err, ErrInProgress) {
		t.Fatalf("approving while the call may run: %v, want ErrInProgress", err)
	}
	e.clock.add(RepairAfter)
	a, err := m.Approve(ctx, key, alice, true, "")
	if err != nil || !a.Result.Call.IsError || a.Result.Body != msgUnknownOutcome || a.Task.Status.Phase != v1alpha1.PhaseTodo {
		t.Fatalf("repair: %+v, %v", a, err)
	}
	if n := tools.calls.Load(); n != 0 {
		t.Fatalf("%d calls, want none", n)
	}
}

// Without a ToolCaller an approved call is recorded as not made.
func TestApproveWithoutTools(t *testing.T) {
	e := newEnv(t, 1)
	key := e.addTask(t, "t", v1alpha1.AssigneeAgent)
	e.asked(t, e.m, key)
	a, err := e.m.Approve(ctx, key, alice, true, "")
	if err != nil || !a.Result.Call.IsError || !strings.Contains(a.Result.Body, "not made") {
		t.Fatalf("approval: %+v, %v", a, err)
	}
}
