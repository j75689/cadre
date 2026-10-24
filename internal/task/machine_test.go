package task

import (
	"errors"
	"slices"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
)

const (
	runA = "01JB00000000000000000000RA"
	runB = "01JB00000000000000000000RB"
)

var now = time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)

func item(id string) v1alpha1.OutboxItem {
	return v1alpha1.OutboxItem{ID: id, Kind: v1alpha1.OutboxCreateSubtask, Spec: &v1alpha1.TaskSpec{Title: "child " + id}}
}

// taskIn returns an assigned task in phase p with one pending subtask. A
// Running task holds runA in the given mode at step 3, with a lease that
// expired a minute ago.
func taskIn(p v1alpha1.Phase, mode v1alpha1.RunMode) *v1alpha1.Task {
	t := &v1alpha1.Task{Spec: v1alpha1.TaskSpec{
		Title:    "t",
		Assignee: &v1alpha1.Assignee{Kind: v1alpha1.AssigneeAgent, Name: "coder"},
	}}
	t.Status.Phase = p
	if p != unborn {
		t.Status.Number = 1
		t.Status.Outbox = []v1alpha1.OutboxItem{item("01JB0000000000000000000000")}
	}
	switch p {
	case v1alpha1.PhaseRunning:
		t.Status.Run = &v1alpha1.Run{ID: runA, Mode: mode, Runner: "w1", Step: 3, LeaseUntil: metav1.NewTime(now.Add(-time.Minute))}
	case v1alpha1.PhaseWaiting:
		t.Status.Waiting = &v1alpha1.Waiting{Reason: v1alpha1.WaitingQuestion, Since: metav1.NewTime(now)}
	}
	return t
}

var (
	step    = v1alpha1.RunModeStep
	runtime = v1alpha1.RunModeRuntime
)

// validEvents holds one event of each kind, with the run mode it is valid in,
// that succeeds from every phase the table allows it from.
var validEvents = []struct {
	ev   Event
	mode v1alpha1.RunMode
}{
	{Create{Number: 1}, step},
	{Claim{RunID: runB, Mode: runtime, Runner: "r", Lease: time.Minute}, step},
	{Heartbeat{RunID: runA, Lease: time.Minute}, step},
	{Advance{RunID: runA, Step: 3, Runner: "w2", Lease: time.Minute}, step},
	{Record{RunID: runA, Outbox: []v1alpha1.OutboxItem{item("01JB0000000000000000000001")}}, runtime},
	{Finish{RunID: runA, Step: 3, Outcome: OutcomeDone}, step},
	{Reply{}, step},
	{Reclaim{}, step},
	{Cancel{}, step},
	{AckOutbox{}, step},
	{ThreadAdvanced{Seq: 1}, step},
	{WaitingAt{ItemID: "01JB0000000000000000000001", Seq: 1}, step},
	{ReserveItem{ID: "01JB0000000000000000000001", To: 1}, step},
}

func TestTableIsExhaustive(t *testing.T) {
	for _, v := range validEvents {
		if _, ok := From[KindOf(v.ev)]; !ok {
			t.Errorf("%s has no row in From", KindOf(v.ev))
		}
	}
	if len(From) != len(validEvents) {
		t.Errorf("From has %d rows, the test covers %d", len(From), len(validEvents))
	}
}

func TestEveryPhaseAndEvent(t *testing.T) {
	for _, p := range append([]v1alpha1.Phase{unborn}, allPhases...) {
		for _, v := range validEvents {
			before := taskIn(p, v.mode)
			snapshot := before.DeepCopy()
			_, err := Apply(before, v.ev, now)
			allowed := slices.Contains(From[KindOf(v.ev)], p)
			// A run's event against a task without its run is a fencing
			// failure, whatever the phase.
			want := ErrInvalidTransition
			if _, ok := v.ev.(fenced); ok {
				want = ErrRunMismatch
			}
			switch {
			case allowed && err != nil:
				t.Errorf("%s from %q: unexpected error %v", KindOf(v.ev), p, err)
			case !allowed && !errors.Is(err, want):
				t.Errorf("%s from %q: got %v, want %v", KindOf(v.ev), p, err, want)
			}
			if !equality.Semantic.DeepEqual(before, snapshot) {
				t.Errorf("%s from %q modified its input", KindOf(v.ev), p)
			}
		}
	}
}

