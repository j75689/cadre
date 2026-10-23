// Package mcp serves the per-run MCP endpoint, /mcp/run/<token>: the tools a
// run may use, executed on the Cadre side with the run's identity. The token
// is handed to the runner at claim, is bound to that one run and dies with
// it; every request resolves it on a fresh read of the task.
//
// The endpoint is stateless: no MCP session is kept between requests, so any
// cadre-server replica can serve any request, and every tool call is fenced
// on the run on its own.
package mcp

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	"github.com/j75689/cadre/internal/manager"
)

// PathPrefix is where the endpoint is served; the token follows it.
const PathPrefix = "/mcp/run/"

// Server serves the per-run MCP endpoint on a Task Manager.
type Server struct {
	m   *manager.Manager
	log *slog.Logger
	mcp http.Handler
}

// NewServer returns the endpoint.
func NewServer(m *manager.Manager, log *slog.Logger) *Server {
	s := &Server{m: m, log: log}
	s.mcp = sdk.NewStreamableHTTPHandler(s.serverFor, &sdk.StreamableHTTPOptions{Stateless: true, Logger: log})
	return s
}

// Register adds the endpoint to mux.
func (s *Server) Register(mux *http.ServeMux) {
	mux.Handle(PathPrefix, s)
}

type runKey struct{}

// runOf returns the task a request's token resolved to.
func runOf(ctx context.Context) *v1alpha1.Task {
	t, _ := ctx.Value(runKey{}).(*v1alpha1.Task)
	return t
}

// ServeHTTP resolves the token, then hands the request to the MCP handler
// with the run's task in its context. A token that names no active run is
// answered 404 whatever the reason, so a caller learns nothing about which
// part was wrong.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	token := strings.TrimPrefix(r.URL.Path, PathPrefix)
	if token == "" || strings.Contains(token, "/") {
		http.NotFound(w, r)
		return
	}
	t, err := s.m.RunByToken(r.Context(), token)
	switch {
	case errors.Is(err, manager.ErrBadToken):
		http.NotFound(w, r)
		return
	case err != nil:
		s.log.Warn("resolve MCP token", "err", err)
		http.Error(w, "temporarily unavailable; retry", http.StatusServiceUnavailable)
		return
	}
	s.mcp.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), runKey{}, t)))
}

// serverFor builds the MCP server for one request. The run's tools arrive
// with the next change; for now the endpoint only authenticates.
func (s *Server) serverFor(r *http.Request) *sdk.Server {
	name := "cadre"
	if t := runOf(r.Context()); t != nil && t.Status.Run != nil {
		name = "cadre run " + t.Status.Run.ID
	}
	return sdk.NewServer(&sdk.Implementation{Name: name, Version: v1alpha1.GroupVersion.Version}, nil)
}
