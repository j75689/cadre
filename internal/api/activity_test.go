package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	"github.com/j75689/cadre/internal/api"
	"github.com/j75689/cadre/internal/authz"
	"github.com/j75689/cadre/internal/manager"
	"github.com/j75689/cadre/internal/storage/memory"
	"github.com/j75689/cadre/internal/testenv"
)

var activityStart = time.Date(2026, 11, 1, 5, 0, 0, 0, time.UTC)

// event is an Event in ns about obj, of apiVersion, at minute minute.
func event(ns, name, apiVersion, kind, obj, reason string, minute int) *eventsv1.Event {
	return &eventsv1.Event{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		EventTime:  metav1.NewMicroTime(activityStart.Add(time.Duration(minute) * time.Minute)),
		Regarding:  corev1.ObjectReference{APIVersion: apiVersion, Kind: kind, Namespace: ns, Name: obj},
		Type:       corev1.EventTypeNormal, Reason: reason, Action: "Act", Note: reason + " happened",
		ReportingController: "cadre-server", ReportingInstance: "cadre-server-0",
	}
}

// activityEvents are team-a's Events: about a Team, an AgentAccess, a
// Task, an Agent, a Schedule seen over and over, one written through
// core/v1, and a Pod's, which is not cadre's; and one in team-b.
func activityEvents() []client.Object {
	cadre := v1alpha1.GroupVersion.String()
	repeated := event("team-a", "e-schedule", cadre, "Schedule", "nightly", "MissedTicks", 0)
	repeated.Type = corev1.EventTypeWarning
	repeated.Series = &eventsv1.EventSeries{Count: 7, LastObservedTime: metav1.NewMicroTime(activityStart.Add(30 * time.Minute))}
	core := event("team-a", "e-agent", cadre, "Agent", "writer", "JobRefused", 0)
	core.EventTime = metav1.MicroTime{}
	core.DeprecatedLastTimestamp = metav1.NewTime(activityStart.Add(5 * time.Minute))
	core.DeprecatedCount = 3
	return []client.Object{
		event("team-a", "e-team", cadre, "Team", v1alpha1.TeamName, "TeamUpdated", 10),
		event("team-a", "e-access", cadre, "AgentAccess", "bot", "TokenRotated", 20),
		event("team-a", "e-task", cadre, "Task", "x", "Notified", 1),
		repeated, core,
		event("team-a", "e-pod", "v1", "Pod", "web", "Pulled", 40),
		event("team-b", "e-other", cadre, "Team", v1alpha1.TeamName, "TeamUpdated", 50),
	}
}

// activityCluster is the API server holding activityEvents, selecting by
// regarding.apiVersion as it does.
func activityCluster() *fake.ClientBuilder {
	return fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).WithObjects(activityEvents()...).
		WithIndex(&eventsv1.Event{}, "regarding.apiVersion", func(o client.Object) []string {
			return []string{o.(*eventsv1.Event).Regarding.APIVersion}
		})
}

func TestActivityByRole(t *testing.T) {
	e := startRolesWith(t, memory.NewBackend().Open(), nil, &authz.Policy{AdminUsers: []string{"Root@Example.test"}, FoldUsernames: true},
		api.WithActivity(activityCluster().Build()))
	admin := e.as("lead@example.test")
	root := e.as("root@example.test")
	for _, c := range []struct {
		name, token, path string
		code              int
		says              string
	}{
		{"no login", "", "/namespaces/team-a/activity", 401, "unauthenticated"},
		{"a viewer", e.as("eve@example.test", "readers"), "/namespaces/team-a/activity", 403, "needs the admin role"},
		{"a member", e.as("dana@example.test", "backend", "readers"), "/namespaces/team-a/activity", 403, "needs the admin role"},
		{"a member AgentAccess", "bot.member", "/namespaces/team-a/activity", 403, "needs the admin role"},
		{"a viewer AgentAccess", "root.agent", "/namespaces/team-a/activity", 403, "needs the admin role"},
		{"a stranger", e.as("mallory@example.test", "elsewhere"), "/namespaces/team-a/activity", 403, "no role there"},
		{"an admin elsewhere", admin, "/namespaces/team-b/activity", 403, "no role there"},
		{"a namespace without a Team", admin, "/namespaces/team-c/activity", 403, "no role there"},
		{"a bootstrap admin without a Team", root, "/namespaces/team-c/activity", 200, `{"items":[],"offset":0,"limit":100}`},
		{"a bootstrap admin", root, "/namespaces/team-b/activity", 200, `"reason":"TeamUpdated"`},
		{"an admin", admin, "/namespaces/team-a/activity", 200, `"reason":"TokenRotated"`},
	} {
		if code, out := e.do(c.token, "GET", c.path, ""); code != c.code || !strings.Contains(out, c.says) {
			t.Errorf("%s: %d %s, want %d %q", c.name, code, out, c.code, c.says)
		}
	}
}

