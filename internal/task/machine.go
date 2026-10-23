// Package task is the Task aggregate's state machine. It is pure: every
// transition takes the current Task and an event and returns the next status.
// It never reads a clock, mints an id or touches storage; callers pass those
// in and write the result with a single conditional write.
package task

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
)

// Errors returned by Apply. Callers map ErrRunMismatch to HTTP 409: the run
// has lost its task and must stop.
var (
	ErrInvalidTransition = errors.New("transition not allowed in this phase")
	ErrRunMismatch       = errors.New("run id does not match the task's run")
	ErrStepMismatch      = errors.New("step does not match the run's current step")
	ErrLeaseNotExpired   = errors.New("lease has not expired")
	ErrNotClaimable      = errors.New("task is not assigned to an agent")
	ErrInvalidEvent      = errors.New("invalid event")
)

// Kind names an event.
type Kind string

// Event kinds, one per transition in docs/execution.md, plus AckOutbox for
// the dispatcher clearing applied items.
const (
	KindCreate    Kind = "Create"
	KindClaim     Kind = "Claim"
	KindHeartbeat Kind = "Heartbeat"
	KindAdvance   Kind = "Advance"
	KindRecord    Kind = "Record"
	KindFinish    Kind = "Finish"
	KindReply     Kind = "Reply"
	KindReclaim   Kind = "Reclaim"
	KindCancel    Kind = "Cancel"
	KindAckOutbox Kind = "AckOutbox"
	KindThread    Kind = "ThreadAdvanced"
)

// unborn is the phase of a Task object that exists but has not been numbered.
const unborn v1alpha1.Phase = ""

var allPhases = []v1alpha1.Phase{
	v1alpha1.PhaseTodo, v1alpha1.PhaseRunning, v1alpha1.PhaseWaiting, v1alpha1.PhaseReview,
	v1alpha1.PhaseDone, v1alpha1.PhaseCancelled, v1alpha1.PhaseFailed,
}

// From is the transition table: the phases each event is allowed from.
var From = map[Kind][]v1alpha1.Phase{
	KindCreate:    {unborn},
	KindClaim:     {v1alpha1.PhaseTodo},
	KindHeartbeat: {v1alpha1.PhaseRunning},
	KindAdvance:   {v1alpha1.PhaseRunning},
	KindRecord:    {v1alpha1.PhaseRunning},
	KindFinish:    {v1alpha1.PhaseRunning},
	KindReply:     {v1alpha1.PhaseWaiting},
	KindReclaim:   {v1alpha1.PhaseRunning},
	KindCancel:    allPhases,
	KindAckOutbox: allPhases,
	KindThread:    append([]v1alpha1.Phase{unborn}, allPhases...),
}

// Event is one input to the state machine.
type Event interface{ kind() Kind }

// KindOf returns the kind of ev.
func KindOf(ev Event) Kind { return ev.kind() }

// fenced is an event sent by a run. It is checked against the task's run
// before anything else, so a run that lost its task always learns so with
// ErrRunMismatch, whatever phase the task has moved to.
type fenced interface{ runID() string }

// Create numbers a new task and puts it in Todo.
type Create struct {
	Number int64 // claimed from the Team's counter
}

// Claim starts a run. Only a task assigned to an Agent can be claimed; this is
// decided here, on the fresh read, never from the index. The caller must
// already hold a Team slot.
type Claim struct {
	RunID  string
	Mode   v1alpha1.RunMode
	Runner string
	Lease  time.Duration
}

// Heartbeat renews the run's lease.
type Heartbeat struct {
	RunID string
	Lease time.Duration
}

// Advance commits step Step of a step-mode run: the step becomes Step+1,
// Outbox is written, and the run is handed to Runner, the worker taking the
// next step, with a fresh lease, all in the same write. Re-delivering the same
// step fails with ErrStepMismatch, so the step's effects land exactly once.
type Advance struct {
	RunID  string
	Step   int64
	Runner string
	Lease  time.Duration
	Outbox []v1alpha1.OutboxItem
}

// Record writes outbox items for a runtime-mode run without touching its
// phase. It is fenced on the run id only, because a runtime run has no step;
// a step-mode run commits its outbox with Advance instead.
type Record struct {
	RunID  string
	Outbox []v1alpha1.OutboxItem
}