func apply(t *testing.T, tk *v1alpha1.Task, ev Event) Result {
	t.Helper()
	res, err := Apply(tk, ev, now)
	if err != nil {
		t.Fatalf("%s: %v", KindOf(ev), err)
	}
	return res
}

func wantErr(t *testing.T, tk *v1alpha1.Task, ev Event, want error) {
	t.Helper()
	if _, err := Apply(tk, ev, now); !errors.Is(err, want) {
		t.Errorf("%s %+v: got %v, want %v", KindOf(ev), ev, err, want)
	}
}

// runEnded returns the RunEnded item for runA, or nil.
func runEnded(s v1alpha1.TaskStatus) *v1alpha1.RunRecord {
	for _, it := range s.Outbox {
		if it.Kind == v1alpha1.OutboxRunEnded && it.ID == runA {
			return it.Run
		}
	}
	return nil
}

func TestCreateAndClaim(t *testing.T) {
	tk := taskIn(unborn, step)
	wantErr(t, tk, Create{}, ErrInvalidEvent)
	tk.Status = apply(t, tk, Create{Number: 123}).Status
	if tk.Status.Phase != v1alpha1.PhaseTodo || tk.Status.Number != 123 {
		t.Fatalf("after create: %+v", tk.Status)
	}

	res := apply(t, tk, Claim{RunID: runA, Mode: runtime, Runner: "runner-7f3a", Lease: 1500 * time.Millisecond})
	s := res.Status
	if s.Phase != v1alpha1.PhaseRunning || s.Run.ID != runA || s.Run.Runner != "runner-7f3a" {
		t.Fatalf("after claim: %+v", s)
	}
	if want := now.Add(2 * time.Second); !s.Run.LeaseUntil.Time.Equal(want) {
		t.Fatalf("leaseUntil = %v, want %v rounded up to the second", s.Run.LeaseUntil, want)
	}
	if s.Runs.Count != 1 || s.Runs.LastID != runA || res.ReleasesSlot {
		t.Fatalf("runs = %+v, releases = %v", s.Runs, res.ReleasesSlot)
	}

	unassigned := taskIn(v1alpha1.PhaseTodo, step)
	unassigned.Spec.Assignee = nil
	wantErr(t, unassigned, Claim{RunID: runA, Mode: runtime, Lease: time.Minute}, ErrNotClaimable)
	human := taskIn(v1alpha1.PhaseTodo, step)
	human.Spec.Assignee.Kind = v1alpha1.AssigneeUser
	wantErr(t, human, Claim{RunID: runA, Mode: runtime, Lease: time.Minute}, ErrNotClaimable)
	// Decided for one agent, but the task was reassigned meanwhile.
	wantErr(t, taskIn(v1alpha1.PhaseTodo, step), Claim{RunID: runA, Mode: runtime, Lease: time.Minute, Agent: "reviewer"}, ErrNotClaimable)
	apply(t, taskIn(v1alpha1.PhaseTodo, step), Claim{RunID: runA, Mode: runtime, Lease: time.Minute, Agent: "coder"})
	wantErr(t, taskIn(v1alpha1.PhaseTodo, step), Claim{RunID: runA, Mode: runtime}, ErrInvalidEvent)
	wantErr(t, taskIn(v1alpha1.PhaseTodo, step), Claim{RunID: runA, Mode: "batch", Lease: time.Minute}, ErrInvalidEvent)
}

