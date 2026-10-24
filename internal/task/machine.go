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
	// ErrForbidden means the request came from a runner other than the one
	// holding the run.
	ErrForbidden       = errors.New("the run is held by another runner")
	ErrLeaseNotExpired = errors.New("lease has not expired")
	ErrNotClaimable    = errors.New("task is not assigned to an agent")
	// ErrStepTaken means a step-mode run's current step is held by another
	// worker whose lease has not run out.
	ErrStepTaken = errors.New("the step is taken")
	// ErrStepExhausted means the step was taken MaxAttempts times: no
	// worker may take it again, and the run fails at its next reclaim.
	ErrStepExhausted = errors.New("the step was taken as many times as allowed")
	// ErrAttemptMismatch means the sender's attempt at a step is over: the
	// step was freed or taken again. The worker must stop.
	ErrAttemptMismatch = errors.New("attempt does not match the step's current attempt")
	ErrInvalidEvent    = errors.New("invalid event")
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
	KindTakeStep  Kind = "TakeStep"
	KindRelease   Kind = "ReleaseStep"
	KindRecord    Kind = "Record"
	KindFinish    Kind = "Finish"
	KindReply     Kind = "Reply"
	KindReclaim   Kind = "Reclaim"
	KindCancel    Kind = "Cancel"
	KindAckOutbox Kind = "AckOutbox"
	KindThread    Kind = "ThreadAdvanced"
	KindWaitingAt Kind = "WaitingAt"
	KindReserve   Kind = "ReserveItem"
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
	KindTakeStep:  {v1alpha1.PhaseRunning},
	KindRelease:   {v1alpha1.PhaseRunning},
	KindRecord:    {v1alpha1.PhaseRunning},
	KindFinish:    {v1alpha1.PhaseRunning},
	KindReply:     {v1alpha1.PhaseWaiting},
	KindReclaim:   {v1alpha1.PhaseRunning},
	KindCancel:    allPhases,
	KindAckOutbox: allPhases,
	KindThread:    append([]v1alpha1.Phase{unborn}, allPhases...),
	KindWaitingAt: allPhases,
	KindReserve:   allPhases,
}

// Event is one input to the state machine.
type Event interface{ kind() Kind }

// KindOf returns the kind of ev.
func KindOf(ev Event) Kind { return ev.kind() }

// fenced is an event sent by a run. It is checked against the task's run
// before anything else, so a run that lost its task always learns so with
// ErrRunMismatch, whatever phase the task has moved to.
type fenced interface{ runID() string }

// sent is an event that names the authenticated runner sending it. After
// the run id, the runner must be the one holding the run; an empty runner
// (a server that does not authenticate runners) is not checked.
type sent interface{ sender() string }

// Create numbers a new task and puts it in Todo.
type Create struct {
	Number int64 // claimed from the Team's counter
}

// Claim starts a run. Only a task assigned to an Agent can be claimed; this is
// decided here, on the fresh read, never from the index. The caller must
// already hold a Team slot.
type Claim struct {
	RunID string
	Mode  v1alpha1.RunMode
	// Runner is the claiming runner's identity, RunnerName the name it
	// gave itself.
	Runner     string
	RunnerName string
	Lease      time.Duration
	// Agent, if set, is the agent the claim was decided for; the claim is
	// refused if the fresh task is assigned to anyone else.
	Agent string
	// MCPTokenHash is the hash of the run's MCP token, written with the
	// claim so the token works from the moment the runner has it.
	MCPTokenHash string
}

// Heartbeat renews the run's lease; in step mode, the held step's.
type Heartbeat struct {
	RunID   string
	Runner  string // the sending runner's identity
	Step    int64  // in step mode, the step held
	Attempt int32  // in step mode, the attempt at it
	Lease   time.Duration
}

// MaxAttempts is how many workers may take one step of a step-mode run;
// after that the run fails.
const MaxAttempts = 3

// DefaultStepIdle is how long a free step waits for a worker when an event
// does not say: after that the run fails rather than hold its Team slot.
const DefaultStepIdle = time.Hour

