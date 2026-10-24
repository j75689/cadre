package chaos_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	"github.com/j75689/cadre/internal/builtin"
	"github.com/j75689/cadre/internal/client"
	"github.com/j75689/cadre/internal/manager"
	"github.com/j75689/cadre/internal/mcp"
	"github.com/j75689/cadre/internal/runner"
	"github.com/j75689/cadre/internal/storage"
	"github.com/j75689/cadre/internal/storage/kubernetes"
	"github.com/j75689/cadre/internal/task"
)

// Step mode under chaos: step workers (builtin.RunSteps, on a fake model)
// come and go, are killed in the middle of a step, and lose their
// connection right after a write; the transcript's appends and the step
// commits get the store's faults like every other write.
const (
	stepWorkers = 3
	// stepIdle is short, so a run whose step no worker takes (they all
	// stop with the chaos) fails within the quiescence.
	stepIdle = 4 * time.Second
	// postTurns is how many turns of a step-mode run post to the thread
	// before the last one, which only answers.
	postTurns = 2
)

// stepKey names a step of a run.
type stepKey struct {
	run  string
	step int64
}

// held is a worker's belief that it held a step: from the claim's answer
// until its lease ran out, or it sent what ended the step.
type held struct {
	worker     string
	from, till time.Time
	ended      bool
}

// stepRecorder keeps what the step workers and the fake model saw.
type stepRecorder struct {
	mcpURL string
	mu     sync.Mutex
	takes  map[stepKey]int     // how often each step was taken
	kinds  map[stepKey]string  // the kind each claim said
	held   map[stepKey][]*held // in claim order
	calls  map[string][]int    // successful model calls by the run's nonce, per turn
	// tokens counts the step tokens checked dead after their commit, and
	// tokenErrs what was found alive; checks waits for the checks.
	tokens    int
	tokenErrs []string
	checks    sync.WaitGroup
	workers   sync.WaitGroup // every step worker, killed or not
	deaths    int            // workers killed around an append or a commit
	adopted   int            // entries a worker found written by an earlier one
}

func newStepRecorder(mcpBase string) *stepRecorder {
	return &stepRecorder{
		mcpURL: mcpBase + mcp.Path,
		takes:  map[stepKey]int{}, kinds: map[stepKey]string{}, held: map[stepKey][]*held{}, calls: map[string][]int{},
	}
}

// model is a fake StepProvider. Each run's opening carries a nonce, so a
// model call can be told apart by run; turn n of a run posts "post
// <nonce> <n>" to the thread, and the last turn answers only. Now and then a
// call fails, as an overloaded API does.
type model struct {
	rnd *lockedRand
	rec *stepRecorder
}

func (m *model) Start(context.Context, builtin.Setup) (builtin.Conversation, error) {
	return nil, errors.New("step mode only")
}

func (m *model) Begin(_ context.Context, s builtin.Setup) (*builtin.Opening, error) {
	nonce := strconv.FormatUint(uint64(m.rnd.IntN(1<<62)), 36)
	return &builtin.Opening{Provider: v1alpha1.ProviderBedrock, SDK: "chaos-model@" + nonce, Model: s.Model.Model, System: s.System, Prompt: s.Prompt(0), Tools: s.Tools}, nil
}

func (m *model) Resume(_ context.Context, _ builtin.Setup, o *builtin.Opening, steps []builtin.Recorded) (builtin.StepConversation, error) {
	c := &turn{m: m, nonce: strings.TrimPrefix(o.SDK, "chaos-model@")}
	for _, st := range steps {
		if st.Kind == builtin.RecordedTurn {
			c.n++
		}
	}
	return c, nil
}

type turn struct {
	m     *model
	nonce string
	n     int
}