func TestFencing(t *testing.T) {
	stale := []Event{
		Heartbeat{RunID: runB, Lease: time.Minute},
		Advance{RunID: runB, Step: 3, Lease: time.Minute},
		Record{RunID: runB},
		Finish{RunID: runB, Step: 3, Outcome: OutcomeDone},
	}
	for _, ev := range stale {
		wantErr(t, taskIn(v1alpha1.PhaseRunning, step), ev, ErrRunMismatch)
	}

	// A runner whose task was cancelled or reclaimed must get a 409 on its
	// next call, not a phase error, so it knows to stop.
	running := taskIn(v1alpha1.PhaseRunning, runtime)
	cancelled := apply(t, running, Cancel{}).Status
	reclaimed := apply(t, running, Reclaim{}).Status
	for _, s := range []v1alpha1.TaskStatus{cancelled, reclaimed} {
		gone := &v1alpha1.Task{Spec: running.Spec, Status: s}
		for _, ev := range []Event{
			Heartbeat{RunID: runA, Lease: time.Minute},
			Record{RunID: runA},
			Finish{RunID: runA, Outcome: OutcomeDone},
		} {
			wantErr(t, gone, ev, ErrRunMismatch)
		}
	}
}

func TestHeartbeatExtendsLease(t *testing.T) {
	s := apply(t, taskIn(v1alpha1.PhaseRunning, runtime), Heartbeat{RunID: runA, Lease: time.Minute}).Status
	if !s.Run.LeaseUntil.Time.Equal(now.Add(time.Minute)) {
		t.Fatalf("leaseUntil = %v", s.Run.LeaseUntil)
	}
	wantErr(t, taskIn(v1alpha1.PhaseRunning, runtime), Heartbeat{RunID: runA}, ErrInvalidEvent)
}

func TestAdvance(t *testing.T) {
	tk := taskIn(v1alpha1.PhaseRunning, step)
	commit := Advance{RunID: runA, Step: 3, Runner: "w2", Lease: time.Minute, Outbox: []v1alpha1.OutboxItem{item("01JB0000000000000000000001")}}
	tk.Status = apply(t, tk, commit).Status
	r := tk.Status.Run
	if r.Step != 4 || r.Runner != "w2" || !r.LeaseUntil.Time.Equal(now.Add(time.Minute)) || len(tk.Status.Outbox) != 2 {
		t.Fatalf("after advance: run %+v, outbox %d", r, len(tk.Status.Outbox))
	}
	wantErr(t, tk, commit, ErrStepMismatch)
	wantErr(t, taskIn(v1alpha1.PhaseRunning, step), Advance{RunID: runA, Step: 3}, ErrInvalidEvent)
	wantErr(t, taskIn(v1alpha1.PhaseRunning, runtime), Advance{RunID: runA, Step: 3, Lease: time.Minute}, ErrInvalidTransition)
}

// W1 stalls at step 3; W2 redoes step 3 and advances to 4. W1's late finish
// or record must not land.
func TestSupersededStepIsDropped(t *testing.T) {
	tk := taskIn(v1alpha1.PhaseRunning, step)
	tk.Status = apply(t, tk, Advance{RunID: runA, Step: 3, Runner: "w2", Lease: time.Minute}).Status

	wantErr(t, tk, Finish{RunID: runA, Step: 3, Outcome: OutcomeDone}, ErrStepMismatch)
	wantErr(t, tk, Record{RunID: runA, Outbox: []v1alpha1.OutboxItem{item("01JB0000000000000000000002")}}, ErrInvalidTransition)
	if s := apply(t, tk, Finish{RunID: runA, Step: 4, Outcome: OutcomeDone}).Status; s.Phase != v1alpha1.PhaseDone {
		t.Fatalf("finish at the current step: %+v", s)
	}
}

