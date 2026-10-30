package api_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	"github.com/j75689/cadre/internal/api"
	"github.com/j75689/cadre/internal/authz"
	"github.com/j75689/cadre/internal/manager"
	"github.com/j75689/cadre/internal/storage"
	"github.com/j75689/cadre/internal/storage/memory"
)

const (
	run1 = "01J00000000000000000000001"
	run2 = "01J00000000000000000000002"
)

// runsEnv serves team-a with a hub that tails runs, and task y running
// run1.
func runsEnv(t *testing.T) (*rolesEnv, storage.Driver, *api.Hub) {
	t.Helper()
	store := memory.NewBackend().Open()
	hub := api.NewHub(store)
	hub.TailRuns(store)
	e := startRolesWith(t, store, nil, &authz.Policy{}, api.WithWatch(hub, fastWatch))
	if err := store.CreateTask(ctx, &v1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "y"}, Spec: v1alpha1.TaskSpec{Title: "y"}}); err != nil {
		t.Fatal(err)
	}
	setRun(t, store, run1)
	return e, store, hub
}

// setRun gives y run, or no run for "".
func setRun(t *testing.T, store storage.Driver, run string) {
	t.Helper()
	tk, err := store.GetTask(ctx, storage.Key{Namespace: "team-a", Name: "y"})
	if err != nil {
		t.Fatal(err)
	}
	tk.Status.Phase, tk.Status.Run = v1alpha1.PhaseRunning, &v1alpha1.Run{ID: run}
	if run == "" {
		tk.Status.Phase, tk.Status.Run = v1alpha1.PhaseDone, nil
	}
	if err := store.UpdateTaskStatus(ctx, tk); err != nil {
		t.Fatal(err)
	}
}

