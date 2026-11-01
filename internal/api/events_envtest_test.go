package api_test

import (
	"context"
	"encoding/json"
	"fmt"
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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	"github.com/j75689/cadre/internal/api"
	"github.com/j75689/cadre/internal/auth"
	"github.com/j75689/cadre/internal/authz"
	"github.com/j75689/cadre/internal/controller"
	"github.com/j75689/cadre/internal/manager"
	"github.com/j75689/cadre/internal/storage/memory"
	"github.com/j75689/cadre/internal/testenv"
)

// eventsCluster serves the API on an API server with Events on, its
// Team's admins given by roles, and answers do as the user named.
func eventsCluster(t *testing.T, ns string) (ctrlclient.Client, func(user, method, path, body string) string) {
	t.Helper()
	cfg := testenv.APIServer(t)
	mgr, err := controller.NewManager(cfg)
	if err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- mgr.Start(runCtx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("manager: %v", err)
		}
	})
	c, err := ctrlclient.New(cfg, ctrlclient.Options{Scheme: mgr.GetScheme()})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil {
		t.Fatal(err)
	}
	recorder := mgr.GetEventRecorder("cadre-server")
	mux := http.NewServeMux()
	api.NewServer(manager.New(memory.NewBackend().Open(), manager.Config{}), slog.New(slog.NewTextHandler(io.Discard, nil)),
		api.WithAccess(auth.ModeOIDC, &authz.Policy{AdminUsers: []string{"root@example.test"}}),
		api.WithAgentAccesses(controller.AgentAccessStore(mgr.GetClient(), mgr.GetAPIReader(), recorder)),
		api.WithEvents(recorder), api.WithActivity(mgr.GetAPIReader())).Register(mux)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mux.ServeHTTP(w, r.WithContext(auth.WithIdentity(r.Context(), &auth.Identity{Username: r.Header.Get("X-Test-User")})))
	}))
	t.Cleanup(srv.Close)
	return c, func(user, method, path, body string) string {
		t.Helper()
		req, _ := http.NewRequest(method, srv.URL+api.Base+"/namespaces/"+ns+path, strings.NewReader(body))
		req.Header.Set("X-Test-User", user)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = res.Body.Close() }()
		out, _ := io.ReadAll(res.Body)
		if res.StatusCode >= 300 {
			t.Fatalf("%s %s as %s: %d %s", method, path, user, res.StatusCode, out)
		}
		return string(out)
	}
}

// listEvents lists ns's Events once n are there, or after 10 s.
func listEvents(t *testing.T, c ctrlclient.Client, ns string, n int) []eventsv1.Event {
	t.Helper()
	var list eventsv1.EventList
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		if err := c.List(ctx, &list, ctrlclient.InNamespace(ns)); err != nil {
			t.Fatal(err)
		}
		if len(list.Items) >= n || time.Now().After(deadline) {
			return list.Items
		}
	}
}

