package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	"github.com/j75689/cadre/internal/httpjson"
	"github.com/j75689/cadre/internal/manager"
	"github.com/j75689/cadre/internal/storage"
	"github.com/j75689/cadre/internal/storage/memory"
)

const ns = "team-a"

type env struct {
	srv   *httptest.Server
	store storage.Driver
	m     *manager.Manager
}

func newEnv(t *testing.T) *env {
	t.Helper()
	store := memory.NewBackend().Open()
	ctx := context.Background()
	if err := store.CreateTeam(ctx, &v1alpha1.Team{ObjectMeta: metav1.ObjectMeta{Name: v1alpha1.TeamName, Namespace: ns}, Spec: v1alpha1.TeamSpec{MaxConcurrentRuns: 1}}); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateTask(ctx, &v1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "fix-login", Namespace: ns},
		Spec:       v1alpha1.TaskSpec{Title: "Fix login", Assignee: &v1alpha1.Assignee{Kind: v1alpha1.AssigneeAgent, Name: "coder"}},
	}); err != nil {
		t.Fatal(err)
	}
	m := manager.New(store, manager.Config{})
	if err := m.NumberNew(ctx); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	NewServer(m, slog.New(slog.NewTextHandler(io.Discard, nil))).Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &env{srv: srv, store: store, m: m}
}

// post sends body and decodes a 2xx JSON response into out, or an error body.
func (e *env) post(t *testing.T, path string, body any, out any) (int, ErrorBody) {
	t.Helper()
	var buf bytes.Buffer
	switch b := body.(type) {
	case string:
		buf.WriteString(b)
	default:
		if err := json.NewEncoder(&buf).Encode(b); err != nil {
			t.Fatal(err)
		}
	}
	resp, err := http.Post(e.srv.URL+path, "application/json", &buf)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 {
		var er ErrorResponse
		if err := json.NewDecoder(resp.Body).Decode(&er); err != nil {
			t.Fatalf("%s: status %d with an undecodable body: %v", path, resp.StatusCode, err)
		}
		return resp.StatusCode, er.Error
	}
	if out != nil && resp.StatusCode == http.StatusOK {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			t.Fatal(err)
		}
	}
	return resp.StatusCode, ErrorBody{}
}

func TestRunnerProtocol(t *testing.T) {
	e := newEnv(t)

	var claim ClaimResponse
	if status, _ := e.post(t, PathClaim, ClaimRequest{Runner: "runner-1"}, &claim); status != http.StatusOK {
		t.Fatalf("claim: status %d", status)
	}
	if claim.Task.Name != "fix-login" || claim.Task.Number != 1 || claim.Task.Title != "Fix login" || claim.RunID == "" ||
		claim.Mode != v1alpha1.RunModeRuntime || claim.LeaseSeconds != 30 || claim.HeartbeatSeconds != 10 || claim.LeaseUntil.IsZero() {
		t.Fatalf("claim: %+v", claim)
	}
	if status, _ := e.post(t, PathClaim, ClaimRequest{Runner: "runner-2"}, nil); status != http.StatusNoContent {
		t.Fatalf("second claim: status %d, want 204", status)
	}

	ref := TaskRef{Namespace: ns, Name: claim.Task.Name}
	var hb HeartbeatResponse
	if status, _ := e.post(t, PathHeartbeat, HeartbeatRequest{Task: ref, RunID: claim.RunID}, &hb); status != http.StatusOK || hb.LeaseUntil.IsZero() {
		t.Fatalf("heartbeat: %d %+v", status, hb)
	}
	ev := EventsRequest{Task: ref, RunID: claim.RunID, Seq: 1, Usage: Usage{InputTokens: 100}, Progress: "reading"}
	for range 2 {
		if status, _ := e.post(t, PathEvents, ev, nil); status != http.StatusAccepted {
			t.Fatalf("events: status %d, want 202", status)
		}
	}

	finish := FinishRequest{
		Task: ref, RunID: claim.RunID, Outcome: v1alpha1.RunOutcomeWaiting,
		Waiting:  &Waiting{Reason: v1alpha1.WaitingQuestion, ThreadSeq: 2},
		Subtasks: []Subtask{{Spec: v1alpha1.TaskSpec{Title: "child"}}},
	}
	var fin FinishResponse
	if status, _ := e.post(t, PathFinish, finish, &fin); status != http.StatusOK || fin.Phase != v1alpha1.PhaseWaiting || fin.AlreadyFinished {
		t.Fatalf("finish: %d %+v", status, fin)
	}
	if status, _ := e.post(t, PathFinish, finish, &fin); status != http.StatusOK || !fin.AlreadyFinished {
		t.Fatalf("finish retry: %d %+v", status, fin)
	}
	if status, body := e.post(t, PathHeartbeat, HeartbeatRequest{Task: ref, RunID: claim.RunID}, nil); status != http.StatusConflict || body.Code != CodeRunMismatch {
		t.Fatalf("heartbeat after finish: %d %+v", status, body)
	}
	if status, body := e.post(t, PathEvents, EventsRequest{Task: ref, RunID: claim.RunID, Seq: 2}, nil); status != http.StatusConflict || body.Code != CodeRunMismatch {
		t.Fatalf("events after finish: %d %+v", status, body)
	}
}

