package manager

import (
	"context"
	"errors"
	"fmt"
	"strings"
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
	if took := time.Since(start); took >= numberWait/2 {
		t.Fatalf("a number never given took %s", took)
	}

	start = time.Now()
	if _, err := e.m.TaskByNumber(ctx, ns, gone.Status.Number); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("a number whose task is gone: %v", err)
	}
	if took := time.Since(start); took < numberWait || took >= 3*numberWait {
		t.Fatalf("a number whose task is gone took %s, want the %s wait", took, numberWait)
	}
	for _, n := range []int64{0, -1} {
		if _, err := e.m.TaskByNumber(ctx, ns, n); !errors.Is(err, storage.ErrNotFound) {
			t.Errorf("#%d: %v", n, err)
		}
	}
}

// A number given long ago, more than numberRecent before the next, with no
// task is not found at once: the index cannot be lagging that far, and a
// stale link costs no wait.
func TestTaskByNumberOldIsNotWaitedFor(t *testing.T) {
	e, idx := lagging(t)
	gone, err := e.m.CreateTask(ctx, ns, "gone", v1alpha1.TaskSpec{Title: "gone"})
	if err != nil {
		t.Fatal(err)
	}
	idx.hide("gone", time.Now().Add(time.Hour))
	for i := range numberRecent {
		if _, err := e.m.CreateTask(ctx, ns, fmt.Sprintf("later-%d", i), v1alpha1.TaskSpec{Title: "later"}); err != nil {
			t.Fatal(err)
		}
	}
	start := time.Now()
	if _, err := e.m.TaskByNumber(ctx, ns, gone.Status.Number); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("a number given long ago: %v", err)
	}
	if took := time.Since(start); took >= numberWait/2 {
		t.Fatalf("a number given long ago took %s", took)
	}
}

// A number two tasks share, after a Team made again, say, names neither:
// it is an error that names both.
func TestTaskByNumberAmbiguous(t *testing.T) {
	e, _ := lagging(t)
	for _, name := range []string{"old", "new"} {
		tk := &v1alpha1.Task{Spec: v1alpha1.TaskSpec{Title: name}}
		tk.Namespace, tk.Name = ns, name
		if err := e.store.CreateTask(ctx, tk); err != nil {
			t.Fatal(err)
		}
		got, err := e.store.GetTask(ctx, storage.Key{Namespace: ns, Name: name})
		if err != nil {
			t.Fatal(err)
		}
		got.Status.Number, got.Status.Phase = 3, v1alpha1.PhaseTodo
		if err := e.store.UpdateTaskStatus(ctx, got); err != nil {
			t.Fatal(err)
		}
	}
	_, err := e.m.TaskByNumber(ctx, ns, 3)
	if !errors.Is(err, ErrAmbiguous) || !strings.Contains(err.Error(), "new, old") {
		t.Fatalf("#3 shared: %v", err)
	}
}

// A Team made again in a namespace whose tasks outlived the old one goes on
// numbering above them: no number is given twice.
func TestRecreatedTeamResumesNumbering(t *testing.T) {
	e, idx := lagging(t)
	for i := range 5 {
		if _, err := e.m.CreateTask(ctx, ns, fmt.Sprintf("old-%d", i), v1alpha1.TaskSpec{Title: "old"}); err != nil {
			t.Fatal(err)
		}
	}
	// The Team made again: its status, the counter with it, starts empty.
	team, err := e.store.GetTeam(ctx, ns)
	if err != nil {
		t.Fatal(err)
	}
	team.Status = v1alpha1.TeamStatus{}
	if err := e.store.UpdateTeamStatus(ctx, team); err != nil {
		t.Fatal(err)
	}
	before := len(idx.reads())
	created, err := e.m.CreateTask(ctx, ns, "new", v1alpha1.TaskSpec{Title: "new"})
	if err != nil || created.Status.Number != 6 {
		t.Fatalf("the first task of the Team made again: %+v, %v; want #6", created, err)
	}
	if reads := idx.reads()[before:]; len(reads) != 1 {
		t.Fatalf("index reads %+v, want one", reads)
	}
	// The counter has started: the next one reads nothing more.
	before = len(idx.reads())
	if next, err := e.m.CreateTask(ctx, ns, "newer", v1alpha1.TaskSpec{Title: "newer"}); err != nil || next.Status.Number != 7 {
		t.Fatalf("the second: %+v, %v; want #7", next, err)
	}
	if reads := idx.reads()[before:]; len(reads) != 0 {
		t.Fatalf("index reads %+v, want none", reads)
	}
	if got, err := e.m.TaskByNumber(ctx, ns, 3); err != nil || got.Name != "old-2" {
		t.Fatalf("#3: %+v, %v", got, err)
	}
	// A new Team in an empty namespace starts at 1.
	fresh, _ := lagging(t)
	if first, err := fresh.m.CreateTask(ctx, ns, "first", v1alpha1.TaskSpec{Title: "first"}); err != nil || first.Status.Number != 1 {
		t.Fatalf("a new Team's first task: %+v, %v", first, err)
	}
}
