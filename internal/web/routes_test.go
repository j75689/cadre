package web_test

import (
	"io"
	"log/slog"
	"net/http"
	"testing"

	"github.com/j75689/cadre/internal/api"
	"github.com/j75689/cadre/internal/mcp"
	"github.com/j75689/cadre/internal/runner"
	"github.com/j75689/cadre/internal/web"
)

// cadre-server's routes and the UI register on one mux without a conflict
// (ServeMux panics on overlapping patterns), and the mux keeps its 405s.
func TestRoutesCoexist(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	mux := http.NewServeMux()
	runner.NewServer(nil, log, "http://localhost").Register(mux)
	mcp.NewServer(nil, log).Register(mux)
	api.NewServer(nil, log).Register(mux)
	mux.HandleFunc("GET /healthz", func(http.ResponseWriter, *http.Request) {})
	web.Register(mux)
	for _, c := range []struct {
		method, path string
		code         int
	}{
		{http.MethodGet, "/api/v1/namespaces/a/tasks/x/cancel", http.StatusMethodNotAllowed},
		{http.MethodGet, "/runner/v1/claim", http.StatusMethodNotAllowed},
		{http.MethodPost, "/healthz", http.StatusMethodNotAllowed},
		{http.MethodGet, "/", http.StatusOK},
	} {
		if w := get(t, mux, c.method, c.path); w.Code != c.code {
			t.Errorf("%s %s: %d, want %d", c.method, c.path, w.Code, c.code)
		}
	}
}
