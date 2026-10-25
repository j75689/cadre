package conformance

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	"github.com/j75689/cadre/internal/storage"
	"github.com/j75689/cadre/internal/task"
)

const parties = 8

// Two parties conditionally write the same object at once; exactly one
// succeeds.
func testConcurrentConditionalWrite(t *testing.T, s *suite) {
	a, b := s.open(), s.open()
	s.newTask(a, "shared", 1, "")
	read := s.get(a, "shared")

	wins, errs := race(parties, []storage.Driver{a, b}, func(i int, d storage.Driver) error {
		mine := read.DeepCopy()
		mine.Status.Thread = &v1alpha1.ThreadStatus{Revision: int64(i + 1)}
		return d.UpdateTaskStatus(s.ctx, mine)
	})
	if wins != 1 {
		t.Fatalf("%d writers succeeded, want exactly 1", wins)
	}
	allErrorsAre(t, errs, storage.ErrConflict)
}

// A write carrying a version older than the stored one conflicts, and a
// successful write returns the new version.
func testStaleVersionConflicts(t *testing.T, s *suite) {
	d := s.open()
	s.newTask(d, "t", 1, "")
	first := s.get(d, "t")
	stale := first.DeepCopy()

	first.Status.Thread = &v1alpha1.ThreadStatus{Revision: 1}
	if err := d.UpdateTaskStatus(s.ctx, first); err != nil {
		t.Fatalf("first write: %v", err)
	}
	if first.ResourceVersion == stale.ResourceVersion {
		t.Fatal("a successful write did not update the version in place")
	}
	if got := s.get(d, "t"); got.ResourceVersion != first.ResourceVersion || got.Status.Thread == nil || got.Status.Thread.Revision != 1 {
		t.Fatalf("read back version %q thread %+v", got.ResourceVersion, got.Status.Thread)
	}
	// The returned version is usable for the next write.
	first.Status.Thread.Revision = 2
	if err := d.UpdateTaskStatus(s.ctx, first); err != nil {
		t.Fatalf("second write with the returned version: %v", err)
	}
	if err := d.UpdateTaskStatus(s.ctx, stale); !errors.Is(err, storage.ErrConflict) {
		t.Fatalf("stale write: got %v, want ErrConflict", err)
	}

	team := &v1alpha1.Team{ObjectMeta: metav1.ObjectMeta{Name: v1alpha1.TeamName, Namespace: s.ns}, Spec: v1alpha1.TeamSpec{MaxConcurrentRuns: 1}}
	if err := d.CreateTeam(s.ctx, team); err != nil {
		t.Fatalf("create team: %v", err)
	}
	staleTeam := team.DeepCopy()
	team.Status.NextNumber = 5
	if err := d.UpdateTeamStatus(s.ctx, team); err != nil {
		t.Fatalf("team write: %v", err)
	}
	if err := d.UpdateTeamStatus(s.ctx, staleTeam); !errors.Is(err, storage.ErrConflict) {
		t.Fatalf("stale team write: got %v, want ErrConflict", err)
	}
}

// Creating the same name from many parties succeeds exactly once, which is
// what makes the outbox dispatcher's create idempotent.
func testCreateIsCreateIfNotExists(t *testing.T, s *suite) {
	a, b := s.open(), s.open()
	wins, errs := race(parties, []storage.Driver{a, b}, func(_ int, d storage.Driver) error {
		return d.CreateTask(s.ctx, &v1alpha1.Task{
			ObjectMeta: metav1.ObjectMeta{Name: "once", Namespace: s.ns},
			Spec:       v1alpha1.TaskSpec{Title: "once"},
		})
	})
	if wins != 1 {
		t.Fatalf("%d creates succeeded, want exactly 1", wins)
	}
	allErrorsAre(t, errs, storage.ErrAlreadyExists)
}

