package web_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/j75689/cadre/internal/web"
)

func get(t *testing.T, h http.Handler, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(method, path, nil))
	return w
}

func TestWebHandler(t *testing.T) {
	h := web.FS(fstest.MapFS{
		".gitkeep":             {},
		"index.html":           {Data: []byte("<!doctype html><title>Cadre</title>")},
		"assets/index-AbC1.js": {Data: []byte("console.log(1)")},
		"favicon.svg":          {Data: []byte("<svg/>")},
	})
	for _, path := range []string{"/", "/t/x", "/t/x/tasks/y", "/index.html", "/assets/missing.js", "/.gitkeep"} {
		w := get(t, h, http.MethodGet, path)
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "<title>Cadre</title>") || w.Header().Get("Cache-Control") != "no-cache" {
			t.Errorf("%s: %d %q, Cache-Control %q; want index.html, no-cache", path, w.Code, w.Body.String(), w.Header().Get("Cache-Control"))
		}
	}
	w := get(t, h, http.MethodGet, "/assets/index-AbC1.js")
	if w.Code != http.StatusOK || w.Body.String() != "console.log(1)" || !strings.Contains(w.Header().Get("Cache-Control"), "max-age=31536000") {
		t.Errorf("hashed asset: %d %q, Cache-Control %q", w.Code, w.Body.String(), w.Header().Get("Cache-Control"))
	}
	if w := get(t, h, http.MethodGet, "/favicon.svg"); w.Code != http.StatusOK || w.Header().Get("Cache-Control") != "no-cache" {
		t.Errorf("an unhashed file: %d, Cache-Control %q", w.Code, w.Header().Get("Cache-Control"))
	}
	// What cadre-server serves itself is never the UI.
	for _, path := range []string{"/api/v1/nope", "/api", "/runner/v1/nope", "/mcp", "/mcp/nope", "/healthz", "/readyz"} {
		if w := get(t, h, http.MethodGet, path); w.Code != http.StatusNotFound {
			t.Errorf("%s: %d, want 404", path, w.Code)
		}
	}
	if w := get(t, h, http.MethodPost, "/t/x"); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /t/x: %d, want 405", w.Code)
	}
}

func TestWebHandlerNotBuilt(t *testing.T) {
	h := web.FS(fstest.MapFS{".gitkeep": {}})
	for _, path := range []string{"/", "/t/x/tasks/y"} {
		w := get(t, h, http.MethodGet, path)
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "make ui") || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/plain") {
			t.Errorf("%s: %d %q", path, w.Code, w.Body.String())
		}
	}
	if w := get(t, h, http.MethodGet, "/api/v1/nope"); w.Code != http.StatusNotFound {
		t.Errorf("/api/v1/nope: %d, want 404", w.Code)
	}
}

// Mounted beside other routes, as in cadre-server, the UI leaves their
// paths to the mux: a wrong method is its 405, an unknown path its 404.
func TestMountedBesideRoutes(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/tasks/{name}/cancel", func(http.ResponseWriter, *http.Request) {})
	mux.HandleFunc("GET /healthz", func(http.ResponseWriter, *http.Request) {})
	mux.Handle("/mcp/run", http.NotFoundHandler())
	web.RegisterFS(mux, web.FS(fstest.MapFS{"index.html": {Data: []byte("<title>Cadre</title>")}, "assets/a-1.js": {}}))
	for _, c := range []struct {
		method, path string
		code         int
	}{
		{http.MethodGet, "/api/v1/tasks/x/cancel", http.StatusMethodNotAllowed},
		{http.MethodPost, "/healthz", http.StatusMethodNotAllowed},
		{http.MethodGet, "/api/v1/nope", http.StatusNotFound},
		{http.MethodGet, "/nope", http.StatusNotFound},
		{http.MethodPost, "/t/x", http.StatusMethodNotAllowed},
		{http.MethodGet, "/", http.StatusOK},
		{http.MethodGet, "/t/x", http.StatusOK},
		{http.MethodHead, "/t/x/tasks/y", http.StatusOK},
		{http.MethodGet, "/assets/a-1.js", http.StatusOK},
	} {
		if w := get(t, mux, c.method, c.path); w.Code != c.code {
			t.Errorf("%s %s: %d, want %d", c.method, c.path, w.Code, c.code)
		}
	}
}

// The embedded dist serves either the built UI or the not-built page.
func TestEmbedded(t *testing.T) {
	w := get(t, web.Handler(), http.MethodGet, "/")
	body := w.Body.String()
	built, notBuilt := strings.Contains(body, `<div id="root">`), strings.Contains(body, "make ui")
	if w.Code != http.StatusOK || built == notBuilt {
		t.Errorf("/: %d %q", w.Code, body)
	}
}