// Writes through the API reach the cluster as events.k8s.io Events about
// the Team and the AgentAccess, reported by cadre-server, as the activity
// route lists them.
func TestEventsInTheCluster(t *testing.T) {
	const ns = "team-events"
	c, as := eventsCluster(t, ns)
	do := func(method, path, body string) string { t.Helper(); return as("root@example.test", method, path, body) }

	var team v1alpha1.Team
	if err := json.Unmarshal([]byte(do("PUT", "/team", `{}`)), &team); err != nil {
		t.Fatal(err)
	}
	do("PUT", "/team", `{"resourceVersion":"`+team.ResourceVersion+`","roles":[{"role":"admin","subjects":[{"kind":"user","name":"hidden-admin@example.test"}]}]}`)
	var aa api.AgentAccessView
	if err := json.Unmarshal([]byte(do("PUT", "/agentaccesses/bot", `{"spec":{"role":"member"}}`)), &aa); err != nil {
		t.Fatal(err)
	}
	do("PUT", "/agentaccesses/bot", `{"resourceVersion":"`+aa.ResourceVersion+`","spec":{"role":"viewer"}}`)
	do("DELETE", "/agentaccesses/bot", "")

	want := map[string]string{
		"TeamCreated":   "Team " + v1alpha1.TeamName + ": Team created by root@example.test",
		"TeamUpdated":   "Team " + v1alpha1.TeamName + ": Team roles changed by root@example.test",
		"AccessCreated": "AgentAccess bot: AgentAccess bot created by root@example.test",
		"AccessUpdated": "AgentAccess bot: AgentAccess bot updated by root@example.test",
		"AccessDeleted": "AgentAccess bot: AgentAccess bot deleted by root@example.test",
	}
	var reasons []string
	for _, ev := range listEvents(t, c, ns, len(want)) {
		reasons = append(reasons, ev.Reason)
		raw, _ := json.Marshal(ev)
		if ev.Type != corev1.EventTypeNormal || ev.ReportingController != "cadre-server" || ev.Regarding.APIVersion != "cadre.dev/v1alpha1" ||
			ev.Regarding.Kind+" "+ev.Regarding.Name+": "+ev.Note != want[ev.Reason] {
			t.Errorf("Event %s", raw)
		}
		if strings.Contains(string(raw), "hidden-admin") {
			t.Errorf("an Event names whom the roles name: %s", raw)
		}
	}
	slices.Sort(reasons)
	if strings.Join(reasons, " ") != "AccessCreated AccessDeleted AccessUpdated TeamCreated TeamUpdated" {
		t.Errorf("Events %v, want one of each", reasons)
	}
}

// Two users writing the same names, within the recorder's series window,
// each get an Event naming themselves, as does each AgentAccess made again:
// none is counted into another's.
func TestEventsByTwoUsersInTheCluster(t *testing.T) {
	const ns = "team-two-users"
	c, as := eventsCluster(t, ns)
	const lead, root = "lead@example.test", "root@example.test"
	var team v1alpha1.Team
	if err := json.Unmarshal([]byte(as(root, "PUT", "/team", `{"roles":[{"role":"admin","subjects":[{"kind":"user","name":"`+lead+`"}]}]}`)), &team); err != nil {
		t.Fatal(err)
	}
	roles := `"roles":[{"role":"admin","subjects":[{"kind":"user","name":"` + lead + `"}]}]`
	if err := json.Unmarshal([]byte(as(lead, "PUT", "/team", `{"resourceVersion":"`+team.ResourceVersion+`","displayName":"A",`+roles+`}`)), &team); err != nil {
		t.Fatal(err)
	}
	as(root, "PUT", "/team", `{"resourceVersion":"`+team.ResourceVersion+`","displayName":"B",`+roles+`}`)
	as(lead, "PUT", "/agentaccesses/bot", `{"spec":{"role":"member"}}`)
	as(lead, "DELETE", "/agentaccesses/bot", "")
	as(lead, "PUT", "/agentaccesses/bot", `{"spec":{"role":"member"}}`)
	as(root, "DELETE", "/agentaccesses/bot", "")

	want := []string{
		"AccessCreated AgentAccess bot created by " + lead + " 1",
		"AccessCreated AgentAccess bot created by " + lead + " 1",
		"AccessDeleted AgentAccess bot deleted by " + lead + " 1",
		"AccessDeleted AgentAccess bot deleted by " + root + " 1",
		"TeamCreated Team created by " + root + " 1",
		"TeamUpdated Team displayName changed by " + lead + " 1",
		"TeamUpdated Team displayName changed by " + root + " 1",
	}
	// As the activity route answers, once they are all there.
	listEvents(t, c, ns, len(want))
	var page api.ActivityList
	if err := json.Unmarshal([]byte(as(root, "GET", "/activity", "")), &page); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, a := range page.Items {
		got = append(got, fmt.Sprintf("%s %s %d", a.Reason, a.Note, a.Count))
	}
	if len(got) == 0 || got[0] != "AccessDeleted AgentAccess bot deleted by "+root+" 1" {
		t.Errorf("newest activity %q, want root's delete", got)
	}
	slices.Sort(got)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("Events:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}
