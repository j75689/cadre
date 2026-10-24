package auth

import (
	"net/http"

	"github.com/j75689/cadre/internal/httpjson"
)

// None is --auth=none: nobody logs in, and a request's author is what the
// client says. For local development and tests only.
type None struct{}

// Info implements Authenticator.
func (None) Info() Info { return Info{Mode: ModeNone} }

// Wrap implements Authenticator: nothing is checked.
func (None) Wrap(next http.Handler, _ string) http.Handler { return next }

// Register implements Authenticator.
func (n None) Register(mux *http.ServeMux, base string) {
	mux.HandleFunc("GET "+base+"/auth", func(w http.ResponseWriter, _ *http.Request) { httpjson.Write(w, http.StatusOK, n.Info()) })
}