// Create writes spec and metadata only, as the Kubernetes API does.
func testCreateIgnoresStatus(t *testing.T, s *suite) {
	d := s.open()
	tk := &v1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "t", Namespace: s.ns},
		Spec:       v1alpha1.TaskSpec{Title: "t"},
		Status:     v1alpha1.TaskStatus{Phase: v1alpha1.PhaseDone, Number: 9},
	}
	if err := d.CreateTask(s.ctx, tk); err != nil {
		t.Fatalf("create: %v", err)
	}
	if got := s.get(d, "t"); got.Status.Phase != "" || got.Status.Number != 0 || got.Spec.Title != "t" {
		t.Fatalf("stored %+v / %+v", got.Spec, got.Status)
	}
}

func testNotFound(t *testing.T, s *suite) {
	d := s.open()
	if _, err := d.GetTask(s.ctx, s.key("missing")); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("get missing task: got %v", err)
	}
	ghost := &v1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: "missing", Namespace: s.ns, ResourceVersion: "1"}}
	if err := d.UpdateTaskStatus(s.ctx, ghost); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("update missing task: got %v", err)
	}
	if _, err := d.GetTeam(s.ctx, s.ns); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("get missing team: got %v", err)
	}
}

// Two parties claim the same work item; exactly one gets it.
func testClaimExactlyOnce(t *testing.T, s *suite) {
	a, b := s.open(), s.open()
	s.newTask(a, "work", 1, "")
	readers := readBarrier(parties)
	wins, errs := race(parties, []storage.Driver{a, b}, func(i int, d storage.Driver) error {
		return transitionAfter(s.ctx, d, s.key("work"), task.Claim{
			RunID: runID(i), Mode: v1alpha1.RunModeRuntime, Runner: fmt.Sprintf("runner-%d", i), Lease: time.Minute,
		}, readers)
	})
	if wins != 1 {
		t.Fatalf("%d claims succeeded, want exactly 1", wins)
	}
	allErrorsAre(t, errs, task.ErrInvalidTransition)
	got := s.get(a, "work").Status
	if got.Phase != v1alpha1.PhaseRunning || got.Run == nil || got.Runs == nil || got.Runs.Count != 1 || got.Run.ID != got.Runs.LastID {
		t.Fatalf("after the race: %+v", got)
	}
}

// Re-delivering the same step advances state exactly once, and its outbox
// item is written once.
func testRedeliveredStepAdvancesOnce(t *testing.T, s *suite) {
	a, b := s.open(), s.open()
	s.newTask(a, "steps", 1, "")
	run := runID(1)
	if err := transition(s.ctx, a, s.key("steps"), task.Claim{RunID: run, Mode: v1alpha1.RunModeStep, Lease: time.Minute}); err != nil {
		t.Fatalf("claim: %v", err)
	}
	step := task.Advance{RunID: run, Step: 0, Attempt: 1, Runner: "w", Outbox: []v1alpha1.OutboxItem{{
		ID: runID(100), Kind: v1alpha1.OutboxCreateSubtask, Spec: &v1alpha1.TaskSpec{Title: "child"},
	}}}
	readers := readBarrier(parties)
	wins, errs := race(parties, []storage.Driver{a, b}, func(_ int, d storage.Driver) error {
		return transitionAfter(s.ctx, d, s.key("steps"), step, readers)
	})
	if wins != 1 {
		t.Fatalf("step committed %d times, want exactly 1", wins)
	}
	allErrorsAre(t, errs, task.ErrStepMismatch)
	got := s.get(b, "steps").Status
	if got.Run == nil || got.Run.Step != 1 || len(got.Outbox) != 1 {
		t.Fatalf("after the race: run %+v, outbox %+v", got.Run, got.Outbox)
	}
}

