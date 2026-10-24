// Package builtin is Cadre's own agent loop, run by cadre-runner in runtime
// mode: it claims builtin agents' tasks, talks to the agent's model through
// a Provider, and acts only through the run's MCP endpoint, so every tool
// call is fenced on the run Cadre-side.
//
// A run's conversation lives in memory and is append-only. It starts from
// the task and its thread, read with thread_read; nothing else carries over
// from earlier runs.
package builtin

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"strings"
	"time"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	"github.com/j75689/cadre/internal/client"
	"github.com/j75689/cadre/internal/httpjson"
	"github.com/j75689/cadre/internal/runner"
)

// Secrets reads a ModelProvider's credential, fresh for every run.
type Secrets interface {
	Value(ctx context.Context, namespace string, ref v1alpha1.SecretKeyRef) (string, error)
}

// Options configure the loop.
type Options struct {
	Runner     string
	Namespaces []string
	// Labels are sent with every claim, with cadre.dev/runtime: builtin
	// added.
	Labels    map[string]string
	Providers map[v1alpha1.ProviderType]Provider
	Secrets   Secrets
	Log       *slog.Logger

	Poll     time.Duration // between claims when there is no work; default 1s
	Once     bool          // stop after one run
	MaxTurns int           // model responses per run; default 100
	// RetryFor bounds how long a failing model call or start-up read is
	// retried before the run fails; default 5 minutes.
	RetryFor time.Duration
	// Transport carries the MCP requests; default http.DefaultTransport.
	Transport http.RoundTripper
	// Backoff is the first wait before claiming again after a failed
	// claim, or a run abandoned because its MCP endpoint did not answer; it
	// doubles with each in a row, up to MaxBackoff. Defaults 5s and 5
	// minutes.
	Backoff, MaxBackoff time.Duration
	// CallTimeout bounds one request to the run's MCP endpoint; default a
	// little over the server's bound for it.
	CallTimeout time.Duration
}

// Defaults.
const (
	DefaultMaxTurns    = 100
	DefaultRetryFor    = 5 * time.Minute
	DefaultCallTimeout = 105 * time.Second // cadre-server bounds an MCP request at 90s
	// contextCeiling is the share of the model's window at which a run
	// stops and hands the task to a human.
	contextCeiling = 0.8
)

// Run claims and works builtin agents' tasks until ctx is done, or after one
// run with Once.
func Run(ctx context.Context, c *client.Client, o Options) error {
	if o.Poll <= 0 {
		o.Poll = time.Second
	}
	if o.MaxTurns <= 0 {
		o.MaxTurns = DefaultMaxTurns
	}
	if o.RetryFor <= 0 {
		o.RetryFor = DefaultRetryFor
	}
	if o.Backoff <= 0 {
		o.Backoff = 5 * time.Second
	}
	if o.MaxBackoff < o.Backoff {
		o.MaxBackoff = max(5*time.Minute, o.Backoff)
	}
	if o.CallTimeout <= 0 {
		o.CallTimeout = DefaultCallTimeout
	}
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	labels := maps.Clone(o.Labels)
	if labels == nil {
		labels = map[string]string{}
	}
	labels[v1alpha1.LabelRuntime] = string(v1alpha1.RuntimeBuiltin)
	// A failed claim, and a run abandoned because its MCP endpoint did not
	// answer (it still holds a Team slot until its lease runs out), make
	// the next claim wait, longer with each in a row: a broken server or
	// endpoint cannot turn into a tight loop of claims.
	backoff := time.Duration(0)
	wait := func(d time.Duration) bool {
		select {
		case <-ctx.Done():
			return false
		case <-time.After(d):
			return true
		}
	}
	grow := func() time.Duration {
		backoff = min(max(2*backoff, o.Backoff), o.MaxBackoff)
		return backoff
	}
	for {
		sent := time.Now()
		claim, err := c.Claim(ctx, runner.ClaimRequest{Runner: o.Runner, Namespaces: o.Namespaces, Labels: labels})
		switch {
		case ctx.Err() != nil:
			return nil
		case err != nil:
			d := grow()
			o.Log.Warn("claim failed; waiting before the next", "err", err, "wait", d)
			if !wait(d) {
				return nil
			}
			continue
		case claim == nil:
			backoff = 0
			if !wait(o.Poll) {
				return nil
			}
			continue
		}
		abandoned := (&run{o: o, c: c, claim: claim, ref: runner.TaskRef{Namespace: claim.Task.Namespace, Name: claim.Task.Name}}).work(ctx, sent)
		if o.Once || ctx.Err() != nil {
			return nil
		}
		if !abandoned {
			backoff = 0
			continue
		}
		d := grow()
		o.Log.Warn("waiting before the next claim", "wait", d)
		if !wait(d) {
			return nil
		}
	}
}

