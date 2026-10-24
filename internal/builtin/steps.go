package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	"github.com/j75689/cadre/internal/client"
	"github.com/j75689/cadre/internal/httpjson"
	"github.com/j75689/cadre/internal/manager"
	"github.com/j75689/cadre/internal/runner"
)

// RunSteps claims steps of builtin agents' step-mode runs and does them,
// one at a time, until ctx is done, or after one step with Once.
//
// A step-mode run moves between workers a step at a time: a worker knows
// only the step it took, and rebuilds the conversation from the run's
// transcript. Start (step 0) describes the model and records the
// conversation's opening; llm_turn makes one model call and records its
// reply; exec_tools makes the calls the turn asked for, through the step's
// MCP endpoint, each with the model's call id, and records their results;
// finish posts the agent's summary with the finish. A step whose entry is
// there already (its worker died before committing it) is committed as it
// is, without doing it again.
func RunSteps(ctx context.Context, c *client.Client, o Options) error {
	o = o.withDefaults()
	return claims(ctx, c, o, v1alpha1.RunModeStep, func(claim *runner.ClaimResponse, sent time.Time) bool {
		task := runner.TaskRef{Namespace: claim.Task.Namespace, Name: claim.Task.Name}
		st := &step{o: o, c: c, claim: claim, task: task,
			ref: runner.StepRef{Task: task, RunID: claim.RunID, Step: claim.Step, Attempt: claim.Attempt}}
		return st.work(ctx, sent)
	})
}

// step is one claimed step.
type step struct {
	o     Options
	c     *client.Client
	claim *runner.ClaimResponse
	task  runner.TaskRef
	ref   runner.StepRef
}

// errStepTimeout ends a step that ran past its timeout.
var errStepTimeout = errors.New("the step timed out")

// outcome is how a step ends: its entry appended and the step committed,
// the run finished, the step given back for another worker, or nothing at
// all (the step is no longer this worker's, or the server ended the run).
type outcome struct {
	entry   *manager.TranscriptEntry
	finish  bool
	result  v1alpha1.RunOutcome
	comment string
	release string // why the step is given back
}

func finishStep(result v1alpha1.RunOutcome, comment string) outcome {
	return outcome{finish: true, result: result, comment: comment}
}

// work does the claimed step. It reports whether to wait before claiming
// again: when the step was given back for a fault of the worker's
// surroundings, not the task's.
func (s *step) work(ctx context.Context, claimSent time.Time) (backOff bool) {
	log := s.o.Log.With("task", s.task.Namespace+"/"+s.task.Name, "run", s.claim.RunID, "step", s.ref.Step, "attempt", s.ref.Attempt)
	log.Info("took a step", "kind", s.claim.StepKind)
	stepCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	stepCtx, stop := context.WithTimeoutCause(stepCtx, s.o.StepTimeout, errStepTimeout)
	defer stop()
	beating := make(chan struct{})
	go func() { defer close(beating); s.heartbeat(stepCtx, cancel, claimSent) }()
	defer func() { cancel(nil); <-beating }()

	end := s.do(stepCtx, log)
	if errors.Is(context.Cause(stepCtx), errStepTimeout) {
		end = outcome{release: fmt.Sprintf("the step timed out after %s", s.o.StepTimeout)}
	} else if stepCtx.Err() != nil {
		log.Info("step stopped", "why", context.Cause(stepCtx))
		return false
	}
	// What ends the step is written even when the step's own time is up:
	// the next worker depends on it.
	endCtx := context.WithoutCancel(ctx)
	switch {
	case end.release != "":
		log.Warn("giving the step back", "why", end.release)
		if err := retryShort(endCtx, func() error {
			return s.c.StepRelease(endCtx, runner.StepReleaseRequest{StepRef: s.ref, Error: end.release})
		}); err != nil && !lostStep(err) {
			log.Warn("give the step back", "err", err)
		}
		return true
	case end.finish:
		fin, err := s.finish(endCtx, end)
		if err != nil {
			log.Warn("finish", "outcome", end.result, "err", err)
			return false
		}
		log.Info("finished", "outcome", end.result, "phase", fin.Phase)
	case end.entry != nil:
		s.commit(stepCtx, log, end.entry)
	}
	return false
}