func TestRecordAndOutbox(t *testing.T) {
	tk := taskIn(v1alpha1.PhaseRunning, runtime)
	a, b := item("01JB0000000000000000000001"), item("01JB0000000000000000000002")
	res := apply(t, tk, Record{RunID: runA, Outbox: []v1alpha1.OutboxItem{a, b}})
	if res.Status.Phase != v1alpha1.PhaseRunning || res.Status.Run.Step != 3 || len(res.Status.Outbox) != 3 {
		t.Fatalf("after record: %+v", res.Status)
	}
	tk.Status = res.Status
	wantErr(t, tk, Record{RunID: runA, Outbox: []v1alpha1.OutboxItem{a}}, ErrInvalidEvent)
	noSpec := v1alpha1.OutboxItem{ID: "01JB0000000000000000000003", Kind: v1alpha1.OutboxCreateSubtask}
	wantErr(t, tk, Record{RunID: runA, Outbox: []v1alpha1.OutboxItem{noSpec}}, ErrInvalidEvent)
	forged := v1alpha1.OutboxItem{ID: "01JB0000000000000000000004", Kind: v1alpha1.OutboxRunEnded, Run: &v1alpha1.RunRecord{}}
	wantErr(t, tk, Record{RunID: runA, Outbox: []v1alpha1.OutboxItem{forged}}, ErrInvalidEvent)
	for name, spec := range map[string]v1alpha1.TaskSpec{
		"blank title":       {Title: "  "},
		"unknown priority":  {Title: "x", Priority: "whenever"},
		"unknown assignee":  {Title: "x", Assignee: &v1alpha1.Assignee{Kind: "Robot", Name: "r"}},
		"nameless assignee": {Title: "x", Assignee: &v1alpha1.Assignee{Kind: v1alpha1.AssigneeAgent}},
	} {
		bad := v1alpha1.OutboxItem{ID: "01JB0000000000000000000005", Kind: v1alpha1.OutboxCreateSubtask, Spec: &spec}
		if _, err := Apply(tk, Record{RunID: runA, Outbox: []v1alpha1.OutboxItem{bad}}, now); !errors.Is(err, ErrInvalidEvent) {
			t.Errorf("subtask with %s: got %v, want ErrInvalidEvent", name, err)
		}
	}

	tk.Status = apply(t, tk, AckOutbox{IDs: []string{a.ID, "unknown"}}).Status
	if len(tk.Status.Outbox) != 2 || tk.Status.Outbox[1].ID != b.ID {
		t.Fatalf("after ack: %+v", tk.Status.Outbox)
	}
}

func TestFinish(t *testing.T) {
	for outcome, phase := range map[Outcome]v1alpha1.Phase{
		OutcomeDone: v1alpha1.PhaseDone, OutcomeReview: v1alpha1.PhaseReview, OutcomeFailed: v1alpha1.PhaseFailed,
	} {
		res := apply(t, taskIn(v1alpha1.PhaseRunning, runtime), Finish{RunID: runA, Outcome: outcome})
		if res.Status.Phase != phase || res.Status.Run != nil || !res.ReleasesSlot {
			t.Errorf("finish %s: %+v releases=%v", outcome, res.Status, res.ReleasesSlot)
		}
		rec := runEnded(res.Status)
		if rec == nil || rec.Reason != v1alpha1.RunFinished || rec.Outcome != outcome || rec.Runner != "w1" || rec.Step != 3 || !rec.EndedAt.Time.Equal(now) {
			t.Errorf("finish %s: run record %+v", outcome, rec)
		}
	}

	res := apply(t, taskIn(v1alpha1.PhaseRunning, runtime), Finish{
		RunID: runA, Outcome: OutcomeWaiting, Reason: v1alpha1.WaitingQuestion, ThreadSeq: 41,
		Outbox: []v1alpha1.OutboxItem{item("01JB0000000000000000000001")},
	})
	w := res.Status.Waiting
	if res.Status.Phase != v1alpha1.PhaseWaiting || w == nil || w.Reason != v1alpha1.WaitingQuestion || w.ThreadSeq != 41 || !w.Since.Time.Equal(now) {
		t.Fatalf("finish waiting: %+v", res.Status)
	}
	if len(res.Status.Outbox) != 3 {
		t.Fatalf("finish outbox: want the pending item, its own and RunEnded, got %+v", res.Status.Outbox)
	}

	for _, ev := range []Finish{
		{RunID: runA, Outcome: OutcomeWaiting},
		{RunID: runA, Outcome: OutcomeWaiting, Reason: "Lunch"},
		{RunID: runA, Outcome: OutcomeWaiting, Reason: v1alpha1.WaitingQuestion, ThreadSeq: -1},
		{RunID: runA, Outcome: "Paused"},
	} {
		wantErr(t, taskIn(v1alpha1.PhaseRunning, runtime), ev, ErrInvalidEvent)
	}
}

