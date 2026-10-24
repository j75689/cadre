package runner

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	"github.com/j75689/cadre/internal/manager"
)

func TestStepProtocol(t *testing.T) {
	e := newEnv(t, withSteps, withAuth)
	cr, other := e.as("builtin"), e.as("other")
	stepClaim := ClaimRequest{Runner: "worker-1", Labels: builtin, Mode: v1alpha1.RunModeStep}

	if status, body := other.post(t, PathClaim, stepClaim, nil); status != http.StatusForbidden || body.Code != CodeForbidden {
		t.Fatalf("a step claim by another runner: %d %+v, want 403", status, body)
	}
	if status, _ := cr.post(t, PathClaim, ClaimRequest{Runner: "worker-1", Labels: builtin}, nil); status != http.StatusNoContent {
		t.Fatalf("a runtime claim of a step-mode task: status %d, want 204", status)
	}
	if status, body := cr.post(t, PathClaim, ClaimRequest{Runner: "worker-1", Mode: "batch"}, nil); status != http.StatusBadRequest || body.Code != CodeInvalid {
		t.Fatalf("an unknown mode: %d %+v", status, body)
	}

	var claim ClaimResponse
	if status, _ := cr.post(t, PathClaim, stepClaim, &claim); status != http.StatusOK {
		t.Fatalf("step claim: status %d", status)
	}
	if claim.Mode != v1alpha1.RunModeStep || claim.Step != 0 || claim.StepKind != string(manager.StepStart) || claim.Attempt != 1 ||
		claim.MaxAttempts != 3 || claim.MCP == nil || claim.MCP.Token == "" {
		t.Fatalf("step claim: %+v", claim)
	}
	task := TaskRef{Namespace: ns, Name: claim.Task.Name}
	ref := StepRef{Task: task, RunID: claim.RunID, Step: claim.Step, Attempt: claim.Attempt}

	var hb HeartbeatResponse
	if status, _ := cr.post(t, PathHeartbeat, HeartbeatRequest{Task: task, RunID: ref.RunID, Step: 0, Attempt: 1}, &hb); status != http.StatusOK || hb.LeaseUntil.IsZero() {
		t.Fatalf("step heartbeat: %d %+v", status, hb)
	}
	if status, body := cr.post(t, PathHeartbeat, HeartbeatRequest{Task: task, RunID: ref.RunID, Step: 1, Attempt: 1}, nil); status != http.StatusConflict || body.Code != CodeStepMismatch {
		t.Fatalf("heartbeat of another step: %d %+v", status, body)
	}
	if status, body := cr.post(t, PathHeartbeat, HeartbeatRequest{Task: task, RunID: ref.RunID, Step: 0, Attempt: 2}, nil); status != http.StatusConflict || body.Code != CodeAttemptMismatch {
		t.Fatalf("heartbeat of another attempt: %d %+v", status, body)
	}
	if status, body := cr.post(t, PathStepAdvance, StepAdvanceRequest{StepRef: ref}, nil); status != http.StatusBadRequest || body.Code != CodeInvalid {
		t.Fatalf("advance before the entry: %d %+v", status, body)
	}

	if status, body := cr.post(t, PathStepAppend, StepAppendRequest{StepRef: ref, Entry: json.RawMessage(`{"kind":"llm_turn"}`)}, nil); status != http.StatusBadRequest || body.Code != CodeInvalid {
		t.Fatalf("an entry of another kind: %d %+v", status, body)
	}
	// An entry over the usual 1 MiB body limit, as an exec_tools step's can
	// be, is taken.
	big := strings.Repeat("x", 2<<20)
	entry := json.RawMessage(`{"kind":"start","provider":"anthropic","model":"claude-opus-5-5","request":{"system":"<s> & ` + big + `"}}`)
	var app StepAppendResponse
	// Sent as it is, not through an encoder that would escape it: the
	// server keeps what it is sent.
	raw, err := json.Marshal(ref)
	if err != nil {
		t.Fatal(err)
	}
	appendBody := strings.TrimSuffix(string(raw), "}") + `,"entry":` + string(entry) + `}`
	if status, body := cr.post(t, PathStepAppend, appendBody, &app); status != http.StatusOK || app.Adopted {
		t.Fatalf("append: %d %+v", status, body)
	}
	if status, _ := cr.post(t, PathStepAppend, StepAppendRequest{StepRef: ref, Entry: json.RawMessage(`{"kind":"start","model":"other"}`)}, &app); status != http.StatusOK || !app.Adopted {
		t.Fatalf("append again: %d", status)
	}
	var adopted manager.TranscriptEntry
	if err := json.Unmarshal(app.Entry, &adopted); err != nil || adopted.Model != "claude-opus-5-5" || string(adopted.Request) != `{"system":"<s> & `+big+`"}` {
		t.Fatalf("adopted entry: %v, model %q, request %.40s", err, adopted.Model, adopted.Request)
	}
	var tr StepTranscript
	if status, _ := cr.post(t, PathStepTranscript, ref, &tr); status != http.StatusOK || len(tr.Entries) != 1 || !strings.Contains(string(tr.Entries[0]), `"<s> & `) {
		t.Fatalf("transcript: %d, %d entries", status, len(tr.Entries))
	}
	var fin FinishResponse
	if status, _ := cr.post(t, PathStepAdvance, StepAdvanceRequest{StepRef: ref}, &fin); status != http.StatusOK || fin.Phase != v1alpha1.PhaseRunning {
		t.Fatalf("advance: %d %+v", status, fin)
	}
	if status, body := cr.post(t, PathStepAdvance, StepAdvanceRequest{StepRef: ref}, nil); status != http.StatusConflict || body.Code != CodeStepMismatch {
		t.Fatalf("advance again: %d %+v", status, body)
	}
	// Step 1 is free: nobody's to work on.
	free := StepRef{Task: task, RunID: claim.RunID, Step: 1}
	if status, body := cr.post(t, PathStepTranscript, free, nil); status != http.StatusConflict || body.Code != CodeAttemptMismatch {
		t.Fatalf("a transcript read of a free step: %d %+v", status, body)
	}

	if status, _ := cr.post(t, PathClaim, stepClaim, &claim); status != http.StatusOK || claim.Step != 1 || claim.StepKind != string(manager.StepLLMTurn) {
		t.Fatalf("next step: %d %+v", status, claim)
	}
	ref = StepRef{Task: task, RunID: claim.RunID, Step: claim.Step, Attempt: claim.Attempt}
	if status, _ := cr.post(t, PathStepRelease, StepReleaseRequest{StepRef: ref, Error: "model overloaded"}, nil); status != http.StatusNoContent {
		t.Fatalf("release: status %d", status)
	}
	if status, body := cr.post(t, PathStepRelease, StepReleaseRequest{StepRef: ref}, nil); status != http.StatusConflict || body.Code != CodeAttemptMismatch {
		t.Fatalf("release of a released attempt: %d %+v", status, body)
	}
	if status, _ := cr.post(t, PathClaim, stepClaim, &claim); status != http.StatusOK || claim.Step != 1 || claim.Attempt != 2 {
		t.Fatalf("the released step again: %d %+v", status, claim)
	}
}

