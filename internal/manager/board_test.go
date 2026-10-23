package manager

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	"github.com/j75689/cadre/internal/storage"
	"github.com/j75689/cadre/internal/task"
)

var alice = Author{Kind: AuthorUser, Name: "alice"}

func TestCreateTaskNumbersAtOnce(t *testing.T) {
	e := newEnv(t, 1)
	named, err := e.m.CreateTask(ctx, ns, "fix-login", v1alpha1.TaskSpec{Title: "Fix login"})
	if err != nil || named.Name != "fix-login" || named.Status.Number != 1 || named.Status.Phase != v1alpha1.PhaseTodo {
		t.Fatalf("create: %+v, %v", named, err)
	}
	minted, err := e.m.CreateTask(ctx, ns, "", v1alpha1.TaskSpec{Title: "Anything"})
	if err != nil || len(minted.Name) != 26 || minted.Status.Number != 2 {
		t.Fatalf("create without a name: %+v, %v", minted, err)
	}
	if _, err := e.m.CreateTask(ctx, ns, "fix-login", v1alpha1.TaskSpec{Title: "again"}); !errors.Is(err, storage.ErrAlreadyExists) {
		t.Fatalf("duplicate name: %v", err)
	}
	if _, err := e.m.CreateTask(ctx, "no-team", "", v1alpha1.TaskSpec{Title: "x"}); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("namespace without a Team: %v", err)
	}
	if _, err := e.m.CreateTask(ctx, ns, "", v1alpha1.TaskSpec{Title: "  "}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("blank title: %v", err)
	}
}

func TestThreadAppendsRaceForSequenceNumbers(t *testing.T) {
	e := newEnv(t, 1)
	key := e.addTask(t, "t", v1alpha1.AssigneeAgent)
	const writers, each = 4, 5
	var wg sync.WaitGroup
	for w := range writers {
		m := e.manager(e.backend.Open())
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range each {
				if _, err := m.AppendThread(ctx, key, Author{Name: fmt.Sprintf("w%d", w)}, EntryComment, fmt.Sprintf("comment %d", i)); err != nil {
					t.Errorf("append: %v", err)
				}
			}
		}()
	}
	wg.Wait()

	entries, err := e.m.ReadThread(ctx, key, 0, 0)
	if err != nil || len(entries) != writers*each {
		t.Fatalf("read: %d entries, %v", len(entries), err)
	}
	for i, en := range entries {
		if en.Seq != int64(i+1) {
			t.Fatalf("entry %d has seq %d; sequence numbers must be 1..%d without gaps", i, en.Seq, writers*each)
		}
	}
	if rev := e.get(t, key).Status.Thread.Revision; rev != writers*each {
		t.Fatalf("revision %d, want %d", rev, writers*each)
	}
	page, _ := e.m.ReadThread(ctx, key, 18, 5)
	if len(page) != 2 || page[0].Seq != 19 {
		t.Fatalf("read after 18: %+v", page)
	}
}

// An appender that died after its append but before advancing the revision
// leaves the revision behind; the next append skips past its entry.
func TestThreadSkipsAnEntryWhoseRevisionWasLost(t *testing.T) {
	e := newEnv(t, 1)
	key := e.addTask(t, "t", v1alpha1.AssigneeAgent)
	if err := e.store.Append(ctx, ThreadStream(key), storage.SeqKey(1), []byte(`{"seq":1,"author":{"kind":"User","name":"ghost"},"kind":"Comment","body":"lost"}`)); err != nil {
		t.Fatal(err)
	}
	entry, err := e.m.AppendThread(ctx, key, alice, EntryComment, "hello")
	if err != nil || entry.Seq != 2 {
		t.Fatalf("append after a lost revision: %+v, %v", entry, err)
	}
	if rev := e.get(t, key).Status.Thread.Revision; rev != 2 {
		t.Fatalf("revision %d, want 2", rev)
	}
	if _, err := e.m.AppendThread(ctx, key, Author{}, EntryComment, "x"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("append without an author: %v", err)
	}
}

func TestAnswer(t *testing.T) {
	e := newEnv(t, 1)
	key := e.addTask(t, "t", v1alpha1.AssigneeAgent)

	// Not waiting: refused before anything is written.
	if _, _, err := e.m.Answer(ctx, key, alice, "too early"); !errors.Is(err, task.ErrInvalidTransition) {
		t.Fatalf("answer a Todo task: %v", err)
	}
	if entries, _ := e.m.ReadThread(ctx, key, 0, 0); len(entries) != 0 {
		t.Fatalf("a refused answer wrote %d entries", len(entries))
	}

	c := e.claim(t)
	if _, _, err := e.m.Finish(ctx, key, FinishRequest{RunID: c.RunID, Outcome: task.OutcomeWaiting, Reason: v1alpha1.WaitingQuestion}); err != nil {
		t.Fatal(err)
	}
	tk, entry, err := e.m.Answer(ctx, key, alice, "use the staging database")
	if err != nil || tk.Status.Phase != v1alpha1.PhaseTodo || entry.Kind != EntryAnswer || entry.Seq != 1 {
		t.Fatalf("answer: %+v %+v %v", tk, entry, err)
	}
}

func TestRunsAndEvents(t *testing.T) {
	e := newEnv(t, 1)
	key := e.addTask(t, "t", v1alpha1.AssigneeAgent)
	var ids []string
	for range 2 {
		c := e.claim(t)
		ids = append(ids, c.RunID)
		if err := e.m.RecordEvent(ctx, key, Event{RunID: c.RunID, Seq: 1, Progress: "working"}); err != nil {
			t.Fatal(err)
		}
		e.clock.add(time.Minute) // let the lease lapse; reclaim ends the run
		if err := e.m.ReclaimExpired(ctx); err != nil {
			t.Fatal(err)
		}
		if err := e.m.Dispatch(ctx); err != nil {
			t.Fatal(err)
		}
	}
	runs, err := e.m.Runs(ctx, key)
	if err != nil || len(runs) != 2 || !slices.IsSortedFunc(runs, func(a, b v1alpha1.RunRecord) int { return strings.Compare(a.ID, b.ID) }) {
		t.Fatalf("runs: %+v, %v", runs, err)
	}
	for _, r := range runs {
		if r.Reason != v1alpha1.RunInterrupted || !slices.Contains(ids, r.ID) {
			t.Errorf("run record %+v", r)
		}
	}
	events, err := e.m.RunEvents(ctx, key, ids[0])
	if err != nil || len(events) != 1 || events[0].Progress != "working" {
		t.Fatalf("events: %+v, %v", events, err)
	}
	if _, err := e.m.RunEvents(ctx, key, "../../x"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("events for a bad run id: %v", err)
	}
	if _, err := e.m.Runs(ctx, storage.Key{Namespace: ns, Name: "missing"}); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("runs of a missing task: %v", err)
	}
}

func TestInitTeam(t *testing.T) {
	e := newEnv(t, 1)
	team, err := e.m.InitTeam(ctx, "team-b", v1alpha1.TeamSpec{DisplayName: "Backend"})
	if err != nil || team.Spec.MaxConcurrentRuns != 1 {
		t.Fatalf("init: %+v, %v", team, err)
	}
	if _, err := e.m.InitTeam(ctx, "team-b", v1alpha1.TeamSpec{}); !errors.Is(err, storage.ErrAlreadyExists) {
		t.Fatalf("init twice: %v", err)
	}
	teams, _ := e.m.ListTeams(ctx)
	if len(teams) != 2 {
		t.Fatalf("%d teams, want 2", len(teams))
	}
}
