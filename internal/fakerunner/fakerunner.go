// Package fakerunner is a runner that does no real work: it claims a task,
// keeps the run alive with heartbeats, pretends to work for a while and
// finishes with a chosen outcome. It drives a task through its whole life
// without an LLM, for development and for the chaos test.
package fakerunner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	"github.com/j75689/cadre/internal/client"
	"github.com/j75689/cadre/internal/httpjson"
	"github.com/j75689/cadre/internal/runner"
)

// Plan is what one run does.
type Plan struct {
	Work     time.Duration       // how long the run pretends to work
	Outcome  v1alpha1.RunOutcome // Done, Review, Failed or Waiting
	Reason   v1alpha1.WaitingReason
	Subtasks int // subtasks the run asks for when it finishes

	// Stall, if positive, makes the run behave like a process that freezes:
	// after StallAfter heartbeats it stops renewing for Stall, then wakes
	// unaware of the time lost and sends one more heartbeat and its finish
	// anyway, with no lease check and no deadline. Fencing must reject them
	// if the task was reclaimed meanwhile.
	Stall      time.Duration
	StallAfter int
}

// Options configures a fake runner.
type Options struct {
	Runner     string
	Namespaces []string
	// Labels are sent with every claim; they decide which agents' tasks the
	// runner may take.
	Labels map[string]string
	Plan   // used for every run unless Decide is set
	// Decide picks the plan for each claimed task, overriding Plan.
	Decide func(runner.ClaimedTask) Plan
	// Observe, if set, is told about every claim, renewal, finish and loss,
	// as soon as the runner learns of it.
	Observe func(Observation)
	Once    bool          // stop after one run
	Poll    time.Duration // wait between claims when there is no work
	Log     io.Writer
}

// ObservationKind is what a runner learned.
type ObservationKind string

// Observation kinds.
const (
	Claimed    ObservationKind = "Claimed"    // a claim response arrived
	Renewed    ObservationKind = "Renewed"    // a heartbeat succeeded
	FinishSent ObservationKind = "FinishSent" // the run stopped working and sent its finish
	Finished   ObservationKind = "Finished"   // a finish succeeded
	Rejected   ObservationKind = "Rejected"   // a request got 409 run_mismatch: the run lost its task
	Expired    ObservationKind = "Expired"    // the run could not renew its lease in time and stopped
)

// Observation is one thing a runner learned, and when.
type Observation struct {
	Kind  ObservationKind
	Task  runner.TaskRef
	RunID string
	At    time.Time
	// LeaseUntil is the lease the server granted, for Claimed and Renewed.
	LeaseUntil time.Time
	// Outcome and Subtasks are what the run asked for, for FinishSent,
	// Finished and a Rejected finish.
	Outcome  v1alpha1.RunOutcome
	Subtasks []string
	Phase    v1alpha1.Phase // Finished only
	// Late marks what a stalled run sent after it woke up.
	Late bool
}

// Result is what happened to one run.
type Result struct {
	Task  runner.TaskRef
	RunID string
	// Lost is true when the run lost its task (cancelled or reclaimed) or
	// could not renew its lease in time, and stopped without finishing.
	Lost  bool
	Phase v1alpha1.Phase
}

// Validate checks a plan before anything is claimed with it.
func (p Plan) Validate() error {
	switch {
	case p.Outcome != "" && !slices.Contains(outcomes, p.Outcome):
		return fmt.Errorf("outcome %q: want one of %v", p.Outcome, outcomes)
	case p.Reason != "" && !slices.Contains(reasons, p.Reason):
		return fmt.Errorf("waiting reason %q: want one of %v", p.Reason, reasons)
	case p.Subtasks < 0:
		return fmt.Errorf("subtasks %d: must not be negative", p.Subtasks)
	case p.Work < 0:
		return fmt.Errorf("work %s: must not be negative", p.Work)
	case p.Stall < 0 || p.StallAfter < 0:
		return fmt.Errorf("stall %s after %d heartbeats: must not be negative", p.Stall, p.StallAfter)
	}
	return nil
}

var (
	outcomes = []v1alpha1.RunOutcome{v1alpha1.RunOutcomeDone, v1alpha1.RunOutcomeReview, v1alpha1.RunOutcomeFailed, v1alpha1.RunOutcomeWaiting}
	reasons  = []v1alpha1.WaitingReason{v1alpha1.WaitingQuestion, v1alpha1.WaitingToolApproval, v1alpha1.WaitingReview}
)

// lostTask reports whether err means the run no longer holds its task: the
// protocol tells a runner to stop on run_mismatch and on not_found alike.
func lostTask(err error) bool {
	return client.IsCode(err, httpjson.CodeRunMismatch) || client.IsCode(err, httpjson.CodeNotFound)
}