// Outcome is how a run finished.
type Outcome = v1alpha1.RunOutcome

// Run outcomes, as accepted by /runner/v1/finish.
const (
	OutcomeDone    = v1alpha1.RunOutcomeDone
	OutcomeReview  = v1alpha1.RunOutcomeReview
	OutcomeWaiting = v1alpha1.RunOutcomeWaiting
	OutcomeFailed  = v1alpha1.RunOutcomeFailed
)

// Finish ends the run with an outcome. Outbox is written in the same write.
// In step mode Step must be the run's current step, as for Advance.
type Finish struct {
	RunID     string
	Step      int64
	Outcome   Outcome
	Reason    v1alpha1.WaitingReason // required for OutcomeWaiting
	ThreadSeq int64                  // optional, for OutcomeWaiting
	Outbox    []v1alpha1.OutboxItem
}

// Reply is a human answer or approval that ends the wait.
type Reply struct{}

// Reclaim takes back a run whose lease has passed by more than Grace. The
// grace absorbs clock skew between the replica that renewed the lease and the
// one reclaiming it, so a live run is never reclaimed.
type Reclaim struct {
	Grace time.Duration
}

// Cancel is a human cancelling the task.
type Cancel struct{}

// AckOutbox removes outbox items the dispatcher has applied.
type AckOutbox struct {
	IDs []string
}

// ThreadAdvanced records that the thread entry Seq was appended: the thread
// revision becomes at least Seq, so watchers of the Task learn the thread
// changed.
//
// The log entry is the commit and the revision is only a hint. An appender
// that dies between its append and this write leaves the revision behind;
// the next appender finds the taken sequence numbers in the log, appends
// after them and advances the revision past both. The revision never moves
// backwards.
type ThreadAdvanced struct {
	Seq int64
}

func (ThreadAdvanced) kind() Kind { return KindThread }

func (Create) kind() Kind    { return KindCreate }
func (Claim) kind() Kind     { return KindClaim }
func (Heartbeat) kind() Kind { return KindHeartbeat }
func (Advance) kind() Kind   { return KindAdvance }
func (Record) kind() Kind    { return KindRecord }
func (Finish) kind() Kind    { return KindFinish }
func (Reply) kind() Kind     { return KindReply }
func (Reclaim) kind() Kind   { return KindReclaim }
func (Cancel) kind() Kind    { return KindCancel }
func (AckOutbox) kind() Kind { return KindAckOutbox }

func (e Heartbeat) runID() string { return e.RunID }
func (e Advance) runID() string   { return e.RunID }
func (e Record) runID() string    { return e.RunID }
func (e Finish) runID() string    { return e.RunID }

// Result is the outcome of a transition.
type Result struct {
	// Status is the next status, to be written with a conditional write.
	Status v1alpha1.TaskStatus
	// ReleasesSlot is true when the transition ended a run, so the caller
	// releases the Team slot after the write succeeds. The ended run itself
	// is recorded by a RunEnded outbox item in Status.
	ReleasesSlot bool
}

