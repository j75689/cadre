package manager

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/oklog/ulid/v2"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	"github.com/j75689/cadre/internal/storage"
	"github.com/j75689/cadre/internal/task"
)

var errTimeout = errors.New("timeout")

// flaky wraps a driver so that chosen status writes fail.
type flaky struct {
	storage.Driver
	// commitThenFail makes the next status write commit but report an
	// error, like a timeout after the server applied it.
	commitThenFail atomic.Bool
	// failBeforeCommit makes the next status write fail without effect.
	failBeforeCommit atomic.Bool
	// readsFailAfterWrite makes every read after a status write fail, so
	// the write's outcome cannot be checked.
	readsFailAfterWrite atomic.Bool
	wrote               atomic.Bool
}

func (f *flaky) UpdateTaskStatus(c context.Context, t *v1alpha1.Task) error {
	defer f.wrote.Store(true)
	if f.failBeforeCommit.CompareAndSwap(true, false) {
		return errTimeout
	}
	err := f.Driver.UpdateTaskStatus(c, t)
	if err == nil && f.commitThenFail.CompareAndSwap(true, false) {
		return errTimeout
	}
	return err
}

func (f *flaky) GetTask(c context.Context, key storage.Key) (*v1alpha1.Task, error) {
	if f.readsFailAfterWrite.Load() && f.wrote.Load() {
		return nil, errTimeout
	}
	return f.Driver.GetTask(c, key)
}

// A claim or finish whose write committed but reported an error keeps or
// releases the slot exactly as if the write had succeeded.
func TestAmbiguousWritesKeepTheSemaphoreRight(t *testing.T) {
	e := newEnv(t, 1)
	key := e.addTask(t, "t", v1alpha1.AssigneeAgent)
	f := &flaky{Driver: e.store}
	m := e.manager(f)

	f.commitThenFail.Store(true)
	c, err := m.Claim(ctx, ClaimRequest{Runner: "r"})
	if err != nil || c == nil || e.running(t) != 1 {
		t.Fatalf("claim whose write landed: %+v, %v, running %d", c, err, e.running(t))
	}
	f.commitThenFail.Store(true)
	if _, _, err := m.Finish(ctx, key, FinishRequest{RunID: c.RunID, Outcome: task.OutcomeDone}); err != nil || e.running(t) != 0 {
		t.Fatalf("finish whose write landed: %v, running %d", err, e.running(t))
	}
	if _, again, err := m.Finish(ctx, key, FinishRequest{RunID: c.RunID, Outcome: task.OutcomeDone}); err != nil || !again || e.running(t) != 0 {
		t.Fatalf("retried finish: again %v, %v, running %d", again, err, e.running(t))
	}
}

func TestFailedAndUncertainClaimWrites(t *testing.T) {
	e := newEnv(t, 1)
	e.addTask(t, "t", v1alpha1.AssigneeAgent)

	// The write failed for sure: the slot goes back.
	f := &flaky{Driver: e.store}
	f.failBeforeCommit.Store(true)
	if c, err := e.manager(f).Claim(ctx, ClaimRequest{Runner: "r"}); c != nil || !errors.Is(err, errTimeout) || e.running(t) != 0 {
		t.Fatalf("failed claim: %+v, %v, running %d", c, err, e.running(t))
	}

	// The write landed but cannot be confirmed: the slot is kept.
	f = &flaky{Driver: e.store}
	f.commitThenFail.Store(true)
	f.readsFailAfterWrite.Store(true)
	if c, err := e.manager(f).Claim(ctx, ClaimRequest{Runner: "r"}); c != nil || !errors.Is(err, ErrUncertain) || e.running(t) != 1 {
		t.Fatalf("uncertain claim: %+v, %v, running %d", c, err, e.running(t))
	}
}

