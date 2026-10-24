package manager

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	"github.com/j75689/cadre/internal/storage"
	"github.com/j75689/cadre/internal/storage/memory"
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
	for name, spec := range map[string]v1alpha1.TaskSpec{
		"blank title":       {Title: "  "},
		"unknown priority":  {Title: "x", Priority: "whenever"},
		"unknown assignee":  {Title: "x", Assignee: &v1alpha1.Assignee{Kind: "Robot", Name: "r"}},
		"nameless assignee": {Title: "x", Assignee: &v1alpha1.Assignee{Kind: v1alpha1.AssigneeAgent}},
		"nameless parent":   {Title: "x", ParentRef: &v1alpha1.TaskReference{}},
		"own parent":        {Title: "x", ParentRef: &v1alpha1.TaskReference{Name: "self"}},
	} {
		if _, err := e.m.CreateTask(ctx, ns, "self", spec); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v, want ErrInvalid", name, err)
		}
	}
}

func TestCreateTaskRejectsBadNames(t *testing.T) {
	e := newEnv(t, 1)
	for _, name := range []string{"Fix_Login", "-x", strings.Repeat("a", 254), "a/b"} {
		if _, err := e.m.CreateTask(ctx, ns, name, v1alpha1.TaskSpec{Title: "x"}); !errors.Is(err, ErrInvalid) {
			t.Errorf("name %.20q: %v, want ErrInvalid", name, err)
		}
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
				if _, err := m.AppendThread(ctx, key, Author{Kind: AuthorAgent, Name: fmt.Sprintf("w%d", w)}, EntryComment, fmt.Sprintf("comment %d", i)); err != nil {
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

// A reply whose Reply transition failed is retried: the thread keeps one
// answer, and the retry moves the task on.
func TestAnswerRetryWritesOneAnswer(t *testing.T) {
	e := newEnv(t, 1)
	key := e.addTask(t, "t", v1alpha1.AssigneeAgent)
	c := e.claim(t)
	if _, _, err := e.m.Finish(ctx, key, FinishRequest{RunID: c.RunID, Outcome: task.OutcomeWaiting, Reason: v1alpha1.WaitingQuestion}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.m.AppendThread(ctx, key, alice, EntryComment, "looking"); err != nil {
		t.Fatal(err)
	}
	// The answer's revision advance lands; the Reply write after it fails.
	m := e.manager(&failSecondWrite{Driver: e.store})
	if _, _, err := m.Answer(ctx, key, alice, "use staging"); !errors.Is(err, errTimeout) {
		t.Fatalf("first answer: %v, want the injected failure", err)
	}
	tk, entry, err := e.m.Answer(ctx, key, alice, "use staging")
	if err != nil || tk.Status.Phase != v1alpha1.PhaseTodo || entry.Seq != 2 {
		t.Fatalf("retried answer: %+v %+v %v", tk, entry, err)
	}
	entries, _ := e.m.ReadThread(ctx, key, 0, 0)
	answers := 0
	for _, en := range entries {
		if en.Kind == EntryAnswer {
			answers++
		}
	}
	if answers != 1 {
		t.Fatalf("%d answers in the thread, want 1: %+v", answers, entries)
	}

	// The next wait gets its own answer.
	c = e.claim(t)
	if _, _, err := e.m.Finish(ctx, key, FinishRequest{RunID: c.RunID, Outcome: task.OutcomeWaiting, Reason: v1alpha1.WaitingQuestion, ThreadSeq: 2}); err != nil {
		t.Fatal(err)
	}
	if _, entry, err := e.m.Answer(ctx, key, alice, "and now production"); err != nil || entry.Seq != 3 {
		t.Fatalf("answer to the next wait: %+v %v", entry, err)
	}
}

// failSecondWrite lets the first task status write through and fails the
// second, without effect: the answer's revision advance lands, the Reply
// does not.
type failSecondWrite struct {
	storage.Driver
	n int
}

func (f *failSecondWrite) UpdateTaskStatus(c context.Context, t *v1alpha1.Task) error {
	f.n++
	if f.n == 2 {
		return errTimeout
	}
	return f.Driver.UpdateTaskStatus(c, t)
}

// A run that just ended is in the history before the dispatcher moves it out
// of the outbox.
func TestRunsIncludePendingRunEnded(t *testing.T) {
	e := newEnv(t, 1)
	key := e.addTask(t, "t", v1alpha1.AssigneeAgent)
	c := e.claim(t)
	if _, _, err := e.m.Finish(ctx, key, FinishRequest{RunID: c.RunID, Outcome: task.OutcomeDone}); err != nil {
		t.Fatal(err)
	}
	for _, dispatched := range []bool{false, true} {
		if dispatched {
			if err := e.m.Dispatch(ctx); err != nil {
				t.Fatal(err)
			}
		}
		runs, err := e.m.Runs(ctx, key)
		if err != nil || len(runs) != 1 || runs[0].ID != c.RunID || runs[0].Outcome != task.OutcomeDone {
			t.Fatalf("runs (dispatched %v): %+v, %v", dispatched, runs, err)
		}
	}
	// Events are found by a lowercase run id too, and only for runs the task
	// had.
	if _, err := e.m.RunEvents(ctx, key, strings.ToLower(c.RunID)); err != nil {
		t.Fatalf("events by lowercase run id: %v", err)
	}
	if _, err := e.m.RunEvents(ctx, key, "01JB0000000000000000000000"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("events of a run the task never had: %v", err)
	}
}

func TestAppendThreadKinds(t *testing.T) {
	e := newEnv(t, 1)
	key := e.addTask(t, "t", v1alpha1.AssigneeAgent)
	for name, tc := range map[string]struct {
		author Author
		kind   EntryKind
	}{
		"no author kind":      {Author{Name: "a"}, EntryComment},
		"unknown author kind": {Author{Kind: "Robot", Name: "a"}, EntryComment},
		"unknown entry kind":  {alice, "Shout"},
	} {
		if _, err := e.m.AppendThread(ctx, key, tc.author, tc.kind, "x"); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v, want ErrInvalid", name, err)
		}
	}
}

// A transition that changes nothing writes nothing.
func TestNoOpTransitionDoesNotWrite(t *testing.T) {
	e := newEnv(t, 1)
	key := e.addTask(t, "t", v1alpha1.AssigneeAgent)
	if _, err := e.m.AppendThread(ctx, key, alice, EntryComment, "x"); err != nil {
		t.Fatal(err)
	}
	before := e.get(t, key).ResourceVersion
	if _, err := e.m.transition(ctx, key, task.ThreadAdvanced{Seq: 1}); err != nil {
		t.Fatal(err)
	}
	if after := e.get(t, key).ResourceVersion; after != before {
		t.Fatalf("a no-op transition wrote: version %s → %s", before, after)
	}
}

// Unnumbered tasks in a namespace whose Team is gone are reported.
func TestNumberNewReportsAMissingTeam(t *testing.T) {
	b := memory.NewBackend()
	store := b.Open()
	if err := store.CreateTeam(ctx, &v1alpha1.Team{ObjectMeta: metav1.ObjectMeta{Name: v1alpha1.TeamName, Namespace: ns}}); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateTask(ctx, &v1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: "t", Namespace: ns}, Spec: v1alpha1.TaskSpec{Title: "t"}}); err != nil {
		t.Fatal(err)
	}
	m := New(&teamGone{Driver: store}, Config{})
	if err := m.NumberNew(ctx); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("NumberNew with the Team gone: %v", err)
	}
}