// A Notify item, and what it records of its delivery, is read back whole
// by another driver.
func testNotifyItemRoundTrips(t *testing.T, s *suite) {
	first := s.open()
	s.newTask(first, "told", 1, "")
	tk := s.get(first, "told")
	at := metav1.NewTime(time.Date(2026, 10, 25, 9, 0, 0, 0, time.UTC))
	tk.Status.Outbox = append(tk.Status.Outbox, v1alpha1.OutboxItem{ID: runID(200), Kind: v1alpha1.OutboxNotify,
		Notify: &v1alpha1.Notification{Event: v1alpha1.NotifyQuestion, ThreadSeq: 3, At: at, To: []string{"hook", "chat"}}})
	if err := first.UpdateTaskStatus(s.ctx, tk); err != nil {
		t.Fatalf("write the item: %v", err)
	}
	if err := transition(s.ctx, first, s.key("told"), task.NotifyDelivered{ID: runID(200), To: []string{"hook"}}); err != nil {
		t.Fatalf("record a delivery: %v", err)
	}
	got := s.get(s.open(), "told").Status.Outbox
	if len(got) != 1 || got[0].Kind != v1alpha1.OutboxNotify || got[0].Notify == nil {
		t.Fatalf("outbox %+v", got)
	}
	n := got[0].Notify
	if n.Event != v1alpha1.NotifyQuestion || n.ThreadSeq != 3 || !n.At.Equal(&at) || len(n.Delivered) != 1 || n.Delivered[0] != "hook" ||
		strings.Join(n.To, ",") != "hook,chat" {
		t.Errorf("notification %+v", n)
	}
	// The last of its Notifiers taking it removes it, in the same write.
	if err := transition(s.ctx, first, s.key("told"), task.NotifyDelivered{ID: runID(200), To: []string{"chat"}}); err != nil {
		t.Fatalf("record the last delivery: %v", err)
	}
	if got := s.get(s.open(), "told").Status.Outbox; len(got) != 0 {
		t.Errorf("outbox %+v once all took it", got)
	}
}

// Outbox items are reprocessed after the dispatcher dies and take effect
// exactly once.
func testOutboxAppliedOnce(t *testing.T, s *suite) {
	first := s.open()
	s.newTask(first, "parent", 1, "")
	run := runID(1)
	if err := transition(s.ctx, first, s.key("parent"), task.Claim{RunID: run, Mode: v1alpha1.RunModeRuntime, Lease: time.Minute}); err != nil {
		t.Fatalf("claim: %v", err)
	}
	items := []v1alpha1.OutboxItem{
		{ID: runID(101), Kind: v1alpha1.OutboxCreateSubtask, Spec: &v1alpha1.TaskSpec{Title: "one"}},
		{ID: runID(102), Kind: v1alpha1.OutboxCreateSubtask, Spec: &v1alpha1.TaskSpec{Title: "two"}},
	}
	if err := transition(s.ctx, first, s.key("parent"), task.Finish{RunID: run, Outcome: task.OutcomeDone, Outbox: items}); err != nil {
		t.Fatalf("finish: %v", err)
	}

	runs := s.ns + "/parent/runs"
	apply := func(d storage.Driver, it v1alpha1.OutboxItem) error {
		if it.Kind == v1alpha1.OutboxRunEnded {
			err := d.Append(s.ctx, runs, it.ID, []byte(it.Run.Reason))
			if errors.Is(err, storage.ErrAlreadyExists) {
				return nil
			}
			return err
		}
		sub := &v1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: task.SubtaskName(it), Namespace: s.ns}, Spec: *it.Spec.DeepCopy()}
		sub.Spec.ParentRef = &v1alpha1.TaskReference{Name: "parent"}
		if err := d.CreateTask(s.ctx, sub); err != nil && !errors.Is(err, storage.ErrAlreadyExists) {
			return err
		}
		return nil
	}

	// The first dispatcher applies one item, then dies before acking.
	if err := apply(first, items[0]); err != nil {
		t.Fatalf("apply: %v", err)
	}
	_ = first.Close()

	// Two dispatchers restart and both reprocess the whole outbox.
	a, b := s.open(), s.open()
	_, errs := race(2, []storage.Driver{a, b}, func(_ int, d storage.Driver) error {
		pending := s.get(d, "parent").Status.Outbox
		for _, it := range pending {
			if err := apply(d, it); err != nil {
				return err
			}
		}
		ids := make([]string, 0, len(pending))
		for _, it := range pending {
			ids = append(ids, it.ID)
		}
		return transition(s.ctx, d, s.key("parent"), task.AckOutbox{IDs: ids})
	})
	if len(errs) > 0 {
		t.Fatalf("dispatchers: %v", errs)
	}

	if got := s.get(a, "parent"); len(got.Status.Outbox) != 0 {
		t.Fatalf("outbox not cleared: %+v", got.Status.Outbox)
	}
	for _, it := range items {
		sub := s.get(a, task.SubtaskName(it))
		if sub.Spec.Title != it.Spec.Title || sub.Spec.ParentRef == nil || sub.Spec.ParentRef.Name != "parent" {
			t.Errorf("subtask %s: %+v", sub.Name, sub.Spec)
		}
	}
	history, err := a.Read(s.ctx, runs, "", 0)
	if err != nil || len(history) != 1 || history[0].Key != run || string(history[0].Data) != string(v1alpha1.RunFinished) {
		t.Fatalf("run history: %+v, err %v; want one Finished entry for %s", history, err, run)
	}
	eventually(t, "index holds parent and two subtasks", func() error {
		ts, err := a.ListTasks(s.ctx, storage.Query{Namespace: s.ns})
		if err != nil {
			return err
		}
		if len(ts) != 3 {
			return fmt.Errorf("%d tasks, want 3", len(ts))
		}
		return nil
	})
}