// Run claims and works tasks until ctx is done, or after one run with Once.
// A plan is validated before any task is claimed with it.
func Run(ctx context.Context, c *client.Client, o Options) ([]Result, error) {
	if o.Decide == nil {
		if err := o.Validate(); err != nil {
			return nil, err
		}
	}
	if o.Poll <= 0 {
		o.Poll = time.Second
	}
	if o.Log == nil {
		o.Log = io.Discard
	}
	if o.Observe == nil {
		o.Observe = func(Observation) {}
	}
	var results []Result
	for {
		// The lease is counted from before the request, so the claim's
		// round trip can only make the runner stop earlier.
		sent := time.Now()
		claim, err := c.Claim(ctx, runner.ClaimRequest{Runner: o.Runner, Namespaces: o.Namespaces, Labels: o.Labels})
		if err != nil && ctx.Err() == nil {
			logf(o.Log, "claim failed: %v\n", err)
		}
		if claim == nil {
			select {
			case <-ctx.Done():
				return results, nil
			case <-time.After(o.Poll):
				continue
			}
		}
		plan := o.Plan
		if o.Decide != nil {
			plan = o.Decide(claim.Task)
			if err := plan.Validate(); err != nil {
				return results, err
			}
		}
		res, err := work(ctx, c, o, plan, claim, sent)
		if err != nil {
			return results, err
		}
		results = append(results, res)
		if o.Once || ctx.Err() != nil {
			return results, nil
		}
	}
}

func work(ctx context.Context, c *client.Client, o Options, plan Plan, claim *runner.ClaimResponse, claimSent time.Time) (Result, error) {
	ref := runner.TaskRef{Namespace: claim.Task.Namespace, Name: claim.Task.Name}
	res := Result{Task: ref, RunID: claim.RunID}
	observe := func(ob Observation) {
		ob.Task, ob.RunID, ob.At = ref, claim.RunID, time.Now()
		o.Observe(ob)
	}
	observe(Observation{Kind: Claimed, LeaseUntil: claim.LeaseUntil})
	logf(o.Log, "claimed #%d %s/%s run %s\n", claim.Task.Number, ref.Namespace, ref.Name, claim.RunID)
	_ = c.Event(ctx, runner.EventsRequest{Task: ref, RunID: claim.RunID, Seq: 1, Progress: "started"})

	// The run may act only while its lease is known to be held. The lease
	// is counted from before each request, so a slow response can only
	// make the runner stop earlier, and no request may outlive it.
	lease := time.Duration(claim.LeaseSeconds) * time.Second
	heldUntil := claimSent.Add(lease)
	every := time.Duration(claim.HeartbeatSeconds) * time.Second
	if every <= 0 {
		every = lease / 3
	}
	if every <= 0 {
		every = time.Second
	}
	lost := func(kind ObservationKind, why string) (Result, error) {
		logf(o.Log, "run %s %s; stopping\n", claim.RunID, why)
		observe(Observation{Kind: kind})
		res.Lost = true
		return res, nil
	}

	beat := time.NewTicker(every)
	defer beat.Stop()
	done := time.After(plan.Work)
	beats := 0
	for working := true; working; {
		select {
		case <-ctx.Done():
			// Stopped mid-run, like a killed runner: the lease expires and
			// the server reclaims the task.
			return res, nil
		case <-beat.C:
			if plan.Stall > 0 && beats == plan.StallAfter {
				return stalled(ctx, c, plan, claim, observe, res)
			}
			beats++
			sent := time.Now()
			hbCtx, cancel := context.WithDeadline(ctx, heldUntil)
			hb, err := c.Heartbeat(hbCtx, runner.HeartbeatRequest{Task: ref, RunID: claim.RunID})
			cancel()
			switch {
			case ctx.Err() != nil:
				return res, nil // killed mid-run
			case lostTask(err):
				return lost(Rejected, "lost its task")
			case err == nil:
				heldUntil = sent.Add(lease)
				observe(Observation{Kind: Renewed, LeaseUntil: hb.LeaseUntil})
			case time.Now().After(heldUntil):
				return lost(Expired, "could not renew its lease in time")
			default:
				logf(o.Log, "heartbeat failed: %v\n", err)
			}
		case <-done:
			working = false
		}
	}
	if time.Now().After(heldUntil) {
		return lost(Expired, "could not renew its lease in time")
	}

	req := runner.FinishRequest{Task: ref, RunID: claim.RunID, Outcome: plan.Outcome}
	if req.Outcome == "" {
		req.Outcome = v1alpha1.RunOutcomeDone
	}
	if req.Outcome == v1alpha1.RunOutcomeWaiting {
		reason := plan.Reason
		if reason == "" {
			reason = v1alpha1.WaitingQuestion
		}
		req.Waiting = &runner.Waiting{Reason: reason}
	}
	ids := make([]string, 0, plan.Subtasks)
	for i := range plan.Subtasks {
		// Minted here, so a retried finish asks for the same subtasks.
		id := ulid.Make().String()
		ids = append(ids, id)
		req.Subtasks = append(req.Subtasks, runner.Subtask{
			ID: id,
			Spec: v1alpha1.TaskSpec{
				Title:    fmt.Sprintf("Subtask %d of %s", i+1, claim.Task.Title),
				Assignee: claim.Task.Assignee,
			},
		})
	}
	observe(Observation{Kind: FinishSent, Outcome: req.Outcome, Subtasks: ids})
	finCtx, cancel := context.WithDeadline(ctx, heldUntil)
	defer cancel()
	fin, err := finish(finCtx, c, req)
	switch {
	case lostTask(err):
		observe(Observation{Kind: Rejected, Outcome: req.Outcome, Subtasks: ids})
		logf(o.Log, "run %s lost its task before finishing\n", claim.RunID)
		res.Lost = true
		return res, nil
	case err != nil && ctx.Err() != nil:
		return res, nil // killed while finishing; the finish may or may not have landed
	case err != nil && finCtx.Err() != nil:
		// The lease ran out before the finish was confirmed. It may or may
		// not have landed; the run stops either way.
		return lost(Expired, "could not finish within its lease")
	case err != nil:
		return res, fmt.Errorf("finish %s/%s: %w", ref.Namespace, ref.Name, err)
	}
	res.Phase = fin.Phase
	observe(Observation{Kind: Finished, Outcome: req.Outcome, Subtasks: ids, Phase: fin.Phase})
	logf(o.Log, "finished #%d %s/%s: %s\n", claim.Task.Number, ref.Namespace, ref.Name, strings.ToLower(string(fin.Phase)))
	return res, nil
}

