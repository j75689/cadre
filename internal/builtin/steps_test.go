package builtin_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	"github.com/j75689/cadre/internal/builtin"
	"github.com/j75689/cadre/internal/client"
	"github.com/j75689/cadre/internal/manager"
	"github.com/j75689/cadre/internal/runner"
)

// stepScripted is a StepProvider whose model follows a script; a resumed
// conversation finds its place in it from the turns recorded so far.
type stepScripted struct {
	mu    sync.Mutex
	steps []step
	calls int // model calls made
	setup builtin.Setup
}

func (p *stepScripted) Start(context.Context, builtin.Setup) (builtin.Conversation, error) {
	return nil, errors.New("a step-mode run is never started whole")
}

func (p *stepScripted) Begin(_ context.Context, s builtin.Setup) (*builtin.Opening, error) {
	p.mu.Lock()
	p.setup = s
	p.mu.Unlock()
	return &builtin.Opening{Provider: v1alpha1.ProviderAnthropic, Model: s.Model.Model, System: s.System, Prompt: s.Prompt(0), Tools: s.Tools}, nil
}

func (p *stepScripted) Resume(_ context.Context, _ builtin.Setup, _ *builtin.Opening, steps []builtin.Recorded) (builtin.StepConversation, error) {
	c := &stepConversation{p: p}
	for _, st := range steps {
		switch st.Kind {
		case builtin.RecordedTurn:
			c.turn++
		case builtin.RecordedResults:
			c.last = st.Results
		}
	}
	return c, nil
}

type stepConversation struct {
	p    *stepScripted
	turn int
	last []builtin.ToolResult
}

func (c *stepConversation) Turn(ctx context.Context) (*builtin.Reply, error) {
	c.p.mu.Lock()
	c.p.calls++
	if c.turn >= len(c.p.steps) {
		c.p.mu.Unlock()
		return &builtin.Reply{Stop: builtin.StopEndTurn, Text: "out of script"}, nil
	}
	s := c.p.steps[c.turn]
	c.p.mu.Unlock()
	return s(ctx, c.last)
}

func (c *stepConversation) LastTurn() builtin.Recorded {
	msg, _ := json.Marshal(map[string]int{"turn": c.turn})
	return builtin.Recorded{Kind: builtin.RecordedTurn, Messages: []json.RawMessage{msg}}
}

func (c *stepConversation) AddResults([]builtin.ToolResult) {}
func (c *stepConversation) ContextLimit() int64             { return 0 }

func (p *stepScripted) modelCalls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

// steps does steps with one worker after another, each doing one step,
// until the run is over or n steps were taken.
func (e *env) steps(t *testing.T, p builtin.Provider, o builtin.Options, n int) {
	t.Helper()
	e.stepsWith(t, e.c, p, o, n)
}

// stepsWith is steps, the workers talking to the server through c.
func (e *env) stepsWith(t *testing.T, c *client.Client, p builtin.Provider, o builtin.Options, n int) {
	t.Helper()
	o.Once, o.Poll = true, 10*time.Millisecond
	o.Providers = map[v1alpha1.ProviderType]builtin.Provider{v1alpha1.ProviderAnthropic: p}
	if o.RetryFor == 0 {
		o.RetryFor = 3 * time.Second
	}
	for i := range n {
		if e.task(t).Status.Phase != v1alpha1.PhaseRunning && i > 0 {
			return
		}
		o.Runner = "worker-" + string(rune('a'+i%26))
		stepCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
		err := builtin.RunSteps(stepCtx, c, o)
		cancel()
		if err != nil {
			t.Fatal(err)
		}
	}
}

func (e *env) transcript(t *testing.T) []manager.TranscriptEntry {
	t.Helper()
	tk := e.task(t)
	runID := tk.Status.Runs.LastID
	entries, err := e.m.Transcript(ctx, key, runID)
	if err != nil {
		t.Fatal(err)
	}
	return entries
}

