package task

import (
	"errors"
	"slices"
	"testing"
	"time"

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

// taskIn returns an assigned task in phase p. A Running task holds runA in step
// mode at step 3 with a lease that expired a minute ago.
func taskIn(p v1alpha1.Phase) *v1alpha1.Task {
	t := &v1alpha1.Task{Spec: v1alpha1.TaskSpec{
		Title:    "t",
		Assignee: &v1alpha1.Assignee{Kind: v1alpha1.AssigneeAgent, Name: "coder"},
	}}
	t.Status.Phase = p
	if p != unborn {
		t.Status.Number = 1
	}
	switch p {
	case v1alpha1.PhaseRunning:
		t.Status.Run = &v1alpha1.Run{ID: runA, Mode: v1alpha1.RunModeStep, Step: 3, LeaseUntil: metav1.NewTime(now.Add(-time.Minute))}
	case v1alpha1.PhaseWaiting:
		t.Status.Waiting = &v1alpha1.Waiting{Reason: v1alpha1.WaitingQuestion, Since: metav1.NewTime(now)}
	}
	return t
}

// validEvents holds one event of each kind that succeeds from every phase the
// table allows it from.
var validEvents = []Event{
	Create{Number: 1},
	Claim{RunID: runB, Mode: v1alpha1.RunModeRuntime, Runner: "r", Lease: time.Minute},
	Heartbeat{RunID: runA, Lease: time.Minute},
	Advance{RunID: runA, Step: 3},
	Record{RunID: runA, Outbox: []v1alpha1.OutboxItem{item("01JB0000000000000000000001")}},
	Finish{RunID: runA, Outcome: OutcomeDone},
	Reply{},
	Reclaim{},
	Cancel{},
	AckOutbox{},
}

func TestTableIsExhaustive(t *testing.T) {
	for _, ev := range validEvents {
		if _, ok := From[ev.kind()]; !ok {
			t.Errorf("%s has no row in From", ev.kind())
		}
	}
	if len(From) != len(validEvents) {
		t.Errorf("From has %d rows, the test covers %d", len(From), len(validEvents))
	}
}

func TestEveryPhaseAndEvent(t *testing.T) {
	for _, p := range append([]v1alpha1.Phase{unborn}, allPhases...) {
		for _, ev := range validEvents {
			before := taskIn(p)
			snapshot := before.DeepCopy()
			_, err := Apply(before, ev, now)
			allowed := slices.Contains(From[ev.kind()], p)
			switch {
			case allowed && err != nil:
				t.Errorf("%s from %q: unexpected error %v", ev.kind(), p, err)
			case !allowed && !errors.Is(err, ErrInvalidTransition):
				t.Errorf("%s from %q: got %v, want ErrInvalidTransition", ev.kind(), p, err)
			}
			if before.Status.Phase != snapshot.Status.Phase || (before.Status.Run == nil) != (snapshot.Status.Run == nil) {
				t.Errorf("%s from %q modified its input", ev.kind(), p)
			}
		}
	}
}

func apply(t *testing.T, tk *v1alpha1.Task, ev Event) Result {
	t.Helper()
	res, err := Apply(tk, ev, now)
	if err != nil {
		t.Fatalf("%s: %v", ev.kind(), err)
	}
	return res
}

func TestCreateAndClaim(t *testing.T) {
	tk := taskIn(unborn)
	if _, err := Apply(tk, Create{}, now); !errors.Is(err, ErrInvalidEvent) {
		t.Fatalf("create without a number: got %v", err)
	}
	tk.Status = apply(t, tk, Create{Number: 123}).Status
	if tk.Status.Phase != v1alpha1.PhaseTodo || tk.Status.Number != 123 {
		t.Fatalf("after create: %+v", tk.Status)
	}

	res := apply(t, tk, Claim{RunID: runA, Mode: v1alpha1.RunModeRuntime, Runner: "runner-7f3a", Lease: 1500 * time.Millisecond})
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

	unassigned := taskIn(v1alpha1.PhaseTodo)
	unassigned.Spec.Assignee = nil
	if _, err := Apply(unassigned, Claim{RunID: runA, Lease: time.Minute}, now); !errors.Is(err, ErrUnassigned) {
		t.Fatalf("claim unassigned: got %v", err)
	}
	if _, err := Apply(taskIn(v1alpha1.PhaseTodo), Claim{RunID: runA}, now); !errors.Is(err, ErrInvalidEvent) {
		t.Fatalf("claim without a lease: got %v", err)
	}
}

func TestFencing(t *testing.T) {
	for _, ev := range []Event{
		Heartbeat{RunID: runB, Lease: time.Minute},
		Advance{RunID: runB, Step: 3},
		Record{RunID: runB},
		Finish{RunID: runB, Outcome: OutcomeDone},
	} {
		if _, err := Apply(taskIn(v1alpha1.PhaseRunning), ev, now); !errors.Is(err, ErrRunMismatch) {
			t.Errorf("%s with a stale run id: got %v, want ErrRunMismatch", ev.kind(), err)
		}
	}
}

func TestHeartbeatExtendsLease(t *testing.T) {
	s := apply(t, taskIn(v1alpha1.PhaseRunning), Heartbeat{RunID: runA, Lease: time.Minute}).Status
	if !s.Run.LeaseUntil.Time.Equal(now.Add(time.Minute)) {
		t.Fatalf("leaseUntil = %v", s.Run.LeaseUntil)
	}
}

func TestAdvanceExactlyOnce(t *testing.T) {
	tk := taskIn(v1alpha1.PhaseRunning)
	step := Advance{RunID: runA, Step: 3, Outbox: []v1alpha1.OutboxItem{item("01JB0000000000000000000001")}}
	tk.Status = apply(t, tk, step).Status
	if tk.Status.Run.Step != 4 || len(tk.Status.Outbox) != 1 {
		t.Fatalf("after advance: step %d, outbox %d", tk.Status.Run.Step, len(tk.Status.Outbox))
	}
	if _, err := Apply(tk, step, now); !errors.Is(err, ErrStepMismatch) {
		t.Fatalf("re-delivered step: got %v, want ErrStepMismatch", err)
	}

	runtime := taskIn(v1alpha1.PhaseRunning)
	runtime.Status.Run.Mode = v1alpha1.RunModeRuntime
	if _, err := Apply(runtime, Advance{RunID: runA, Step: 3}, now); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("advance in runtime mode: got %v", err)
	}
}