// The Team counter hands out every number once, the semaphore never lets
// more than MaxConcurrentRuns runs in, and a slot is given back once however
// often it is released.
func testTeamSemaphoreAndCounter(t *testing.T, s *suite) {
	a, b := s.open(), s.open()
	const maxRuns = 3
	team := &v1alpha1.Team{ObjectMeta: metav1.ObjectMeta{Name: v1alpha1.TeamName, Namespace: s.ns}, Spec: v1alpha1.TeamSpec{MaxConcurrentRuns: maxRuns}}
	if err := a.CreateTeam(s.ctx, team); err != nil {
		t.Fatalf("create team: %v", err)
	}

	numbers := make([]int64, parties)
	if _, errs := race(parties, []storage.Driver{a, b}, func(i int, d storage.Driver) error {
		return s.writeTeam(d, func(cur *v1alpha1.Team) (v1alpha1.TeamStatus, error) {
			n, next := task.ClaimNumber(cur)
			numbers[i] = n
			return next, nil
		})
	}); len(errs) > 0 {
		t.Fatalf("claim numbers: %v", errs)
	}
	slices.Sort(numbers)
	for i, n := range numbers {
		if n != int64(i+1) {
			t.Fatalf("numbers %v, want 1..%d each once", numbers, parties)
		}
	}

	wins, errs := race(parties, []storage.Driver{a, b}, func(i int, d storage.Driver) error {
		return s.writeTeam(d, func(cur *v1alpha1.Team) (v1alpha1.TeamStatus, error) {
			return task.AcquireSlot(cur, runID(i), "work", testNow)
		})
	})
	if wins != maxRuns {
		t.Fatalf("%d parties acquired a slot, want %d", wins, maxRuns)
	}
	allErrorsAre(t, errs, task.ErrNoSlot)
	got := s.team(a)
	if got.Status.Running != maxRuns || len(got.Status.Slots) != maxRuns || got.Status.NextNumber != parties+1 {
		t.Fatalf("team status %+v", got.Status)
	}

	// Releasing is idempotent across processes: every party releases the
	// same slot, and exactly one slot is given back.
	held := got.Status.Slots[0].RunID
	if _, errs := race(parties, []storage.Driver{a, b}, func(_ int, d storage.Driver) error {
		return s.writeTeam(d, func(cur *v1alpha1.Team) (v1alpha1.TeamStatus, error) {
			return task.ReleaseSlot(cur, held), nil
		})
	}); len(errs) > 0 {
		t.Fatalf("release: %v", errs)
	}
	if got := s.team(a); got.Status.Running != maxRuns-1 || slices.ContainsFunc(got.Status.Slots, func(sl v1alpha1.Slot) bool { return sl.RunID == held }) {
		t.Fatalf("after %d releases of one slot: %+v", parties, got.Status)
	}
}