func TestRunnerErrors(t *testing.T) {
	e := newEnv(t)
	var claim ClaimResponse
	e.post(t, PathClaim, ClaimRequest{Runner: "r"}, &claim)
	ref := TaskRef{Namespace: ns, Name: claim.Task.Name}

	for name, tc := range map[string]struct {
		path   string
		body   any
		status int
		code   string
	}{
		"not json":            {PathClaim, "{", 400, CodeInvalid},
		"two JSON values":     {PathClaim, `{"runner":"r"} {"runner":"s"}`, 400, CodeInvalid},
		"oversized body":      {PathClaim, `{"runner":"` + strings.Repeat("r", httpjson.MaxBody) + `"}`, 413, CodeInvalid},
		"claim without name":  {PathClaim, ClaimRequest{}, 400, CodeInvalid},
		"heartbeat no run":    {PathHeartbeat, HeartbeatRequest{Task: ref}, 400, CodeInvalid},
		"missing task":        {PathHeartbeat, HeartbeatRequest{Task: TaskRef{Namespace: ns, Name: "nope"}, RunID: claim.RunID}, 404, CodeNotFound},
		"stale run":           {PathHeartbeat, HeartbeatRequest{Task: ref, RunID: "01JB0000000000000000000000"}, 409, CodeRunMismatch},
		"waiting without why": {PathFinish, FinishRequest{Task: ref, RunID: claim.RunID, Outcome: v1alpha1.RunOutcomeWaiting}, 400, CodeInvalid},
		"why without waiting": {PathFinish, FinishRequest{Task: ref, RunID: claim.RunID, Outcome: v1alpha1.RunOutcomeDone, Waiting: &Waiting{Reason: v1alpha1.WaitingQuestion}}, 400, CodeInvalid},
		"unknown outcome":     {PathFinish, FinishRequest{Task: ref, RunID: claim.RunID, Outcome: "Paused"}, 400, CodeInvalid},
		"bad subtask id":      {PathFinish, FinishRequest{Task: ref, RunID: claim.RunID, Outcome: v1alpha1.RunOutcomeDone, Subtasks: []Subtask{{ID: "x"}}}, 400, CodeInvalid},
		"event without seq":   {PathEvents, EventsRequest{Task: ref, RunID: claim.RunID}, 400, CodeInvalid},
	} {
		status, body := e.post(t, tc.path, tc.body, nil)
		if status != tc.status || body.Code != tc.code {
			t.Errorf("%s: %d %+v, want %d %s", name, status, body, tc.status, tc.code)
		}
	}

	resp, err := http.Get(e.srv.URL + PathClaim)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET claim: status %d", resp.StatusCode)
	}

	// The protocol grows by adding fields: an older server ignores them.
	if status, _ := e.post(t, PathHeartbeat, `{"task":{"namespace":"`+ns+`","name":"`+ref.Name+`"},"runId":"`+claim.RunID+`","newField":1}`, nil); status != http.StatusOK {
		t.Errorf("heartbeat with an unknown field: status %d, want 200", status)
	}
}