// run is one claimed run.
type run struct {
	o     Options
	c     *client.Client
	claim *runner.ClaimResponse
	ref   runner.TaskRef
	seq   int64 // the last event sent
}

// errLost ends a run whose task is no longer its own, or whose lease can no
// longer be known to be held.
var errLost = errors.New("the run lost its task")

// ending is how a run ends: finished with an outcome and a comment, or not
// finished at all because the server ended it or it lost its task, or
// abandoned to its lease because its MCP endpoint stopped answering.
type ending struct {
	finish    bool
	abandoned bool
	outcome   v1alpha1.RunOutcome
	comment   string
}

// abandon leaves the run to its lease: without its endpoint it can neither
// act nor comment, and the outage is not the task's fault.
func (r *run) abandon(err error) ending {
	r.o.Log.Error("the run's MCP endpoint stopped answering; leaving the task to its lease",
		"task", r.ref.Namespace+"/"+r.ref.Name, "run", r.claim.RunID, "err", err)
	return ending{abandoned: true}
}

func finishWith(outcome v1alpha1.RunOutcome, comment string) ending {
	return ending{finish: true, outcome: outcome, comment: comment}
}

// work runs the claimed task. It reports whether the run was abandoned
// because its MCP endpoint could not be reached.
func (r *run) work(ctx context.Context, claimSent time.Time) (abandoned bool) {
	log := r.o.Log.With("task", r.ref.Namespace+"/"+r.ref.Name, "run", r.claim.RunID)
	log.Info("claimed", "number", r.claim.Task.Number)
	runCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	beating := make(chan struct{})
	go func() { defer close(beating); r.heartbeat(runCtx, cancel, claimSent) }()
	defer func() { cancel(nil); <-beating }()

	s, err := connect(runCtx, r.claim.MCP, r.o.Transport, r.o.CallTimeout)
	if err != nil {
		// Without the endpoint the run can neither read the task nor
		// comment: leave it to the lease, which hands it out again. A 404
		// here is more likely a misconfigured endpoint than a run cancelled
		// in the moment since its claim, so it backs off too.
		log.Error("the run's MCP endpoint could not be reached; leaving the task to its lease", "err", err)
		return runCtx.Err() == nil
	}
	defer func() { _ = s.Close() }()

	end := r.drive(runCtx, s)
	switch {
	case runCtx.Err() != nil:
		log.Info("run stopped", "why", context.Cause(runCtx))
		return false
	case end.abandoned:
		return true
	case !end.finish:
		log.Info("run ended by the server")
		return false
	}
	// The comment says why the run ended; it is retried like the finish,
	// and the run finishes after it either way.
	if end.comment != "" {
		if err := retryShort(runCtx, func() error { return s.post(runCtx, end.comment) }); err != nil {
			log.Warn("post the run's last comment", "err", err)
		}
	}
	fin, err := r.finish(runCtx, runner.FinishRequest{Task: r.ref, RunID: r.claim.RunID, Outcome: end.outcome})
	if err != nil {
		log.Warn("finish", "outcome", end.outcome, "err", err)
		return false
	}
	log.Info("finished", "outcome", end.outcome, "phase", fin.Phase)
	return false
}

