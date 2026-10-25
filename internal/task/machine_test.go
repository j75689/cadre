package task

import (
	"errors"
	"slices"
	"strings"
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
		if mode == step {
			t.Status.Run.Attempt = 1 // held by w1, at its first attempt
		}
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
	{Heartbeat{RunID: runA, Step: 3, Attempt: 1, Lease: time.Minute}, step},
	{Advance{RunID: runA, Step: 3, Attempt: 1}, step},
	{TakeStep{RunID: runA, Step: 3, Runner: "w2", RunnerName: "w2", Lease: time.Minute}, step},
	{ReleaseStep{RunID: runA, Step: 3, Attempt: 1}, step},
	{Record{RunID: runA, Outbox: []v1alpha1.OutboxItem{item("01JB0000000000000000000001")}}, runtime},
	{Finish{RunID: runA, Step: 3, Attempt: 1, Outcome: OutcomeDone}, step},
	{Reply{}, step},
	{Reclaim{}, step},
	{Cancel{}, step},
	{AckOutbox{}, step},
	{ThreadAdvanced{Seq: 1}, step},
	{WaitingAt{ItemID: "01JB0000000000000000000001", Seq: 1}, step},
	{ReserveItem{ID: "01JB0000000000000000000001", To: 1}, step},
	{NotifyDelivered{ID: "01JB0000000000000000000001", To: []string{"hook"}}, step},
	{NotifyClaim{ID: "01JB0000000000000000000003", By: "s1", Until: time.Now().Add(time.Hour)}, step},
}