func (c *turn) Turn(ctx context.Context) (*builtin.Reply, error) {
	if c.m.rnd.Float64() < 0.1 {
		return nil, errors.New("overloaded")
	}
	sleep(ctx, c.m.rnd.between(0, 150*time.Millisecond))
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	r := c.m.rec
	r.mu.Lock()
	for len(r.calls[c.nonce]) <= c.n {
		r.calls[c.nonce] = append(r.calls[c.nonce], 0)
	}
	r.calls[c.nonce][c.n]++
	r.mu.Unlock()
	if c.n >= postTurns {
		return &builtin.Reply{Stop: builtin.StopEndTurn, Text: "done " + c.nonce}, nil
	}
	input, _ := json.Marshal(map[string]string{"body": postBody(c.nonce, c.n)})
	return &builtin.Reply{Stop: builtin.StopToolUse, Calls: []builtin.ToolCall{{ID: fmt.Sprintf("call-%d", c.n), Name: "thread_post", Input: input}}}, nil
}

func postBody(nonce string, n int) string { return fmt.Sprintf("post %s %d", nonce, n) }

func (c *turn) LastTurn() builtin.Recorded {
	msg, _ := json.Marshal(map[string]int{"turn": c.n})
	return builtin.Recorded{Kind: builtin.RecordedTurn, Messages: []json.RawMessage{msg}}
}

func (*turn) AddResults([]builtin.ToolResult) {}
func (*turn) ContextLimit() int64             { return 0 }

// stepSlot keeps one step worker alive, killing and replacing it at random
// moments, often in the middle of a step. A worker killed around a write is
// replaced at once: what is left of it fails every request it still tries,
// and is waited for (rec.workers) before the checks.
func stepSlot(chaos context.Context, base string, rnd *lockedRand, rec *stepRecorder, name string) {
	m := &model{rnd: rnd, rec: rec}
	for chaos.Err() == nil {
		life, kill := context.WithTimeout(chaos, rnd.between(4*time.Second, 20*time.Second))
		w := &watched{worker: name, rec: rec, rnd: rnd, kill: kill, next: http.DefaultTransport}
		rec.workers.Add(1)
		go func() {
			defer rec.workers.Done()
			_ = builtin.RunSteps(life, client.New(base, &http.Client{Transport: w}), builtin.Options{
				Runner: name, Poll: 50 * time.Millisecond, Log: slog.New(slog.DiscardHandler),
				Providers: map[v1alpha1.ProviderType]builtin.Provider{v1alpha1.ProviderBedrock: m},
				// A failed model call is retried within the take; one that
				// keeps failing gives the step back.
				RetryFor: 3 * time.Second, StepTimeout: 8 * time.Second, Backoff: 100 * time.Millisecond, MaxBackoff: time.Second,
				CallTimeout: 5 * time.Second,
			})
		}()
		<-life.Done()
		kill()
		sleep(chaos, rnd.between(0, 500*time.Millisecond))
	}
}

// watched is a step worker's connection to cadre-server. It records the
// steps the worker took and how long it believed it held them, checks that
// a committed step's MCP token is dead, and now and then kills the worker
// around a transcript append or a commit: before the request goes out, or
// right after it went through. A killed worker sends nothing more.
type watched struct {
	worker string
	rec    *stepRecorder
	rnd    *lockedRand
	kill   context.CancelFunc
	next   http.RoundTripper
	dead   atomic.Bool

	mu    sync.Mutex
	cur   stepKey
	token string
	h     *held
}

var errDied = errors.New("injected: the worker died")

func (w *watched) RoundTrip(r *http.Request) (*http.Response, error) {
	if w.dead.Load() {
		return nil, errDied
	}
	path := r.URL.Path
	var reqBody []byte
	if path == runner.PathHeartbeat && r.Body != nil {
		reqBody, _ = io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(reqBody))
	}
	dies := (path == runner.PathStepAppend || path == runner.PathStepAdvance) && w.rnd.Float64() < 0.08
	if dies && w.rnd.Float64() < 0.5 {
		w.die()
		return nil, errDied
	}
	if path == runner.PathStepAdvance || path == runner.PathStepRelease || path == runner.PathFinish {
		w.end()
	}
	resp, err := w.next.RoundTrip(r)
	if err != nil {
		return nil, err
	}
	if dies {
		_ = resp.Body.Close()
		w.die()
		return nil, errDied
	}
	if resp.StatusCode != http.StatusOK {
		return resp, nil
	}
	switch path {
	case runner.PathClaim, runner.PathHeartbeat, runner.PathStepAppend:
		data, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		resp.Body = io.NopCloser(bytes.NewReader(data))
		switch path {
		case runner.PathClaim:
			w.claimed(data)
		case runner.PathHeartbeat:
			w.renewed(reqBody, data)
		default:
			var app struct{ Adopted bool }
			if json.Unmarshal(data, &app) == nil && app.Adopted {
				w.rec.mu.Lock()
				w.rec.adopted++
				w.rec.mu.Unlock()
			}
		}
	case runner.PathStepAdvance:
		w.committed()
	}
	return resp, nil
}

