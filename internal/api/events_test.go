package api_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	eventsv1 "k8s.io/api/events/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"
	"k8s.io/client-go/tools/reference"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	"github.com/j75689/cadre/internal/api"
	"github.com/j75689/cadre/internal/authz"
	"github.com/j75689/cadre/internal/storage/memory"
)

// recorded is an Event as a recorder would send it: what it is about, by
// reference only, and its words.
type recorded struct {
	Regarding                  string
	Type, Reason, Action, Note string
}

// recorder keeps the Events it is given, each regarding its object's
// reference, as the API server gets it.
type recorder struct {
	mu     sync.Mutex
	scheme *runtime.Scheme
	got    []recorded
}

func newRecorder(t *testing.T) *recorder {
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return &recorder{scheme: scheme}
}

func (r *recorder) Eventf(regarding, _ runtime.Object, typ, reason, action, note string, args ...any) {
	ref, err := reference.GetReference(r.scheme, regarding)
	if err != nil {
		panic(err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.got = append(r.got, recorded{Regarding: ref.APIVersion + " " + ref.Kind + " " + ref.Namespace + "/" + ref.Name, Type: typ, Reason: reason, Action: action, Note: fmt.Sprintf(note, args...)})
}

// take returns the Events recorded since it was last called.
func (r *recorder) take() []recorded {
	r.mu.Lock()
	defer r.mu.Unlock()
	got := r.got
	r.got = nil
	return got
}

// Creating a Team, and changing one, is told with an Event on it naming
// who did it and which fields changed: never whom the roles name.
func TestTeamEvents(t *testing.T) {
	rec := newRecorder(t)
	e := startRolesWith(t, memory.NewBackend().Open(), nil, &authz.Policy{AdminUsers: []string{"Root@Example.test"}, FoldUsernames: true}, api.WithEvents(rec))
	lead, member, root := e.as("lead@example.test"), e.as("dev@example.test", "backend"), e.as("root@example.test")
	team := "cadre.dev/v1alpha1 Team team-a/" + v1alpha1.TeamName

	read := func() (string, []v1alpha1.TeamRole) {
		t.Helper()
		var got v1alpha1.Team
		if code, body := e.do(lead, "GET", "/namespaces/team-a/team", ""); code != 200 || json.Unmarshal([]byte(body), &got) != nil {
			t.Fatalf("GET team: %d %s", code, body)
		}
		return got.ResourceVersion, got.Spec.Roles
	}
	put := func(token, ns string, req api.TeamRequest) int {
		t.Helper()
		body, _ := json.Marshal(req)
		code, _ := e.do(token, "PUT", "/namespaces/"+ns+"/team", string(body))
		return code
	}
	version, roles := read()
	newcomer := v1alpha1.TeamRole{Role: v1alpha1.RoleMember, Subjects: []v1alpha1.RoleSubject{{Kind: v1alpha1.SubjectUser, Name: "newcomer@example.test"}}}
	for _, c := range []struct {
		name, token, ns string
		req             func() api.TeamRequest
		code            int
		want            []recorded
	}{
		{"a bootstrap admin creates one", root, "team-c", func() api.TeamRequest { return api.TeamRequest{} }, 201,
			[]recorded{{"cadre.dev/v1alpha1 Team team-c/" + v1alpha1.TeamName, "Normal", "TeamCreated", "CreateTeam", "Team created by root@example.test"}}},
		{"creating it again", root, "team-c", func() api.TeamRequest { return api.TeamRequest{} }, 409, nil},
		// The Team was stored without a cap, which the edit defaults to 1.
		{"an admin changes its name and roles", lead, "team-a", func() api.TeamRequest {
			return api.TeamRequest{TeamSpec: v1alpha1.TeamSpec{DisplayName: "Team A", Roles: append(roles, newcomer)}, ResourceVersion: version}
		}, 200, []recorded{{team, "Normal", "TeamUpdated", "UpdateTeam", "Team displayName, maxConcurrentRuns, roles changed by lead@example.test"}}},
		{"an edit from the version before", lead, "team-a", func() api.TeamRequest {
			return api.TeamRequest{TeamSpec: v1alpha1.TeamSpec{MaxConcurrentRuns: 4}, ResourceVersion: version}
		}, 409, nil},
		{"a member's edit", member, "team-a", func() api.TeamRequest {
			v, r := read()
			return api.TeamRequest{TeamSpec: v1alpha1.TeamSpec{MaxConcurrentRuns: 4, Roles: r}, ResourceVersion: v}
		}, 403, nil},
		{"an edit that changes nothing", lead, "team-a", func() api.TeamRequest {
			v, r := read()
			return api.TeamRequest{TeamSpec: v1alpha1.TeamSpec{DisplayName: "Team A", Roles: r}, ResourceVersion: v}
		}, 200, nil},
		{"an admin changes the cap", lead, "team-a", func() api.TeamRequest {
			v, r := read()
			return api.TeamRequest{TeamSpec: v1alpha1.TeamSpec{DisplayName: "Team A", MaxConcurrentRuns: 4, Roles: r}, ResourceVersion: v}
		}, 200, []recorded{{team, "Normal", "TeamUpdated", "UpdateTeam", "Team maxConcurrentRuns changed by lead@example.test"}}},
		{"an admin removes its name", lead, "team-a", func() api.TeamRequest {
			v, r := read()
			return api.TeamRequest{TeamSpec: v1alpha1.TeamSpec{MaxConcurrentRuns: 4, Roles: r}, ResourceVersion: v}
		}, 200, []recorded{{team, "Normal", "TeamUpdated", "UpdateTeam", "Team displayName changed by lead@example.test"}}},
	} {
		code := put(c.token, c.ns, c.req())
		got := rec.take()
		if code != c.code || fmt.Sprint(got) != fmt.Sprint(c.want) {
			t.Errorf("%s: %d %v, want %d %v", c.name, code, got, c.code, c.want)
		}
		for _, ev := range got {
			for _, who := range []string{"newcomer", "backend", "readers"} {
				if strings.Contains(fmt.Sprint(ev), who) {
					t.Errorf("%s: an Event names %q: %+v", c.name, who, ev)
				}
			}
		}
	}
}

// Making, editing and deleting an AgentAccess is each told with an Event
// on it naming who did it: never with its token, nor its hash.
func TestAgentAccessEvents(t *testing.T) {
	rec := newRecorder(t)
	store := &fakeAccesses{objs: map[string]*v1alpha1.AgentAccess{}}
	e := startRolesWith(t, memory.NewBackend().Open(), nil, &authz.Policy{FoldUsernames: true}, api.WithAgentAccesses(store), api.WithEvents(rec))
	admin, member := e.as("lead@example.test"), e.as("dana@example.test", "backend")
	const path = "/namespaces/team-a/agentaccesses"
	// Deleted before, and held by a finalizer: a delete still answers 204.
	store.objs["team-a/held"] = &v1alpha1.AgentAccess{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "held", ResourceVersion: "1",
		DeletionTimestamp: &metav1.Time{Time: time.Now()}, Finalizers: []string{"example.test/hold"}}, Spec: v1alpha1.AgentAccessSpec{Role: "member"}}
	bot := "cadre.dev/v1alpha1 AgentAccess team-a/bot"
	for _, c := range []struct {
		name, token, method, path, body string
		code                            int
		want                            []recorded
	}{
		{"an admin makes one", admin, "PUT", path + "/bot", `{"spec":{"role":"member"}}`, 201,
			[]recorded{{bot, "Normal", "AccessCreated", "CreateAgentAccess", "AgentAccess bot created by lead@example.test"}}},
		{"making it again", admin, "PUT", path + "/bot", `{"spec":{"role":"member"}}`, 409, nil},
		{"one the CRD refuses", admin, "PUT", path + "/boss", `{"spec":{"role":"admin"}}`, 400, nil},
		{"a member may not make one", member, "PUT", path + "/mine", `{"spec":{"role":"member"}}`, 403, nil},
		{"an edit from a stale version", admin, "PUT", path + "/bot", `{"resourceVersion":"0","spec":{"role":"viewer"}}`, 409, nil},
		{"an edit from its version", admin, "PUT", path + "/bot", `{"resourceVersion":"1","spec":{"role":"viewer"}}`, 200,
			[]recorded{{bot, "Normal", "AccessUpdated", "UpdateAgentAccess", "AgentAccess bot updated by lead@example.test"}}},
		{"an edit that changes nothing", admin, "PUT", path + "/bot", `{"resourceVersion":"2","spec":{"role":"viewer"}}`, 200, nil},
		// Told by the store, which these fakes are not.
		{"a rotation", admin, "POST", path + "/bot/token", "", 200, nil},
		{"a member may not delete", member, "DELETE", path + "/bot", "", 403, nil},
		{"an admin deletes one", admin, "DELETE", path + "/bot", "", 204,
			[]recorded{{bot, "Normal", "AccessDeleted", "DeleteAgentAccess", "AgentAccess bot deleted by lead@example.test"}}},
		{"deleting it again", admin, "DELETE", path + "/bot", "", 404, nil},
		{"deleting one already being deleted", admin, "DELETE", path + "/held", "", 204, nil},
	} {
		code, out := e.do(c.token, c.method, c.path, c.body)
		got := rec.take()
		if code != c.code || fmt.Sprint(got) != fmt.Sprint(c.want) {
			t.Errorf("%s: %d %s %v, want %d %v", c.name, code, out, got, c.code, c.want)
		}
		for _, ev := range got {
			if s := fmt.Sprint(ev); strings.Contains(s, tokenHash) || strings.Contains(s, mintedToken) {
				t.Errorf("%s: an Event carries the token or its hash: %s", c.name, s)
			}
		}
	}
}

// failingSink is an API server refusing every Event, and counting them.
type failingSink struct{ calls atomic.Int32 }

func (f *failingSink) Create(context.Context, *eventsv1.Event) (*eventsv1.Event, error) {
	f.calls.Add(1)
	return nil, fmt.Errorf("events.events.k8s.io is forbidden")
}

func (f *failingSink) Update(context.Context, *eventsv1.Event) (*eventsv1.Event, error) {
	f.calls.Add(1)
	return nil, fmt.Errorf("events.events.k8s.io is forbidden")
}

func (f *failingSink) Patch(context.Context, *eventsv1.Event, []byte) (*eventsv1.Event, error) {
	f.calls.Add(1)
	return nil, fmt.Errorf("events.events.k8s.io is forbidden")
}

// An API server refusing the Events leaves every write's answer as it is.
func TestRefusedEventsKeepTheAnswer(t *testing.T) {
	sink := &failingSink{}
	broadcaster := events.NewBroadcaster(sink)
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop); broadcaster.Shutdown() })
	broadcaster.StartRecordingToSink(stop)
	store := &fakeAccesses{objs: map[string]*v1alpha1.AgentAccess{}}
	e := startRolesWith(t, memory.NewBackend().Open(), nil, &authz.Policy{AdminUsers: []string{"root@example.test"}, FoldUsernames: true},
		api.WithAgentAccesses(store), api.WithEvents(broadcaster.NewRecorder(newRecorder(t).scheme, "cadre-server")))
	root := e.as("root@example.test")
	for _, c := range []struct {
		method, path, body string
		code               int
	}{
		{"PUT", "/namespaces/team-c/team", `{}`, 201},
		{"PUT", "/namespaces/team-a/agentaccesses/bot", `{"spec":{"role":"member"}}`, 201},
		{"PUT", "/namespaces/team-a/agentaccesses/bot", `{"resourceVersion":"1","spec":{"role":"viewer"}}`, 200},
		{"DELETE", "/namespaces/team-a/agentaccesses/bot", "", 204},
	} {
		if code, out := e.do(root, c.method, c.path, c.body); code != c.code {
			t.Errorf("%s %s with Events refused: %d %s, want %d", c.method, c.path, code, out, c.code)
		}
	}
	for deadline := time.Now().Add(10 * time.Second); sink.calls.Load() == 0; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("no Event reached the API server: nothing was refused")
		}
	}
}