// stalled freezes the run for plan.Stall, then sends a late heartbeat and a
// late finish as if no time had passed.
func stalled(ctx context.Context, c *client.Client, plan Plan, claim *runner.ClaimResponse, observe func(Observation), res Result) (Result, error) {
	select {
	case <-ctx.Done():
		return res, nil
	case <-time.After(plan.Stall):
	}
	ref := runner.TaskRef{Namespace: claim.Task.Namespace, Name: claim.Task.Name}
	// A woken run's requests may succeed (the task was never reclaimed), be
	// turned away as lost, or not get through (a server unavailable, a cut
	// connection: the run learns nothing and stops). Any other answer is a
	// protocol surprise.
	late := func(err error, ok Observation) (bool, error) {
		var cerr *client.Error
		switch {
		case err == nil:
			ok.Late = true
			observe(ok)
			return true, nil
		case lostTask(err):
			observe(Observation{Kind: Rejected, Late: true})
			return false, nil
		case ctx.Err() != nil, !errors.As(err, &cerr), cerr.Status >= 500:
			return false, nil
		default:
			return false, fmt.Errorf("late request of stalled run %s: %w", claim.RunID, err)
		}
	}
	hb, err := c.Heartbeat(ctx, runner.HeartbeatRequest{Task: ref, RunID: claim.RunID})
	renewed := Observation{Kind: Renewed}
	if hb != nil {
		renewed.LeaseUntil = hb.LeaseUntil
	}
	if _, err := late(err, renewed); err != nil {
		return res, err
	}
	outcome := plan.Outcome
	if outcome == "" || outcome == v1alpha1.RunOutcomeWaiting {
		outcome = v1alpha1.RunOutcomeDone
	}
	observe(Observation{Kind: FinishSent, Outcome: outcome, Late: true})
	fin, err := c.Finish(ctx, runner.FinishRequest{Task: ref, RunID: claim.RunID, Outcome: outcome})
	finished := Observation{Kind: Finished, Outcome: outcome}
	if fin != nil {
		finished.Phase = fin.Phase
	}
	ok, err := late(err, finished)
	if err != nil {
		return res, err
	}
	if ok {
		res.Phase = fin.Phase
	} else {
		res.Lost = true
	}
	return res, nil
}

// finish retries transient failures; a retry of a finish that already
// landed is answered with alreadyFinished, so retrying is safe.
func finish(ctx context.Context, c *client.Client, req runner.FinishRequest) (*runner.FinishResponse, error) {
	var err error
	for attempt := range 8 {
		var fin *runner.FinishResponse
		fin, err = c.Finish(ctx, req)
		var cerr *client.Error
		if err == nil || (errors.As(err, &cerr) && cerr.Status < 500) {
			return fin, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Duration(attempt+1) * 200 * time.Millisecond):
		}
	}
	return nil, err
}

// logf writes a progress line; a failing log writer must not stop a run.
func logf(w io.Writer, format string, args ...any) { _, _ = fmt.Fprintf(w, format, args...) }