// commit appends the step's entry and commits the step; an entry another
// worker wrote first is committed instead.
func (s *step) commit(ctx context.Context, log interface{ Warn(string, ...any) }, e *manager.TranscriptEntry) {
	data, err := json.Marshal(e)
	if err != nil {
		log.Warn("encode the step's entry", "err", err)
		return
	}
	var app *runner.StepAppendResponse
	if err := retryShort(ctx, func() (err error) {
		app, err = s.c.StepAppend(ctx, runner.StepAppendRequest{StepRef: s.ref, Entry: data})
		return err
	}); err != nil {
		log.Warn("append the step's entry", "err", err)
		return
	}
	if app.Adopted {
		log.Warn("the step was written by an earlier worker; committing that")
	}
	if s.o.StepDone != nil && !s.o.StepDone(s.ref.Step, string(e.Kind)) {
		return
	}
	s.advance(ctx, log)
}

func (s *step) advance(ctx context.Context, log interface{ Warn(string, ...any) }) {
	if err := retryShort(ctx, func() error {
		_, err := s.c.StepAdvance(ctx, runner.StepAdvanceRequest{StepRef: s.ref})
		return err
	}); err != nil {
		log.Warn("commit the step", "err", err)
	}
}

// do does the step and says how it ends.
func (s *step) do(ctx context.Context, log interface{ Warn(string, ...any) }) outcome {
	var tr *runner.StepTranscript
	if err := retryShort(ctx, func() (err error) { tr, err = s.c.StepTranscript(ctx, s.ref); return err }); err != nil {
		if lostStep(err) {
			return outcome{}
		}
		return outcome{release: "the run's transcript could not be read"}
	}
	entries := make([]manager.TranscriptEntry, len(tr.Entries))
	for i, raw := range tr.Entries {
		if err := json.Unmarshal(raw, &entries[i]); err != nil {
			return finishStep(v1alpha1.RunOutcomeFailed, fmt.Sprintf("This run failed: its transcript's step %d could not be read.", i))
		}
	}
	if int64(len(entries)) > s.ref.Step {
		// A worker that died after writing the step's entry: the step is
		// done, only its commit is missing.
		s.advance(ctx, log)
		return outcome{}
	}
	p, credential, failure := model(ctx, s.o, s.task.Namespace, s.claim.Agent)
	if failure != "" {
		return finishStep(v1alpha1.RunOutcomeFailed, failure)
	}
	sp, ok := p.(StepProvider)
	if !ok {
		return finishStep(v1alpha1.RunOutcomeFailed, fmt.Sprintf("This run could not start: cadre-runner does not run ModelProvider type %q a step at a time.", s.claim.Agent.Model.Type))
	}
	setup := Setup{Model: *s.claim.Agent.Model, Effort: s.claim.Agent.Effort, Credential: credential}
	switch manager.StepKind(s.claim.StepKind) {
	case manager.StepStart:
		return s.start(ctx, sp, setup)
	case manager.StepLLMTurn:
		return s.turn(ctx, sp, setup, entries)
	case manager.StepExecTools:
		return s.tools(ctx, entries[len(entries)-1])
	case manager.StepFinish:
		summary := strings.TrimSpace(entries[len(entries)-1].Text)
		if summary == "" {
			summary = "The agent finished without a summary."
		}
		return finishStep(v1alpha1.RunOutcomeReview, summary)
	}
	return finishStep(v1alpha1.RunOutcomeFailed, fmt.Sprintf("This run failed: cadre-runner does not know steps of kind %q.", s.claim.StepKind))
}

// start describes the model and records the conversation's opening.
func (s *step) start(ctx context.Context, p StepProvider, setup Setup) outcome {
	sess, err := connect(ctx, s.claim.MCP, s.o.Transport, s.o.CallTimeout)
	if err != nil {
		return outcome{release: "the run's MCP endpoint could not be reached"}
	}
	defer func() { _ = sess.Close() }()
	agent := s.claim.Agent
	err = s.retry(ctx, func() error {
		tools, err := sess.tools(ctx)
		if err != nil {
			return err
		}
		thread, err := sess.thread(ctx)
		if err != nil {
			return err
		}
		files, err := sess.files(ctx)
		if err != nil {
			return err
		}
		setup.System, setup.Tools = systemPrompt(agent), tools
		setup.Prompt = func(window int64) string { return firstPrompt(s.claim.Task, thread, files, promptBudget(window)) }
		return nil
	})
	switch {
	case ctx.Err() != nil, errors.Is(err, errRunGone):
		return outcome{}
	case err != nil:
		return outcome{release: "the run's MCP endpoint could not serve the task"}
	}
	var o *Opening
	if err := s.retry(ctx, func() (err error) { o, err = p.Begin(ctx, setup); return err }); err != nil {
		return s.failed(ctx, "This run could not start the model", err)
	}
	opening, err := json.Marshal(o)
	if err != nil {
		return finishStep(v1alpha1.RunOutcomeFailed, "This run could not start: its opening could not be recorded.")
	}
	return outcome{entry: &manager.TranscriptEntry{
		Kind: manager.StepStart, Provider: string(o.Provider), Model: o.Model, SDK: o.SDK, Effort: string(agent.Effort), Request: opening,
	}}
}