func (w *watched) die() {
	w.dead.Store(true)
	w.rec.mu.Lock()
	w.rec.deaths++
	w.rec.mu.Unlock()
	w.kill()
}

func (w *watched) claimed(data []byte) {
	var c runner.ClaimResponse
	if json.Unmarshal(data, &c) != nil || c.Mode != v1alpha1.RunModeStep {
		return
	}
	k := stepKey{c.RunID, c.Step}
	h := &held{worker: w.worker, from: time.Now(), till: c.LeaseUntil}
	w.mu.Lock()
	w.cur, w.h = k, h
	if c.MCP != nil {
		w.token = c.MCP.Token
	}
	w.mu.Unlock()
	w.rec.mu.Lock()
	w.rec.takes[k]++
	w.rec.kinds[k] = c.StepKind
	w.rec.held[k] = append(w.rec.held[k], h)
	w.rec.mu.Unlock()
}

func (w *watched) renewed(req, data []byte) {
	var hb runner.HeartbeatRequest
	var resp runner.HeartbeatResponse
	if json.Unmarshal(req, &hb) != nil || json.Unmarshal(data, &resp) != nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.h == nil || hb.RunID != w.cur.run || hb.Step != w.cur.step {
		return
	}
	w.rec.mu.Lock()
	if !w.h.ended {
		w.h.till = later(w.h.till, resp.LeaseUntil)
	}
	w.rec.mu.Unlock()
}

// end is the worker sending what ends its step: from here on it acts on
// it no more.
func (w *watched) end() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.h == nil {
		return
	}
	w.rec.mu.Lock()
	w.h.till, w.h.ended = earlier(w.h.till, time.Now()), true
	w.rec.mu.Unlock()
}