// appendEvent appends event seq to y's run, as RecordEvent does.
func appendEvent(t *testing.T, store storage.Driver, run string, seq int64) {
	t.Helper()
	stream := manager.EventsStream(storage.Key{Namespace: "team-a", Name: "y"}, run)
	if err := store.Append(ctx, stream, storage.SeqKey(seq), []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
}

// runHint skips task hints to the next run hint, which it returns.
func runHint(t *testing.T, events <-chan sseEvent) string {
	t.Helper()
	for {
		if ev := next(t, events); ev.kind == "run" {
			return ev.data
		} else if ev.kind != "task" {
			t.Fatalf("event %+v, want a run hint", ev)
		}
	}
}

// taskHint skips to a task hint of y whose data has want.
func taskHint(t *testing.T, events <-chan sseEvent, want string) {
	t.Helper()
	for ev := next(t, events); ev.kind != "task" || !strings.Contains(ev.data, want); ev = next(t, events) {
		if ev.kind == "run" {
			t.Fatalf("run hint %s, want a task hint", ev.data)
		}
	}
}

// tailers waits up to a second for hub to have want run tailers.
func tailers(t *testing.T, hub *api.Hub, want int64) {
	t.Helper()
	for start := time.Now(); hub.RunTailers() != want; time.Sleep(10 * time.Millisecond) {
		if time.Since(start) > time.Second {
			t.Fatalf("%d run tailers, want %d", hub.RunTailers(), want)
		}
	}
}

func TestWatchRuns(t *testing.T) {
	e, store, hub := runsEnv(t)
	viewer := e.as("eve@example.test", "readers")
	_, events := watch(t, e.url, viewer, "/namespaces/team-a/watch?task=y")
	if ev := next(t, events); ev.kind != "hello" || !strings.Contains(ev.data, `"runs":true`) {
		t.Fatalf("first event %+v, want hello with runs", ev)
	}
	// Streams of another task, or of none, get no run hints of y.
	_, others := watch(t, e.url, viewer, "/namespaces/team-a/watch?task=x")
	if ev := next(t, others); ev.kind != "hello" || !strings.Contains(ev.data, `"runs":true`) {
		t.Fatalf("x's first event %+v, want hello with runs: x gets them once it has a run", ev)
	}
	_, board := watch(t, e.url, viewer, "/namespaces/team-a/watch")
	if ev := next(t, board); !strings.Contains(ev.data, `"runs":false`) {
		t.Fatalf("the board's first event %+v, want hello without runs", ev)
	}
	tailers(t, hub, 1)

	start := time.Now()
	appendEvent(t, store, run1, 1)
	if got := runHint(t, events); got != `{"task":"y","run":"`+run1+`","seq":1}` {
		t.Fatalf("run hint %s, want seq 1 of run1", got)
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("hint %s after the append, want ≤ 1s", d)
	}
	// Appends between two listings are one hint, with the latest seq.
	for seq := range int64(3) {
		appendEvent(t, store, run1, seq+2)
	}
	for got := runHint(t, events); !strings.Contains(got, `"seq":4`); got = runHint(t, events) {
		if !strings.Contains(got, run1) {
			t.Fatalf("run hint %s, want run1's", got)
		}
	}

	// A new run is followed, and the old one no longer tailed: the task
	// hint comes after the switch.
	setRun(t, store, run2)
	taskHint(t, events, `"phase":"Running"`)
	tailers(t, hub, 1)
	appendEvent(t, store, run1, 5)
	appendEvent(t, store, run2, 1)
	if got := runHint(t, events); got != `{"task":"y","run":"`+run2+`","seq":1}` {
		t.Fatalf("run hint %s, want seq 1 of run2", got)
	}
	// An ended run is not tailed.
	setRun(t, store, "")
	taskHint(t, events, `"phase":"Done"`)
	tailers(t, hub, 0)
	appendEvent(t, store, run2, 2)
	quiet(t, events, 700*time.Millisecond)
	for _, s := range []<-chan sseEvent{others, board} {
		for len(s) > 0 {
			if ev := <-s; ev.kind == "run" {
				t.Fatalf("run hint %s on a stream of another task", ev.data)
			}
		}
	}
}

// A stream opened before its task has a run gets the run's hints once it
// has one.
func TestWatchRunsBeforeRun(t *testing.T) {
	e, store, hub := runsEnv(t)
	setRun(t, store, "")
	_, events := watch(t, e.url, e.as("eve@example.test", "readers"), "/namespaces/team-a/watch?task=y")
	if ev := next(t, events); ev.kind != "hello" || !strings.Contains(ev.data, `"runs":true`) {
		t.Fatalf("first event %+v, want hello with runs", ev)
	}
	tailers(t, hub, 0)
	setRun(t, store, run1)
	taskHint(t, events, `"phase":"Running"`)
	start := time.Now()
	appendEvent(t, store, run1, 1)
	if got := runHint(t, events); got != `{"task":"y","run":"`+run1+`","seq":1}` {
		t.Fatalf("run hint %s, want seq 1 of run1", got)
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("hint %s after the append, want ≤ 1s", d)
	}
}

// Streams of one task share its tailer, which stops with the last of them.
func TestWatchRunsShared(t *testing.T) {
	e, _, hub := runsEnv(t)
	viewer := e.as("eve@example.test", "readers")
	var cancels []context.CancelFunc
	for range 3 {
		open, cancel := context.WithCancel(ctx)
		cancels = append(cancels, cancel)
		_, events := watchWith(open, t, http.DefaultClient, e.url, viewer, "/namespaces/team-a/watch?task=y")
		next(t, events)
	}
	tailers(t, hub, 1)
	for _, cancel := range cancels[1:] {
		cancel()
	}
	time.Sleep(100 * time.Millisecond)
	tailers(t, hub, 1)
	cancels[0]()
	tailers(t, hub, 0)
}

// Past MaxRunTails tailed streams, a stream says runs false; a closed one
// frees its place.
func TestWatchRunsCap(t *testing.T) {
	e, _, hub := runsEnv(t)
	viewer := e.as("eve@example.test", "readers")
	all, closeAll := context.WithCancel(ctx)
	t.Cleanup(closeAll)
	hello := func(open context.Context) string {
		t.Helper()
		res, events := watchWith(open, t, http.DefaultClient, e.url, viewer, "/namespaces/team-a/watch?task=y")
		if res.StatusCode != http.StatusOK {
			t.Fatalf("watch: %d", res.StatusCode)
		}
		return next(t, events).data
	}
	first, cancel := context.WithCancel(all)
	if got := hello(first); !strings.Contains(got, `"runs":true`) {
		t.Fatalf("stream 1: %s, want runs", got)
	}
	for i := 2; i <= api.MaxRunTails; i++ {
		if got := hello(all); !strings.Contains(got, `"runs":true`) {
			t.Fatalf("stream %d: %s, want runs", i, got)
		}
	}
	if got := hello(all); !strings.Contains(got, `"runs":false`) {
		t.Fatalf("stream %d: %s, want no runs", api.MaxRunTails+1, got)
	}
	tailers(t, hub, 1)
	cancel()
	for start := time.Now(); !strings.Contains(hello(all), `"runs":true`); {
		if time.Since(start) > time.Second {
			t.Fatal("a closed stream's place is not freed")
		}
	}
}

func TestWatchRunsUnknownTask(t *testing.T) {
	e, _, hub := runsEnv(t)
	if code, _ := watch(t, e.url, e.as("eve@example.test", "readers"), "/namespaces/team-a/watch?task=nope"); code != http.StatusNotFound {
		t.Errorf("an unknown task: %d, want 404", code)
	}
	tailers(t, hub, 0)
}

// A hub without a Notifier cannot follow a run, so tails none.
func TestWatchRunsWithoutNotifier(t *testing.T) {
	store := memory.NewBackend().Open()
	hub := api.NewHub(nil)
	hub.TailRuns(store)
	e := startRolesWith(t, store, nil, &authz.Policy{}, api.WithWatch(hub, fastWatch))
	if err := store.CreateTask(ctx, &v1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "y"}, Spec: v1alpha1.TaskSpec{Title: "y"}}); err != nil {
		t.Fatal(err)
	}
	setRun(t, store, run1)
	_, events := watch(t, e.url, e.as("eve@example.test", "readers"), "/namespaces/team-a/watch?task=y")
	if ev := next(t, events); !strings.Contains(ev.data, `"runs":false`) {
		t.Fatalf("first event %+v, want hello without runs", ev)
	}
}
