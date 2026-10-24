package manager

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	"github.com/j75689/cadre/internal/storage"
	"github.com/j75689/cadre/internal/task"
)

// stepEnv has two builtin agents: stepper, run a step at a time, and
// coder, run in runtime mode.
func stepEnv(t *testing.T) (*env, *Manager) {
	t.Helper()
	e := newEnv(t, 2)
	agents := ReadyBuiltin(ns, "coder", "claude")
	stepper := ReadyAgent(ns, "stepper", "claude", v1alpha1.AgentRuntime{Type: v1alpha1.RuntimeBuiltin, Execution: v1alpha1.RunModeStep})
	agents.Agents[ns+"/stepper"] = stepper
	m := New(e.store, Config{Lease: 30 * time.Second, Heartbeat: 10 * time.Second, Agents: agents}, WithClock(e.clock.now))
	return e, m
}

func (e *env) stepTask(t *testing.T, m *Manager, name, agent string) storage.Key {
	t.Helper()
	if _, err := m.CreateTask(ctx, ns, name, v1alpha1.TaskSpec{Title: name, Assignee: &v1alpha1.Assignee{Kind: v1alpha1.AssigneeAgent, Name: agent}}); err != nil {
		t.Fatal(err)
	}
	return storage.Key{Namespace: ns, Name: name}
}

func takeStep(t *testing.T, m *Manager, worker string) *Claimed {
	t.Helper()
	c, err := m.Claim(ctx, ClaimRequest{Runner: worker, Labels: builtin, Mode: v1alpha1.RunModeStep})
	if err != nil {
		t.Fatalf("step claim: %v", err)
	}
	return c
}

// ref is the step a claim holds, with its attempt.
func ref(c *Claimed) StepRequest {
	return StepRequest{RunID: c.RunID, Step: c.Step, Attempt: c.Attempt}
}

func commit(t *testing.T, m *Manager, key storage.Key, c *Claimed, e TranscriptEntry) {
	t.Helper()
	if _, adopted, err := m.AppendStep(ctx, key, ref(c), e); err != nil || adopted {
		t.Fatalf("append step %d: adopted %v, %v", c.Step, adopted, err)
	}
	if _, err := m.Advance(ctx, key, ref(c), nil); err != nil {
		t.Fatalf("advance step %d: %v", c.Step, err)
	}
}

func decode(t *testing.T, data json.RawMessage) TranscriptEntry {
	t.Helper()
	var e TranscriptEntry
	if err := json.Unmarshal(data, &e); err != nil {
		t.Fatal(err)
	}
	return e
}

// failure is the System comment a failed step-mode run leaves, once the
// dispatcher applied its RunEnded record.
func failure(t *testing.T, e *env, m *Manager, key storage.Key) string {
	t.Helper()
	if err := m.Dispatch(ctx); err != nil {
		t.Fatal(err)
	}
	if err := m.Dispatch(ctx); err != nil { // applying it again writes nothing
		t.Fatal(err)
	}
	if tk := e.get(t, key); tk.Status.Phase != v1alpha1.PhaseFailed {
		t.Fatalf("phase %s, want Failed", tk.Status.Phase)
	}
	thread, _ := m.ReadThread(ctx, key, 0, 0)
	if len(thread) != 1 || thread[0].Author.Kind != AuthorSystem {
		t.Fatalf("thread %+v; want one System comment", thread)
	}
	return thread[0].Body
}