// teamGone lists the Team but cannot read it, as after a deletion the index
// has not seen yet.
type teamGone struct{ storage.Driver }

func (teamGone) GetTeam(context.Context, string) (*v1alpha1.Team, error) {
	return nil, storage.ErrNotFound
}

// thread_ask is one write: the Question rides on the Finish. A retry after
// the Finish landed (its answer lost, say) is answered as finished, and the
// thread keeps one Question, the first attempt's, which the wait names.
func TestRunAskWritesOneQuestion(t *testing.T) {
	e := newEnv(t, 1)
	key := e.addTask(t, "t", v1alpha1.AssigneeAgent)
	c := e.claim(t)
	first, err := e.m.RunAsk(ctx, key, c.RunID, "Staging or production?")
	if err != nil || first.Seq != 1 {
		t.Fatalf("ask: %+v, %v", first, err)
	}
	again, err := e.m.RunAsk(ctx, key, c.RunID, "Staging or production?")
	if !errors.Is(err, task.ErrRunMismatch) && (err != nil || again.ItemID != first.ItemID) {
		t.Fatalf("retried ask: %+v, %v; want the first Question or the run gone", again, err)
	}
	if err := e.m.Dispatch(ctx); err != nil {
		t.Fatal(err)
	}
	thread, _ := e.m.ReadThread(ctx, key, 0, 0)
	if len(thread) != 1 || thread[0].Kind != EntryQuestion {
		t.Fatalf("%d entries, want one Question: %+v", len(thread), thread)
	}
	if tk := e.get(t, key); tk.Status.Phase != v1alpha1.PhaseWaiting || tk.Status.Waiting.ThreadSeq != 1 {
		t.Fatalf("after the retry: %+v", tk.Status)
	}
}

// takenFiles makes every version of every file look taken.
type takenFiles struct{ storage.Driver }

func (f takenFiles) Append(c context.Context, stream, key string, data []byte) error {
	if strings.Contains(stream, "/files/") {
		return fmt.Errorf("%s: %w", key, storage.ErrAlreadyExists)
	}
	return f.Driver.Append(c, stream, key, data)
}

// A write that cannot win a version stays pending: the tool reports no
// version yet, the dispatcher reports itself busy, and nothing is indexed.
func TestWriteFileGivesUpOnAnEndlessVersionRace(t *testing.T) {
	e := newEnv(t, 1)
	key := e.addTask(t, "t", v1alpha1.AssigneeAgent)
	c := e.claim(t)
	m := e.manager(takenFiles{e.store})
	info, err := m.RunWriteFile(ctx, key, c.RunID, "a.txt", "x")
	if err != nil || info.Version != 0 {
		t.Fatalf("write: %+v, %v; want it pending", info, err)
	}
	if err := m.Dispatch(ctx); !errors.Is(err, ErrBusy) {
		t.Fatalf("dispatch: %v, want ErrBusy", err)
	}
	if out := e.get(t, key).Status.Outbox; len(out) != 1 || out[0].Kind != v1alpha1.OutboxWriteFile {
		t.Fatalf("outbox %+v, want the write still pending", out)
	}
	if files, err := e.m.RunListFiles(ctx, key, c.RunID); err != nil || len(files) != 0 {
		t.Fatalf("index after a lost write: %+v, %v", files, err)
	}
}
