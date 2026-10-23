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
}

// Options configures a fake runner.
type Options struct {
	Runner     string
	Namespaces []string
	Plan       // used for every run unless Decide is set
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

// Run claims and works tasks until ctx is done, or after one run with Once.
func Run(ctx context.Context, c *client.Client, o Options) ([]Result, error) {
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
		claim, err := c.Claim(ctx, runner.ClaimRequest{Runner: o.Runner, Namespaces: o.Namespaces})
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
		}
		res, err := work(ctx, c, o, plan, claim)
		if err != nil {
			return results, err
		}
		results = append(results, res)
		if o.Once || ctx.Err() != nil {
			return results, nil
		}
	}
}

func work(ctx context.Context, c *client.Client, o Options, plan Plan, claim *runner.ClaimResponse) (Result, error) {
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
	// make the runner stop earlier.
	lease := time.Duration(claim.LeaseSeconds) * time.Second
	heldUntil := time.Now().Add(lease)
	every := time.Duration(claim.HeartbeatSeconds) * time.Second
	if every <= 0 {
		every = lease / 3
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
	for working := true; working; {
		select {
		case <-ctx.Done():
			// Stopped mid-run, like a killed runner: the lease expires and
			// the server reclaims the task.
			return res, nil
		case <-beat.C:
			sent := time.Now()
			hb, err := c.Heartbeat(ctx, runner.HeartbeatRequest{Task: ref, RunID: claim.RunID})
			switch {
			case client.IsCode(err, httpjson.CodeRunMismatch):
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
	fin, err := finish(ctx, c, req)
	switch {
	case client.IsCode(err, httpjson.CodeRunMismatch):
		observe(Observation{Kind: Rejected, Outcome: req.Outcome, Subtasks: ids})
		logf(o.Log, "run %s lost its task before finishing\n", claim.RunID)
		res.Lost = true
		return res, nil
	case err != nil && ctx.Err() != nil:
		return res, nil // killed while finishing; the finish may or may not have landed
	case err != nil:
		return res, fmt.Errorf("finish %s/%s: %w", ref.Namespace, ref.Name, err)
	}
	res.Phase = fin.Phase
	observe(Observation{Kind: Finished, Outcome: req.Outcome, Subtasks: ids, Phase: fin.Phase})
	logf(o.Log, "finished #%d %s/%s: %s\n", claim.Task.Number, ref.Namespace, ref.Name, strings.ToLower(string(fin.Phase)))
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