// The process is killed and restarted; the index rebuilds completely.
func testIndexRebuilds(t *testing.T, s *suite) {
	first := s.open()
	const n = 25
	for i := 1; i <= n; i++ {
		s.newTask(first, fmt.Sprintf("t-%02d", i), int64(i), "")
	}
	if err := transition(s.ctx, first, s.key("t-07"), task.Cancel{}); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	_ = first.Close()

	restarted := s.open()
	eventually(t, "index rebuilt after restart", func() error {
		ts, err := restarted.ListTasks(s.ctx, storage.Query{Namespace: s.ns})
		if err != nil {
			return err
		}
		if len(ts) != n {
			return fmt.Errorf("%d tasks, want %d", len(ts), n)
		}
		for i, tk := range ts {
			want := v1alpha1.PhaseTodo
			if tk.Name == "t-07" {
				want = v1alpha1.PhaseCancelled
			}
			if tk.Status.Number != int64(i+1) || tk.Status.Phase != want {
				return fmt.Errorf("entry %d: %s #%d %s", i, tk.Name, tk.Status.Number, tk.Status.Phase)
			}
		}
		return nil
	})

	// The rebuilt index keeps following writes made by another process.
	other := s.open()
	s.newTask(other, "late", n+1, "")
	eventually(t, "index sees a later write", func() error {
		ts, err := restarted.ListTasks(s.ctx, storage.Query{Namespace: s.ns, Phases: []v1alpha1.Phase{v1alpha1.PhaseTodo}})
		if err != nil {
			return err
		}
		if len(ts) != n {
			return fmt.Errorf("%d Todo tasks, want %d", len(ts), n)
		}
		return nil
	})
}

func testIndexQuery(t *testing.T, s *suite) {
	d := s.open()
	s.newTask(d, "low", 1, v1alpha1.PriorityLow)
	s.newTask(d, "urgent", 2, v1alpha1.PriorityUrgent)
	s.newTask(d, "medium-a", 3, v1alpha1.PriorityMedium)
	s.newTask(d, "high", 4, v1alpha1.PriorityHigh)
	s.newTask(d, "medium-b", 5, "")
	// Subtasks of medium-a, and one of a task that does not exist.
	s.newSubtask(d, "child-low", 6, v1alpha1.PriorityLow, "medium-a")
	s.newSubtask(d, "child-c", 9, "", "medium-a")
	s.newSubtask(d, "child-b", 7, "", "medium-a")
	s.newSubtask(d, "child-urgent", 8, v1alpha1.PriorityUrgent, "medium-a")
	s.newSubtask(d, "stray", 10, "", "gone")
	for _, name := range []string{"high", "child-b"} {
		if err := transition(s.ctx, d, s.key(name), task.Cancel{}); err != nil {
			t.Fatalf("cancel %s: %v", name, err)
		}
	}

	names := func(q storage.Query) ([]string, error) {
		ts, err := d.ListTasks(s.ctx, q)
		out := make([]string, 0, len(ts))
		for _, tk := range ts {
			out = append(out, tk.Name)
		}
		return out, err
	}
	for _, tc := range []struct {
		q    storage.Query
		want []string
	}{
		{storage.Query{Namespace: s.ns}, []string{"urgent", "child-urgent", "high", "medium-a", "medium-b", "child-b", "child-c", "stray", "low", "child-low"}},
		{storage.Query{Namespace: s.ns, Phases: []v1alpha1.Phase{v1alpha1.PhaseCancelled}}, []string{"high", "child-b"}},
		{storage.Query{Namespace: s.ns, Offset: 1, Limit: 2}, []string{"child-urgent", "high"}},
		{storage.Query{Namespace: s.ns, Offset: 99}, []string{}},
		{storage.Query{Namespace: "elsewhere"}, []string{}},
		// Subtasks: only a parent's own, in board order, with the other
		// filters and paging applied to them.
		{storage.Query{Namespace: s.ns, Parent: "medium-a"}, []string{"child-urgent", "child-b", "child-c", "child-low"}},
		{storage.Query{Namespace: s.ns, Parent: "medium-a", Phases: []v1alpha1.Phase{v1alpha1.PhaseTodo}}, []string{"child-urgent", "child-c", "child-low"}},
		{storage.Query{Namespace: s.ns, Parent: "medium-a", Offset: 1, Limit: 2}, []string{"child-b", "child-c"}},
		{storage.Query{Namespace: s.ns, Parent: "medium-a", Offset: 4}, []string{}},
		{storage.Query{Namespace: s.ns, Parent: "low"}, []string{}},
		{storage.Query{Namespace: s.ns, Parent: "unknown"}, []string{}},
		{storage.Query{Namespace: "elsewhere", Parent: "medium-a"}, []string{}},
	} {
		eventually(t, fmt.Sprintf("query %+v", tc.q), func() error {
			got, err := names(tc.q)
			if err != nil {
				return err
			}
			if !slices.Equal(got, tc.want) {
				return fmt.Errorf("got %v, want %v", got, tc.want)
			}
			return nil
		})
	}
}

