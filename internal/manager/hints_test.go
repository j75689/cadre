package manager

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	"github.com/j75689/cadre/internal/storage"
	"github.com/j75689/cadre/internal/task"
)

// With the scans an hour apart, a task applied as kubectl would is
// numbered, and a finished run's subtask created and numbered, at once:
// the store's notifications drive both.
func TestHintsNumberAndDispatchAtOnce(t *testing.T) {
	e := newEnv(t, 2)
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	e.m.FollowHints(runCtx, slog.New(slog.NewTextHandler(io.Discard, nil)))

	key := storage.Key{Namespace: ns, Name: "applied"}
	if err := e.store.CreateTask(ctx, &v1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: key.Name},
		Spec:       v1alpha1.TaskSpec{Title: "applied", Assignee: &v1alpha1.Assignee{Kind: v1alpha1.AssigneeAgent, Name: "coder"}},
	}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the applied task numbered", func() bool { return e.get(t, key).Status.Number > 0 })

	c := e.claim(t)
	if _, _, err := e.m.Finish(ctx, key, FinishRequest{RunID: c.RunID, Outcome: task.OutcomeDone, Subtasks: []Subtask{{Spec: subSpec("follow-up")}}}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the subtask created and numbered", func() bool {
		subs, err := e.m.ListTasks(ctx, storage.Query{Namespace: ns})
		if err != nil {
			return false
		}
		for _, s := range subs {
			if s.Spec.ParentRef != nil && s.Spec.ParentRef.Name == key.Name && s.Status.Number > 0 {
				return len(e.get(t, key).Status.Outbox) == 0
			}
		}
		return false
	})
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatalf("%s: not within 3s, with the scans an hour away", what)
}

// A task the Manager creates and numbers itself, and a subtask a run
// creates, take one number each, whether or not the hints their writes
// send get to them first.
func TestOneNumberPerTask(t *testing.T) {
	e := newEnv(t, 5)
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	e.m.FollowHints(runCtx, slog.New(slog.NewTextHandler(io.Discard, nil)))
	for i := range 20 {
		if _, err := e.m.CreateTask(ctx, ns, fmt.Sprintf("t%d", i), v1alpha1.TaskSpec{Title: "t", Assignee: &v1alpha1.Assignee{Kind: v1alpha1.AssigneeAgent, Name: "coder"}}); err != nil {
			t.Fatal(err)
		}
	}
	c := e.claim(t)
	for i := range 5 {
		if _, err := e.m.RunCreateSubtask(ctx, storage.Key{Namespace: ns, Name: c.Task.Name}, c.RunID, subSpec(fmt.Sprintf("sub %d", i))); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(200 * time.Millisecond) // let any stray hint land
	team, err := e.store.GetTeam(ctx, ns)
	if err != nil {
		t.Fatal(err)
	}
	if got := team.Status.NextNumber; got != 26 {
		t.Fatalf("next number %d after 25 tasks, want 26: numbers were claimed twice", got)
	}
}

// createHook is a store that calls after with each task it creates, before
// the create returns.
type createHook struct {
	storage.Driver
	after func(storage.Key)
}

func (s *createHook) CreateTask(ctx context.Context, t *v1alpha1.Task) error {
	if err := s.Driver.CreateTask(ctx, t); err != nil {
		return err
	}
	s.after(storage.Key{Namespace: t.Namespace, Name: t.Name})
	return nil
}

// A subtask's creation hint can be handled before the run that asked for
// it numbers it: a hint worker that claimed the parent's hint dispatches
// the parent, and the subtask's own hint comes in before RunCreateSubtask
// marks it busy. Both then number it; it takes one number all the same.
func TestSubtaskNumberedOnceWhenItsHintComesFirst(t *testing.T) {
	e := newEnv(t, 5)
	parent := e.addTask(t, "parent", v1alpha1.AssigneeAgent)
	c := e.claim(t)
	hooked := &createHook{Driver: e.store}
	m := e.manager(hooked)
	var hinted *v1alpha1.Task
	hooked.after = func(key storage.Key) {
		// The subtask's hint, handled as a worker would, in this window.
		if err := m.handleHint(ctx, key); err != nil {
			t.Errorf("hint: %v", err)
		}
		hinted = e.get(t, key)
	}
	before := e.team(t).Status.NextNumber
	created, err := m.RunCreateSubtask(ctx, parent, c.RunID, subSpec("raced"))
	if err != nil {
		t.Fatal(err)
	}
	if hinted == nil || hinted.Status.Number == 0 {
		t.Fatalf("the hint did not number the subtask first: %+v", hinted)
	}
	if created.Number != hinted.Status.Number {
		t.Errorf("RunCreateSubtask says #%d, the hint numbered it #%d", created.Number, hinted.Status.Number)
	}
	if got := e.team(t).Status.NextNumber; got != before+1 {
		t.Errorf("next number %d after one subtask from %d: a number was claimed twice", got, before)
	}
}

func (e *env) team(t *testing.T) *v1alpha1.Team {
	t.Helper()
	team, err := e.store.GetTeam(ctx, ns)
	if err != nil {
		t.Fatal(err)
	}
	return team
}

// Calls numbering one task at once take turns: one claims, the rest find it
// numbered.
func TestConcurrentNumberingClaimsOnce(t *testing.T) {
	e := newEnv(t, 5)
	key := storage.Key{Namespace: ns, Name: "contended"}
	if err := e.store.CreateTask(ctx, &v1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: key.Name}, Spec: subSpec("contended")}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	numbers := make([]int64, 8)
	for i := range numbers {
		wg.Go(func() {
			got, err := e.m.numberTask(ctx, key)
			if err != nil {
				t.Errorf("number: %v", err)
				return
			}
			numbers[i] = got.Status.Number
		})
	}
	wg.Wait()
	for _, n := range numbers {
		if n != numbers[0] || n == 0 {
			t.Fatalf("callers saw numbers %v, want one number", numbers)
		}
	}
	// The counter moved past the one number claimed, no further.
	if got := e.team(t).Status.NextNumber; got != numbers[0]+1 {
		t.Errorf("next number %d after #%d: a number was claimed more than once", got, numbers[0])
	}
}

