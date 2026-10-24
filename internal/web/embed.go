// Package web serves the board UI that web/ builds into dist, embedded in
// cadre-server. Without a built UI (dist holds only .gitkeep, as in a
// checkout) it serves a page saying how to build one.
package web

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

//go:embed all:dist
var dist embed.FS

// Reserved are the path prefixes cadre-server serves itself. A request under
// one that reaches the UI matched none of their routes, and is a 404, not
// the UI.
var Reserved = []string{"/api/", "/runner/", "/mcp/", "/healthz", "/readyz"}

// notBuilt is served at every UI path when dist holds no index.html.
const notBuilt = `The Cadre web UI is not built into this binary.

Build it with "make ui" (needs Node.js, see web/.nvmrc), then "make build",
or use the published cadre-server image, which carries it.
`

// Paths are the UI's own, the ones Register mounts: the app at / and under
// /t/, and the build's assets. Any other path stays the mux's, so a wrong
// method on one of cadre-server's routes is still its 405, and an unknown
// path its 404.
var Paths = []string{"/{$}", "/t/", "/assets/"}

// Register mounts the UI on mux, for GET (and so HEAD) at Paths.
func Register(mux *http.ServeMux) { RegisterFS(mux, Handler()) }

// RegisterFS mounts h, a UI handler, as Register does.
func RegisterFS(mux *http.ServeMux, h http.Handler) {
	for _, p := range Paths {
		mux.Handle("GET "+p, h)
	}
}

// Handler serves the UI from the embedded dist.
func Handler() http.Handler {
	root, err := fs.Sub(dist, "dist")
	if err != nil {
		panic(err) // dist is embedded by this package
	}
	return FS(root)
}

// FS serves the UI from root, a built dist.
func FS(root fs.FS) http.Handler {
	index, err := fs.ReadFile(root, "index.html")
	built := err == nil
	assets := http.FileServerFS(root)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, p := range Reserved {
			if strings.HasPrefix(r.URL.Path, p) || r.URL.Path == strings.TrimSuffix(p, "/") {
				http.NotFound(w, r)
				return
			}
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			// Mounted by Register, it sees none.
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if !built {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.Header().Set("Cache-Control", "no-cache")
			_, _ = w.Write([]byte(notBuilt))
			return
		}
		name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if name != "" && name != "index.html" && name != ".gitkeep" {
			if info, err := fs.Stat(root, name); err == nil && !info.IsDir() {
				// Vite names what it emits under assets/ after its
				// content: a file there never changes.
				if strings.HasPrefix(name, "assets/") {
					w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
				} else {
					w.Header().Set("Cache-Control", "no-cache")
				}
				assets.ServeHTTP(w, r)
				return
			}
		}
		// Any other path is a route of the app: index.html, never cached,
		// so a new build is picked up at once.
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		_, _ = w.Write(index)
	})
}