func testLogAppend(t *testing.T, s *suite) {
	a, b := s.open(), s.open()
	stream := s.ns + "/t/thread"

	wins, errs := race(parties, []storage.Driver{a, b}, func(i int, d storage.Driver) error {
		return d.Append(s.ctx, stream, storage.SeqKey(1), []byte(fmt.Sprintf("writer %d", i)))
	})
	if wins != 1 {
		t.Fatalf("%d appends of seq 1 succeeded, want exactly 1", wins)
	}
	allErrorsAre(t, errs, storage.ErrAlreadyExists)

	for seq := int64(2); seq <= 11; seq++ {
		if err := a.Append(s.ctx, stream, storage.SeqKey(seq), []byte(fmt.Sprint(seq))); err != nil {
			t.Fatalf("append %d: %v", seq, err)
		}
	}
	if err := b.Append(s.ctx, s.ns+"/other/thread", storage.SeqKey(1), []byte("x")); err != nil {
		t.Fatalf("append to another stream: %v", err)
	}

	all, err := b.Read(s.ctx, stream, "", 0)
	if err != nil || len(all) != 11 {
		t.Fatalf("read all: %d entries, err %v", len(all), err)
	}
	for i, e := range all {
		if e.Key != storage.SeqKey(int64(i+1)) {
			t.Fatalf("entry %d has key %s; seq 10 must sort after seq 9", i, e.Key)
		}
	}
	page, err := b.Read(s.ctx, stream, storage.SeqKey(9), 1)
	if err != nil || len(page) != 1 || string(page[0].Data) != "10" {
		t.Fatalf("read after 9, limit 1: %+v, err %v", page, err)
	}
	if empty, err := b.Read(s.ctx, s.ns+"/none", "", 0); err != nil || len(empty) != 0 {
		t.Fatalf("read a missing stream: %+v, err %v", empty, err)
	}
}

// A status write never persists the spec it carries, so the state machine can
// never overwrite a human's concurrent spec edit.
func testStatusWritesLeaveSpecAlone(t *testing.T, s *suite) {
	d := s.open()
	s.newTask(d, "t", 1, "")
	tk := s.get(d, "t")
	tk.Spec.Title = "rewritten by a status write"
	tk.Status.Thread = &v1alpha1.ThreadStatus{Revision: 1}
	if err := d.UpdateTaskStatus(s.ctx, tk); err != nil {
		t.Fatalf("task status write: %v", err)
	}
	if got := s.get(d, "t"); got.Spec.Title != "t" || got.Status.Thread == nil {
		t.Errorf("task: spec %q, thread %+v; want the spec untouched and the status written", got.Spec.Title, got.Status.Thread)
	}

	team := &v1alpha1.Team{ObjectMeta: metav1.ObjectMeta{Name: v1alpha1.TeamName, Namespace: s.ns}, Spec: v1alpha1.TeamSpec{DisplayName: "Backend", MaxConcurrentRuns: 1}}
	if err := d.CreateTeam(s.ctx, team); err != nil {
		t.Fatalf("create team: %v", err)
	}
	team.Spec.DisplayName = "rewritten by a status write"
	team.Spec.MaxConcurrentRuns = 50
	team.Status.NextNumber = 2
	if err := d.UpdateTeamStatus(s.ctx, team); err != nil {
		t.Fatalf("team status write: %v", err)
	}
	if got := s.team(d); got.Spec.DisplayName != "Backend" || got.Spec.MaxConcurrentRuns != 1 || got.Status.NextNumber != 2 {
		t.Errorf("team: spec %+v, status %+v; want the spec untouched and the status written", got.Spec, got.Status)
	}
}