// setups give a task what an event of a kind acts on, where the task of
// every phase does not have it.
var setups = map[Kind]func(*v1alpha1.Task){
	KindSending: func(t *v1alpha1.Task) {
		t.Status.Outbox = append(t.Status.Outbox, v1alpha1.OutboxItem{ID: "01JB0000000000000000000003", Kind: v1alpha1.OutboxNotify, Notify: &v1alpha1.Notification{Event: v1alpha1.NotifyDone}})
	},
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
			if setup := setups[KindOf(v.ev)]; setup != nil {
				setup(before)
			}
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
	for _, parent := range []string{"Not_A_Name", "#1", "-a"} {
		if err := ValidateSpec(&v1alpha1.TaskSpec{Title: "x", ParentRef: &v1alpha1.TaskReference{Name: parent}}); !errors.Is(err, ErrInvalidEvent) {
			t.Errorf("parentRef %q: %v", parent, err)
		}
	}
	if err := ValidateSpec(&v1alpha1.TaskSpec{Title: "x", ParentRef: &v1alpha1.TaskReference{Name: "release.2026"}}); err != nil {
		t.Errorf("a dotted parent: %v", err)
	}
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
		Advance{RunID: runB, Step: 3},
		TakeStep{RunID: runB, Step: 3, Runner: "w2", Lease: time.Minute},
		ReleaseStep{RunID: runB, Step: 3},
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

// Advance commits the step and frees it: no worker, no MCP token, the
// lease set to the idle deadline, attempts reset, so any worker takes the
// next one.
func TestAdvance(t *testing.T) {
	tk := taskIn(v1alpha1.PhaseRunning, step)
	tk.Status.Run.MCPTokenHash, tk.Status.Run.Attempt = strings.Repeat("a", 64), 2
	commit := Advance{RunID: runA, Step: 3, Attempt: 2, Idle: 10 * time.Minute, Outbox: []v1alpha1.OutboxItem{item("01JB0000000000000000000001")}}
	wantErr(t, tk, Advance{RunID: runA, Step: 3, Attempt: 1}, ErrAttemptMismatch)
	tk.Status = apply(t, tk, commit).Status
	r := tk.Status.Run
	if r.Step != 4 || !r.StepFree || r.Runner != "" || r.LastRunner != "w1" || !r.LeaseUntil.Time.Equal(now.Add(10*time.Minute)) ||
		r.MCPTokenHash != "" || r.Attempt != 0 || len(tk.Status.Outbox) != 2 {
		t.Fatalf("after advance: run %+v, outbox %d", r, len(tk.Status.Outbox))
	}
	wantErr(t, tk, commit, ErrStepMismatch)
	wantErr(t, tk, Advance{RunID: runA, Step: 4}, ErrAttemptMismatch) // free: nobody's to commit
	wantErr(t, tk, Heartbeat{RunID: runA, Step: 4, Lease: time.Minute}, ErrAttemptMismatch)
	wantErr(t, taskIn(v1alpha1.PhaseRunning, runtime), Advance{RunID: runA, Step: 3}, ErrInvalidTransition)
}

func TestTakeAndReleaseSteps(t *testing.T) {
	tk := taskIn(v1alpha1.PhaseRunning, step)
	tk.Status = apply(t, tk, Advance{RunID: runA, Step: 3, Attempt: 1}).Status // step 4, free
	take := TakeStep{RunID: runA, Step: 4, Runner: "sa:w", RunnerName: "w-1", Lease: time.Minute, MCPTokenHash: strings.Repeat("b", 64)}
	wantErr(t, tk, TakeStep{RunID: runA, Step: 4, Runner: "sa:w", Lease: time.Minute}, ErrInvalidEvent) // unnamed
	tk.Status = apply(t, tk, take).Status
	r := tk.Status.Run
	if r.StepFree || r.Runner != "sa:w" || r.RunnerName != "w-1" || r.Attempt != 1 || r.MCPTokenHash != take.MCPTokenHash || !r.LeaseUntil.Time.Equal(now.Add(time.Minute)) {
		t.Fatalf("taken: %+v", r)
	}
	// The holder takes it again, as when the answer was lost: the same
	// attempt, a new token.
	again := take
	again.MCPTokenHash = strings.Repeat("c", 64)
	if r := apply(t, tk, again).Status.Run; r.Attempt != 1 || r.MCPTokenHash != again.MCPTokenHash {
		t.Fatalf("taken again by its holder: %+v", r)
	}
	wantErr(t, tk, TakeStep{RunID: runA, Step: 4, Runner: "sa:w", RunnerName: "w-2", Lease: time.Minute}, ErrStepTaken)
	wantErr(t, tk, TakeStep{RunID: runA, Step: 3, Runner: "sa:w", RunnerName: "w-1", Lease: time.Minute}, ErrStepMismatch)
	wantErr(t, tk, Heartbeat{RunID: runA, Step: 3, Attempt: 1, Lease: time.Minute}, ErrStepMismatch)
	wantErr(t, tk, Heartbeat{RunID: runA, Step: 4, Attempt: 2, Lease: time.Minute}, ErrAttemptMismatch)
	apply(t, tk, Heartbeat{RunID: runA, Step: 4, Attempt: 1, Runner: "sa:w", Lease: time.Minute})

	wantErr(t, tk, ReleaseStep{RunID: runA, Step: 4, Attempt: 1, Runner: "sa:other"}, ErrForbidden)
	for attempt := int32(1); attempt < MaxAttempts; attempt++ {
		tk.Status = apply(t, tk, ReleaseStep{RunID: runA, Step: 4, Attempt: attempt, Runner: "sa:w"}).Status
		if r := tk.Status.Run; !r.StepFree || r.Runner != "" || r.MCPTokenHash != "" || r.Attempt != attempt {
			t.Fatalf("released: %+v", r)
		}
		// The released attempt is over for its worker.
		wantErr(t, tk, Record{RunID: runA, Step: 4, Attempt: attempt, Outbox: []v1alpha1.OutboxItem{item("01JB0000000000000000000009")}}, ErrAttemptMismatch)
		tk.Status = apply(t, tk, take).Status
	}
	tk.Status = apply(t, tk, ReleaseStep{RunID: runA, Step: 4, Attempt: MaxAttempts, Runner: "sa:w"}).Status
	wantErr(t, tk, take, ErrStepExhausted)
	if r := tk.Status.Run; !r.LeaseUntil.Time.Equal(now) {
		t.Fatalf("a step with no attempt left waits until %v, want now", r.LeaseUntil)
	}
}

// Without authentication a run has no Runner; its step is still held,
// fenced on the attempt, and freed only by a release or a reclaim.
func TestStepsWithoutAnIdentity(t *testing.T) {
	tk := &v1alpha1.Task{Spec: v1alpha1.TaskSpec{Title: "t", Assignee: &v1alpha1.Assignee{Kind: v1alpha1.AssigneeAgent, Name: "coder"}}}
	tk.Status = v1alpha1.TaskStatus{Phase: v1alpha1.PhaseTodo, Number: 1}
	tk.Status = apply(t, tk, Claim{RunID: runA, Mode: step, RunnerName: "w-1", Lease: time.Minute}).Status
	if r := tk.Status.Run; r.StepFree || r.Attempt != 1 {
		t.Fatalf("claimed: %+v", r)
	}
	wantErr(t, tk, TakeStep{RunID: runA, Step: 0, RunnerName: "w-2", Lease: time.Minute}, ErrStepTaken)
	wantErr(t, tk, Reclaim{}, ErrLeaseNotExpired)
	tk.Status = apply(t, tk, ReleaseStep{RunID: runA, Step: 0, Attempt: 1}).Status
	if r := apply(t, tk, TakeStep{RunID: runA, Step: 0, RunnerName: "w-2", Lease: time.Minute}).Status.Run; r.Attempt != 2 || r.RunnerName != "w-2" {
		t.Fatalf("taken by the next worker: %+v", r)
	}
}

// An expired step is freed by the reclaim, not sent back to Todo, until
// its attempts run out; then the run fails. So does a free step no worker
// took by its idle deadline.
func TestReclaimInStepMode(t *testing.T) {
	tk := taskIn(v1alpha1.PhaseRunning, step) // held by w1, lease a minute past
	s := apply(t, tk, Reclaim{Idle: time.Hour}).Status
	if s.Phase != v1alpha1.PhaseRunning || s.Run == nil || !s.Run.StepFree || s.Run.Runner != "" || s.Run.Attempt != 1 {
		t.Fatalf("after reclaiming an expired step: %+v", s)
	}
	free := &v1alpha1.Task{Spec: tk.Spec, Status: s}
	wantErr(t, free, Reclaim{}, ErrLeaseNotExpired)
	// The slow worker of the freed attempt is fenced out.
	wantErr(t, free, Advance{RunID: runA, Step: 3, Attempt: 1}, ErrAttemptMismatch)

	tk.Status.Run.Attempt = MaxAttempts
	res := apply(t, tk, Reclaim{})
	if res.Status.Phase != v1alpha1.PhaseFailed || res.Status.Run != nil || !res.ReleasesSlot {
		t.Fatalf("after the last attempt expired: %+v", res.Status)
	}
	if rec := ended(t, res.Status); rec.Runner != "w1" || rec.Attempt != MaxAttempts || rec.Outcome != OutcomeFailed {
		t.Fatalf("record %+v", rec)
	}

	// The last attempt given up: no worker may take the step, so the
	// reclaim fails the run too, and records the last worker.
	exhausted := free.DeepCopy()
	exhausted.Status.Run.Attempt = MaxAttempts
	wantErr(t, exhausted, TakeStep{RunID: runA, Step: 3, Runner: "w2", RunnerName: "w2", Lease: time.Minute}, ErrStepExhausted)
	res = apply(t, exhausted, Reclaim{})
	if res.Status.Phase != v1alpha1.PhaseFailed || !res.ReleasesSlot || ended(t, res.Status).Runner != "w1" {
		t.Fatalf("after the last attempt was given up: %+v", res.Status)
	}

	// No worker came by the idle deadline.
	idle := free.DeepCopy()
	if _, err := Apply(idle, Reclaim{}, now.Add(59*time.Minute)); !errors.Is(err, ErrLeaseNotExpired) {
		t.Fatalf("before the idle deadline: %v", err)
	}
	res, err := Apply(idle, Reclaim{}, now.Add(time.Hour+time.Second))
	if err != nil || res.Status.Phase != v1alpha1.PhaseFailed || ended(t, res.Status).Attempt != 1 {
		t.Fatalf("past the idle deadline: %+v, %v", res.Status, err)
	}
}

// ended is the RunEnded record a transition wrote.
func ended(t *testing.T, s v1alpha1.TaskStatus) *v1alpha1.RunRecord {
	t.Helper()
	for _, it := range s.Outbox {
		if it.Kind == v1alpha1.OutboxRunEnded {
			return it.Run
		}
	}
	t.Fatal("no RunEnded record")
	return nil
}

// W1 stalls at step 3; W2 redoes step 3 and advances to 4. W1's late finish
// or record must not land.
func TestSupersededStepIsDropped(t *testing.T) {
	tk := taskIn(v1alpha1.PhaseRunning, step)
	tk.Status = apply(t, tk, Advance{RunID: runA, Step: 3, Attempt: 1}).Status

	wantErr(t, tk, Finish{RunID: runA, Step: 3, Attempt: 1, Outcome: OutcomeDone}, ErrStepMismatch)
	wantErr(t, tk, Record{RunID: runA, Step: 3, Attempt: 1, Outbox: []v1alpha1.OutboxItem{item("01JB0000000000000000000002")}}, ErrStepMismatch)
	tk.Status = apply(t, tk, TakeStep{RunID: runA, Step: 4, Runner: "w2", RunnerName: "w2", Lease: time.Minute}).Status
	if s := apply(t, tk, Finish{RunID: runA, Runner: "w2", Step: 4, Attempt: 1, Outcome: OutcomeDone}).Status; s.Phase != v1alpha1.PhaseDone {
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

// NotifyDelivered adds to a Notify item's Delivered, once each, and leaves
// other items alone; once every Notifier of the item took it, or is gone,
// it removes the item.
func TestNotifyDelivered(t *testing.T) {
	const id = "01JB0000000000000000000001"
	tk := taskIn(v1alpha1.PhaseWaiting, step)
	tk.Status.Outbox = []v1alpha1.OutboxItem{
		{ID: id, Kind: v1alpha1.OutboxNotify, Notify: &v1alpha1.Notification{Event: v1alpha1.NotifyQuestion, To: []string{"a", "b", "c", "d", "e"}, Delivered: []string{"b"}}},
		item("01JB0000000000000000000002"),
	}
	tk.Status = apply(t, tk, NotifyDelivered{ID: id, To: []string{"c", "a", "b"}}).Status
	if got := tk.Status.Outbox[0].Notify.Delivered; strings.Join(got, ",") != "a,b,c" {
		t.Errorf("delivered %v", got)
	}
	if tk.Status.Outbox[1].Notify != nil {
		t.Errorf("another item changed: %+v", tk.Status.Outbox[1])
	}
	// d gone is not enough while e has not taken it.
	tk.Status = apply(t, tk, NotifyDelivered{ID: id, Gone: []string{"d"}}).Status
	if len(tk.Status.Outbox) != 2 {
		t.Fatalf("removed with e pending: %+v", tk.Status.Outbox)
	}
	tk.Status = apply(t, tk, NotifyDelivered{ID: id, To: []string{"e"}, Gone: []string{"d"}}).Status
	// An item with no Notifiers is not removed by a turn's end.
	tk.Status.Outbox = append(tk.Status.Outbox, v1alpha1.OutboxItem{ID: id, Kind: v1alpha1.OutboxNotify, Notify: &v1alpha1.Notification{Event: v1alpha1.NotifyQuestion}})
	if got := apply(t, tk, NotifyDelivered{ID: id, By: "s1"}).Status.Outbox; len(got) != 2 {
		t.Errorf("a turn's end removed an item with no to: %+v", got)
	}
	tk.Status.Outbox = tk.Status.Outbox[:1]
	if len(tk.Status.Outbox) != 1 || tk.Status.Outbox[0].ID != "01JB0000000000000000000002" {
		t.Errorf("outbox once all took it %+v, want the other item only", tk.Status.Outbox)
	}
}

// One server at a time sends a notification: another's turn holds until it
// runs out or is ended by its record of deliveries.
func TestNotifyClaim(t *testing.T) {
	const id = "01JB0000000000000000000001"
	now := time.Date(2026, 10, 25, 9, 0, 0, 0, time.UTC)
	tk := taskIn(v1alpha1.PhaseWaiting, step)
	tk.Status.Outbox = []v1alpha1.OutboxItem{{ID: id, Kind: v1alpha1.OutboxNotify, Notify: &v1alpha1.Notification{Event: v1alpha1.NotifyQuestion, To: []string{"a", "b"}}}}
	claim := func(by string, at time.Time) error {
		res, err := Apply(tk, NotifyClaim{ID: id, By: by, Until: at.Add(30 * time.Second)}, at)
		if err == nil {
			tk.Status = res.Status
		}
		return err
	}
	if err := claim("s1", now); err != nil {
		t.Fatal(err)
	}
	if err := claim("s2", now.Add(time.Second)); !errors.Is(err, ErrNotifySending) {
		t.Errorf("s2 during s1's turn: %v", err)
	}
	if err := claim("s1", now.Add(time.Second)); err != nil {
		t.Errorf("s1 again: %v", err)
	}
	if err := claim("s2", now.Add(time.Minute)); err != nil {
		t.Errorf("s2 once s1's turn ran out: %v", err)
	}
	res, err := Apply(tk, NotifyDelivered{ID: id, To: []string{"a"}, By: "s2"}, now.Add(time.Minute))
	if err != nil || res.Status.Outbox[0].Notify.SendingBy != "" {
		t.Errorf("a record of deliveries keeps the turn: %+v %v", res.Status.Outbox[0].Notify, err)
	}
}

func TestNotifyClaimOnAGoneItem(t *testing.T) {
	tk := taskIn(v1alpha1.PhaseDone, step)
	if _, err := Apply(tk, NotifyClaim{ID: "01JB0000000000000000000009", By: "s1", Until: time.Now().Add(time.Minute)}, time.Now()); !errors.Is(err, ErrNotifyGone) {
		t.Errorf("a claim on an item another server finished: %v", err)
	}
}