func TestRecordAndOutbox(t *testing.T) {
	tk := taskIn(v1alpha1.PhaseRunning)
	a, b := item("01JB0000000000000000000001"), item("01JB0000000000000000000002")
	res := apply(t, tk, Record{RunID: runA, Outbox: []v1alpha1.OutboxItem{a, b}})
	if res.Status.Phase != v1alpha1.PhaseRunning || res.Status.Run.Step != 3 || len(res.Status.Outbox) != 2 {
		t.Fatalf("after record: %+v", res.Status)
	}
	tk.Status = res.Status
	if _, err := Apply(tk, Record{RunID: runA, Outbox: []v1alpha1.OutboxItem{a}}, now); !errors.Is(err, ErrInvalidEvent) {
		t.Fatalf("duplicate outbox id: got %v", err)
	}
	bad := v1alpha1.OutboxItem{ID: "01JB0000000000000000000003", Kind: v1alpha1.OutboxCreateSubtask}
	if _, err := Apply(tk, Record{RunID: runA, Outbox: []v1alpha1.OutboxItem{bad}}, now); !errors.Is(err, ErrInvalidEvent) {
		t.Fatalf("subtask without a spec: got %v", err)
	}

	tk.Status = apply(t, tk, AckOutbox{IDs: []string{a.ID, "unknown"}}).Status
	if len(tk.Status.Outbox) != 1 || tk.Status.Outbox[0].ID != b.ID {
		t.Fatalf("after ack: %+v", tk.Status.Outbox)
	}
}