func TestReplyClearsWaiting(t *testing.T) {
	s := apply(t, taskIn(v1alpha1.PhaseWaiting, step), Reply{}).Status
	if s.Phase != v1alpha1.PhaseTodo || s.Waiting != nil {
		t.Fatalf("after reply: %+v", s)
	}
}

func TestReplyToOneWait(t *testing.T) {
	waiting := taskIn(v1alpha1.PhaseWaiting, step)
	waiting.Status.Waiting.ThreadSeq = 4
	if s := apply(t, waiting, Reply{ThreadSeq: 4}).Status; s.Phase != v1alpha1.PhaseTodo {
		t.Fatalf("a reply to the wait's entry: %+v", s)
	}
	wantErr(t, waiting, Reply{ThreadSeq: 3}, ErrInvalidTransition)
}

func TestReclaim(t *testing.T) {
	live := taskIn(v1alpha1.PhaseRunning, runtime)
	live.Status.Run.LeaseUntil = metav1.NewTime(now)
	wantErr(t, live, Reclaim{}, ErrLeaseNotExpired)

	// The lease expired a minute ago, but not by more than the grace.
	wantErr(t, taskIn(v1alpha1.PhaseRunning, runtime), Reclaim{Grace: time.Minute}, ErrLeaseNotExpired)
	apply(t, taskIn(v1alpha1.PhaseRunning, runtime), Reclaim{Grace: time.Minute - time.Second})

	corrupt := taskIn(v1alpha1.PhaseRunning, runtime)
	corrupt.Status.Run = nil
	wantErr(t, corrupt, Reclaim{}, ErrInvalidTransition)

	res := apply(t, taskIn(v1alpha1.PhaseRunning, runtime), Reclaim{})
	if res.Status.Phase != v1alpha1.PhaseTodo || res.Status.Run != nil || !res.ReleasesSlot {
		t.Fatalf("after reclaim: %+v", res.Status)
	}
	if rec := runEnded(res.Status); rec == nil || rec.Reason != v1alpha1.RunInterrupted {
		t.Fatalf("reclaim run record: %+v", rec)
	}
}

func TestCancel(t *testing.T) {
	for _, p := range allPhases {
		res := apply(t, taskIn(p, runtime), Cancel{})
		if res.Status.Phase != v1alpha1.PhaseCancelled || res.Status.Run != nil || res.Status.Waiting != nil {
			t.Errorf("cancel from %s: %+v", p, res.Status)
		}
		running := p == v1alpha1.PhaseRunning
		rec := runEnded(res.Status)
		if res.ReleasesSlot != running || (rec != nil) != running {
			t.Errorf("cancel from %s: releases=%v record=%+v", p, res.ReleasesSlot, rec)
		}
		if running && rec.Reason != v1alpha1.RunCancelled {
			t.Errorf("cancel from Running: reason %s", rec.Reason)
		}
		for _, it := range res.Status.Outbox {
			if it.Kind == v1alpha1.OutboxCreateSubtask {
				t.Errorf("cancel from %s kept pending subtask %s", p, it.ID)
			}
		}
	}

	// Cancelling after a finish keeps the finish's run record.
	finished := apply(t, taskIn(v1alpha1.PhaseRunning, runtime), Finish{RunID: runA, Outcome: OutcomeReview})
	s := apply(t, &v1alpha1.Task{Status: finished.Status}, Cancel{}).Status
	if rec := runEnded(s); rec == nil || rec.Reason != v1alpha1.RunFinished {
		t.Fatalf("cancel dropped the run record: %+v", s.Outbox)
	}
}

func TestThreadAdvanced(t *testing.T) {
	tk := taskIn(v1alpha1.PhaseDone, step)
	tk.Status = apply(t, tk, ThreadAdvanced{Seq: 3}).Status
	if tk.Status.Thread.Revision != 3 || tk.Status.Phase != v1alpha1.PhaseDone {
		t.Fatalf("after advancing to 3: %+v", tk.Status)
	}
	// A late writer of an older entry never moves the revision back.
	tk.Status = apply(t, tk, ThreadAdvanced{Seq: 2}).Status
	if tk.Status.Thread.Revision != 3 {
		t.Fatalf("revision moved back to %d", tk.Status.Thread.Revision)
	}
	wantErr(t, tk, ThreadAdvanced{}, ErrInvalidEvent)
}

