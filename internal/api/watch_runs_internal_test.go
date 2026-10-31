package api

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	"github.com/j75689/cadre/internal/storage"
	"github.com/j75689/cadre/internal/storage/memory"
)

// stuckLog is a Log whose Keys waits until its context ends.
type stuckLog struct {
	storage.Log
	listing chan struct{}
}

func (l stuckLog) Keys(ctx context.Context, _, _ string, _ int) ([]storage.KeyInfo, error) {
	l.listing <- struct{}{}
	<-ctx.Done()
	return nil, ctx.Err()
}

// countLog is a Log that counts its listings.
type countLog struct {
	storage.Log
	n *atomic.Int64
}

func (l countLog) Keys(ctx context.Context, stream, after string, limit int) ([]storage.KeyInfo, error) {
	l.n.Add(1)
	return l.Log.Keys(ctx, stream, after, limit)
}

// tailHub is a hub that tails runs from l, with w watching team-a/y's run.
func tailHub(t *testing.T, l storage.Log) (*Hub, *watcher) {
	t.Helper()
	h := NewHub(memory.NewBackend().Open())
	h.TailRuns(l)
	w, _ := h.subscribe("team-a", 10)
	key := storage.Key{Namespace: "team-a", Name: "y"}
	if !h.tailRun(key, w) {
		t.Fatal("y's run is not tailed")
	}
	h.followRun(key, "01J00000000000000000000001")
	t.Cleanup(func() { h.untailRun(key, w) })
	return h, w
}

// A store that does not answer holds up no publisher, no stream and no
// run switch: the tailer lists without h.mu.
func TestRunTailOutsideLock(t *testing.T) {
	l := stuckLog{listing: make(chan struct{}, 1)}
	h, _ := tailHub(t, l)
	listed := func() {
		t.Helper()
		select {
		case <-l.listing:
		case <-time.After(time.Second):
			t.Fatal("no listing in a second")
		}
	}
	listed()
	done := make(chan struct{})
	go func() {
		defer close(done)
		y := &v1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "y"},
			Status: v1alpha1.TaskStatus{Run: &v1alpha1.Run{ID: "01J00000000000000000000002"}}}
		h.publishTask(y)
		w, _ := h.subscribe("team-a", 10)
		h.unsubscribe("team-a", w)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("the hub waits on a tailer's listing")
	}
	// The stuck listing is cancelled with its run, and run 2 listed.
	listed()
	for start := time.Now(); h.runs.live.Load() != 1; time.Sleep(time.Millisecond) {
		if time.Since(start) > time.Second {
			t.Fatalf("%d tailers, want run 2's only", h.runs.live.Load())
		}
	}
}

// Another namespace's task of the same name and run is another tail.
func TestRunTailNamespaces(t *testing.T) {
	store := memory.NewBackend().Open()
	h, w := tailHub(t, store)
	other, _ := h.subscribe("team-b", 10)
	key := storage.Key{Namespace: "team-b", Name: "y"}
	if !h.tailRun(key, other) {
		t.Fatal("team-b's y is not tailed")
	}
	h.followRun(key, "01J00000000000000000000001")
	defer h.untailRun(key, other)
	if err := store.Append(t.Context(), "team-b/y/events/01J00000000000000000000001", storage.SeqKey(1), []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	if ev := <-other.events; ev.kind != "run" || ev.data.(RunHint).Task != "y" || ev.data.(RunHint).Seq != 1 {
		t.Fatalf("team-b's event %+v, want a run hint", ev)
	}
	time.Sleep(2 * h.runs.every)
	if len(w.events) != 0 {
		t.Fatalf("team-a's stream got %+v", <-w.events)
	}
}

// A run change published between a stream's read and followRun wins over
// the older read, and the old run's tailer hints no more.
func TestRunTailReadAfterPublish(t *testing.T) {
	h := NewHub(memory.NewBackend().Open())
	h.TailRuns(memory.NewBackend().Open())
	w, _ := h.subscribe("team-a", 10)
	key := storage.Key{Namespace: "team-a", Name: "y"}
	h.tailRun(key, w)
	defer h.untailRun(key, w)
	h.publishTask(&v1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "y"},
		Status: v1alpha1.TaskStatus{Run: &v1alpha1.Run{ID: "01J00000000000000000000002"}}})
	if h.followRun(key, "01J00000000000000000000001"); h.runs.tasks[key].run != "01J00000000000000000000002" {
		t.Fatalf("tailing %q, want the published run", h.runs.tasks[key].run)
	}
	// A listing of the run before, done as it was switched, is no hint.
	if ev := <-w.events; ev.kind != "task" {
		t.Fatalf("event %+v, want the task hint", ev)
	}
	h.publishRun(key, "01J00000000000000000000001", 9)
	if len(w.events) != 0 {
		t.Fatalf("hint %+v from the run before", <-w.events)
	}
}

// A tailer lists its run every 500 ms.
func TestRunTailInterval(t *testing.T) {
	l := countLog{Log: memory.NewBackend().Open(), n: &atomic.Int64{}}
	tailHub(t, l)
	time.Sleep(1200 * time.Millisecond)
	if n := l.n.Load(); n < 2 || n > 4 {
		t.Errorf("%d listings in 1.2s, want 3", n)
	}
}

// A run hint to a full stream is one resync, as a task hint is, and holds
// up no publisher.
func TestRunTailFullStream(t *testing.T) {
	store := memory.NewBackend().Open()
	h, w := tailHub(t, store)
	// A send that waits would hold h.mu: free it for the cleanup.
	t.Cleanup(func() {
		select {
		case <-w.events:
		default:
		}
	})
	for range cap(w.events) {
		w.events <- watchEvent{kind: "task"}
	}
	if err := store.Append(t.Context(), "team-a/y/events/01J00000000000000000000001", storage.SeqKey(1), []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		// After the tailer's next listing.
		time.Sleep(2 * h.runs.every)
		h.publishTask(&v1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "y"},
			Status: v1alpha1.TaskStatus{Run: &v1alpha1.Run{ID: "01J00000000000000000000001"}}})
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("the hub waits on a full stream")
	}
	if ev := <-w.events; ev.kind != "resync" {
		t.Fatalf("event %+v, want a resync", ev)
	}
	if ev := <-w.events; ev.kind != "task" || len(w.events) != 0 {
		t.Fatalf("event %+v and %d more, want the task hint alone", ev, len(w.events))
	}
}