// Apply runs one transition. It does not modify t.
func Apply(t *v1alpha1.Task, ev Event, now time.Time) (Result, error) {
	if f, ok := ev.(fenced); ok {
		if err := checkRun(&t.Status, f.runID()); err != nil {
			return Result{}, err
		}
	}
	cur := t.Status.Phase
	if !slices.Contains(From[ev.kind()], cur) {
		return Result{}, fmt.Errorf("%s from phase %q: %w", ev.kind(), cur, ErrInvalidTransition)
	}
	res := Result{Status: *t.Status.DeepCopy()}
	s := &res.Status
	switch ev := ev.(type) {
	case Create:
		if ev.Number < 1 {
			return Result{}, fmt.Errorf("number %d: %w", ev.Number, ErrInvalidEvent)
		}
		s.Number = ev.Number
		s.Phase = v1alpha1.PhaseTodo

	case Claim:
		if t.Spec.Assignee == nil || t.Spec.Assignee.Kind != v1alpha1.AssigneeAgent {
			return Result{}, ErrNotClaimable
		}
		if ev.RunID == "" || ev.Lease <= 0 {
			return Result{}, fmt.Errorf("claim needs a run id and a positive lease: %w", ErrInvalidEvent)
		}
		if ev.Mode != v1alpha1.RunModeRuntime && ev.Mode != v1alpha1.RunModeStep {
			return Result{}, fmt.Errorf("run mode %q: %w", ev.Mode, ErrInvalidEvent)
		}
		s.Phase = v1alpha1.PhaseRunning
		s.Run = &v1alpha1.Run{ID: ev.RunID, Mode: ev.Mode, Runner: ev.Runner, LeaseUntil: leaseUntil(now, ev.Lease)}
		if s.Runs == nil {
			s.Runs = &v1alpha1.RunsStatus{}
		}
		s.Runs.Count++
		s.Runs.LastID = ev.RunID

	case Heartbeat:
		if ev.Lease <= 0 {
			return Result{}, fmt.Errorf("heartbeat needs a positive lease: %w", ErrInvalidEvent)
		}
		s.Run.LeaseUntil = leaseUntil(now, ev.Lease)

	case Advance:
		if s.Run.Mode != v1alpha1.RunModeStep {
			return Result{}, fmt.Errorf("advance in %s mode: %w", s.Run.Mode, ErrInvalidTransition)
		}
		if err := checkStep(s.Run, ev.Step); err != nil {
			return Result{}, err
		}
		if ev.Lease <= 0 {
			return Result{}, fmt.Errorf("advance needs a positive lease: %w", ErrInvalidEvent)
		}
		if err := appendOutbox(s, ev.Outbox); err != nil {
			return Result{}, err
		}
		s.Run.Step++
		s.Run.Runner = ev.Runner
		s.Run.LeaseUntil = leaseUntil(now, ev.Lease)

	case Record:
		if s.Run.Mode != v1alpha1.RunModeRuntime {
			return Result{}, fmt.Errorf("record in %s mode, use advance: %w", s.Run.Mode, ErrInvalidTransition)
		}
		if err := appendOutbox(s, ev.Outbox); err != nil {
			return Result{}, err
		}

	case Finish:
		if s.Run.Mode == v1alpha1.RunModeStep {
			if err := checkStep(s.Run, ev.Step); err != nil {
				return Result{}, err
			}
		}
		switch ev.Outcome {
		case OutcomeDone:
			s.Phase = v1alpha1.PhaseDone
		case OutcomeReview:
			s.Phase = v1alpha1.PhaseReview
		case OutcomeFailed:
			s.Phase = v1alpha1.PhaseFailed
		case OutcomeWaiting:
			if !slices.Contains(waitingReasons, ev.Reason) {
				return Result{}, fmt.Errorf("waiting reason %q: %w", ev.Reason, ErrInvalidEvent)
			}
			if ev.ThreadSeq < 0 {
				return Result{}, fmt.Errorf("thread sequence %d: %w", ev.ThreadSeq, ErrInvalidEvent)
			}
			s.Phase = v1alpha1.PhaseWaiting
			s.Waiting = &v1alpha1.Waiting{Reason: ev.Reason, Since: metaTime(now), ThreadSeq: ev.ThreadSeq}
		default:
			return Result{}, fmt.Errorf("outcome %q: %w", ev.Outcome, ErrInvalidEvent)
		}
		if err := appendOutbox(s, ev.Outbox); err != nil {
			return Result{}, err
		}
		endRun(&res, v1alpha1.RunFinished, ev.Outcome, now)

	case Reply:
		s.Waiting = nil
		s.Phase = v1alpha1.PhaseTodo

	case Reclaim:
		if s.Run == nil {
			return Result{}, fmt.Errorf("running task has no run: %w", ErrInvalidTransition)
		}
		if !now.After(s.Run.LeaseUntil.Add(ev.Grace)) {
			return Result{}, ErrLeaseNotExpired
		}
		s.Phase = v1alpha1.PhaseTodo
		endRun(&res, v1alpha1.RunInterrupted, "", now)

	case Cancel:
		// The agent's subtasks that were never created are not wanted any
		// more; run history still has to be written.
		s.Outbox = slices.DeleteFunc(s.Outbox, func(it v1alpha1.OutboxItem) bool {
			return it.Kind == v1alpha1.OutboxCreateSubtask
		})
		if s.Run != nil {
			endRun(&res, v1alpha1.RunCancelled, "", now)
		}
		s.Waiting = nil
		s.Phase = v1alpha1.PhaseCancelled

	case ThreadAdvanced:
		if ev.Seq < 1 {
			return Result{}, fmt.Errorf("thread sequence %d: %w", ev.Seq, ErrInvalidEvent)
		}
		if s.Thread == nil {
			s.Thread = &v1alpha1.ThreadStatus{}
		}
		s.Thread.Revision = max(s.Thread.Revision, ev.Seq)

	case AckOutbox:
		s.Outbox = slices.DeleteFunc(s.Outbox, func(it v1alpha1.OutboxItem) bool {
			return slices.Contains(ev.IDs, it.ID)
		})

	default:
		return Result{}, fmt.Errorf("event %T: %w", ev, ErrInvalidEvent)
	}
	return res, nil
}

