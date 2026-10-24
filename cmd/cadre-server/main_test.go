package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/j75689/cadre/internal/manager"
	"github.com/j75689/cadre/internal/storage/memory"
)

func TestValidateTimings(t *testing.T) {
	for _, tc := range []struct {
		lease, heartbeat time.Duration
		ok               bool
	}{
		{30 * time.Second, 10 * time.Second, true},
		{2 * time.Second, time.Second, true},
		{30 * time.Second, 500 * time.Millisecond, false},
		{1500 * time.Millisecond, time.Second, false},
		{10 * time.Second, 60 * time.Second, false},
		{10 * time.Second, 10 * time.Second, false},
		{0, 0, false},
	} {
		if err := validateTimings(tc.lease, tc.heartbeat); (err == nil) != tc.ok {
			t.Errorf("lease %s heartbeat %s: %v, want ok=%v", tc.lease, tc.heartbeat, err, tc.ok)
		}
	}
}

func TestBoundRequests(t *testing.T) {
	var deadline time.Time
	h := boundRequests(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		deadline, _ = r.Context().Deadline()
	}), time.Minute, 2*time.Minute)
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil))
	if until := time.Until(deadline); until <= 0 || until > time.Minute {
		t.Fatalf("request deadline in %s, want within a minute", until)
	}
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/mcp/run", nil))
	if until := time.Until(deadline); until <= time.Minute || until > 2*time.Minute {
		t.Fatalf("MCP request deadline in %s, want the longer bound", until)
	}
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/v1/namespaces/team-a/tasks/fix-login/approve", nil))
	if until := time.Until(deadline); until <= time.Minute || until > 2*time.Minute {
		t.Fatalf("approval deadline in %s, want the longer bound", until)
	}
}

func TestResolvePublicURL(t *testing.T) {
	for _, tc := range []struct {
		flag, listen string
		inCluster    bool
		want         string
		ok           bool
	}{
		{"", ":8080", false, "http://localhost:8080", true},
		{"", "[::]:9090", false, "http://localhost:9090", true},
		{"", "0.0.0.0:80", false, "http://localhost:80", true},
		{"https://cadre.example.com", ":8080", true, "https://cadre.example.com", true},
		{"", ":8080", true, "", false},
		{"", "8080", false, "", false},
	} {
		got, err := resolvePublicURL(tc.flag, tc.listen, tc.inCluster)
		if (err == nil) != tc.ok || got != tc.want {
			t.Errorf("resolvePublicURL(%q, %q, %v) = %q, %v; want %q, ok=%v", tc.flag, tc.listen, tc.inCluster, got, err, tc.want, tc.ok)
		}
	}
}

// Every route cadre-server serves registers on one mux without a conflict
// (ServeMux panics on overlapping patterns), and the mux keeps its 405s.
func TestRoutesCoexist(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	m := manager.New(memory.NewBackend().Open(), manager.Config{})
	mux := newMux(m, log, "http://localhost", nil)
	for _, c := range []struct {
		method, path string
		code         int
	}{
		{http.MethodGet, "/api/v1/namespaces/a/tasks/x/cancel", http.StatusMethodNotAllowed},
		{http.MethodPost, "/api/v1/namespaces/a/agents", http.StatusMethodNotAllowed},
		{http.MethodGet, "/runner/v1/claim", http.StatusMethodNotAllowed},
		{http.MethodPost, "/healthz", http.StatusMethodNotAllowed},
		{http.MethodGet, "/healthz", http.StatusOK},
		{http.MethodGet, "/nope", http.StatusNotFound},
		{http.MethodGet, "/", http.StatusOK},
		{http.MethodGet, "/t/a/tasks/x", http.StatusOK},
	} {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest(c.method, c.path, nil))
		if w.Code != c.code {
			t.Errorf("%s %s: %d, want %d", c.method, c.path, w.Code, c.code)
		}
	}
}