// An admin gets the Events about cadre's objects, newest first by when
// each last happened, with how many times, and nothing else of them.
func TestActivityOrderAndFields(t *testing.T) {
	e := startRolesWith(t, memory.NewBackend().Open(), nil, &authz.Policy{}, api.WithActivity(activityCluster().Build()))
	code, out := e.do(e.as("lead@example.test"), "GET", "/namespaces/team-a/activity", "")
	if code != 200 {
		t.Fatalf("%d %s", code, out)
	}
	var got api.ActivityList
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatal(err)
	}
	var reasons []string
	for _, a := range got.Items {
		reasons = append(reasons, a.Reason)
	}
	if want := []string{"MissedTicks", "TokenRotated", "TeamUpdated", "JobRefused", "Notified"}; !slices.Equal(reasons, want) {
		t.Errorf("listed %v, want %v (the Pod's and team-b's left out): %s", reasons, want, out)
	}
	if want := (api.Activity{
		Time: activityStart.Add(30 * time.Minute), Type: "Warning", Reason: "MissedTicks", Action: "Act", Note: "MissedTicks happened",
		Regarding: api.ActivityObject{Kind: "Schedule", Name: "nightly"}, ReportedBy: "cadre-server", Count: 7,
	}); got.Items[0] != want {
		t.Errorf("a repeated Event %+v, want %+v", got.Items[0], want)
	}
	if a := got.Items[3]; !a.Time.Equal(activityStart.Add(5*time.Minute)) || a.Count != 3 {
		t.Errorf("a core/v1 Event %+v, want its last timestamp and count", a)
	}
	if a := got.Items[4]; a.Count != 1 {
		t.Errorf("a one-off Event's count %d, want 1", a.Count)
	}
	var raw struct{ Items []map[string]json.RawMessage }
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		t.Fatal(err)
	}
	var keys []string
	for k := range raw.Items[0] {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	if want := []string{"action", "count", "note", "reason", "regarding", "reportedBy", "time", "type"}; !slices.Equal(keys, want) {
		t.Errorf("an item's fields %v, want %v", keys, want)
	}
	if !strings.Contains(out, `"regarding":{"kind":"Schedule","name":"nightly"}`) {
		t.Errorf("regarding has more than its kind and name: %s", out)
	}
}

func TestActivityPages(t *testing.T) {
	e := startRolesWith(t, memory.NewBackend().Open(), nil, &authz.Policy{}, api.WithActivity(activityCluster().Build()))
	admin := e.as("lead@example.test")
	for _, c := range []struct {
		query string
		code  int
		says  []string
	}{
		{"?offset=1&limit=2", 200, []string{`"reason":"TokenRotated"`, `"reason":"TeamUpdated"`, `"offset":1,"limit":2}`}},
		{"?offset=4", 200, []string{`"reason":"Notified"`, `"offset":4,"limit":100}`}},
		{"?offset=9", 200, []string{`{"items":[],"offset":9,"limit":100}`}},
		{"?limit=9999", 200, []string{`"limit":500}`}},
		{"?limit=-1", 400, []string{"limit must be a non-negative integer"}},
		{"?offset=x", 400, []string{"offset must be a non-negative integer"}},
		{"?offset=1&offset=2", 400, []string{"offset given 2 times"}},
	} {
		code, out := e.do(admin, "GET", "/namespaces/team-a/activity"+c.query, "")
		if code != c.code {
			t.Errorf("%s: %d %s, want %d", c.query, code, out, c.code)
		}
		for _, s := range c.says {
			if !strings.Contains(out, s) {
				t.Errorf("%s: %s, want %q", c.query, out, s)
			}
		}
	}
	if _, out := e.do(admin, "GET", "/namespaces/team-a/activity?offset=1&limit=2", ""); strings.Count(out, `"reason"`) != 2 {
		t.Errorf("a page of 2: %s", out)
	}
}

// cadre-server's ClusterRole from before the route lacks list on Events: a
// mistake in its install, answered as one.
func TestActivityServerForbidden(t *testing.T) {
	forbidden := activityCluster().WithInterceptorFuncs(interceptor.Funcs{
		List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
			return apierrors.NewForbidden(schema.GroupResource{Group: "events.k8s.io", Resource: "events"}, "", errors.New("RBAC: list not allowed"))
		},
	}).Build()
	e := startRolesWith(t, memory.NewBackend().Open(), nil, &authz.Policy{}, api.WithActivity(forbidden))
	if code, out := e.do(e.as("lead@example.test"), "GET", "/namespaces/team-a/activity", ""); code != 500 || !strings.Contains(out, `"code":"server_forbidden"`) || strings.Contains(out, "RBAC") {
		t.Errorf("%d %s, want 500 server_forbidden without the API server's text", code, out)
	}
}

func TestActivityNotServedWithoutIt(t *testing.T) {
	e := startRolesWith(t, memory.NewBackend().Open(), nil, &authz.Policy{})
	if code, out := e.do(e.as("lead@example.test"), "GET", "/namespaces/team-a/activity", ""); code != 404 {
		t.Errorf("%d %s, want 404", code, out)
	}
}

// The API server itself selects the Events about cadre's objects, by the
// field selector the route sends.
func TestActivityFromTheAPIServer(t *testing.T) {
	cfg := testenv.APIServer(t)
	c, err := client.New(cfg, client.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil {
		t.Fatal(err)
	}
	cadre := v1alpha1.GroupVersion.String()
	for _, o := range []*eventsv1.Event{
		event(ns, "e-team", cadre, "Team", v1alpha1.TeamName, "TeamUpdated", 10),
		event(ns, "e-task", cadre, "Task", "x", "Notified", 20),
		event(ns, "e-pod", "v1", "Pod", "web", "Pulled", 30),
	} {
		if err := c.Create(ctx, o); err != nil {
			t.Fatal(err)
		}
	}
	mux := http.NewServeMux()
	api.NewServer(manager.New(memory.NewBackend().Open(), manager.Config{}), slog.New(slog.NewTextHandler(io.Discard, nil)), api.WithActivity(c)).Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	res, err := http.Get(srv.URL + api.Base + "/namespaces/" + ns + "/activity")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	var got api.ActivityList
	if err := json.NewDecoder(res.Body).Decode(&got); err != nil || res.StatusCode != 200 {
		t.Fatalf("%d %v", res.StatusCode, err)
	}
	var reasons []string
	for _, a := range got.Items {
		reasons = append(reasons, a.Reason)
	}
	if want := []string{"Notified", "TeamUpdated"}; !slices.Equal(reasons, want) {
		t.Errorf("listed %v, want %v", reasons, want)
	}
}