var waitingReasons = []v1alpha1.WaitingReason{
	v1alpha1.WaitingQuestion, v1alpha1.WaitingToolApproval, v1alpha1.WaitingReview,
}

func checkRun(s *v1alpha1.TaskStatus, runID string) error {
	if s.Run == nil || s.Run.ID != runID {
		return ErrRunMismatch
	}
	return nil
}

func checkStep(run *v1alpha1.Run, step int64) error {
	if step != run.Step {
		return fmt.Errorf("step %d, run is at %d: %w", step, run.Step, ErrStepMismatch)
	}
	return nil
}

// endRun clears the run from the status and writes its RunEnded item, keyed
// on the run id, in the same write.
func endRun(res *Result, reason v1alpha1.RunEndReason, outcome Outcome, now time.Time) {
	run := res.Status.Run
	res.Status.Outbox = append(res.Status.Outbox, v1alpha1.OutboxItem{
		ID:   run.ID,
		Kind: v1alpha1.OutboxRunEnded,
		Run: &v1alpha1.RunRecord{
			ID: run.ID, Mode: run.Mode, Runner: run.Runner, Step: run.Step,
			Reason: reason, Outcome: outcome, EndedAt: metaTime(now),
		},
	})
	res.Status.Run = nil
	res.ReleasesSlot = true
}

// appendOutbox adds CreateSubtask items written by a run. A subtask spec the
// API server would reject is refused here, so it can never become an outbox
// item the dispatcher fails on forever.
func appendOutbox(s *v1alpha1.TaskStatus, items []v1alpha1.OutboxItem) error {
	for _, it := range items {
		if it.ID == "" || it.Kind != v1alpha1.OutboxCreateSubtask || it.Spec == nil {
			return fmt.Errorf("outbox item %q: %w", it.ID, ErrInvalidEvent)
		}
		if err := validSpec(it.Spec); err != nil {
			return fmt.Errorf("outbox item %q: %w", it.ID, err)
		}
		if slices.ContainsFunc(s.Outbox, func(o v1alpha1.OutboxItem) bool { return o.ID == it.ID }) {
			return fmt.Errorf("duplicate outbox item %q: %w", it.ID, ErrInvalidEvent)
		}
		s.Outbox = append(s.Outbox, *it.DeepCopy())
	}
	return nil
}

var (
	priorities    = []v1alpha1.Priority{"", v1alpha1.PriorityLow, v1alpha1.PriorityMedium, v1alpha1.PriorityHigh, v1alpha1.PriorityUrgent}
	assigneeKinds = []v1alpha1.AssigneeKind{v1alpha1.AssigneeAgent, v1alpha1.AssigneeUser}
)

// validSpec checks what the Task CRD validates on a spec.
func validSpec(spec *v1alpha1.TaskSpec) error {
	switch {
	case strings.TrimSpace(spec.Title) == "":
		return fmt.Errorf("subtask without a title: %w", ErrInvalidEvent)
	case !slices.Contains(priorities, spec.Priority):
		return fmt.Errorf("priority %q: %w", spec.Priority, ErrInvalidEvent)
	case spec.Assignee != nil && (!slices.Contains(assigneeKinds, spec.Assignee.Kind) || spec.Assignee.Name == ""):
		return fmt.Errorf("assignee %+v: %w", *spec.Assignee, ErrInvalidEvent)
	case spec.ParentRef != nil && spec.ParentRef.Name == "":
		return fmt.Errorf("parentRef without a name: %w", ErrInvalidEvent)
	}
	return nil
}
