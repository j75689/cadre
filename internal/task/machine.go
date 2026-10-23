// Package task is the Task aggregate's state machine. It is pure: every
// transition takes the current Task and an event and returns the next status.
// It never reads a clock, mints an id or touches storage; callers pass those
// in and write the result with a single conditional write.
package task

import (
	"errors"
	"fmt"
	"slices"
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
	ErrUnassigned        = errors.New("task has no assignee")
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
}

// Event is one input to the state machine.
type Event interface{ kind() Kind }

// KindOf returns the kind of ev.
func KindOf(ev Event) Kind { return ev.kind() }

// Create numbers a new task and puts it in Todo.
type Create struct {
	Number int64 // claimed from the Team's counter
}

// Claim starts a run. The caller must already hold a Team slot.
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

// Advance commits step Step of a step-mode run: the step becomes Step+1 and
// Outbox is written in the same write. Re-delivering the same step fails with
// ErrStepMismatch, so the step's effects land exactly once.
type Advance struct {
	RunID  string
	Step   int64
	Outbox []v1alpha1.OutboxItem
}

// Record writes outbox items for a run without touching its phase or step. It
// is fenced on the run id only, because a runtime-mode run has no step.
type Record struct {
	RunID  string
	Outbox []v1alpha1.OutboxItem
}

// Outcome is how a run finished.
type Outcome string

// Run outcomes, as accepted by /runner/v1/finish.
const (
	OutcomeDone    Outcome = "Done"
	OutcomeReview  Outcome = "Review"
	OutcomeWaiting Outcome = "Waiting"
	OutcomeFailed  Outcome = "Failed"
)

// Finish ends the run with an outcome. Outbox is recorded exactly as Record
// would, in the same write as the phase change.
type Finish struct {
	RunID     string
	Outcome   Outcome
	Reason    v1alpha1.WaitingReason // required for OutcomeWaiting
	ThreadSeq int64                  // optional, for OutcomeWaiting
	Outbox    []v1alpha1.OutboxItem
}

// Reply is a human answer or approval that ends the wait.
type Reply struct{}

// Reclaim takes back a run whose lease has passed.
type Reclaim struct{}

// Cancel is a human cancelling the task.
type Cancel struct{}

// AckOutbox removes outbox items the dispatcher has applied.
type AckOutbox struct {
	IDs []string
}

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

// EndReason says why a run ended.
type EndReason string

// End reasons.
const (
	EndFinished    EndReason = "Finished"
	EndInterrupted EndReason = "Interrupted"
	EndCancelled   EndReason = "Cancelled"
)

// EndedRun describes a run a transition ended, for the run history log.
type EndedRun struct {
	Run     v1alpha1.Run
	Reason  EndReason
	Outcome Outcome // set when Reason is EndFinished
	At      time.Time
}

// Result is the outcome of a transition.
type Result struct {
	// Status is the next status, to be written with a conditional write.
	Status v1alpha1.TaskStatus
	// ReleasesSlot is true when the transition ended a run, so the caller
	// releases the Team slot after the write succeeds.
	ReleasesSlot bool
	// Ended is the run the transition ended, if any.
	Ended *EndedRun
}