func TestFinish(t *testing.T) {
	for outcome, phase := range map[Outcome]v1alpha1.Phase{
		OutcomeDone: v1alpha1.PhaseDone, OutcomeReview: v1alpha1.PhaseReview, OutcomeFailed: v1alpha1.PhaseFailed,
	} {
		res := apply(t, taskIn(v1alpha1.PhaseRunning), Finish{RunID: runA, Outcome: outcome})
		if res.Status.Phase != phase || res.Status.Run != nil || !res.ReleasesSlot {
			t.Errorf("finish %s: %+v releases=%v", outcome, res.Status, res.ReleasesSlot)
		}
		if res.Ended == nil || res.Ended.Run.ID != runA || res.Ended.Reason != EndFinished || res.Ended.Outcome != outcome {
			t.Errorf("finish %s: ended = %+v", outcome, res.Ended)
		}
	}

	res := apply(t, taskIn(v1alpha1.PhaseRunning), Finish{
		RunID: runA, Outcome: OutcomeWaiting, Reason: v1alpha1.WaitingQuestion, ThreadSeq: 41,
		Outbox: []v1alpha1.OutboxItem{item("01JB0000000000000000000001")},
	})
	w := res.Status.Waiting
	if res.Status.Phase != v1alpha1.PhaseWaiting || w == nil || w.Reason != v1alpha1.WaitingQuestion || w.ThreadSeq != 41 || !w.Since.Time.Equal(now) {
		t.Fatalf("finish waiting: %+v", res.Status)
	}
	if len(res.Status.Outbox) != 1 {
		t.Fatalf("finish did not record its outbox: %+v", res.Status.Outbox)
	}

	for _, ev := range []Finish{
		{RunID: runA, Outcome: OutcomeWaiting},
		{RunID: runA, Outcome: "Paused"},
	} {
		if _, err := Apply(taskIn(v1alpha1.PhaseRunning), ev, now); !errors.Is(err, ErrInvalidEvent) {
			t.Errorf("finish %+v: got %v, want ErrInvalidEvent", ev, err)
		}
	}
}

func TestReplyClearsWaiting(t *testing.T) {
	s := apply(t, taskIn(v1alpha1.PhaseWaiting), Reply{}).Status
	if s.Phase != v1alpha1.PhaseTodo || s.Waiting != nil {
		t.Fatalf("after reply: %+v", s)
	}
}

func TestReclaim(t *testing.T) {
	live := taskIn(v1alpha1.PhaseRunning)
	live.Status.Run.LeaseUntil = metav1.NewTime(now)
	if _, err := Apply(live, Reclaim{}, now); !errors.Is(err, ErrLeaseNotExpired) {
		t.Fatalf("reclaim at the lease deadline: got %v", err)
	}
	res := apply(t, taskIn(v1alpha1.PhaseRunning), Reclaim{})
	if res.Status.Phase != v1alpha1.PhaseTodo || res.Status.Run != nil || !res.ReleasesSlot || res.Ended.Reason != EndInterrupted {
		t.Fatalf("after reclaim: %+v ended=%+v", res.Status, res.Ended)
	}
}

func TestCancel(t *testing.T) {
	for _, p := range allPhases {
		res := apply(t, taskIn(p), Cancel{})
		if res.Status.Phase != v1alpha1.PhaseCancelled || res.Status.Run != nil || res.Status.Waiting != nil {
			t.Errorf("cancel from %s: %+v", p, res.Status)
		}
		running := p == v1alpha1.PhaseRunning
		if res.ReleasesSlot != running || (res.Ended != nil) != running {
			t.Errorf("cancel from %s: releases=%v ended=%+v", p, res.ReleasesSlot, res.Ended)
		}
		if running && res.Ended.Reason != EndCancelled {
			t.Errorf("cancel from Running: reason %s", res.Ended.Reason)
		}
	}
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

	for i := 0; i < 2; i++ {
		s, err := AcquireSlot(team)
		if err != nil {
			t.Fatalf("acquire %d: %v", i, err)
		}
		team.Status = s
	}
	if _, err := AcquireSlot(team); !errors.Is(err, ErrNoSlot) {
		t.Fatalf("acquire past the limit: got %v", err)
	}
	team.Status = ReleaseSlot(team)
	team.Status = ReleaseSlot(team)
	team.Status = ReleaseSlot(team)
	if team.Status.Running != 0 {
		t.Fatalf("running = %d after over-release", team.Status.Running)
	}
}