// turn makes one model call on the conversation rebuilt from the
// transcript.
func (s *step) turn(ctx context.Context, p StepProvider, setup Setup, entries []manager.TranscriptEntry) outcome {
	var o Opening
	if err := json.Unmarshal(entries[0].Request, &o); err != nil {
		return finishStep(v1alpha1.RunOutcomeFailed, "This run failed: its transcript's opening could not be read.")
	}
	steps := make([]Recorded, 0, len(entries)-1)
	turns := 0
	var last *manager.TranscriptEntry
	for i := range entries[1:] {
		e := &entries[i+1]
		switch e.Kind {
		case manager.StepLLMTurn:
			rec := Recorded{Kind: RecordedTurn, Messages: e.Messages}
			if len(e.Settings) > 0 {
				rec.Settings = new(Settings)
				if err := json.Unmarshal(e.Settings, rec.Settings); err != nil {
					return finishStep(v1alpha1.RunOutcomeFailed, fmt.Sprintf("This run failed: its transcript's step %d could not be read.", i+1))
				}
			}
			steps, turns, last = append(steps, rec), turns+1, e
		case manager.StepExecTools:
			var results []ToolResult
			if err := json.Unmarshal(e.Results, &results); err != nil && len(e.Results) > 0 {
				return finishStep(v1alpha1.RunOutcomeFailed, fmt.Sprintf("This run failed: its transcript's step %d could not be read.", i+1))
			}
			steps = append(steps, ResultsRecord(results))
		}
	}
	if turns >= s.o.MaxTurns {
		return finishStep(v1alpha1.RunOutcomeReview, fmt.Sprintf("This run stopped after %d model turns, its limit. A new run starts again from the thread.", s.o.MaxTurns))
	}
	if limit := o.Settings.ContextLimit; last != nil && limit > 0 && float64(last.ContextTokens) >= contextCeiling*float64(limit) {
		return finishStep(v1alpha1.RunOutcomeReview, fmt.Sprintf("This run stopped at %d of the model's %d context tokens, before running out. A new run starts again from the thread.", last.ContextTokens, limit))
	}
	conv, err := p.Resume(ctx, setup, &o, steps)
	if err != nil {
		return s.failed(ctx, "This run could not resume its conversation", err)
	}
	var reply *Reply
	if err := s.retry(ctx, func() (err error) { reply, err = conv.Turn(ctx); return err }); err != nil {
		return s.failed(ctx, "The model call failed", err)
	}
	s.event(ctx, reply, turns+1)
	switch reply.Stop {
	case StopRefusal:
		why := "The model declined to continue"
		if reply.RefusalCategory != "" {
			why += " (category: " + reply.RefusalCategory + ")"
		}
		return finishStep(v1alpha1.RunOutcomeFailed, why+".")
	case StopMaxTokens:
		return finishStep(v1alpha1.RunOutcomeFailed, "The model's reply was cut off at its output limit, so the run stopped.")
	case StopPaused:
		return finishStep(v1alpha1.RunOutcomeReview, "This run stopped because the model's turn was paused too often. A new run starts again from the thread.")
	case StopContextFull:
		return finishStep(v1alpha1.RunOutcomeReview, "This run stopped because the model's context window is full. A new run starts again from the thread.")
	}
	rec := conv.LastTurn()
	usage, _ := json.Marshal(reply.Usage)
	e := &manager.TranscriptEntry{
		Kind: manager.StepLLMTurn, Messages: rec.Messages, Text: reply.Text, Stop: string(reply.Stop),
		Usage: usage, ContextTokens: reply.ContextTokens,
	}
	if rec.Settings != nil {
		e.Settings, _ = json.Marshal(rec.Settings)
	}
	for _, c := range reply.Calls {
		e.Calls = append(e.Calls, manager.StepCall{ID: c.ID, Name: c.Name, Input: c.Input})
	}
	return outcome{entry: e}
}