// committed checks that the committed step's MCP token no longer works.
func (w *watched) committed() {
	w.mu.Lock()
	token := w.token
	w.mu.Unlock()
	if token == "" {
		return
	}
	w.rec.checks.Add(1)
	go func() {
		defer w.rec.checks.Done()
		status := 0
		for range 20 {
			req, _ := http.NewRequest(http.MethodPost, w.rec.mcpURL, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
			req.Header.Set("Authorization", "Bearer "+token)
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Accept", "application/json, text/event-stream")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				time.Sleep(100 * time.Millisecond)
				continue
			}
			status = resp.StatusCode
			_ = resp.Body.Close()
			if status != http.StatusServiceUnavailable && status != http.StatusBadGateway {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		w.rec.mu.Lock()
		defer w.rec.mu.Unlock()
		w.rec.tokens++
		if status != http.StatusNotFound {
			w.rec.tokenErrs = append(w.rec.tokenErrs, fmt.Sprintf("a committed step's token got %d, not 404", status))
		}
	}()
}

// orphanStep takes a step of a step-mode run, a new one if need be, and
// gives it back, once no step worker is left to take it.
func orphanStep(t *testing.T, cl *client.Client) {
	t.Helper()
	ctx := context.Background()
	var err error
	for range 20 {
		if _, err = cl.CreateTask(ctx, "chaos-a", "", v1alpha1.TaskSpec{Title: "orphan", Assignee: &v1alpha1.Assignee{Kind: v1alpha1.AssigneeAgent, Name: "stepper"}}); err == nil {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("create the orphan's task: %v", err)
	}
	labels := map[string]string{v1alpha1.LabelRuntime: string(v1alpha1.RuntimeBuiltin)}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		c, err := cl.Claim(ctx, runner.ClaimRequest{Runner: "orphan", Labels: labels, Mode: v1alpha1.RunModeStep})
		if err != nil || c == nil || c.Attempt >= c.MaxAttempts {
			// A step at its last attempt would fail its run as taken too
			// often when given back, not for want of a worker: it is left
			// to its lease, and another taken.
			time.Sleep(200 * time.Millisecond)
			continue
		}
		ref := runner.StepRef{Task: runner.TaskRef{Namespace: c.Task.Namespace, Name: c.Task.Name}, RunID: c.RunID, Step: c.Step, Attempt: c.Attempt}
		// Given back, or, if the servers keep refusing, left to its lease:
		// either way nobody takes it.
		for range 10 {
			if err = cl.StepRelease(ctx, runner.StepReleaseRequest{StepRef: ref, Error: "the worker left"}); err == nil {
				break
			}
			time.Sleep(200 * time.Millisecond)
		}
		t.Logf("orphaned step %d of run %s (given back: %v)", c.Step, c.RunID, err == nil)
		return
	}
	t.Fatal("no step to orphan")
}

// checkSteps verifies step mode's invariants once the system has quiesced.
func checkSteps(t *testing.T, cfg kubernetes.Config, rec *stepRecorder) {
	ctx := context.Background()
	drv, err := kubernetes.Open(ctx, cfg)
	if err != nil {
		t.Fatalf("open a fresh driver: %v", err)
	}
	defer func() { _ = drv.Close() }()
	m := manager.New(drv, managerConfig)
	rec.workers.Wait() // what is left of killed workers
	rec.checks.Wait()  // the token checks still in flight
	rec.mu.Lock()
	defer rec.mu.Unlock()

	stepRuns, steps, idle, exhausted, posts := 0, 0, 0, 0, 0
	for ns := range teams {
		tasks, err := drv.ListTasks(ctx, storage.Query{Namespace: ns})
		if err != nil {
			t.Fatal(err)
		}
		for _, tk := range tasks {
			key := storage.Key{Namespace: ns, Name: tk.Name}
			runs, err := m.Runs(ctx, key)
			if err != nil {
				t.Fatal(err)
			}
			thread, err := m.ReadThread(ctx, key, 0, 0)
			if err != nil {
				t.Fatal(err)
			}
			bodies := map[string]int{}
			for _, e := range thread {
				bodies[e.Body]++
				if strings.HasPrefix(e.Body, "post ") && bodies[e.Body] > 1 {
					t.Errorf("%s: %q is in the thread %d times", key, e.Body, bodies[e.Body])
				}
				switch {
				case e.Author.Kind != manager.AuthorSystem:
				case strings.Contains(e.Body, "no worker took step"):
					idle++
				case strings.Contains(e.Body, fmt.Sprintf("was taken %d times", task.MaxAttempts)):
					// A step whose model calls kept failing three takes in a
					// row: expected now and then, with a failing model.
					exhausted++
				}
			}
			for _, run := range runs {
				if run.Mode != v1alpha1.RunModeStep {
					continue
				}
				stepRuns++
				entries, err := m.Transcript(ctx, key, run.ID)
				if err != nil {
					t.Fatal(err)
				}
				steps += len(entries)
				posts += checkTranscript(t, key, run, entries, bodies, rec)
			}
		}
	}

	// No step was held by two workers at once.
	for k, hs := range rec.held {
		slices.SortFunc(hs, func(a, b *held) int { return a.from.Compare(b.from) })
		for i := 1; i < len(hs); i++ {
			if hs[i].from.Before(hs[i-1].till) {
				t.Errorf("run %s step %d held twice: %s until %s, %s from %s", k.run, k.step,
					hs[i-1].worker, hs[i-1].till.Format(time.RFC3339Nano), hs[i].worker, hs[i].from.Format(time.RFC3339Nano))
			}
		}
	}
	for _, e := range rec.tokenErrs {
		t.Error(e)
	}
	takes := 0
	for _, n := range rec.takes {
		takes += n
	}
	t.Logf("step mode: %d runs, %d entries, %d posts checked, %d takes of %d steps, %d workers killed mid-step, %d entries adopted, %d dead tokens checked, %d runs failed idle, %d ran out of attempts",
		stepRuns, steps, posts, takes, len(rec.takes), rec.deaths, rec.adopted, rec.tokens, idle, exhausted)
	switch {
	case stepRuns == 0 || steps == 0:
		t.Error("no step-mode run happened; the chaos test exercised nothing of step mode")
	case rec.tokens == 0:
		t.Error("no committed step's token was checked")
	case idle == 0:
		t.Error("no run failed for want of a worker; the step idle timeout went untested")
	}
}

// checkTranscript checks one step-mode run's transcript: one entry per
// step taken to its commit, each of the kind the one before dictates; no
// turn called the model more often than its step was taken; every post a
// committed exec_tools step made is in the thread once. It returns how
// many posts it checked.
func checkTranscript(t *testing.T, key storage.Key, run v1alpha1.RunRecord, entries []manager.TranscriptEntry, bodies map[string]int, rec *stepRecorder) int {
	if n := int64(len(entries)); n != run.Step && n != run.Step+1 {
		t.Errorf("%s run %s reached step %d with %d transcript entries", key, run.ID, run.Step, n)
	}
	if len(entries) == 0 {
		return 0
	}
	nonce := strings.TrimPrefix(entries[0].SDK, "chaos-model@")
	posts, turns := 0, 0
	var prev *manager.TranscriptEntry
	for i := range entries {
		e := &entries[i]
		if int64(i) != e.Step {
			t.Errorf("%s run %s: entry %d says it is step %d", key, run.ID, i, e.Step)
		}
		want := manager.StepStart
		if prev != nil {
			want = manager.NextKind(prev)
		}
		if e.Kind != want {
			t.Errorf("%s run %s: step %d is %s, want %s", key, run.ID, i, e.Kind, want)
		}
		switch e.Kind {
		case manager.StepLLMTurn:
			if calls := rec.calls[nonce]; turns < len(calls) && calls[turns] > rec.takes[stepKey{run.ID, int64(i)}] {
				t.Errorf("%s run %s: turn %d called the model %d times, its step was taken %d times", key, run.ID, turns, calls[turns], rec.takes[stepKey{run.ID, int64(i)}])
			}
			turns++
		case manager.StepExecTools:
			if prev == nil {
				break // reported above: exec_tools cannot come first
			}
			// A post the endpoint answered is in the thread once; one that
			// failed (a store fault, say: the model sees the error) is in
			// it at most once.
			var results []builtin.ToolResult
			_ = json.Unmarshal(e.Results, &results)
			failed := map[string]string{}
			for _, r := range results {
				if r.IsError {
					failed[r.CallID] = r.Text
				}
			}
			for _, c := range prev.Calls {
				var in struct{ Body string }
				_ = json.Unmarshal(c.Input, &in)
				posts++
				why, isErr := failed[c.ID]
				switch n := bodies[in.Body]; {
				case !isErr && n != 1:
					t.Errorf("%s run %s: step %d posted %q, the thread has it %d times", key, run.ID, i, in.Body, n)
				case isErr && n > 1:
					t.Errorf("%s run %s: step %d's post %q failed (%s), yet the thread has it %d times", key, run.ID, i, in.Body, why, n)
				}
			}
		}
		prev = e
	}
	// A turn its step's worker died in, uncommitted, called the model no
	// more often than the step was taken either.
	if k := (stepKey{run.ID, run.Step}); rec.kinds[k] == string(manager.StepLLMTurn) && int64(len(entries)) == run.Step {
		if calls := rec.calls[nonce]; turns < len(calls) && calls[turns] > rec.takes[k] {
			t.Errorf("%s run %s: its last turn called the model %d times, its step was taken %d times", key, run.ID, calls[turns], rec.takes[k])
		}
	}
	return posts
}
