package manager

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	"github.com/j75689/cadre/internal/storage"
	"github.com/j75689/cadre/internal/storage/memory"
	"github.com/j75689/cadre/internal/task"
)

const ns = "team-a"

var ctx = context.Background()

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type env struct {
	backend *memory.Backend
	store   storage.Driver
	clock   *clock
	m       *Manager
}

// newEnv returns a manager on a fresh store with a Team allowing maxRuns.
func newEnv(t *testing.T, maxRuns int32) *env {
	t.Helper()
	b := memory.NewBackend()
	e := &env{backend: b, store: b.Open(), clock: &clock{t: time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC)}}
	e.m = e.manager(e.store)
	team := &v1alpha1.Team{ObjectMeta: metav1.ObjectMeta{Name: v1alpha1.TeamName, Namespace: ns}, Spec: v1alpha1.TeamSpec{MaxConcurrentRuns: maxRuns}}
	if err := e.store.CreateTeam(ctx, team); err != nil {
		t.Fatal(err)
	}
	return e
}

// manager returns another Task Manager instance on the same store and clock.
func (e *env) manager(store storage.Driver) *Manager {
	return New(store, Config{Lease: 30 * time.Second, Heartbeat: 10 * time.Second}, WithClock(e.clock.now))
}

// addTask applies a task as kubectl would and numbers it.
func (e *env) addTask(t *testing.T, name string, assignee v1alpha1.AssigneeKind) storage.Key {
	t.Helper()
	tk := &v1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       v1alpha1.TaskSpec{Title: name, Assignee: &v1alpha1.Assignee{Kind: assignee, Name: "coder"}},
	}
	if err := e.store.CreateTask(ctx, tk); err != nil {
		t.Fatal(err)
	}
	if err := e.m.NumberNew(ctx); err != nil {
		t.Fatal(err)
	}
	return storage.Key{Namespace: ns, Name: name}
}