func TestTeamReconcileForgetsAFixedExcess(t *testing.T) {
	e := newEnv(t, 5)
	setRunning := func(n int32) {
		team, _ := e.store.GetTeam(ctx, ns)
		team.Status.Running = n
		if err := e.store.UpdateTeamStatus(ctx, team); err != nil {
			t.Fatal(err)
		}
	}
	setRunning(2)
	var r teamReconciler
	for range 2 {
		if err := r.reconcile(ctx, e.m); err != nil {
			t.Fatal(err)
		}
	}
	if e.running(t) != 0 {
		t.Fatalf("running %d after two scans, want 0", e.running(t))
	}
	// A claim holds its slot but is not Running in the index yet. One scan
	// must not take the slot, however large the excess fixed before it.
	setRunning(1)
	if err := r.reconcile(ctx, e.m); err != nil || e.running(t) != 1 {
		t.Fatalf("the scan after a fix took a live claim's slot: running %d, %v", e.running(t), err)
	}
}

// staleIndex serves a fixed, out-of-date board.
type staleIndex struct {
	storage.Driver
	board []v1alpha1.Task
}

func (s *staleIndex) ListTasks(context.Context, storage.Query) ([]v1alpha1.Task, error) {
	return s.board, nil
}

// The index still shows an agent, but the task belongs to a human now: the
// claim is decided on the fresh read and refused.
func TestClaimDecidesTheAssigneeOnAFreshRead(t *testing.T) {
	e := newEnv(t, 1)
	key := e.addTask(t, "t", v1alpha1.AssigneeUser)
	stale := *e.get(t, key)
	stale.Spec.Assignee = &v1alpha1.Assignee{Kind: v1alpha1.AssigneeAgent, Name: "coder"}

	m := e.manager(&staleIndex{Driver: e.store, board: []v1alpha1.Task{stale}})
	if c, err := m.Claim(ctx, ClaimRequest{Runner: "r"}); c != nil || err != nil || e.running(t) != 0 {
		t.Fatalf("claim of a human's task: %+v, %v, running %d", c, err, e.running(t))
	}
	if e.get(t, key).Status.Phase != v1alpha1.PhaseTodo {
		t.Fatal("the human's task was claimed")
	}
}

// A subtask whose name is taken by an unrelated task stays in the outbox.
func TestDispatchKeepsASubtaskWhoseNameIsTaken(t *testing.T) {
	e := newEnv(t, 1)
	key := e.addTask(t, "parent", v1alpha1.AssigneeAgent)
	id := ulid.Make().String()
	e.addTask(t, strings.ToLower(id), v1alpha1.AssigneeUser) // not a subtask of parent

	c := e.claim(t)
	if _, _, err := e.m.Finish(ctx, key, FinishRequest{RunID: c.RunID, Outcome: task.OutcomeDone, Subtasks: []Subtask{{ID: id, Spec: subSpec("child")}}}); err != nil {
		t.Fatal(err)
	}
	if err := e.m.Dispatch(ctx); !errors.Is(err, storage.ErrAlreadyExists) {
		t.Fatalf("dispatch: %v, want the collision reported", err)
	}
	out := e.get(t, key).Status.Outbox
	if len(out) != 1 || out[0].ID != id {
		t.Fatalf("outbox %+v; want only the colliding subtask left", out)
	}
}

// A retried finish reports the phase it produced, even after the task moved
// on.
func TestAlreadyFinishedReportsItsOwnPhase(t *testing.T) {
	e := newEnv(t, 1)
	key := e.addTask(t, "t", v1alpha1.AssigneeAgent)
	c := e.claim(t)
	req := FinishRequest{RunID: c.RunID, Outcome: task.OutcomeWaiting, Reason: v1alpha1.WaitingQuestion}
	if _, _, err := e.m.Finish(ctx, key, req); err != nil {
		t.Fatal(err)
	}
	if err := e.m.Reply(ctx, key); err != nil {
		t.Fatal(err)
	}
	if e.claim(t) == nil {
		t.Fatal("no second claim")
	}
	phase, again, err := e.m.Finish(ctx, key, req)
	if err != nil || !again || phase != v1alpha1.PhaseWaiting {
		t.Fatalf("retried finish: %s %v %v; want Waiting, already finished", phase, again, err)
	}
}
