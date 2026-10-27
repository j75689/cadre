package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	"github.com/j75689/cadre/internal/auth"
	"github.com/j75689/cadre/internal/httpjson"
	"github.com/j75689/cadre/internal/storage"
)

// AgentAccessStore keeps a namespace's AgentAccesses: the cluster's, through
// the controllers' client. It fails with storage.ErrNotFound,
// ErrAlreadyExists and ErrConflict as the store does, ErrInvalid, with the
// API server's reason, for an object the CRD refuses, and ErrNotPermitted
// when cadre-server itself may not.
type AgentAccessStore interface {
	ListAgentAccesses(ctx context.Context, namespace string) ([]v1alpha1.AgentAccess, error)
	// GetAgentAccess reads one as it is now, not as a cache last saw it:
	// a PUT decides from it whether it makes or edits.
	GetAgentAccess(ctx context.Context, namespace, name string) (*v1alpha1.AgentAccess, error)
	CreateAgentAccess(ctx context.Context, aa *v1alpha1.AgentAccess) error
	// UpdateAgentAccess writes the spec, conditionally on the version.
	UpdateAgentAccess(ctx context.Context, aa *v1alpha1.AgentAccess) error
	DeleteAgentAccess(ctx context.Context, namespace, name string) error
	// RotateAgentAccessToken mints a new token for an AgentAccess without
	// a ServiceAccount, replacing the one in its Secret, and returns it:
	// the only time it is ever read. by is who asked, for its Event. One
	// with a ServiceAccount, or whose Secret is not its own, is ErrInvalid.
	RotateAgentAccessToken(ctx context.Context, namespace, name, by string) (string, error)
}

// ErrNotPermitted means cadre-server's own RBAC refuses what the store was
// asked to do: a missing ClusterRole verb, which no retry fixes.
var ErrNotPermitted = errors.New("cadre-server may not do this")

// WithAgentAccesses lets a Team's admins manage its AgentAccesses through
// the API, kept in store. Without it, or without logins, the routes are not
// served.
func WithAgentAccesses(store AgentAccessStore) Option {
	return func(s *Server) { s.access = store }
}

func (s *Server) listAgentAccesses(w http.ResponseWriter, r *http.Request) {
	list, err := s.access.ListAgentAccesses(r.Context(), r.PathValue("ns"))
	if err != nil {
		s.failAccess(w, "", err)
		return
	}
	items := make([]AgentAccessView, 0, len(list))
	for i := range list {
		items = append(items, viewAccess(&list[i]))
	}
	httpjson.Write(w, http.StatusOK, AgentAccessList{Items: items})
}

func (s *Server) getAgentAccess(w http.ResponseWriter, r *http.Request) {
	aa, err := s.access.GetAgentAccess(r.Context(), r.PathValue("ns"), r.PathValue("name"))
	if err != nil {
		s.failAccess(w, r.PathValue("name"), err)
		return
	}
	httpjson.Write(w, http.StatusOK, viewAccess(aa))
}

// putAgentAccess makes an AgentAccess, or edits its spec, as putTeam does a
// Team: an edit names the version it was made from.
func (s *Server) putAgentAccess(w http.ResponseWriter, r *http.Request) {
	var req AgentAccessRequest
	if !httpjson.Decode(w, r, &req) {
		return
	}
	ns, name := r.PathValue("ns"), r.PathValue("name")
	cur, err := s.access.GetAgentAccess(r.Context(), ns, name)
	switch {
	case errors.Is(err, storage.ErrNotFound) && req.ResourceVersion != "":
		httpjson.WriteError(w, http.StatusConflict, httpjson.CodeConflict, fmt.Sprintf("AgentAccess %s was deleted since it was read: there is nothing to edit", name))
		return
	case errors.Is(err, storage.ErrNotFound):
		aa := &v1alpha1.AgentAccess{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}, Spec: req.Spec}
		if err := s.access.CreateAgentAccess(r.Context(), aa); err != nil {
			s.failAccess(w, name, err)
			return
		}
		httpjson.Write(w, http.StatusCreated, viewAccess(aa))
		return
	case err != nil:
		s.failAccess(w, name, err)
		return
	case req.ResourceVersion == "":
		httpjson.Fail(w, s.log, fmt.Errorf("agent access %s: %w", name, storage.ErrAlreadyExists))
		return
	}
	cur.Spec, cur.ResourceVersion = req.Spec, req.ResourceVersion
	if err := s.access.UpdateAgentAccess(r.Context(), cur); err != nil {
		s.failAccess(w, name, err)
		return
	}
	httpjson.Write(w, http.StatusOK, viewAccess(cur))
}

// rotateAgentAccessToken replaces an AgentAccess's minted token and answers
// with the new one: this answer is the only place it is ever shown, and
// it is never logged, nor stored: with the app's no-store pages, Chrome then
// keeps the page that received it out of its back/forward cache. The old
// token stops at once.
func (s *Server) rotateAgentAccessToken(w http.ResponseWriter, r *http.Request) {
	ns, name := r.PathValue("ns"), r.PathValue("name")
	by := "anonymous"
	if id, ok := auth.FromContext(r.Context()); ok {
		by = id.Username
	}
	tok, err := s.access.RotateAgentAccessToken(r.Context(), ns, name, by)
	switch {
	case errors.Is(err, storage.ErrConflict):
		// Its own message says what changed meanwhile.
		httpjson.WriteError(w, http.StatusConflict, httpjson.CodeConflict, err.Error())
		return
	case err != nil:
		s.failAccess(w, name, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	httpjson.Write(w, http.StatusOK, AgentAccessToken{Token: tok, TokenSecret: v1alpha1.AccessTokenSecretName(name)})
}

func (s *Server) deleteAgentAccess(w http.ResponseWriter, r *http.Request) {
	if err := s.access.DeleteAgentAccess(r.Context(), r.PathValue("ns"), r.PathValue("name")); err != nil {
		s.failAccess(w, r.PathValue("name"), err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// failAccess answers what the store refused. cadre-server's own RBAC
// refusing is logged, as a mistake in its install, and answered as one,
// not as something to retry.
func (s *Server) failAccess(w http.ResponseWriter, name string, err error) {
	switch {
	case errors.Is(err, storage.ErrConflict):
		httpjson.WriteError(w, http.StatusConflict, httpjson.CodeConflict, fmt.Sprintf("AgentAccess %s changed since it was read: read it again, and edit that", name))
	case errors.Is(err, ErrNotPermitted):
		s.log.Error("cadre-server's ClusterRole lacks a verb on agentaccesses", "err", err)
		httpjson.WriteError(w, http.StatusInternalServerError, httpjson.CodeServerForbidden, "cadre-server may not do this: its ClusterRole lacks the verb; see its log")
	default:
		httpjson.Fail(w, s.log, err)
	}
}

// viewAccess is aa as the API shows it: never its token, nor its hash.
func viewAccess(aa *v1alpha1.AgentAccess) AgentAccessView {
	out := AgentAccessView{Name: aa.Name, ResourceVersion: aa.ResourceVersion, Spec: aa.Spec, TokenSecret: aa.Status.TokenSecret, MintedAt: aa.Status.MintedAt}
	out.Ready, out.Reason, out.Message = readiness(aa.Status.ConfigStatus, aa.Generation)
	return out
}