func (e *env) get(t *testing.T, key storage.Key) *v1alpha1.Task {
	t.Helper()
	tk, err := e.store.GetTask(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	return tk
}

func (e *env) running(t *testing.T) int32 {
	t.Helper()
	team, err := e.store.GetTeam(ctx, ns)
	if err != nil {
		t.Fatal(err)
	}
	return team.Status.Running
}

func (e *env) claim(t *testing.T) *Claimed {
	t.Helper()
	c, err := e.m.Claim(ctx, ClaimRequest{Runner: "runner-1"})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	return c
}

func subSpec(title string) v1alpha1.TaskSpec {
	return v1alpha1.TaskSpec{Title: title, Assignee: &v1alpha1.Assignee{Kind: v1alpha1.AssigneeAgent, Name: "coder"}}
}

func TestTaskLife(t *testing.T) {
	e := newEnv(t, 2)
	key := e.addTask(t, "fix-login", v1alpha1.AssigneeAgent)
	if tk := e.get(t, key); tk.Status.Phase != v1alpha1.PhaseTodo || tk.Status.Number != 1 {
		t.Fatalf("after numbering: %+v", tk.Status)
	}

	c := e.claim(t)
	if c == nil || c.Task.Name != key.Name || e.running(t) != 1 {
		t.Fatalf("claim: %+v, running %d", c, e.running(t))
	}
	e.clock.add(20 * time.Second)
	lease, err := e.m.Heartbeat(ctx, key, c.RunID)
	if err != nil || !lease.Equal(e.clock.now().Add(30*time.Second)) {
		t.Fatalf("heartbeat: %v, lease %v", err, lease)
	}

	child := ulid.Make().String()
	phase, again, err := e.m.Finish(ctx, key, FinishRequest{
		RunID: c.RunID, Outcome: task.OutcomeWaiting, Reason: v1alpha1.WaitingQuestion, ThreadSeq: 3,
		Subtasks: []Subtask{{ID: child, Spec: subSpec("write the test")}, {Spec: subSpec("minted id")}},
	})
	if err != nil || phase != v1alpha1.PhaseWaiting || again || e.running(t) != 0 {
		t.Fatalf("finish: phase %s again %v err %v running %d", phase, again, err, e.running(t))
	}

	// The dispatcher creates the subtasks and records the run; numbering
	// then takes the subtasks to Todo.
	if err := e.m.Dispatch(ctx); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if err := e.m.NumberNew(ctx); err != nil {
		t.Fatal(err)
	}
	if out := e.get(t, key).Status.Outbox; len(out) != 0 {
		t.Fatalf("outbox not cleared: %+v", out)
	}
	sub := e.get(t, storage.Key{Namespace: ns, Name: task.SubtaskName(v1alpha1.OutboxItem{ID: child})})
	if sub.Spec.ParentRef == nil || sub.Spec.ParentRef.Name != key.Name || sub.Status.Phase != v1alpha1.PhaseTodo || sub.Status.Number < 2 {
		t.Fatalf("subtask: %+v / %+v", sub.Spec, sub.Status)
	}
	all, _ := e.store.ListTasks(ctx, storage.Query{Namespace: ns})
	if len(all) != 3 {
		t.Fatalf("%d tasks, want the parent and two subtasks", len(all))
	}
	history, err := e.store.Read(ctx, RunsStream(key), "", 0)
	if err != nil || len(history) != 1 || history[0].Key != c.RunID {
		t.Fatalf("run history: %+v, %v", history, err)
	}

	if err := e.m.Reply(ctx, key); err != nil {
		t.Fatalf("reply: %v", err)
	}
	if tk := e.get(t, key); tk.Status.Phase != v1alpha1.PhaseTodo || tk.Status.Waiting != nil {
		t.Fatalf("after reply: %+v", tk.Status)
	}

	// Subtasks are claimable too; cancel each claimed one to release slots.
	for range 3 {
		c := e.claim(t)
		if c == nil {
			t.Fatal("nothing to claim")
		}
		if err := e.m.Cancel(ctx, storage.Key{Namespace: ns, Name: c.Task.Name}); err != nil {
			t.Fatal(err)
		}
		if _, err := e.m.Heartbeat(ctx, storage.Key{Namespace: ns, Name: c.Task.Name}, c.RunID); !errors.Is(err, task.ErrRunMismatch) {
			t.Fatalf("heartbeat after cancel: %v, want run mismatch", err)
		}
	}
	if c := e.claim(t); c != nil || e.running(t) != 0 {
		t.Fatalf("after cancelling everything: claim %+v, running %d", c, e.running(t))
	}
}

func TestClaimRespectsTheSemaphoreAndAssignee(t *testing.T) {
	e := newEnv(t, 1)
	e.addTask(t, "for-a-human", v1alpha1.AssigneeUser)
	e.addTask(t, "a", v1alpha1.AssigneeAgent)
	e.addTask(t, "b", v1alpha1.AssigneeAgent)

	first := e.claim(t)
	if first == nil || first.Task.Name == "for-a-human" {
		t.Fatalf("first claim: %+v", first)
	}
	if c := e.claim(t); c != nil {
		t.Fatalf("claimed %s past maxConcurrentRuns", c.Task.Name)
	}
	if _, _, err := e.m.Finish(ctx, storage.Key{Namespace: ns, Name: first.Task.Name}, FinishRequest{RunID: first.RunID, Outcome: task.OutcomeDone}); err != nil {
		t.Fatal(err)
	}
	second := e.claim(t)
	if second == nil || second.Task.Name == first.Task.Name || second.Task.Name == "for-a-human" {
		t.Fatalf("second claim: %+v", second)
	}
	if c, err := e.m.Claim(ctx, ClaimRequest{Runner: "r", Namespaces: []string{"elsewhere"}}); c != nil || err != nil {
		t.Fatalf("claim in a namespace without a Team: %+v, %v", c, err)
	}
	if _, err := e.m.Claim(ctx, ClaimRequest{}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("claim without a runner: %v", err)
	}
}

func TestFinishRetry(t *testing.T) {
	e := newEnv(t, 1)
	key := e.addTask(t, "t", v1alpha1.AssigneeAgent)
	c := e.claim(t)
	req := FinishRequest{RunID: c.RunID, Outcome: task.OutcomeReview}
	if _, again, err := e.m.Finish(ctx, key, req); err != nil || again {
		t.Fatalf("finish: %v %v", again, err)
	}
	// Retried while the RunEnded item is still in the outbox, then again
	// after the dispatcher moved it to the run history.
	for _, dispatched := range []bool{false, true} {
		if dispatched {
			if err := e.m.Dispatch(ctx); err != nil {
				t.Fatal(err)
			}
		}
		phase, again, err := e.m.Finish(ctx, key, req)
		if err != nil || !again || phase != v1alpha1.PhaseReview {
			t.Fatalf("retry (dispatched %v): phase %s again %v err %v", dispatched, phase, again, err)
		}
		if _, _, err := e.m.Finish(ctx, key, FinishRequest{RunID: c.RunID, Outcome: task.OutcomeDone}); !errors.Is(err, task.ErrRunMismatch) {
			t.Fatalf("finish with another outcome (dispatched %v): %v", dispatched, err)
		}
	}

	// A finish that lost to a cancel is a mismatch, not a retry.
	if err := e.m.Reply(ctx, key); !errors.Is(err, task.ErrInvalidTransition) {
		t.Fatalf("reply from Review: %v", err)
	}
	key2 := e.addTask(t, "t2", v1alpha1.AssigneeAgent)
	c2 := e.claim(t)
	if err := e.m.Cancel(ctx, key2); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.m.Finish(ctx, key2, FinishRequest{RunID: c2.RunID, Outcome: task.OutcomeDone}); !errors.Is(err, task.ErrRunMismatch) {
		t.Fatalf("finish after cancel: %v", err)
	}
	if _, _, err := e.m.Finish(ctx, key, FinishRequest{RunID: c.RunID, Outcome: task.OutcomeDone, Subtasks: []Subtask{{ID: "nope"}}}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad subtask id: %v", err)
	}
}

func TestReclaimWaitsForTheGrace(t *testing.T) {
	e := newEnv(t, 1)
	key := e.addTask(t, "t", v1alpha1.AssigneeAgent)
	c := e.claim(t)

	e.clock.add(39 * time.Second) // lease 30s + heartbeat grace 10s not yet passed
	if err := e.m.ReclaimExpired(ctx); err != nil {
		t.Fatal(err)
	}
	if e.get(t, key).Status.Phase != v1alpha1.PhaseRunning {
		t.Fatal("reclaimed within the grace")
	}
	e.clock.add(2 * time.Second)
	if err := e.m.ReclaimExpired(ctx); err != nil {
		t.Fatal(err)
	}
	tk := e.get(t, key)
	if tk.Status.Phase != v1alpha1.PhaseTodo || e.running(t) != 0 {
		t.Fatalf("after reclaim: %+v, running %d", tk.Status, e.running(t))
	}
	if len(tk.Status.Outbox) != 1 || tk.Status.Outbox[0].Run.Reason != v1alpha1.RunInterrupted {
		t.Fatalf("reclaim record: %+v", tk.Status.Outbox)
	}
	if _, err := e.m.Heartbeat(ctx, key, c.RunID); !errors.Is(err, task.ErrRunMismatch) {
		t.Fatalf("heartbeat after reclaim: %v", err)
	}
}

func TestEmptyRunIDIsInvalid(t *testing.T) {
	e := newEnv(t, 1)
	key := e.addTask(t, "t", v1alpha1.AssigneeAgent)
	e.claim(t)
	if _, err := e.m.Heartbeat(ctx, key, ""); !errors.Is(err, ErrInvalid) {
		t.Errorf("heartbeat: %v", err)
	}
	if _, _, err := e.m.Finish(ctx, key, FinishRequest{Outcome: task.OutcomeDone}); !errors.Is(err, ErrInvalid) {
		t.Errorf("finish: %v", err)
	}
	if err := e.m.RecordEvent(ctx, key, Event{Seq: 1}); !errors.Is(err, ErrInvalid) {
		t.Errorf("event: %v", err)
	}
}

func TestRecordEvent(t *testing.T) {
	e := newEnv(t, 1)
	key := e.addTask(t, "t", v1alpha1.AssigneeAgent)
	c := e.claim(t)
	ev := Event{RunID: c.RunID, Seq: 1, InputTokens: 10, Progress: "reading"}
	for range 2 {
		if err := e.m.RecordEvent(ctx, key, ev); err != nil {
			t.Fatalf("event: %v", err)
		}
	}
	if got, _ := e.store.Read(ctx, EventsStream(key, c.RunID), "", 0); len(got) != 1 {
		t.Fatalf("%d events stored, want 1", len(got))
	}
	if err := e.m.RecordEvent(ctx, key, Event{RunID: "01JB0000000000000000000000", Seq: 2}); !errors.Is(err, task.ErrRunMismatch) {
		t.Fatalf("event from another run: %v", err)
	}
	if err := e.m.RecordEvent(ctx, key, Event{RunID: c.RunID}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("event without seq: %v", err)
	}
}

func TestTeamReconcileNeedsTwoScans(t *testing.T) {
	e := newEnv(t, 5)
	team, _ := e.store.GetTeam(ctx, ns)
	team.Status.Running = 3 // releases that were lost
	if err := e.store.UpdateTeamStatus(ctx, team); err != nil {
		t.Fatal(err)
	}
	var r teamReconciler
	if err := r.reconcile(ctx, e.m); err != nil || e.running(t) != 3 {
		t.Fatalf("first scan: running %d, err %v; want it untouched", e.running(t), err)
	}
	if err := r.reconcile(ctx, e.m); err != nil || e.running(t) != 0 {
		t.Fatalf("second scan: running %d, err %v; want 0", e.running(t), err)
	}
}

// Several managers on separate driver processes claim and finish
// concurrently. No task is ever claimed by two runs at once, and the Team
// never runs more than maxConcurrentRuns.
func TestConcurrentClaims(t *testing.T) {
	const tasks, maxRuns, managers = 30, 3, 4
	e := newEnv(t, maxRuns)
	for i := range tasks {
		e.addTask(t, fmt.Sprintf("t-%02d", i), v1alpha1.AssigneeAgent)
	}

	var inFlight, peak, done atomic.Int32
	var holders sync.Map // task name -> run id
	var wg sync.WaitGroup
	for i := range managers {
		m := e.manager(e.backend.Open())
		wg.Add(1)
		go func() {
			defer wg.Done()
			for done.Load() < tasks {
				c, err := m.Claim(ctx, ClaimRequest{Runner: fmt.Sprintf("runner-%d", i)})
				if err != nil {
					t.Errorf("claim: %v", err)
					return
				}
				if c == nil {
					time.Sleep(time.Millisecond)
					continue
				}
				if prev, loaded := holders.LoadOrStore(c.Task.Name, c.RunID); loaded {
					t.Errorf("%s claimed twice: %v and %s", c.Task.Name, prev, c.RunID)
				}
				// Count the run as in flight from its claim until just before
				// its finish releases the slot, so the count never exceeds
				// the runs actually holding slots.
				raisePeak(&peak, inFlight.Add(1))
				time.Sleep(200 * time.Microsecond)
				inFlight.Add(-1)
				if _, _, err := m.Finish(ctx, storage.Key{Namespace: ns, Name: c.Task.Name}, FinishRequest{RunID: c.RunID, Outcome: task.OutcomeDone}); err != nil {
					t.Errorf("finish: %v", err)
				}
				done.Add(1)
			}
		}()
	}
	wg.Wait()
	if peak.Load() > maxRuns || peak.Load() < 2 {
		t.Fatalf("peak of %d runs in flight at once, want 2 to %d", peak.Load(), maxRuns)
	}
	if done.Load() != tasks || e.running(t) != 0 {
		t.Fatalf("done %d of %d, running %d", done.Load(), tasks, e.running(t))
	}
}

// raisePeak sets peak to n if n is higher.
func raisePeak(peak *atomic.Int32, n int32) {
	for {
		p := peak.Load()
		if n <= p || peak.CompareAndSwap(p, n) {
			return
		}
	}
}