// Only cadre-runner works on steps: another runner is refused on every step
// endpoint, whether the step is held or free, before the task is read.
func TestStepsAreForCadreRunnerOnly(t *testing.T) {
	e := newEnv(t, withSteps, withAuth)
	cr, other := e.as("builtin"), e.as("other")
	var claim ClaimResponse
	if status, _ := cr.post(t, PathClaim, ClaimRequest{Runner: "worker-1", Labels: builtin, Mode: v1alpha1.RunModeStep}, &claim); status != http.StatusOK {
		t.Fatalf("step claim: status %d", status)
	}
	held := StepRef{Task: TaskRef{Namespace: ns, Name: claim.Task.Name}, RunID: claim.RunID, Step: 0, Attempt: 1}
	try := func(when string, ref StepRef) {
		t.Helper()
		for path, body := range map[string]any{
			PathStepAppend:     StepAppendRequest{StepRef: ref, Entry: json.RawMessage(`{"kind":"start"}`)},
			PathStepAdvance:    StepAdvanceRequest{StepRef: ref},
			PathStepRelease:    StepReleaseRequest{StepRef: ref},
			PathStepTranscript: ref,
		} {
			if status, er := other.post(t, path, body, nil); status != http.StatusForbidden || er.Code != CodeForbidden {
				t.Errorf("%s on a %s step: %d %+v, want 403", path, when, status, er)
			}
		}
	}
	try("held", held)
	if status, _ := cr.post(t, PathStepAppend, StepAppendRequest{StepRef: held, Entry: json.RawMessage(`{"kind":"start"}`)}, nil); status != http.StatusOK {
		t.Fatalf("append: status %d", status)
	}
	if status, _ := cr.post(t, PathStepAdvance, StepAdvanceRequest{StepRef: held}, nil); status != http.StatusOK {
		t.Fatalf("advance: status %d", status)
	}
	try("free", StepRef{Task: held.Task, RunID: held.RunID, Step: 1})
	if steps, err := e.m.Transcript(t.Context(), key(held.Task), held.RunID); err != nil || len(steps) != 1 {
		t.Fatalf("transcript %+v, %v; want only cadre-runner's entry", steps, err)
	}
}