func TestTeam(t *testing.T) {
	team := &v1alpha1.Team{Spec: v1alpha1.TeamSpec{MaxConcurrentRuns: 2}}
	n, s := ClaimNumber(team)
	team.Status = s
	if n != 1 || team.Status.NextNumber != 2 {
		t.Fatalf("first number %d, next %d", n, team.Status.NextNumber)
	}
	if n, _ = ClaimNumber(team); n != 2 {
		t.Fatalf("second number %d", n)
	}

	acquire := func(runID string) error {
		s, err := AcquireSlot(team, runID, "task-"+runID, now)
		if err == nil {
			team.Status = s
		}
		return err
	}
	if err := acquire(runA); err != nil {
		t.Fatal(err)
	}
	// Taking a slot the run already holds is not a second slot.
	if err := acquire(runA); err != nil || team.Status.Running != 1 || len(team.Status.Slots) != 1 {
		t.Fatalf("re-acquire: %v, running %d, slots %+v", err, team.Status.Running, team.Status.Slots)
	}
	if err := acquire(runB); err != nil || team.Status.Running != 2 {
		t.Fatalf("second slot: %v, running %d", err, team.Status.Running)
	}
	if err := acquire("01JB00000000000000000000RC"); !errors.Is(err, ErrNoSlot) {
		t.Fatalf("acquire past the limit: got %v", err)
	}
	if sl := team.Status.Slots[0]; sl.Task != "task-"+runA || !sl.AcquiredAt.Time.Equal(now) {
		t.Fatalf("slot %+v", sl)
	}

	// Releases are idempotent: a run gives back only its own slot, once.
	team.Status = ReleaseSlot(team, runA)
	team.Status = ReleaseSlot(team, runA)
	team.Status = ReleaseSlot(team, "01JB00000000000000000000RZ")
	if team.Status.Running != 1 || !HoldsSlot(team, runB) || HoldsSlot(team, runA) {
		t.Fatalf("after releases: %+v", team.Status)
	}

	// The reconciler may add a visibly Running run past the cap.
	if err := acquire(runA); err != nil {
		t.Fatal(err)
	}
	team.Status = AddSlot(team, "01JB00000000000000000000RC", "late", now)
	team.Status = AddSlot(team, "01JB00000000000000000000RC", "late", now)
	if team.Status.Running != 3 {
		t.Fatalf("running %d after AddSlot past the cap, want 3", team.Status.Running)
	}

	unset := &v1alpha1.Team{}
	s, err := AcquireSlot(unset, runA, "t", now)
	if err != nil || s.Running != 1 {
		t.Fatalf("unset maxConcurrentRuns: got %v, running %d; want the default of 1", err, s.Running)
	}
	unset.Status = s
	if _, err := AcquireSlot(unset, runB, "t", now); !errors.Is(err, ErrNoSlot) {
		t.Fatalf("second slot with the default of 1: got %v", err)
	}
}