// In step mode the attempt fences a step the way the run id fences the
// run: the events a worker sends for a step carry the attempt TakeStep (or
// the step-mode Claim) gave it, and are refused with ErrAttemptMismatch once
// the step was freed or taken again.

// TakeStep hands the current step of a step-mode run to a worker: Step
// must be the run's step, free (or held by a worker whose lease passed by
// more than Grace), and not taken MaxAttempts times. The worker gets a
// lease and the run a fresh MCP token hash, and the take is counted: the
// new Attempt is the worker's fence. The worker already holding the step
// may take it again, as when the answer to its take was lost: its lease
// and token are renewed and the attempt stays.
type TakeStep struct {
	RunID        string
	Step         int64
	Runner       string // the worker's identity; empty without authentication
	RunnerName   string // required
	Lease        time.Duration
	Grace        time.Duration
	MCPTokenHash string
}

// Advance commits step Step of a step-mode run: the step becomes Step+1,
// Outbox is written, and the step is freed (no worker, no MCP token: the
// step's token dies; the lease becomes the idle deadline), all in the same
// write, so any worker can take the next. Re-delivering the same step
// fails with ErrStepMismatch, so the step's effects land exactly once.
// Runner is the committing worker's identity.
type Advance struct {
	RunID   string
	Step    int64
	Attempt int32
	Runner  string
	Idle    time.Duration // how long the next step waits for a worker; 0 is DefaultStepIdle
	Outbox  []v1alpha1.OutboxItem
}

// ReleaseStep gives a step back without committing it, after a failed
// attempt: the step is free for the next worker, and its attempts stay
// counted.
type ReleaseStep struct {
	RunID   string
	Step    int64
	Attempt int32
	Runner  string
	Idle    time.Duration // 0 is DefaultStepIdle
}