// Apply runs one transition. It does not modify t.
func Apply(t *v1alpha1.Task, ev Event, now time.Time) (Result, error) {
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
		if t.Spec.Assignee == nil {
			return Result{}, ErrUnassigned
		}
		if ev.RunID == "" || ev.Lease <= 0 {
			return Result{}, fmt.Errorf("claim needs a run id and a positive lease: %w", ErrInvalidEvent)
		}
		s.Phase = v1alpha1.PhaseRunning
		s.Run = &v1alpha1.Run{ID: ev.RunID, Mode: ev.Mode, Runner: ev.Runner, LeaseUntil: leaseUntil(now, ev.Lease)}
		if s.Runs == nil {
			s.Runs = &v1alpha1.RunsStatus{}
		}
		s.Runs.Count++
		s.Runs.LastID = ev.RunID

	case Heartbeat:
		if err := checkRun(s, ev.RunID); err != nil {
			return Result{}, err
		}
		if ev.Lease <= 0 {
			return Result{}, fmt.Errorf("heartbeat needs a positive lease: %w", ErrInvalidEvent)
		}
		s.Run.LeaseUntil = leaseUntil(now, ev.Lease)

	case Advance:
		if err := checkRun(s, ev.RunID); err != nil {
			return Result{}, err
		}
		if s.Run.Mode != v1alpha1.RunModeStep {
			return Result{}, fmt.Errorf("advance in %s mode: %w", s.Run.Mode, ErrInvalidTransition)
		}
		if ev.Step != s.Run.Step {
			return Result{}, fmt.Errorf("step %d, run is at %d: %w", ev.Step, s.Run.Step, ErrStepMismatch)
		}
		if err := appendOutbox(s, ev.Outbox); err != nil {
			return Result{}, err
		}
		s.Run.Step++

	case Record:
		if err := record(s, ev.RunID, ev.Outbox); err != nil {
			return Result{}, err
		}

	case Finish:
		if err := record(s, ev.RunID, ev.Outbox); err != nil {
			return Result{}, err
		}
		switch ev.Outcome {
		case OutcomeDone:
			s.Phase = v1alpha1.PhaseDone
		case OutcomeReview:
			s.Phase = v1alpha1.PhaseReview
		case OutcomeFailed:
			s.Phase = v1alpha1.PhaseFailed
		case OutcomeWaiting:
			if ev.Reason == "" {
				return Result{}, fmt.Errorf("waiting outcome needs a reason: %w", ErrInvalidEvent)
			}
			s.Phase = v1alpha1.PhaseWaiting
			s.Waiting = &v1alpha1.Waiting{Reason: ev.Reason, Since: metaTime(now), ThreadSeq: ev.ThreadSeq}
		default:
			return Result{}, fmt.Errorf("outcome %q: %w", ev.Outcome, ErrInvalidEvent)
		}
		endRun(&res, EndFinished, ev.Outcome, now)

	case Reply:
		s.Waiting = nil
		s.Phase = v1alpha1.PhaseTodo

	case Reclaim:
		if !now.After(s.Run.LeaseUntil.Time) {
			return Result{}, ErrLeaseNotExpired
		}
		s.Phase = v1alpha1.PhaseTodo
		endRun(&res, EndInterrupted, "", now)

	case Cancel:
		if s.Run != nil {
			endRun(&res, EndCancelled, "", now)
		}
		s.Waiting = nil
		s.Phase = v1alpha1.PhaseCancelled

	case AckOutbox:
		s.Outbox = slices.DeleteFunc(s.Outbox, func(it v1alpha1.OutboxItem) bool {
			return slices.Contains(ev.IDs, it.ID)
		})

	default:
		return Result{}, fmt.Errorf("event %T: %w", ev, ErrInvalidEvent)
	}
	return res, nil
}

func checkRun(s *v1alpha1.TaskStatus, runID string) error {
	if s.Run == nil || s.Run.ID != runID {
		return ErrRunMismatch
	}
	return nil
}

// endRun clears the run from the status and records it as ended.
func endRun(res *Result, reason EndReason, outcome Outcome, now time.Time) {
	res.Ended = &EndedRun{Run: *res.Status.Run, Reason: reason, Outcome: outcome, At: now}
	res.Status.Run = nil
	res.ReleasesSlot = true
}

// record is the Record transition, shared with Finish.
func record(s *v1alpha1.TaskStatus, runID string, items []v1alpha1.OutboxItem) error {
	if err := checkRun(s, runID); err != nil {
		return err
	}
	return appendOutbox(s, items)
}

func appendOutbox(s *v1alpha1.TaskStatus, items []v1alpha1.OutboxItem) error {
	for _, it := range items {
		if it.ID == "" || it.Kind != v1alpha1.OutboxCreateSubtask || it.Spec == nil {
			return fmt.Errorf("outbox item %q: %w", it.ID, ErrInvalidEvent)
		}
		if slices.ContainsFunc(s.Outbox, func(o v1alpha1.OutboxItem) bool { return o.ID == it.ID }) {
			return fmt.Errorf("duplicate outbox item %q: %w", it.ID, ErrInvalidEvent)
		}
		s.Outbox = append(s.Outbox, *it.DeepCopy())
	}
	return nil
}