// Only the runner holding a run may renew or finish it; a server that does
// not authenticate runners (an empty sender) is not checked.
func TestOnlyTheHolderMayActOnTheRun(t *testing.T) {
	running := taskIn(v1alpha1.PhaseRunning, runtime)
	running.Status.Run.Runner, running.Status.Run.RunnerName = "sa:holder", "pod-1"
	wantErr(t, running, Heartbeat{RunID: runA, Runner: "sa:other", Lease: time.Minute}, ErrForbidden)
	wantErr(t, running, Finish{RunID: runA, Runner: "sa:other", Outcome: OutcomeDone}, ErrForbidden)
	wantErr(t, running, Heartbeat{RunID: "01JB0000000000000000000009", Runner: "sa:other", Lease: time.Minute}, ErrRunMismatch)
	apply(t, running, Heartbeat{RunID: runA, Runner: "sa:holder", Lease: time.Minute})
	apply(t, running, Heartbeat{RunID: runA, Lease: time.Minute})

	// A run claimed without an identity (authentication off, or before it
	// existed) is not fenced on its sender, so turning authentication on
	// does not cut it.
	for _, run := range [][2]string{{"", "pod-1"}, {"pod-1", ""}} {
		old := taskIn(v1alpha1.PhaseRunning, runtime)
		old.Status.Run.Runner, old.Status.Run.RunnerName = run[0], run[1]
		apply(t, old, Heartbeat{RunID: runA, Runner: "sa:anyone", Lease: time.Minute})
	}

	todo := taskIn(v1alpha1.PhaseTodo, runtime)
	run := apply(t, todo, Claim{RunID: runA, Mode: runtime, Runner: "sa:holder", RunnerName: "pod-1", Lease: time.Minute}).Status.Run
	if run.Runner != "sa:holder" || run.RunnerName != "pod-1" {
		t.Fatalf("claimed run: %+v", run)
	}
}

// A finish can wait on an entry its outbox writes; WaitingAt then records
// the entry's sequence, once, and only for that wait.
func TestWaitingOnAnEntryNotWrittenYet(t *testing.T) {
	q := v1alpha1.OutboxItem{ID: "01JB0000000000000000000007", Kind: v1alpha1.OutboxAppendThread,
		Thread: &v1alpha1.ThreadAppend{Blob: "b", Kind: "Question", Author: "coder", RunID: runA}}
	fin := Finish{RunID: runA, Outcome: OutcomeWaiting, Reason: v1alpha1.WaitingQuestion, WaitingItem: q.ID, Outbox: []v1alpha1.OutboxItem{q}}
	waiting := apply(t, taskIn(v1alpha1.PhaseRunning, runtime), fin)
	if w := waiting.Status.Waiting; w == nil || w.ItemID != q.ID || w.ThreadSeq != 0 {
		t.Fatalf("waiting %+v", w)
	}
	tk := taskIn(v1alpha1.PhaseWaiting, runtime)
	tk.Status = waiting.Status
	if w := apply(t, tk, WaitingAt{ItemID: "01JB0000000000000000000009", Seq: 5}).Status.Waiting; w.ThreadSeq != 0 {
		t.Fatalf("another item set the wait's entry: %+v", w)
	}
	tk.Status = apply(t, tk, WaitingAt{ItemID: q.ID, Seq: 5}).Status
	if w := tk.Status.Waiting; w.ThreadSeq != 5 {
		t.Fatalf("waiting %+v, want entry 5", w)
	}
	if w := apply(t, tk, WaitingAt{ItemID: q.ID, Seq: 9}).Status.Waiting; w.ThreadSeq != 5 {
		t.Fatalf("a second WaitingAt moved the wait: %+v", w)
	}
	// The waiting item must be one the finish writes.
	bad := fin
	bad.Outbox = nil
	wantErr(t, taskIn(v1alpha1.PhaseRunning, runtime), bad, ErrInvalidEvent)
}

// ReserveItem is a compare-and-set on where an item writes.
func TestReserveItem(t *testing.T) {
	tk := taskIn(v1alpha1.PhaseRunning, runtime)
	id := tk.Status.Outbox[0].ID
	tk.Status = apply(t, tk, ReserveItem{ID: id, From: 0, To: 4}).Status
	if tk.Status.Outbox[0].Seq != 4 {
		t.Fatalf("reserved %d, want 4", tk.Status.Outbox[0].Seq)
	}
	if got := apply(t, tk, ReserveItem{ID: id, From: 0, To: 7}).Status.Outbox[0].Seq; got != 4 {
		t.Fatalf("a stale reserve moved it to %d", got)
	}
	if got := apply(t, tk, ReserveItem{ID: id, From: 4, To: 7}).Status.Outbox[0].Seq; got != 7 {
		t.Fatalf("moving on from 4: %d", got)
	}
	wantErr(t, tk, ReserveItem{ID: id, From: 4, To: 0}, ErrInvalidEvent)
}