// gatedGet is a store whose first GetTask stops until it is released, and
// that counts the GetTask calls it has begun.
type gatedGet struct {
	storage.Driver
	mu      sync.Mutex
	started int
	inside  chan struct{} // closed when the first call has stopped
	release chan struct{}
}

func (g *gatedGet) GetTask(ctx context.Context, key storage.Key) (*v1alpha1.Task, error) {
	g.mu.Lock()
	g.started++
	first := g.started == 1
	g.mu.Unlock()
	if first {
		close(g.inside)
		<-g.release
	}
	return g.Driver.GetTask(ctx, key)
}

func (g *gatedGet) begun() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.started
}

// Calls of numberTask for one key take turns: while the first is inside,
// the second waits for the lock without reading the task, and once the
// first is done the second finds the task numbered and claims nothing.
// This holds on every run, unlike the race TestOneNumberPerTask waits for.
func TestNumberTaskCallsTakeTurns(t *testing.T) {
	e := newEnv(t, 5)
	key := storage.Key{Namespace: ns, Name: "unborn"}
	if err := e.store.CreateTask(ctx, &v1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: key.Name},
		Spec:       v1alpha1.TaskSpec{Title: "unborn", Assignee: &v1alpha1.Assignee{Kind: v1alpha1.AssigneeAgent, Name: "coder"}},
	}); err != nil {
		t.Fatal(err)
	}
	gate := &gatedGet{Driver: e.store, inside: make(chan struct{}), release: make(chan struct{})}
	// Released however the test ends, so a failure leaves no call stopped.
	release := sync.OnceFunc(func() { close(gate.release) })
	t.Cleanup(release)
	m := e.manager(gate)

	type result struct {
		task *v1alpha1.Task
		err  error
	}
	call := func() chan result {
		out := make(chan result, 1)
		go func() {
			tk, err := m.numberTask(ctx, key)
			out <- result{tk, err}
		}()
		return out
	}
	first := call()
	<-gate.inside
	second := call()

	// The second call waits for the lock once it is counted as a holder;
	// without the lock it reads the task instead.
	eventually(t, "the second call waiting or reading", func() bool {
		m.numberingMu.Lock()
		defer m.numberingMu.Unlock()
		l := m.numbering[key]
		return gate.begun() > 1 || l != nil && l.holders == 2
	})
	if n := gate.begun(); n != 1 {
		t.Fatalf("%d reads of the task begun while the first call is inside, want 1: the second did not wait", n)
	}

	release()
	var numbers []int64
	for _, c := range []chan result{first, second} {
		r := <-c
		if r.err != nil {
			t.Fatal(r.err)
		}
		numbers = append(numbers, r.task.Status.Number)
	}
	if numbers[0] != 1 || numbers[1] != 1 {
		t.Errorf("numbers %v, want both calls to return the one number 1", numbers)
	}
	if got := e.team(t).Status.NextNumber; got != 2 {
		t.Errorf("next number %d after one task, want 2: a number was claimed twice", got)
	}
}
