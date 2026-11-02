package api

import (
	"context"
	"encoding/json"
	"slices"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	"github.com/j75689/cadre/internal/auth"
)

// WithEvents tells each Team and AgentAccess write made through the API
// with a Normal Event on its object, through recorder, naming who made
// it. Without it no Event is emitted.
func WithEvents(recorder events.EventRecorder) Option {
	return func(s *Server) { s.recorder = recorder }
}

// emit records a Normal Event on regarding, if events are on. A recorder
// queues it and never fails the request: one the API server refuses is
// logged by the recorder alone.
func (s *Server) emit(regarding runtime.Object, reason, action, note string, args ...any) {
	if s.recorder != nil {
		s.recorder.Eventf(regarding, nil, corev1.EventTypeNormal, reason, action, note, args...)
	}
}

// caller is who made the request of ctx, for an Event's note.
func caller(ctx context.Context) string {
	if id, ok := auth.FromContext(ctx); ok {
		return id.Username
	}
	return "anonymous"
}

// changedFields names, by their JSON names and in order, the top-level
// fields of a Team spec that differ between a and b: never their values,
// as the roles name people.
func changedFields(a, b v1alpha1.TeamSpec) []string {
	fields := func(spec v1alpha1.TeamSpec) map[string]string {
		var m map[string]json.RawMessage
		raw, _ := json.Marshal(spec)
		_ = json.Unmarshal(raw, &m)
		out := make(map[string]string, len(m))
		for k, v := range m {
			out[k] = string(v)
		}
		return out
	}
	x, y := fields(a), fields(b)
	var changed []string
	for k := range x {
		if x[k] != y[k] {
			changed = append(changed, k)
		}
	}
	for k := range y {
		if _, ok := x[k]; !ok {
			changed = append(changed, k)
		}
	}
	slices.Sort(changed)
	return changed
}