func testCloseIsIdempotent(t *testing.T, s *suite) {
	d := s.env.Open()
	for i := 0; i < 2; i++ {
		if err := d.Close(); err != nil {
			t.Fatalf("close %d: %v", i+1, err)
		}
	}
}

func testIndexRejectsInvalidQueries(t *testing.T, s *suite) {
	d := s.open()
	s.newTask(d, "t", 1, "")
	for _, q := range []storage.Query{
		{},
		{Namespace: s.ns, Offset: -1},
		{Namespace: s.ns, Limit: -1},
	} {
		if _, err := d.ListTasks(s.ctx, q); !errors.Is(err, storage.ErrInvalidQuery) {
			t.Errorf("query %+v: got %v, want ErrInvalidQuery", q, err)
		}
	}
}

// Streams nest by name, but reading one never returns another's entries, and
// malformed names are rejected the same way by every driver.
func testLogNestedStreams(t *testing.T, s *suite) {
	d := s.open()
	parent, child := s.ns+"/t/thread", s.ns+"/t/thread/files"
	for _, w := range []struct{ stream, key string }{
		{parent, storage.SeqKey(1)}, {parent, storage.SeqKey(2)},
		{child, "a"}, {child, "b"}, {child, "c"},
	} {
		if err := d.Append(s.ctx, w.stream, w.key, []byte(w.stream)); err != nil {
			t.Fatalf("append %s/%s: %v", w.stream, w.key, err)
		}
	}
	for stream, want := range map[string][]string{
		parent: {storage.SeqKey(1), storage.SeqKey(2)},
		child:  {"a", "b", "c"},
	} {
		got, err := d.Read(s.ctx, stream, "", 0)
		if err != nil {
			t.Fatalf("read %s: %v", stream, err)
		}
		keys := make([]string, 0, len(got))
		for _, e := range got {
			keys = append(keys, e.Key)
		}
		if !slices.Equal(keys, want) {
			t.Errorf("read %s: keys %v, want %v", stream, keys, want)
		}
	}
	if got, err := d.Read(s.ctx, parent, storage.SeqKey(1), 1); err != nil || len(got) != 1 || got[0].Key != storage.SeqKey(2) {
		t.Errorf("paged read of %s: %+v, %v", parent, got, err)
	}

	for _, bad := range []struct{ stream, key string }{
		{"", "k"}, {"/" + parent, "k"}, {parent + "/", "k"}, {s.ns + "//t", "k"}, {parent, ""}, {parent, "a/b"},
	} {
		if err := d.Append(s.ctx, bad.stream, bad.key, nil); !errors.Is(err, storage.ErrInvalidName) {
			t.Errorf("append stream %q key %q: got %v, want ErrInvalidName", bad.stream, bad.key, err)
		}
	}
	if _, err := d.Read(s.ctx, s.ns+"//t", "", 0); !errors.Is(err, storage.ErrInvalidName) {
		t.Errorf("read a malformed stream: got %v, want ErrInvalidName", err)
	}
}