// drive runs the agent loop and says how the run ends.
func (r *run) drive(ctx context.Context, s *session) ending {
	agent := r.claim.Agent
	if agent == nil || agent.Model == nil {
		return finishWith(v1alpha1.RunOutcomeFailed, "This run could not start: the claim names no model for the agent.")
	}
	p, ok := r.o.Providers[agent.Model.Type]
	if !ok {
		return finishWith(v1alpha1.RunOutcomeFailed, fmt.Sprintf("This run could not start: cadre-runner does not support ModelProvider type %q yet.", agent.Model.Type))
	}
	var credential string
	if ref := agent.Model.SecretRef; ref != nil {
		if r.o.Secrets == nil {
			return finishWith(v1alpha1.RunOutcomeFailed, "This run could not start: cadre-runner cannot read Secrets.")
		}
		v, err := r.o.Secrets.Value(ctx, r.ref.Namespace, *ref)
		if err != nil {
			r.o.Log.Warn("read the model's credential", "secret", ref.Name, "key", ref.Key, "err", err)
			return finishWith(v1alpha1.RunOutcomeFailed, fmt.Sprintf("This run could not start: the model's credential, key %s of Secret %s, could not be read.", ref.Key, ref.Name))
		}
		credential = strings.TrimRight(v, "\r\n")
	}

	var setup Setup
	err := r.retry(ctx, func() error {
		tools, err := s.tools(ctx)
		if err != nil {
			return err
		}
		thread, err := s.thread(ctx)
		if err != nil {
			return err
		}
		files, err := s.files(ctx)
		if err != nil {
			return err
		}
		setup = Setup{
			Model: *agent.Model, Effort: agent.Effort, Credential: credential, System: systemPrompt(agent), Tools: tools,
			Prompt: func(window int64) string { return firstPrompt(r.claim.Task, thread, files, promptBudget(window)) },
		}
		return nil
	})
	switch {
	case ctx.Err() != nil, errors.Is(err, errRunGone):
		return ending{}
	case err != nil:
		// The run's endpoint serves these reads; when it cannot, the
		// outage is not the task's.
		return r.abandon(err)
	}
	// Start reads the model's description; a transient failure of it is
	// retried like the other start-up reads.
	var conv Conversation
	if err := r.retry(ctx, func() (err error) { conv, err = p.Start(ctx, setup); return err }); err != nil {
		return r.failed(ctx, "This run could not start the model", err)
	}

	for turn := 1; turn <= r.o.MaxTurns; turn++ {
		var reply *Reply
		if err := r.retry(ctx, func() (err error) { reply, err = conv.Turn(ctx); return err }); err != nil {
			return r.failed(ctx, "The model call failed", err)
		}
		r.event(ctx, reply, turn)
		switch reply.Stop {
		case StopRefusal:
			why := "The model declined to continue"
			if reply.RefusalCategory != "" {
				why += " (category: " + reply.RefusalCategory + ")"
			}
			return finishWith(v1alpha1.RunOutcomeFailed, why+".")
		case StopMaxTokens:
			return finishWith(v1alpha1.RunOutcomeFailed, "The model's reply was cut off at its output limit, so the run stopped.")
		case StopPaused:
			return finishWith(v1alpha1.RunOutcomeReview, "This run stopped because the model's turn was paused too often. A new run starts again from the thread.")
		case StopContextFull:
			return finishWith(v1alpha1.RunOutcomeReview, "This run stopped because the model's context window is full. A new run starts again from the thread.")
		}
		if len(reply.Calls) == 0 {
			summary := strings.TrimSpace(reply.Text)
			if summary == "" {
				summary = "The agent finished without a summary."
			}
			return finishWith(v1alpha1.RunOutcomeReview, summary)
		}
		// Calls run one at a time, in order, so a call that ends the run
		// is the last one made.
		results := make([]ToolResult, 0, len(reply.Calls))
		for _, call := range reply.Calls {
			res, err := s.call(ctx, call)
			if errors.Is(err, errTransport) {
				// Not retried: the call may have taken effect with only
				// its answer lost.
				return r.abandon(err)
			}
			if err != nil {
				return ending{}
			}
			if res.runEnded {
				return ending{}
			}
			results = append(results, res.ToolResult)
		}
		conv.AddResults(results)
		if limit := conv.ContextLimit(); limit > 0 && float64(reply.ContextTokens) >= contextCeiling*float64(limit) {
			return finishWith(v1alpha1.RunOutcomeReview, fmt.Sprintf("This run stopped at %d of the model's %d context tokens, before running out. A new run starts again from the thread.", reply.ContextTokens, limit))
		}
	}
	return finishWith(v1alpha1.RunOutcomeReview, fmt.Sprintf("This run stopped after %d model turns, its limit. A new run starts again from the thread.", r.o.MaxTurns))
}

