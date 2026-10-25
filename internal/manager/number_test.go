package manager

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	"github.com/j75689/cadre/internal/storage"
)

// laggingIndex is a store whose index has not seen some tasks yet, and
// counts its reads.
type laggingIndex struct {
	storage.Driver
	mu      sync.Mutex
	hidden  map[string]time.Time // task name: hidden until then
	queries []storage.Query
}

func (l *laggingIndex) hide(name string, until time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.hidden[name] = until
}

func (l *laggingIndex) ListTasks(ctx context.Context, q storage.Query) ([]v1alpha1.Task, error) {
	l.mu.Lock()
	l.queries = append(l.queries, q)
	l.mu.Unlock()
	tasks, err := l.Driver.ListTasks(ctx, q)
	var out []v1alpha1.Task
	for _, t := range tasks {
		l.mu.Lock()
		until, hidden := l.hidden[t.Name]
		l.mu.Unlock()
		if !hidden || time.Now().After(until) {
			out = append(out, t)
		}
	}
	return out, err
}

func (l *laggingIndex) reads() []storage.Query {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]storage.Query(nil), l.queries...)
}

func lagging(t *testing.T) (*env, *laggingIndex) {
	t.Helper()
	e := newEnv(t, 1)
	idx := &laggingIndex{Driver: e.store, hidden: map[string]time.Time{}}
	e.m = e.manager(idx)
	return e, idx
}

// A task is found by its number with one read of the index, even on a
// board of more than one page and with it on the last.
func TestTaskByNumberIsOneIndexedRead(t *testing.T) {
	e, idx := lagging(t)
	var last *v1alpha1.Task
	for i := range 600 {
		var err error
		if last, err = e.m.CreateTask(ctx, ns, fmt.Sprintf("t-%d", i), v1alpha1.TaskSpec{Title: "t", Priority: v1alpha1.PriorityLow}); err != nil {
			t.Fatal(err)
		}
	}
	before := len(idx.reads())
	got, err := e.m.TaskByNumber(ctx, ns, last.Status.Number)
	if err != nil || got.Name != last.Name {
		t.Fatalf("#%d: %+v, %v", last.Status.Number, got, err)
	}
	if reads := idx.reads()[before:]; len(reads) != 1 || reads[0].Number != last.Status.Number || reads[0].Limit != 0 || reads[0].Offset != 0 {
		t.Fatalf("index reads %+v, want one by number", reads)
	}
}

// A task created and looked up at once is found, though the index has not
// seen it yet.
func TestTaskByNumberJustCreated(t *testing.T) {
	e, idx := lagging(t)
	created, err := e.m.CreateTask(ctx, ns, "new", v1alpha1.TaskSpec{Title: "new"})
	if err != nil {
		t.Fatal(err)
	}
	idx.hide("new", time.Now().Add(300*time.Millisecond))
	got, err := e.m.TaskByNumber(ctx, ns, created.Status.Number)
	if err != nil || got.Name != "new" {
		t.Fatalf("a task just created: %+v, %v", got, err)
	}
}

// A number never given is not found at once, with no waiting; one given
// whose task the index never shows, deleted say, is not found once the
// bounded wait is over, and not before.
func TestTaskByNumberNotFound(t *testing.T) {
	e, idx := lagging(t)
	gone, err := e.m.CreateTask(ctx, ns, "gone", v1alpha1.TaskSpec{Title: "gone"})
	if err != nil {
		t.Fatal(err)
	}
	idx.hide("gone", time.Now().Add(time.Hour))

	start := time.Now()
	if _, err := e.m.TaskByNumber(ctx, ns, gone.Status.Number+1); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("a number never given: %v", err)
	}
	if took := time.Since(start); took >= numberPoll {
		t.Fatalf("a number never given took %s", took)
	}

	start = time.Now()
	if _, err := e.m.TaskByNumber(ctx, ns, gone.Status.Number); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("a number whose task is gone: %v", err)
	}
	if took := time.Since(start); took < numberWait || took > numberWait+time.Second {
		t.Fatalf("a number whose task is gone took %s, want the %s wait", took, numberWait)
	}
	for _, n := range []int64{0, -1} {
		if _, err := e.m.TaskByNumber(ctx, ns, n); !errors.Is(err, storage.ErrNotFound) {
			t.Errorf("#%d: %v", n, err)
		}
	}
}