// A step-mode run is done a step at a time, each step by another worker:
// the opening, a turn, its calls, a turn without calls, and the finish with
// the agent's summary.
func TestARunDoneStepByStep(t *testing.T) {
	e := startIn(t, 2*time.Second, v1alpha1.RunModeStep)
	p := &stepScripted{steps: []step{calls(tc("tu_1", "thread_post", map[string]string{"body": "Looking at the clock mock."})), say("Fixed: the mock was off by a day.")}}
	e.steps(t, p, builtin.Options{}, 10)

	if tk := e.task(t); tk.Status.Phase != v1alpha1.PhaseReview {
		t.Fatalf("phase %s, want Review", tk.Status.Phase)
	}
	var kinds []string
	for _, en := range e.transcript(t) {
		kinds = append(kinds, string(en.Kind))
	}
	if strings.Join(kinds, ",") != "start,llm_turn,exec_tools,llm_turn" {
		t.Fatalf("transcript kinds %v", kinds)
	}
	thread := e.thread(t)
	var posted int
	for _, en := range thread {
		if en.Body == "Looking at the clock mock." {
			posted++
		}
	}
	if posted != 1 || last(thread).Body != "Fixed: the mock was off by a day." || last(thread).Author.Kind != manager.AuthorAgent {
		t.Fatalf("thread %+v; want the post once and the summary last, by the agent", thread)
	}
	if p.modelCalls() != 2 || !strings.Contains(p.setup.Prompt(0), "Fix the flaky login test") {
		t.Fatalf("%d model calls, prompt %q", p.modelCalls(), p.setup.Prompt(0))
	}
}

// dying is a worker's connection to cadre-server that goes dead once the
// worker has written a turn's entry: the step is neither committed nor
// given back, as when the worker dies there.
type dying struct {
	mu          sync.Mutex
	armed, died bool
}

func (d *dying) RoundTrip(r *http.Request) (*http.Response, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	switch {
	case r.URL.Path == runner.PathStepAppend && r.Body != nil:
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(body))
		d.armed = d.armed || bytes.Contains(body, []byte(`"kind":"llm_turn"`))
	case d.armed && (r.URL.Path == runner.PathStepAdvance || r.URL.Path == runner.PathStepRelease):
		// The worker is gone: it hears nothing back, and sends nothing on.
		d.died = true
		return &http.Response{StatusCode: http.StatusConflict, Header: http.Header{"Content-Type": {"application/json"}},
			Body: io.NopCloser(strings.NewReader(`{"error":{"code":"attempt_mismatch","message":"gone"}}`)), Request: r}, nil
	}
	return http.DefaultTransport.RoundTrip(r)
}

// A worker that dies after writing its step's entry, before committing it,
// leaves the step to the next worker, which commits that entry without
// calling the model again.
func TestADeadWorkersStepIsAdopted(t *testing.T) {
	e := startIn(t, 2*time.Second, v1alpha1.RunModeStep)
	p := &stepScripted{steps: []step{say("Done.")}}
	d := &dying{}
	e.stepsWith(t, client.New(e.url, &http.Client{Transport: d}), p, builtin.Options{}, 2) // start, then the turn whose worker dies
	if !d.died || p.modelCalls() != 1 {
		t.Fatalf("died %v, %d model calls", d.died, p.modelCalls())
	}
	if r := e.task(t).Status.Run; r == nil || r.StepFree || r.Step != 1 {
		t.Fatalf("run %+v; want step 1 still held by the dead worker", r)
	}
	time.Sleep(3500 * time.Millisecond) // the dead worker's lease and grace run out
	if err := e.m.ReclaimExpired(ctx); err != nil {
		t.Fatal(err)
	}
	e.steps(t, p, builtin.Options{}, 3)
	if tk := e.task(t); tk.Status.Phase != v1alpha1.PhaseReview || last(e.thread(t)).Body != "Done." {
		t.Fatalf("phase %s, thread %+v", tk.Status.Phase, e.thread(t))
	}
	if p.modelCalls() != 1 {
		t.Fatalf("%d model calls; the adopted step called the model again", p.modelCalls())
	}
}