// A step-mode run goes a step at a time, each taken by a claim that says
// which step it is and what it does, read from the transcript; every
// commit frees the step and its token.
func TestAStepModeRunStepByStep(t *testing.T) {
	e, m := stepEnv(t)
	key := e.stepTask(t, m, "steps", "stepper")

	if c, err := m.Claim(ctx, ClaimRequest{Runner: "r", Labels: builtin}); err != nil || c != nil {
		t.Fatalf("a runtime claim of a step-mode task: %+v, %v", c, err)
	}
	want := []StepKind{StepStart, StepLLMTurn, StepExecTools, StepLLMTurn, StepFinish}
	entries := []TranscriptEntry{
		{Kind: StepStart, Provider: "anthropic", Model: "claude-opus-5-5", Request: json.RawMessage(`{"system":"<s>"}`)},
		{Kind: StepLLMTurn, Messages: []json.RawMessage{json.RawMessage(`{"role":"assistant"}`)}, Calls: []StepCall{{ID: "tu_1", Name: "thread_post"}}},
		{Kind: StepExecTools, Messages: []json.RawMessage{json.RawMessage(`{"role":"user"}`)}},
		{Kind: StepLLMTurn, Messages: []json.RawMessage{json.RawMessage(`{"role":"assistant"}`)}},
	}
	var run string
	for i, kind := range want {
		c := takeStep(t, m, "w")
		if c == nil || c.Mode != v1alpha1.RunModeStep || c.Step != int64(i) || c.Kind != kind || c.Attempt != 1 || c.MCPToken == "" {
			t.Fatalf("step %d: %+v; want kind %s", i, c, kind)
		}
		if run == "" {
			run = c.RunID
		} else if c.RunID != run {
			t.Fatalf("step %d is of run %s, want %s", i, c.RunID, run)
		}
		if kind == StepFinish {
			if _, _, err := m.Finish(ctx, key, FinishRequest{RunID: c.RunID, Step: c.Step, Attempt: 2, Outcome: task.OutcomeReview}); !errors.Is(err, task.ErrAttemptMismatch) {
				t.Fatalf("a finish of another attempt: %v", err)
			}
			if _, _, err := m.Finish(ctx, key, FinishRequest{RunID: c.RunID, Step: c.Step, Attempt: c.Attempt, Outcome: task.OutcomeReview}); err != nil {
				t.Fatal(err)
			}
			break
		}
		if _, err := m.Heartbeat(ctx, key, ref(c)); err != nil {
			t.Fatalf("heartbeat of step %d: %v", i, err)
		}
		wrong := entries[i]
		wrong.Kind = StepFinish
		if _, _, err := m.AppendStep(ctx, key, ref(c), wrong); !errors.Is(err, ErrInvalid) {
			t.Fatalf("an entry of another kind: %v, want ErrInvalid", err)
		}
		commit(t, m, key, c, entries[i])
		if r := e.get(t, key).Status.Run; !r.StepFree || r.Runner != "" || r.MCPTokenHash != "" || r.Attempt != 0 || r.Step != int64(i+1) ||
			!r.LeaseUntil.Time.Equal(e.clock.now().Add(task.DefaultStepIdle)) {
			t.Fatalf("after committing step %d: %+v", i, r)
		}
	}
	if tk := e.get(t, key); tk.Status.Phase != v1alpha1.PhaseReview {
		t.Fatalf("phase %s, want Review", tk.Status.Phase)
	}
	got, err := m.Transcript(ctx, key, run)
	if err != nil || len(got) != 4 || got[1].Calls[0].ID != "tu_1" || string(got[0].Request) != `{"system":"<s>"}` {
		t.Fatalf("transcript: %+v, %v", got, err)
	}
	// A step claim does not take runtime-mode agents' tasks either.
	e.stepTask(t, m, "whole", "coder")
	if c := takeStep(t, m, "w"); c != nil {
		t.Fatalf("a step claim took a runtime-mode task: %+v", c)
	}
}