func testIndexListsTeams(t *testing.T, s *suite) {
	a, b := s.open(), s.open()
	team := &v1alpha1.Team{ObjectMeta: metav1.ObjectMeta{Name: v1alpha1.TeamName, Namespace: s.ns}, Spec: v1alpha1.TeamSpec{MaxConcurrentRuns: 1}}
	if err := a.CreateTeam(s.ctx, team); err != nil {
		t.Fatalf("create team: %v", err)
	}
	eventually(t, "another process lists the team", func() error {
		teams, err := b.ListTeams(s.ctx)
		if err != nil {
			return err
		}
		if !slices.ContainsFunc(teams, func(tm v1alpha1.Team) bool { return tm.Namespace == s.ns }) {
			return fmt.Errorf("team in %s not listed: %d teams", s.ns, len(teams))
		}
		if !slices.IsSortedFunc(teams, func(x, y v1alpha1.Team) int { return strings.Compare(x.Namespace, y.Namespace) }) {
			return errors.New("teams are not ordered by namespace")
		}
		return nil
	})
}

// ErrAlreadyExists must mean the entry exists and Read returns it. Writing an
// entry whose key is the path of a nested stream breaks the layout rule, so
// drivers may refuse it; this case pins how. A flat store (the memory driver,
// the S3 fake) accepts the entry and returns it. A store backed by a
// filesystem (SeaweedFS) refuses it, and must say so with an error other
// than ErrAlreadyExists, or an idempotent caller would drop the entry
// silently believing it was written.
func testLogAlreadyExistsMeansReadable(t *testing.T, s *suite) {
	d := s.open()
	parent := s.ns + "/t/runs"
	if err := d.Append(s.ctx, parent+"/R1/events", "0001", []byte("event")); err != nil {
		t.Fatalf("append to the nested stream: %v", err)
	}
	err := d.Append(s.ctx, parent, "R1", []byte("record"))
	if errors.Is(err, storage.ErrAlreadyExists) {
		t.Fatalf("append %s/R1: ErrAlreadyExists, but no such entry was ever written: %v", parent, err)
	}
	if err != nil {
		// Refused loudly: allowed, provided nothing changed.
		if got, rerr := d.Read(s.ctx, parent, "", 0); rerr != nil || len(got) != 0 {
			t.Errorf("after a refused append, %s holds %+v (%v); want no phantom entry", parent, got, rerr)
		}
		if got, rerr := d.Read(s.ctx, parent+"/R1/events", "", 0); rerr != nil || len(got) != 1 || got[0].Key != "0001" {
			t.Errorf("after a refused append, the nested stream holds %+v (%v); want its one entry", got, rerr)
		}
		return
	}
	got, rerr := d.Read(s.ctx, parent, "", 0)
	if rerr != nil || len(got) != 1 || got[0].Key != "R1" || string(got[0].Data) != "record" {
		t.Fatalf("append %s/R1 succeeded but Read returns %+v, %v", parent, got, rerr)
	}
}

// Keys lists what Read would return, without the data: keys and sizes, in
// order, paged by after and limit, and never a nested stream's entries.
func testLogKeys(t *testing.T, s *suite) {
	d := s.open()
	stream := s.ns + "/t/files/a"
	for seq, data := range []string{"x", "yy", "zzz"} {
		if err := d.Append(s.ctx, stream, storage.SeqKey(int64(seq+1)), []byte(data)); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.Append(s.ctx, stream+"/nested", "k", []byte("n")); err != nil {
		t.Fatal(err)
	}
	all, err := d.Keys(s.ctx, stream, "", 0)
	want := []storage.KeyInfo{{Key: storage.SeqKey(1), Size: 1}, {Key: storage.SeqKey(2), Size: 2}, {Key: storage.SeqKey(3), Size: 3}}
	if err != nil || !slices.Equal(all, want) {
		t.Fatalf("keys: %+v, %v; want %+v", all, err, want)
	}
	if page, err := d.Keys(s.ctx, stream, storage.SeqKey(1), 1); err != nil || !slices.Equal(page, want[1:2]) {
		t.Fatalf("keys after 1, limit 1: %+v, %v", page, err)
	}
	if none, err := d.Keys(s.ctx, s.ns+"/t/files/missing", "", 0); err != nil || len(none) != 0 {
		t.Fatalf("keys of a missing stream: %+v, %v", none, err)
	}
	if _, err := d.Keys(s.ctx, s.ns+"//t", "", 0); !errors.Is(err, storage.ErrInvalidName) {
		t.Fatalf("keys of a malformed stream: %v", err)
	}
}