// A model call that keeps failing gives the step back, its attempt
// counted; one that cannot pass finishes the run Failed at once, with a
// comment, burning no attempt.
func TestFailingModelCalls(t *testing.T) {
	transient := func(context.Context, []builtin.ToolResult) (*builtin.Reply, error) {
		return nil, errors.New("overloaded")
	}
	e := startIn(t, 2*time.Second, v1alpha1.RunModeStep)
	e.steps(t, &stepScripted{steps: []step{transient}}, builtin.Options{RetryFor: 50 * time.Millisecond, Backoff: time.Millisecond}, 2)
	r := e.task(t).Status.Run
	if r == nil || !r.StepFree || r.Step != 1 || r.Attempt != 1 {
		t.Fatalf("run %+v; want step 1 given back after one attempt", r)
	}

	permanent := func(context.Context, []builtin.ToolResult) (*builtin.Reply, error) {
		return nil, builtin.Permanent(errors.New("invalid api key"))
	}
	e.steps(t, &stepScripted{steps: []step{permanent}}, builtin.Options{}, 1)
	if tk := e.task(t); tk.Status.Phase != v1alpha1.PhaseFailed || !strings.Contains(last(e.thread(t)).Body, "credential") {
		t.Fatalf("phase %s, thread %+v", tk.Status.Phase, e.thread(t))
	}
}

// The turn limit holds across workers.
func TestTheTurnLimitInStepMode(t *testing.T) {
	e := startIn(t, 2*time.Second, v1alpha1.RunModeStep)
	p := &stepScripted{steps: []step{calls(tc("tu_1", "thread_read", map[string]any{})), calls(tc("tu_2", "thread_read", map[string]any{}))}}
	e.steps(t, p, builtin.Options{MaxTurns: 1}, 10)
	if tk := e.task(t); tk.Status.Phase != v1alpha1.PhaseReview || !strings.Contains(last(e.thread(t)).Body, "after 1 model turns") || p.modelCalls() != 1 {
		t.Fatalf("phase %s, %d model calls, thread %+v", tk.Status.Phase, p.modelCalls(), e.thread(t))
	}
}

// answering answers the requests to path itself, with status and body.
type answering struct {
	path   string
	status int
	body   string
}

func (a answering) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Path != a.path {
		return http.DefaultTransport.RoundTrip(r)
	}
	return &http.Response{StatusCode: a.status, Header: http.Header{"Content-Type": {"application/json"}},
		Body: io.NopCloser(strings.NewReader(a.body)), Request: r}, nil
}

// An entry the server refuses would be refused from every worker: the run
// fails at once, saying so, rather than after three silent attempts.
func TestARefusedEntryFailsTheRun(t *testing.T) {
	e := startIn(t, 2*time.Second, v1alpha1.RunModeStep)
	refusing := client.New(e.url, &http.Client{Transport: answering{runner.PathStepAppend, http.StatusRequestEntityTooLarge, `{"error":{"code":"invalid","message":"request body too large"}}`}})
	e.stepsWith(t, refusing, &stepScripted{}, builtin.Options{}, 1)
	if tk := e.task(t); tk.Status.Phase != v1alpha1.PhaseFailed || !strings.Contains(last(e.thread(t)).Body, "refused step 0's entry") {
		t.Fatalf("phase %s, thread %+v", tk.Status.Phase, e.thread(t))
	}
}

// A transcript read short of the step (a store replica behind) gives the
// step back instead of reading past its end.
func TestAShortTranscriptGivesTheStepBack(t *testing.T) {
	e := startIn(t, 2*time.Second, v1alpha1.RunModeStep)
	p := &stepScripted{steps: []step{say("Done.")}}
	e.steps(t, p, builtin.Options{}, 1) // start
	short := client.New(e.url, &http.Client{Transport: answering{runner.PathStepTranscript, http.StatusOK, `{"entries":[]}`}})
	e.stepsWith(t, short, p, builtin.Options{}, 1)
	if r := e.task(t).Status.Run; r == nil || !r.StepFree || r.Step != 1 || r.Attempt != 1 || p.modelCalls() != 0 {
		t.Fatalf("run %+v, %d model calls; want step 1 given back unmade", r, p.modelCalls())
	}
}