// failed ends the run Failed. The thread is read by everyone on the board,
// so it gets one sentence; the error, which can carry request URLs, ids
// and response bodies, goes to the log only.
func (r *run) failed(ctx context.Context, what string, err error) ending {
	if ctx.Err() != nil {
		return ending{}
	}
	r.o.Log.Error(what, "task", r.ref.Namespace+"/"+r.ref.Name, "run", r.claim.RunID, "err", err)
	hint := "it may pass on a new run"
	if isPermanent(err) {
		hint = "check the agent's ModelProvider and its credential"
	}
	return finishWith(v1alpha1.RunOutcomeFailed, fmt.Sprintf("%s; %s. cadre-runner's log has the details for run %s.", what, hint, r.claim.RunID))
}

// retry retries f with backoff, for at most RetryFor, unless its error is
// Permanent or the run is over.
func (r *run) retry(ctx context.Context, f func() error) error {
	deadline := time.Now().Add(r.o.RetryFor)
	wait := time.Second
	for {
		err := f()
		if err == nil || isPermanent(err) || errors.Is(err, errRunGone) || ctx.Err() != nil || time.Now().Add(wait).After(deadline) {
			return err
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(wait):
		}
		wait = min(2*wait, 30*time.Second)
	}
}

// event reports one model response. It is best effort.
func (r *run) event(ctx context.Context, reply *Reply, turn int) {
	names := make([]string, 0, len(reply.Calls))
	for _, c := range reply.Calls {
		names = append(names, c.Name)
	}
	progress := fmt.Sprintf("turn %d: %s", turn, reply.Stop)
	if len(names) > 0 {
		progress = fmt.Sprintf("turn %d: %s", turn, strings.Join(names, ", "))
	}
	r.seq++
	err := r.c.Event(ctx, runner.EventsRequest{
		Task: r.ref, RunID: r.claim.RunID, Seq: r.seq, Usage: reply.Usage, ContextTokens: reply.ContextTokens, Progress: progress,
	})
	if err != nil && ctx.Err() == nil {
		r.o.Log.Warn("report progress", "err", err)
	}
}

// heartbeat renews the lease until ctx ends. The run may act only while
// its lease is known to be held: a lost task, or a lease that ran out
// without a renewal getting through, cancels it.
func (r *run) heartbeat(ctx context.Context, cancel context.CancelCauseFunc, claimSent time.Time) {
	lease := time.Duration(r.claim.LeaseSeconds) * time.Second
	held := claimSent.Add(lease)
	every := time.Duration(r.claim.HeartbeatSeconds) * time.Second
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
		hb, err := r.c.Heartbeat(hbCtx, runner.HeartbeatRequest{Task: r.ref, RunID: r.claim.RunID})
		stop()
		switch {
		case ctx.Err() != nil:
			return
		case err == nil:
			held = sent.Add(time.Duration(hb.LeaseSeconds) * time.Second)
		case lostTask(err):
			cancel(errLost)
			return
		case !time.Now().Before(held):
			cancel(fmt.Errorf("%w: no renewal got through before the lease ran out: %w", errLost, err))
			return
		}
	}
}

func lostTask(err error) bool {
	return client.IsCode(err, httpjson.CodeRunMismatch) || client.IsCode(err, httpjson.CodeNotFound)
}

// retryShort retries f a few times over a few seconds, for the writes that
// end a run.
func retryShort(ctx context.Context, f func() error) error {
	var err error
	for attempt := range 8 {
		if err = f(); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(time.Duration(attempt+1) * 200 * time.Millisecond):
		}
	}
	return err
}

// finish retries transient failures; a retry of a finish that landed is
// answered AlreadyFinished, so retrying is safe.
func (r *run) finish(ctx context.Context, req runner.FinishRequest) (*runner.FinishResponse, error) {
	var err error
	for attempt := range 8 {
		var fin *runner.FinishResponse
		fin, err = r.c.Finish(ctx, req)
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