// A worker that dies after appending its step's entry leaves it for the
// next worker, which adopts it instead of doing the step again; the dead
// worker's attempt is over.
func TestTheNextWorkerAdoptsAWrittenStep(t *testing.T) {
	e, m := stepEnv(t)
	key := e.stepTask(t, m, "steps", "stepper")
	first := takeStep(t, m, "w1")
	if _, _, err := m.AppendStep(ctx, key, ref(first), TranscriptEntry{Kind: StepStart, Model: "first"}); err != nil {
		t.Fatal(err)
	}
	// w1 dies before committing: its lease runs out and the step is freed.
	e.clock.add(time.Minute)
	if err := m.ReclaimExpired(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Advance(ctx, key, ref(first), nil); !errors.Is(err, task.ErrAttemptMismatch) {
		t.Fatalf("a commit of a freed attempt: %v, want ErrAttemptMismatch", err)
	}
	if _, _, err := m.AppendStep(ctx, key, ref(first), TranscriptEntry{Kind: StepStart}); !errors.Is(err, task.ErrAttemptMismatch) {
		t.Fatalf("an append of a freed attempt: %v, want ErrAttemptMismatch", err)
	}
	if _, err := m.StepTranscript(ctx, key, ref(first)); !errors.Is(err, task.ErrAttemptMismatch) {
		t.Fatalf("a transcript read of a freed attempt: %v, want ErrAttemptMismatch", err)
	}
	second := takeStep(t, m, "w2")
	if second == nil || second.Step != 0 || second.Attempt != 2 {
		t.Fatalf("second take: %+v", second)
	}
	if _, err := m.Heartbeat(ctx, key, ref(first)); !errors.Is(err, task.ErrAttemptMismatch) {
		t.Fatalf("a heartbeat of the first attempt: %v, want ErrAttemptMismatch", err)
	}
	got, adopted, err := m.AppendStep(ctx, key, ref(second), TranscriptEntry{Kind: StepStart, Model: "second"})
	if err != nil || !adopted || decode(t, got).Model != "first" {
		t.Fatalf("append over a written step: %s, adopted %v, %v", got, adopted, err)
	}
	if _, err := m.Advance(ctx, key, ref(second), nil); err != nil {
		t.Fatal(err)
	}
	// w1's late commit of the same step is dropped.
	if _, err := m.Advance(ctx, key, ref(first), nil); !errors.Is(err, task.ErrStepMismatch) {
		t.Fatalf("a late commit: %v, want ErrStepMismatch", err)
	}
	third := takeStep(t, m, "w3")
	if _, err := m.Advance(ctx, key, ref(third), nil); !errors.Is(err, ErrInvalid) {
		t.Fatalf("committing a step without its entry: %v, want ErrInvalid", err)
	}
}

// A step is taken MaxAttempts times at most; when the last attempt's
// worker dies, the reclaim fails the run, and its record says why.
func TestAStepThatKeepsFailing(t *testing.T) {
	e, m := stepEnv(t)
	key := e.stepTask(t, m, "steps", "stepper")
	for attempt := int32(1); attempt < task.MaxAttempts; attempt++ {
		c := takeStep(t, m, "w")
		if c == nil || c.Attempt != attempt {
			t.Fatalf("attempt %d: %+v", attempt, c)
		}
		if err := m.ReleaseStep(ctx, key, ref(c), "model overloaded"); err != nil {
			t.Fatal(err)
		}
		if err := m.ReleaseStep(ctx, key, ref(c), "again"); !errors.Is(err, task.ErrAttemptMismatch) {
			t.Fatalf("releasing a released attempt: %v", err)
		}
	}
	last := takeStep(t, m, "w")
	if last == nil || last.Attempt != task.MaxAttempts {
		t.Fatalf("last attempt: %+v", last)
	}
	e.clock.add(time.Minute) // its worker dies
	if err := m.ReclaimExpired(ctx); err != nil {
		t.Fatal(err)
	}
	if msg := failure(t, e, m, key); !strings.Contains(msg, "step 0 was taken 3 times") {
		t.Fatalf("comment %q", msg)
	}
	// The failed attempts are in the transcript for diagnosis, not as steps.
	entries, err := e.store.Read(ctx, TranscriptStream(key, last.RunID), "", 0)
	if err != nil || len(entries) != 2 {
		t.Fatalf("transcript keys %+v, %v; want two attempt entries", entries, err)
	}
	if steps, err := m.Transcript(ctx, key, last.RunID); err != nil || len(steps) != 0 {
		t.Fatalf("steps %+v, %v; want none", steps, err)
	}
}

// A worker that gives up a step's last attempt leaves a step no worker may
// take: the next reclaim fails the run, with the same comment.
func TestAGivenUpLastAttempt(t *testing.T) {
	e, m := stepEnv(t)
	key := e.stepTask(t, m, "steps", "stepper")
	for range task.MaxAttempts {
		c := takeStep(t, m, "w")
		if err := m.ReleaseStep(ctx, key, ref(c), "model overloaded"); err != nil {
			t.Fatal(err)
		}
	}
	if c := takeStep(t, m, "w"); c != nil {
		t.Fatalf("a step past its attempts was taken: %+v", c)
	}
	e.clock.add(time.Minute)
	if err := m.ReclaimExpired(ctx); err != nil {
		t.Fatal(err)
	}
	if msg := failure(t, e, m, key); !strings.Contains(msg, "taken 3 times") {
		t.Fatalf("comment %q", msg)
	}
}

// A free step no worker takes (the pool is down, say) does not hold its
// Team slot forever: past the idle deadline the run fails.
func TestAStepNoWorkerTakes(t *testing.T) {
	e, m := stepEnv(t)
	key := e.stepTask(t, m, "steps", "stepper")
	commit(t, m, key, takeStep(t, m, "w"), TranscriptEntry{Kind: StepStart})
	e.clock.add(task.DefaultStepIdle - time.Minute)
	if err := m.ReclaimExpired(ctx); err != nil {
		t.Fatal(err)
	}
	if tk := e.get(t, key); tk.Status.Phase != v1alpha1.PhaseRunning {
		t.Fatalf("phase %s before the idle deadline", tk.Status.Phase)
	}
	e.clock.add(2 * time.Minute)
	if err := m.ReclaimExpired(ctx); err != nil {
		t.Fatal(err)
	}
	if msg := failure(t, e, m, key); !strings.Contains(msg, "no worker took step 1 within 1h") {
		t.Fatalf("comment %q", msg)
	}
}

// An attempt entry sorts between a step's key and the next step's; finding
// a step's entry reads past it and never takes it for the step.
func TestAttemptEntriesAreNeverTakenForSteps(t *testing.T) {
	e, m := stepEnv(t)
	key := e.stepTask(t, m, "steps", "stepper")
	commit(t, m, key, takeStep(t, m, "w"), TranscriptEntry{Kind: StepStart})
	// Step 1 fails once before it is done: an attempt entry after
	// SeqKey(1), before SeqKey(2).
	c := takeStep(t, m, "w")
	if err := m.ReleaseStep(ctx, key, ref(c), "timeout"); err != nil {
		t.Fatal(err)
	}
	c = takeStep(t, m, "w")
	// Only the attempt entry is there: step 1 is not done.
	if _, err := m.Advance(ctx, key, ref(c), nil); !errors.Is(err, ErrInvalid) {
		t.Fatalf("committing step 1 with only its attempt entry: %v, want ErrInvalid", err)
	}
	commit(t, m, key, c, TranscriptEntry{Kind: StepLLMTurn, Calls: []StepCall{{ID: "tu_1", Name: "x"}}})
	// Step 2's kind comes from step 1's entry, found by its exact key.
	c = takeStep(t, m, "w")
	if c.Step != 2 || c.Kind != StepExecTools {
		t.Fatalf("step %d of kind %s, want 2 exec_tools", c.Step, c.Kind)
	}
	commit(t, m, key, c, TranscriptEntry{Kind: StepExecTools})
	if c = takeStep(t, m, "w"); c.Step != 3 || c.Kind != StepLLMTurn {
		t.Fatalf("step %d of kind %s, want 3 llm_turn", c.Step, c.Kind)
	}
	keys, err := e.store.Keys(ctx, TranscriptStream(key, c.RunID), "", 0)
	if err != nil || len(keys) != 4 || !strings.HasSuffix(keys[2].Key, "-attempt-1") {
		t.Fatalf("keys %+v, %v", keys, err)
	}
}

// A step done again after its worker died makes the same tool calls; with
// the call ids the worker sends, what they record lands once, and the
// second worker is told where. A call the model did not ask for, or one
// without its id, records nothing.
func TestARedoneStepRecordsOnce(t *testing.T) {
	e, m := stepEnv(t)
	key := e.stepTask(t, m, "steps", "stepper")
	commit(t, m, key, takeStep(t, m, "w"), TranscriptEntry{Kind: StepStart})
	commit(t, m, key, takeStep(t, m, "w"), TranscriptEntry{Kind: StepLLMTurn, Calls: []StepCall{{ID: "tu_1", Name: "thread_post"}, {ID: "tu_2", Name: "file_write"}}})
	var posted, versions []int64
	for worker := range 2 {
		c := takeStep(t, m, "w")
		if c.Kind != StepExecTools || c.Attempt != int32(worker+1) {
			t.Fatalf("take %d: %+v", worker, c)
		}
		entry, err := m.RunPost(WithCallCounter(WithCallID(ctx, "tu_1")), key, c.RunID, "once")
		if err != nil {
			t.Fatal(err)
		}
		info, err := m.RunWriteFile(WithCallCounter(WithCallID(ctx, "tu_2")), key, c.RunID, "a.txt", "x")
		if err != nil {
			t.Fatal(err)
		}
		posted, versions = append(posted, entry.Seq), append(versions, info.Version)
		e.clock.add(time.Minute) // the worker dies
		if err := m.ReclaimExpired(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if posted[0] != 1 || posted[1] != 1 || versions[0] != 1 || versions[1] != 1 {
		t.Fatalf("posted at %v, wrote versions %v; want 1 both times", posted, versions)
	}
	c := takeStep(t, m, "w")
	if _, err := m.RunPost(ctx, key, c.RunID, "without a call id"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a post without a call id: %v, want ErrInvalid", err)
	}
	if _, err := m.RunPost(WithCallCounter(WithCallID(ctx, "tu_9")), key, c.RunID, "made up"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a post for a call the model did not make: %v, want ErrInvalid", err)
	}
	thread, _ := m.ReadThread(ctx, key, 0, 0)
	if len(thread) != 1 || thread[0].Body != "once" {
		t.Fatalf("thread %+v; want the call's post once", thread)
	}
	if vs, err := e.store.Read(ctx, fileStream(key, "a.txt"), "", 0); err != nil || len(vs) != 1 {
		t.Fatalf("versions %+v, %v; want one", vs, err)
	}
	// Committing the step clears what it recorded.
	commit(t, m, key, c, TranscriptEntry{Kind: StepExecTools})
	if r := e.get(t, key).Status.Run; len(r.StepItems) != 0 {
		t.Fatalf("step items %+v after the commit", r.StepItems)
	}
}

// A step-mode run's ask tools finish it at its held step, not step 0.
func TestAskingAtALaterStep(t *testing.T) {
	e, m := stepEnv(t)
	key := e.stepTask(t, m, "steps", "stepper")
	commit(t, m, key, takeStep(t, m, "w"), TranscriptEntry{Kind: StepStart})
	commit(t, m, key, takeStep(t, m, "w"), TranscriptEntry{Kind: StepLLMTurn, Calls: []StepCall{{ID: "tu_1", Name: "thread_ask"}}})
	c := takeStep(t, m, "w")
	if _, err := m.RunAsk(WithCallCounter(WithCallID(ctx, "tu_1")), key, c.RunID, "Staging or production?"); err != nil {
		t.Fatal(err)
	}
	if tk := e.get(t, key); tk.Status.Phase != v1alpha1.PhaseWaiting {
		t.Fatalf("phase %s, want Waiting", tk.Status.Phase)
	}
}