// Record writes a run's outbox items without touching its phase, fenced
// on the run id; in step mode also on the step and the attempt, as the
// items are the current step's. An item the current step recorded before
// (a step done again recording it again) is skipped, even once applied.
type Record struct {
	RunID   string
	Step    int64
	Attempt int32
	Outbox  []v1alpha1.OutboxItem
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
// In step mode Step and Attempt must be the held step's, as for Advance.
type Finish struct {
	RunID     string
	Runner    string // the sending runner's identity
	Step      int64
	Attempt   int32
	Outcome   Outcome
	Reason    v1alpha1.WaitingReason // required for OutcomeWaiting
	ThreadSeq int64                  // optional, for OutcomeWaiting
	// WaitingItem, for OutcomeWaiting, is an AppendThread item in Outbox
	// that writes the entry the wait is on; its sequence is not known yet.
	WaitingItem string
	Outbox      []v1alpha1.OutboxItem
}

// Reply is a human answer or approval that ends the wait. A non-zero
// ThreadSeq names the wait it ends: the reply is refused if the task now
// waits on another entry, so a late reply cannot end a newer wait.
type Reply struct {
	ThreadSeq int64
}

// Reclaim takes back a run whose lease has passed by more than Grace. The
// grace absorbs clock skew between the replica that renewed the lease and the
// one reclaiming it, so a live run is never reclaimed.
//
// In step mode it frees a held step whose lease passed, for the next worker,
// and fails the run when the step was taken MaxAttempts times, or when a
// free step waited past its idle deadline. Idle is the next idle deadline
// for a step it frees; 0 is DefaultStepIdle.
type Reclaim struct {
	Grace time.Duration
	Idle  time.Duration
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

// WaitingAt records the thread entry a wait is on, once the outbox item
// ItemID has written it as Seq. It changes nothing unless the task still
// waits on that item without a sequence: a wait that ended, or moved on,
// is left alone.
type WaitingAt struct {
	ItemID string
	Seq    int64
}

func (WaitingAt) kind() Kind { return KindWaitingAt }

// ReserveItem moves where outbox item ID writes, from From to To. It is a
// compare-and-set: it changes nothing if the item is gone (applied) or
// reserved elsewhere already, so of dispatchers reserving at once one wins
// and the others read its choice.
//
// A reservation writes nothing to the stream: the keys there stay dense
// and in order. An ordinary append may take the reserved key first; the
// item then reserves the next.
type ReserveItem struct {
	ID       string
	From, To int64
}

func (ReserveItem) kind() Kind { return KindReserve }

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

func (e Heartbeat) runID() string    { return e.RunID }
func (e Heartbeat) sender() string   { return e.Runner }
func (e Finish) sender() string      { return e.Runner }
func (e Advance) runID() string      { return e.RunID }
func (e Advance) sender() string     { return e.Runner }
func (e TakeStep) runID() string     { return e.RunID }
func (e ReleaseStep) runID() string  { return e.RunID }
func (e ReleaseStep) sender() string { return e.Runner }
func (TakeStep) kind() Kind          { return KindTakeStep }
func (ReleaseStep) kind() Kind       { return KindRelease }
func (e Record) runID() string       { return e.RunID }
func (e Finish) runID() string       { return e.RunID }

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
		var sender string
		if sv, ok := ev.(sent); ok {
			sender = sv.sender()
		}
		if err := checkRun(&t.Status, f.runID(), sender); err != nil {
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
		if t.Spec.Assignee == nil || t.Spec.Assignee.Kind != v1alpha1.AssigneeAgent ||
			(ev.Agent != "" && t.Spec.Assignee.Name != ev.Agent) {
			return Result{}, ErrNotClaimable
		}
		if ev.RunID == "" || ev.Lease <= 0 {
			return Result{}, fmt.Errorf("claim needs a run id and a positive lease: %w", ErrInvalidEvent)
		}
		if ev.Mode != v1alpha1.RunModeRuntime && ev.Mode != v1alpha1.RunModeStep {
			return Result{}, fmt.Errorf("run mode %q: %w", ev.Mode, ErrInvalidEvent)
		}
		s.Phase = v1alpha1.PhaseRunning
		s.Run = &v1alpha1.Run{ID: ev.RunID, Mode: ev.Mode, Runner: ev.Runner, RunnerName: ev.RunnerName, LeaseUntil: leaseUntil(now, ev.Lease), MCPTokenHash: ev.MCPTokenHash}
		if ev.Mode == v1alpha1.RunModeStep {
			s.Run.Attempt = 1 // the claim takes step 0
		}
		if s.Runs == nil {
			s.Runs = &v1alpha1.RunsStatus{}
		}
		s.Runs.Count++
		s.Runs.LastID = ev.RunID

	case Heartbeat:
		if ev.Lease <= 0 {
			return Result{}, fmt.Errorf("heartbeat needs a positive lease: %w", ErrInvalidEvent)
		}
		if s.Run.Mode == v1alpha1.RunModeStep {
			if err := checkAttempt(s.Run, ev.Step, ev.Attempt); err != nil {
				return Result{}, err
			}
		}
		s.Run.LeaseUntil = leaseUntil(now, ev.Lease)

	case TakeStep:
		if s.Run.Mode != v1alpha1.RunModeStep {
			return Result{}, fmt.Errorf("take a step in %s mode: %w", s.Run.Mode, ErrInvalidTransition)
		}
		if err := checkStep(s.Run, ev.Step); err != nil {
			return Result{}, err
		}
		if ev.Lease <= 0 || ev.RunnerName == "" {
			return Result{}, fmt.Errorf("a step needs a positive lease and a named worker: %w", ErrInvalidEvent)
		}
		if stepHeld(s.Run, now, ev.Grace) {
			if s.Run.Runner != ev.Runner || s.Run.RunnerName != ev.RunnerName {
				return Result{}, ErrStepTaken
			}
			// The holder again, whose take's answer was lost.
			s.Run.LeaseUntil = leaseUntil(now, ev.Lease)
			s.Run.MCPTokenHash = ev.MCPTokenHash
			break
		}
		if s.Run.Attempt >= MaxAttempts {
			return Result{}, fmt.Errorf("step %d was taken %d times: %w", ev.Step, s.Run.Attempt, ErrStepExhausted)
		}
		s.Run.Attempt++
		s.Run.StepFree = false
		s.Run.Runner, s.Run.RunnerName = ev.Runner, ev.RunnerName
		s.Run.LeaseUntil = leaseUntil(now, ev.Lease)
		s.Run.MCPTokenHash = ev.MCPTokenHash

	case ReleaseStep:
		if s.Run.Mode != v1alpha1.RunModeStep {
			return Result{}, fmt.Errorf("release a step in %s mode: %w", s.Run.Mode, ErrInvalidTransition)
		}
		if err := checkAttempt(s.Run, ev.Step, ev.Attempt); err != nil {
			return Result{}, err
		}
		releaseStep(s.Run, now, ev.Idle)

	case Advance:
		if s.Run.Mode != v1alpha1.RunModeStep {
			return Result{}, fmt.Errorf("advance in %s mode: %w", s.Run.Mode, ErrInvalidTransition)
		}
		if err := checkAttempt(s.Run, ev.Step, ev.Attempt); err != nil {
			return Result{}, err
		}
		if err := appendOutbox(s, ev.Outbox); err != nil {
			return Result{}, err
		}
		s.Run.Step++
		releaseStep(s.Run, now, ev.Idle)
		s.Run.Attempt = 0
		s.Run.StepItems = nil

	case Record:
		if s.Run.Mode == v1alpha1.RunModeStep {
			if err := checkAttempt(s.Run, ev.Step, ev.Attempt); err != nil {
				return Result{}, err
			}
		}
		if err := appendOutbox(s, ev.Outbox); err != nil {
			return Result{}, err
		}

	case Finish:
		if s.Run.Mode == v1alpha1.RunModeStep {
			if err := checkAttempt(s.Run, ev.Step, ev.Attempt); err != nil {
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
			if ev.WaitingItem != "" && (ev.ThreadSeq != 0 || !slices.ContainsFunc(ev.Outbox, func(it v1alpha1.OutboxItem) bool {
				return it.ID == ev.WaitingItem && it.Kind == v1alpha1.OutboxAppendThread
			})) {
				return Result{}, fmt.Errorf("waiting item %q is not an entry this finish writes: %w", ev.WaitingItem, ErrInvalidEvent)
			}
			s.Phase = v1alpha1.PhaseWaiting
			s.Waiting = &v1alpha1.Waiting{Reason: ev.Reason, Since: metaTime(now), ThreadSeq: ev.ThreadSeq, ItemID: ev.WaitingItem}
		default:
			return Result{}, fmt.Errorf("outcome %q: %w", ev.Outcome, ErrInvalidEvent)
		}
		if err := appendOutbox(s, ev.Outbox); err != nil {
			return Result{}, err
		}
		endRun(&res, v1alpha1.RunFinished, ev.Outcome, now)

	case Reply:
		if ev.ThreadSeq != 0 && (s.Waiting == nil || s.Waiting.ThreadSeq != ev.ThreadSeq) {
			return Result{}, fmt.Errorf("reply to thread entry %d, but the task waits on another: %w", ev.ThreadSeq, ErrInvalidTransition)
		}
		s.Waiting = nil
		s.Phase = v1alpha1.PhaseTodo

	case Reclaim:
		if s.Run == nil {
			return Result{}, fmt.Errorf("running task has no run: %w", ErrInvalidTransition)
		}
		if s.Run.Mode == v1alpha1.RunModeStep {
			// A step-mode run outlives its workers: an expired step is
			// freed for the next one, until the step's attempts run out or
			// no worker comes for it.
			expired := now.After(s.Run.LeaseUntil.Add(ev.Grace))
			switch {
			case !s.Run.StepFree && !expired:
				return Result{}, ErrLeaseNotExpired
			case s.Run.StepFree && !expired && s.Run.Attempt < MaxAttempts:
				return Result{}, ErrLeaseNotExpired // waiting for a worker
			case !s.Run.StepFree && s.Run.Attempt < MaxAttempts:
				releaseStep(s.Run, now, ev.Idle)
				return res, nil
			}
			// The step's last attempt died or was given up, or no worker
			// took the step by its idle deadline.
			s.Phase = v1alpha1.PhaseFailed
			endRun(&res, v1alpha1.RunInterrupted, OutcomeFailed, now)
			return res, nil
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

	case ReserveItem:
		if ev.To < 1 || ev.To == ev.From {
			return Result{}, fmt.Errorf("reserve %d for item %q: %w", ev.To, ev.ID, ErrInvalidEvent)
		}
		for i := range s.Outbox {
			if s.Outbox[i].ID == ev.ID && s.Outbox[i].Seq == ev.From {
				s.Outbox[i].Seq = ev.To
			}
		}

	case WaitingAt:
		if ev.ItemID == "" || ev.Seq < 1 {
			return Result{}, fmt.Errorf("waiting at %q/%d: %w", ev.ItemID, ev.Seq, ErrInvalidEvent)
		}
		if w := s.Waiting; s.Phase == v1alpha1.PhaseWaiting && w != nil && w.ItemID == ev.ItemID && w.ThreadSeq == 0 {
			w.ThreadSeq = ev.Seq
		}

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

// checkRun fences an event on the task's run: the run id first, so a run
// that lost its task learns so whoever sends, then the sender.
func checkRun(s *v1alpha1.TaskStatus, runID, sender string) error {
	if s.Run == nil || s.Run.ID != runID {
		return ErrRunMismatch
	}
	if !HeldBy(s.Run.Runner, s.Run.RunnerName, sender) {
		return ErrForbidden
	}
	return nil
}

// HeldBy reports whether sender, an authenticated identity, may act on a
// run held by runner. An empty sender (a server that does not authenticate
// runners) is not checked, nor is a run claimed without an identity
// (runner or runnerName empty: claimed with authentication off, or before
// it existed), so turning authentication on does not cut the runs in
// flight.
func HeldBy(runner, runnerName, sender string) bool {
	return sender == "" || runner == "" || runnerName == "" || runner == sender
}

// stepHeld reports whether a worker holds the run's step: it is not free
// and its lease has not passed by more than grace.
func stepHeld(run *v1alpha1.Run, now time.Time, grace time.Duration) bool {
	return !run.StepFree && !now.After(run.LeaseUntil.Add(grace))
}

// releaseStep frees the run's step: no worker and no MCP token, the last
// worker kept for the record, and the lease set to the idle deadline. A
// step with no attempt left has none: the next reclaim fails the run.
func releaseStep(run *v1alpha1.Run, now time.Time, idle time.Duration) {
	switch {
	case run.Attempt >= MaxAttempts:
		idle = 0
	case idle <= 0:
		idle = DefaultStepIdle
	}
	run.LastRunner, run.LastRunnerName = run.Runner, run.RunnerName
	run.Runner, run.RunnerName = "", ""
	run.StepFree = true
	run.LeaseUntil = leaseUntil(now, idle)
	run.MCPTokenHash = ""
}

func checkStep(run *v1alpha1.Run, step int64) error {
	if step != run.Step {
		return fmt.Errorf("step %d, run is at %d: %w", step, run.Step, ErrStepMismatch)
	}
	return nil
}

// checkAttempt fences a worker's event on its step and its attempt at it.
func checkAttempt(run *v1alpha1.Run, step int64, attempt int32) error {
	if err := checkStep(run, step); err != nil {
		return err
	}
	if run.StepFree || attempt != run.Attempt {
		return fmt.Errorf("attempt %d at step %d, the step is at attempt %d (free: %t): %w", attempt, step, run.Attempt, run.StepFree, ErrAttemptMismatch)
	}
	return nil
}

// endRun clears the run from the status and writes its RunEnded item, keyed
// on the run id, in the same write.
func endRun(res *Result, reason v1alpha1.RunEndReason, outcome Outcome, now time.Time) {
	run := res.Status.Run
	runner, runnerName := run.Runner, run.RunnerName
	if run.StepFree {
		runner, runnerName = run.LastRunner, run.LastRunnerName
	}
	res.Status.Outbox = append(res.Status.Outbox, v1alpha1.OutboxItem{
		ID:   run.ID,
		Kind: v1alpha1.OutboxRunEnded,
		Run: &v1alpha1.RunRecord{
			ID: run.ID, Mode: run.Mode, Runner: runner, RunnerName: runnerName, Step: run.Step, Attempt: run.Attempt,
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
		switch {
		case it.ID == "":
			return fmt.Errorf("outbox item without an id: %w", ErrInvalidEvent)
		case it.Kind == v1alpha1.OutboxCreateSubtask && it.Spec != nil:
			if err := ValidateSpec(it.Spec); err != nil {
				return fmt.Errorf("outbox item %q: %w", it.ID, err)
			}
		case it.Kind == v1alpha1.OutboxAppendThread && it.Thread != nil && it.Thread.Blob != "" && it.Thread.Author != "" &&
			(it.Thread.Kind == "Comment" || it.Thread.Kind == "Question" || it.Thread.Kind == "ToolCall"):
		case it.Kind == v1alpha1.OutboxWriteFile && it.File != nil && it.File.Blob != "" && it.File.Path != "" && it.File.Size >= 0:
		default:
			return fmt.Errorf("outbox item %q of kind %q: %w", it.ID, it.Kind, ErrInvalidEvent)
		}
		if s.Run != nil && s.Run.Mode == v1alpha1.RunModeStep {
			if slices.ContainsFunc(s.Run.StepItems, func(o v1alpha1.StepItem) bool { return o.ID == it.ID }) {
				continue // a re-executed step records the same item again
			}
			s.Run.StepItems = append(s.Run.StepItems, v1alpha1.StepItem{ID: it.ID, After: itemAfter(it)})
		}
		if slices.ContainsFunc(s.Outbox, func(o v1alpha1.OutboxItem) bool { return o.ID == it.ID }) {
			return fmt.Errorf("duplicate outbox item %q: %w", it.ID, ErrInvalidEvent)
		}
		s.Outbox = append(s.Outbox, *it.DeepCopy())
	}
	return nil
}

// itemAfter is where an item's entry is looked for once it is applied.
func itemAfter(it v1alpha1.OutboxItem) int64 {
	switch {
	case it.Thread != nil:
		return it.Thread.After
	case it.File != nil:
		return it.File.After
	}
	return 0
}

var (
	priorities    = []v1alpha1.Priority{"", v1alpha1.PriorityLow, v1alpha1.PriorityMedium, v1alpha1.PriorityHigh, v1alpha1.PriorityUrgent}
	assigneeKinds = []v1alpha1.AssigneeKind{v1alpha1.AssigneeAgent, v1alpha1.AssigneeUser}
)

// ValidateSpec checks what the Task CRD validates on a spec, for every path
// that creates a task: a driver without an API server behind it (memory,
// postgres) would otherwise store what Kubernetes would reject. It fails
// with ErrInvalidEvent.
func ValidateSpec(spec *v1alpha1.TaskSpec) error {
	switch {
	case strings.TrimSpace(spec.Title) == "":
		return fmt.Errorf("title is required: %w", ErrInvalidEvent)
	case !slices.Contains(priorities, spec.Priority):
		return fmt.Errorf("priority %q: %w", spec.Priority, ErrInvalidEvent)
	case spec.Assignee != nil && (!slices.Contains(assigneeKinds, spec.Assignee.Kind) || spec.Assignee.Name == ""):
		return fmt.Errorf("assignee %+v: %w", *spec.Assignee, ErrInvalidEvent)
	case spec.ParentRef != nil && spec.ParentRef.Name == "":
		return fmt.Errorf("parentRef without a name: %w", ErrInvalidEvent)
	}
	return nil
}
