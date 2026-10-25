package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	"github.com/j75689/cadre/internal/httpjson"
	"github.com/j75689/cadre/internal/storage"
)

// AgentAccessStore keeps a namespace's AgentAccesses: the cluster's, through
// the controllers' client. It fails with storage.ErrNotFound,
// ErrAlreadyExists and ErrConflict as the store does, and ErrInvalid, with
// the API server's reason, for an object the CRD refuses.
type AgentAccessStore interface {
	ListAgentAccesses(ctx context.Context, namespace string) ([]v1alpha1.AgentAccess, error)
	GetAgentAccess(ctx context.Context, namespace, name string) (*v1alpha1.AgentAccess, error)
	CreateAgentAccess(ctx context.Context, aa *v1alpha1.AgentAccess) error
	// UpdateAgentAccess writes the spec, conditionally on the version.
	UpdateAgentAccess(ctx context.Context, aa *v1alpha1.AgentAccess) error
	DeleteAgentAccess(ctx context.Context, namespace, name string) error
}

// WithAgentAccesses lets a Team's admins manage its AgentAccesses through
// the API, kept in store. Without it the routes are not served.
func WithAgentAccesses(store AgentAccessStore) Option {
	return func(s *Server) { s.access = store }
}

func (s *Server) listAgentAccesses(w http.ResponseWriter, r *http.Request) {
	list, err := s.access.ListAgentAccesses(r.Context(), r.PathValue("ns"))
	if err != nil {
		httpjson.Fail(w, s.log, err)
		return
	}
	items := make([]AgentAccessView, 0, len(list))
	for i := range list {
		items = append(items, viewAccess(&list[i]))
	}
	httpjson.Write(w, http.StatusOK, AgentAccessList{Items: items})
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
		httpjson.Fail(w, s.log, err)
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

func (s *Server) deleteAgentAccess(w http.ResponseWriter, r *http.Request) {
	if err := s.access.DeleteAgentAccess(r.Context(), r.PathValue("ns"), r.PathValue("name")); err != nil {
		httpjson.Fail(w, s.log, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// failAccess answers a write the store refused.
func (s *Server) failAccess(w http.ResponseWriter, name string, err error) {
	if errors.Is(err, storage.ErrConflict) {
		httpjson.WriteError(w, http.StatusConflict, httpjson.CodeConflict, fmt.Sprintf("AgentAccess %s changed since it was read: read it again, and edit that", name))
		return
	}
	httpjson.Fail(w, s.log, err)
}

// viewAccess is aa as the API shows it: never its token, nor its hash. A
// Ready condition decided for an older spec says nothing about this one.
func viewAccess(aa *v1alpha1.AgentAccess) AgentAccessView {
	out := AgentAccessView{
		Name: aa.Name, ResourceVersion: aa.ResourceVersion, Spec: aa.Spec,
		Ready: aa.Status.ReadyFor(aa.Generation), TokenSecret: aa.Status.TokenSecret, MintedAt: aa.Status.MintedAt,
	}
	c := meta.FindStatusCondition(aa.Status.Conditions, v1alpha1.ConditionReady)
	switch {
	case c == nil || c.ObservedGeneration != aa.Generation || aa.Status.ObservedGeneration != aa.Generation:
		out.Reason, out.Message = "Pending", "its current spec has not been checked yet"
	case !out.Ready:
		out.Reason, out.Message = c.Reason, c.Message
	}
	return out
}
