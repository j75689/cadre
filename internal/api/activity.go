package api

import (
	"cmp"
	"fmt"
	"net/http"
	"slices"

	eventsv1 "k8s.io/api/events/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	"github.com/j75689/cadre/internal/httpjson"
	"github.com/j75689/cadre/internal/storage"
)

// The Events the activity route reads, from the API server.
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=get;list;watch

// regardingCadre selects the Events about cadre's own objects.
var regardingCadre = client.MatchingFields{"regarding.apiVersion": v1alpha1.GroupVersion.String()}

// WithActivity serves a namespace's recent activity to its Team's admins:
// the Events about cadre's objects there, as r reads them, which should be
// the API server itself, not a cache. Without it the route is not served.
func WithActivity(r client.Reader) Option {
	return func(s *Server) { s.activity = r }
}

// listActivity answers a page of a namespace's Events about cadre's
// objects, newest first. Only admins get here: a note may name what a
// member is not shown, such as who rotated a token.
func (s *Server) listActivity(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	for _, name := range []string{"offset", "limit"} {
		if n := len(query[name]); n > 1 {
			httpjson.WriteError(w, http.StatusBadRequest, httpjson.CodeInvalid, fmt.Sprintf("%s given %d times: at most once", name, n))
			return
		}
	}
	var q storage.Query
	var ok bool
	if q.Offset, ok = intParam(w, query, "offset"); !ok {
		return
	}
	if q.Limit, ok = intParam(w, query, "limit"); !ok {
		return
	}
	q.Limit = ClampLimit(q.Limit)
	var list eventsv1.EventList
	switch err := s.activity.List(r.Context(), &list, client.InNamespace(r.PathValue("ns")), regardingCadre); {
	case apierrors.IsForbidden(err):
		s.log.Error("cadre-server's ClusterRole lacks list on events: upgrade its RBAC", "err", err)
		httpjson.WriteError(w, http.StatusInternalServerError, httpjson.CodeServerForbidden, "cadre-server may not list Events: its ClusterRole predates the activity route; upgrade it, and see its log")
		return
	case err != nil:
		httpjson.Fail(w, s.log, err)
		return
	}
	items := make([]Activity, 0, len(list.Items))
	for i := range list.Items {
		items = append(items, activity(&list.Items[i]))
	}
	slices.SortStableFunc(items, func(a, b Activity) int { return b.Time.Compare(a.Time) })
	items = storage.Page(q, items)
	if items == nil {
		items = []Activity{}
	}
	httpjson.Write(w, http.StatusOK, ActivityList{Items: items, Offset: q.Offset, Limit: q.Limit})
}

// activity is e as the route shows it: when it last happened, and how
// many times.
func activity(e *eventsv1.Event) Activity {
	out := Activity{
		Type: e.Type, Reason: e.Reason, Action: e.Action, Note: e.Note,
		Regarding: ActivityObject{Kind: e.Regarding.Kind, Name: e.Regarding.Name}, ReportedBy: e.ReportingController,
		Time: e.EventTime.Time, Count: max(e.DeprecatedCount, 1),
	}
	if out.Time.IsZero() { // an Event written through core/v1
		out.Time = cmp.Or(e.DeprecatedLastTimestamp.Time, e.CreationTimestamp.Time)
	}
	if e.Series != nil {
		out.Time, out.Count = e.Series.LastObservedTime.Time, e.Series.Count
	}
	return out
}