// tools makes the calls the last turn asked for, in order, each with the
// model's call id.
func (s *step) tools(ctx context.Context, turn manager.TranscriptEntry) outcome {
	sess, err := connect(ctx, s.claim.MCP, s.o.Transport, s.o.CallTimeout)
	if err != nil {
		return outcome{release: "the run's MCP endpoint could not be reached"}
	}
	defer func() { _ = sess.Close() }()
	sess.callIDs = true
	results := make([]ToolResult, 0, len(turn.Calls))
	for _, c := range turn.Calls {
		res, err := sess.call(ctx, ToolCall{ID: c.ID, Name: c.Name, Input: c.Input})
		switch {
		case errors.Is(err, errTransport):
			// The call may have taken effect with only its answer lost;
			// the next worker makes the step's calls again, which the core
			// tools record once.
			return outcome{release: "a tool call got no answer from the run's MCP endpoint"}
		case err != nil, res.runEnded:
			return outcome{}
		}
		results = append(results, res.ToolResult)
	}
	data, err := json.Marshal(results)
	if err != nil {
		return finishStep(v1alpha1.RunOutcomeFailed, "This run failed: its tool results could not be recorded.")
	}
	return outcome{entry: &manager.TranscriptEntry{Kind: manager.StepExecTools, Results: data}}
}

// failed finishes the run Failed when retrying cannot help, and gives the
// step back for another attempt when it may.
func (s *step) failed(ctx context.Context, what string, err error) outcome {
	if ctx.Err() != nil {
		return outcome{}
	}
	s.o.Log.Error(what, "task", s.task.Namespace+"/"+s.task.Name, "run", s.claim.RunID, "step", s.ref.Step, "err", err)
	if !isPermanent(err) {
		return outcome{release: strings.ToLower(what[:1]) + what[1:] + ", and kept failing"}
	}
	return finishStep(v1alpha1.RunOutcomeFailed, fmt.Sprintf("%s; check the agent's ModelProvider and its credential. cadre-runner's log has the details for run %s.", what, s.claim.RunID))
}

// retry is run.retry for a step.
func (s *step) retry(ctx context.Context, f func() error) error {
	return (&run{o: s.o}).retry(ctx, f)
}

// event reports one model response; the step number keeps events of
// different workers apart. It is best effort.
func (s *step) event(ctx context.Context, reply *Reply, turn int) {
	r := &run{o: s.o, c: s.c, claim: s.claim, ref: s.task, seq: s.ref.Step}
	r.event(ctx, reply, turn)
}

// finish ends the run at the step, with its comment in the same write.
func (s *step) finish(ctx context.Context, end outcome) (*runner.FinishResponse, error) {
	r := &run{o: s.o, c: s.c, claim: s.claim, ref: s.task}
	return r.finish(ctx, runner.FinishRequest{
		Task: s.task, RunID: s.claim.RunID, Step: s.ref.Step, Attempt: s.ref.Attempt, Outcome: end.result, Comment: end.comment,
	})
}

// heartbeat renews the step's lease until ctx ends. The worker may act only
// while its step is known to be held: a step it no longer holds, or a
// lease that ran out without a renewal getting through, cancels it.
func (s *step) heartbeat(ctx context.Context, cancel context.CancelCauseFunc, claimSent time.Time) {
	lease := time.Duration(s.claim.LeaseSeconds) * time.Second
	held := claimSent.Add(lease)
	every := time.Duration(s.claim.HeartbeatSeconds) * time.Second
	if every <= 0 {
		every = max(lease/3, time.Second)
	}
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		sent := time.Now()
		hbCtx, stop := context.WithDeadline(ctx, held)
		hb, err := s.c.Heartbeat(hbCtx, runner.HeartbeatRequest{Task: s.task, RunID: s.claim.RunID, Step: s.ref.Step, Attempt: s.ref.Attempt})
		stop()
		switch {
		case ctx.Err() != nil:
			return
		case err == nil:
			held = sent.Add(time.Duration(hb.LeaseSeconds) * time.Second)
		case lostStep(err):
			cancel(errLost)
			return
		case !time.Now().Before(held):
			cancel(fmt.Errorf("%w: no renewal got through before the lease ran out: %w", errLost, err))
			return
		}
	}
}

// lostStep reports whether err means the step is no longer this worker's:
// the run lost its task, or moved past the step, or the step was freed or
// taken again.
func lostStep(err error) bool {
	return lostTask(err) || client.IsCode(err, httpjson.CodeStepMismatch) || client.IsCode(err, httpjson.CodeAttemptMismatch)
}
