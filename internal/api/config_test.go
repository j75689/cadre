package api_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	"github.com/j75689/cadre/internal/api"
	"github.com/j75689/cadre/internal/auth"
	"github.com/j75689/cadre/internal/authz"
	"github.com/j75689/cadre/internal/manager"
	"github.com/j75689/cadre/internal/storage/memory"
)

// configCluster is a cluster's Agents, ModelProviders, ToolServers,
// Schedules, Notifiers and Runners as the controllers' cache holds them: writer and claude as kubectl apply
// leaves them, reviewer, gpt and execer edited since they were checked,
// gateway and remote with a credential in their URL, prober with some in
// its path and readiness probe, and elsewhere in team-b.
func configCluster(t *testing.T) api.Option {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	writer := manager.ReadyAgent("team-a", "writer", "claude", v1alpha1.AgentRuntime{Type: v1alpha1.RuntimeBuiltin})
	writer.Spec.Instructions = "write the docs"
	writer.Labels = map[string]string{"tier": "one"}
	writer.Annotations = map[string]string{
		"cadre.dev/owner": "docs-team",
		"kubectl.kubernetes.io/last-applied-configuration": `{"spec":{"instructions":"write the docs"}}`,
	}
	writer.ManagedFields = []metav1.ManagedFieldsEntry{{Manager: "kubectl-client-side-apply", Operation: metav1.ManagedFieldsOperationUpdate, APIVersion: "cadre.dev/v1alpha1", FieldsType: "FieldsV1", FieldsV1: metav1.NewFieldsV1(`{"f:spec":{"f:instructions":{}}}`)}}
	reviewer := manager.ReadyAgent("team-a", "reviewer", "claude", v1alpha1.AgentRuntime{Type: v1alpha1.RuntimeBuiltin})
	reviewer.Generation = 2
	elsewhere := manager.ReadyAgent("team-b", "elsewhere", "claude", v1alpha1.AgentRuntime{Type: v1alpha1.RuntimeBuiltin})
	runner := corev1.Container{
		Name: "runner", Image: "runner:1", Command: []string{"run", "--token=ghp_literal_command"}, Args: []string{"--key=ghp_literal_arg"},
		Env: []corev1.EnvVar{
			{Name: "GITHUB_TOKEN", Value: "ghp_literal_secret"},
			{Name: "API_KEY", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "api"}, Key: "key"}}},
		},
	}
	setup := corev1.Container{Name: "setup", Image: "setup:1", Command: []string{"clone", "https://x:ghp_literal_init@example.test"}, Env: []corev1.EnvVar{{Name: "NETRC", Value: "ghp_literal_init_env"}}}
	jobber := manager.ReadyAgent("team-a", "jobber", "claude", v1alpha1.AgentRuntime{Type: v1alpha1.RuntimeRunner, Job: &v1alpha1.AgentJob{
		RunnerRef: "pool", Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{InitContainers: []corev1.Container{setup}, Containers: []corev1.Container{runner}}},
	}})
	provider := func(ns, name string, spec v1alpha1.ModelProviderSpec) *v1alpha1.ModelProvider {
		p := &v1alpha1.ModelProvider{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Generation: 1}, Spec: spec}
		p.Status.ObservedGeneration = 1
		p.Status.Conditions = []metav1.Condition{{Type: v1alpha1.ConditionReady, Status: metav1.ConditionTrue, ObservedGeneration: 1, Reason: "Resolved"}}
		return p
	}
	key := &v1alpha1.Auth{SecretRef: v1alpha1.SecretKeyRef{Name: "anthropic", Key: "api-key"}}
	claude := provider("team-a", "claude", v1alpha1.ModelProviderSpec{Type: v1alpha1.ProviderAnthropic, Model: "claude-opus-5-5", Auth: key, BaseURL: "https://llm.example.test/v1"})
	claude.Labels = writer.Labels
	claude.Annotations = writer.Annotations
	claude.ManagedFields = []metav1.ManagedFieldsEntry{{Manager: "kubectl-client-side-apply", Operation: metav1.ManagedFieldsOperationUpdate, APIVersion: "cadre.dev/v1alpha1", FieldsType: "FieldsV1", FieldsV1: metav1.NewFieldsV1(`{"f:spec":{"f:model":{}}}`)}}
	gpt := provider("team-a", "gpt", v1alpha1.ModelProviderSpec{Type: v1alpha1.ProviderOpenAI, Model: "gpt", Auth: key})
	gpt.Generation = 2
	gateway := provider("team-a", "gateway", v1alpha1.ModelProviderSpec{Type: v1alpha1.ProviderOpenAI, Model: "gpt", Auth: key, BaseURL: "https://bot:sk_literal_user@gw.example.test/v1?api-key=sk_literal_query&sk_literal_bare"})
	fragment := provider("team-a", "fragment", v1alpha1.ModelProviderSpec{Type: v1alpha1.ProviderOpenAI, Model: "gpt", Auth: key, BaseURL: "https://gw.example.test/v1#?k=sk_literal_fragment"})
	badurl := provider("team-a", "badurl", v1alpha1.ModelProviderSpec{Type: v1alpha1.ProviderOpenAI, Model: "gpt", Auth: key, BaseURL: "https://u:sk_literal_bad@gw.example.test/%zz?k=sk_literal_bad"})
	nohost := provider("team-a", "nohost", v1alpha1.ModelProviderSpec{Type: v1alpha1.ProviderOpenAI, Model: "gpt", Auth: key, BaseURL: "https://u:sk_literal_nohost@"})
	vertex := provider("team-b", "vertex", v1alpha1.ModelProviderSpec{Type: v1alpha1.ProviderVertex, Model: "gemini", WorkloadIdentity: true, Project: "p", Location: "us-east5"})
	return api.WithConfig(fake.NewClientBuilder().WithScheme(scheme).WithObjects(writer, reviewer, elsewhere, jobber, claude, gpt, gateway, fragment, badurl, nohost, vertex).
		WithObjects(toolServers()...).WithObjects(schedules()...).WithObjects(notifiers()...).WithObjects(runners()...).Build())
}

// schedules are daily, as kubectl apply leaves it, with no assignee;
// nightly, edited since it was checked; paused, suspended; held, suspended
// and edited since; and yonder in team-b.
func schedules() []client.Object {
	schedule := func(ns, name string, spec v1alpha1.ScheduleSpec) *v1alpha1.Schedule {
		s := &v1alpha1.Schedule{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Generation: 1}, Spec: spec}
		s.Status.ObservedGeneration = 1
		s.Status.Conditions = []metav1.Condition{{Type: v1alpha1.ConditionReady, Status: metav1.ConditionTrue, ObservedGeneration: 1, Reason: "Scheduled"}}
		return s
	}
	daily := schedule("team-a", "daily", v1alpha1.ScheduleSpec{Cron: "@daily", Task: v1alpha1.TaskSpec{Title: "Today's thread", Description: "post the daily thread"}})
	daily.Status.LastTask = "daily-202610310000"
	daily.Labels = map[string]string{"tier": "one"}
	daily.Annotations = map[string]string{"cadre.dev/owner": "ops-team", "kubectl.kubernetes.io/last-applied-configuration": `{"spec":{"cron":"@daily"}}`}
	daily.ManagedFields = []metav1.ManagedFieldsEntry{{Manager: "kubectl-client-side-apply", Operation: metav1.ManagedFieldsOperationUpdate, APIVersion: "cadre.dev/v1alpha1", FieldsType: "FieldsV1", FieldsV1: metav1.NewFieldsV1(`{"f:spec":{"f:cron":{}}}`)}}
	nightly := schedule("team-a", "nightly", v1alpha1.ScheduleSpec{Cron: "0 2 * * *", Task: v1alpha1.TaskSpec{Title: "Nightly sweep"}})
	nightly.Generation = 2
	writer := &v1alpha1.Assignee{Kind: v1alpha1.AssigneeAgent, Name: "writer"}
	paused := schedule("team-a", "paused", v1alpha1.ScheduleSpec{Cron: "@hourly", Suspend: true, Task: v1alpha1.TaskSpec{Title: "Hourly", Assignee: writer}})
	paused.Status.Conditions[0].Reason = "Suspended"
	held := schedule("team-a", "held", v1alpha1.ScheduleSpec{Cron: "@hourly", Suspend: true, Task: v1alpha1.TaskSpec{Title: "Held", Assignee: &v1alpha1.Assignee{Kind: v1alpha1.AssigneeUser, Name: "dana@example.test"}}})
	held.Status.Conditions[0].Reason = "Suspended"
	held.Generation = 2
	yonder := schedule("team-b", "yonder", v1alpha1.ScheduleSpec{Cron: "@weekly", Task: v1alpha1.TaskSpec{Title: "Weekly"}})
	return []client.Object{nightly, daily, paused, held, yonder}
}

// notifiers are hook, a webhook with values in its URL's query and fragment
// and in its headers, as kubectl apply leaves it; caller, a tool with values
// in its arguments, edited since it was checked; badhook, whose URL does not
// parse; and distant in team-b.
func notifiers() []client.Object {
	notifier := func(ns, name string, spec v1alpha1.NotifierSpec) *v1alpha1.Notifier {
		n := &v1alpha1.Notifier{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Generation: 1}, Spec: spec}
		n.Status.ObservedGeneration = 1
		n.Status.Conditions = []metav1.Condition{{Type: v1alpha1.ConditionReady, Status: metav1.ConditionTrue, ObservedGeneration: 1, Reason: "Ready"}}
		return n
	}
	on := []v1alpha1.NotifyEvent{v1alpha1.NotifyQuestion, v1alpha1.NotifyDone}
	hook := notifier("team-a", "hook", v1alpha1.NotifierSpec{On: on, Webhook: &v1alpha1.Webhook{
		URL:       "https://hooks.example.test/notify?token=nf_literal_query&nf_literal_bare#nf_literal_fragment",
		SecretRef: &v1alpha1.SecretKeyRef{Name: "hook", Key: "signing"},
		Headers:   map[string]string{"X-Api-Key": "nf_literal_header", "X-Tenant": "nf_literal_tenant"},
	}})
	hook.Status.LastSent = &metav1.Time{Time: time.Date(2026, 10, 31, 9, 0, 0, 0, time.UTC)}
	hook.Labels = map[string]string{"tier": "one"}
	hook.Annotations = map[string]string{"cadre.dev/owner": "ops-team", "kubectl.kubernetes.io/last-applied-configuration": `{"spec":{"on":["Done"]}}`}
	hook.ManagedFields = []metav1.ManagedFieldsEntry{{Manager: "kubectl-client-side-apply", Operation: metav1.ManagedFieldsOperationUpdate, APIVersion: "cadre.dev/v1alpha1", FieldsType: "FieldsV1", FieldsV1: metav1.NewFieldsV1(`{"f:spec":{"f:on":{}}}`)}}
	caller := notifier("team-a", "caller", v1alpha1.NotifierSpec{On: on, ToolServer: &v1alpha1.NotifierToolServer{
		Name: "remote", Tool: "notify", Arguments: map[string]string{"channel": "nf_literal_channel", "token": "nf_literal_arg"},
	}})
	caller.Generation = 2
	badhook := notifier("team-a", "badhook", v1alpha1.NotifierSpec{On: on, Webhook: &v1alpha1.Webhook{URL: "https://hooks.example.test/%zz?k=nf_literal_bad"}})
	distant := notifier("team-b", "distant", v1alpha1.NotifierSpec{On: on, Webhook: &v1alpha1.Webhook{URL: "https://hooks.example.test/notify"}})
	return []client.Object{hook, caller, badhook, distant}
}

// runnerHash is pool's token hash, which no role is shown.
const runnerHash = "5e1f00d0c0ffee5e1f00d0c0ffee5e1f00d0c0ffee5e1f00d0c0ffee5e1f00d0"

// runners are pool, with a minted token, as kubectl apply leaves it; ci, on
// a ServiceAccount, edited since it was checked; and offsite in team-b.
func runners() []client.Object {
	runner := func(ns, name string, spec v1alpha1.RunnerSpec) *v1alpha1.Runner {
		rn := &v1alpha1.Runner{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Generation: 1}, Spec: spec}
		rn.Status.ObservedGeneration = 1
		rn.Status.Conditions = []metav1.Condition{{Type: v1alpha1.ConditionReady, Status: metav1.ConditionTrue, ObservedGeneration: 1, Reason: "TokenMinted"}}
		return rn
	}
	pool := runner("team-a", "pool", v1alpha1.RunnerSpec{Labels: map[string]string{"pool": "gpu"}})
	pool.Status.TokenSecret, pool.Status.TokenHash = "pool-token", runnerHash
	pool.Status.MintedAt = &metav1.Time{Time: time.Date(2026, 10, 31, 9, 0, 0, 0, time.UTC)}
	pool.Labels = map[string]string{"tier": "one"}
	pool.Annotations = map[string]string{"cadre.dev/owner": "ops-team", "kubectl.kubernetes.io/last-applied-configuration": `{"spec":{"labels":{"pool":"gpu"}}}`}
	pool.ManagedFields = []metav1.ManagedFieldsEntry{{Manager: "kubectl-client-side-apply", Operation: metav1.ManagedFieldsOperationUpdate, APIVersion: "cadre.dev/v1alpha1", FieldsType: "FieldsV1", FieldsV1: metav1.NewFieldsV1(`{"f:spec":{"f:labels":{}}}`)}}
	ci := runner("team-a", "ci", v1alpha1.RunnerSpec{ServiceAccount: &v1alpha1.ServiceAccountRef{Name: "ci-runner"}, Labels: map[string]string{"pool": "ci"}})
	ci.Generation = 2
	offsite := runner("team-b", "offsite", v1alpha1.RunnerSpec{Labels: map[string]string{"pool": "cpu"}})
	return []client.Object{pool, ci, offsite}
}

// slackURL holds its token in its path, as Slack's and Discord's do.
const slackURL = "https://hooks.slack.com/services/T0000/B0000/XXSECRETXX"

// webhooks are slack, whose last delivery failed with an error quoting its
// URL; rooted, on a port with a path of "/"; and bare, with no path.
func webhooks() []client.Object {
	webhook := func(name, url string) *v1alpha1.Notifier {
		return &v1alpha1.Notifier{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: name}, Spec: v1alpha1.NotifierSpec{On: []v1alpha1.NotifyEvent{v1alpha1.NotifyDone}, Webhook: &v1alpha1.Webhook{URL: url}}}
	}
	slack := webhook("slack", slackURL)
	slack.Status.LastError = &v1alpha1.DeliveryError{Message: `Post "` + slackURL + `": context deadline exceeded`, At: metav1.Time{Time: time.Date(2026, 10, 31, 9, 0, 0, 0, time.UTC)}}
	return []client.Object{slack, webhook("rooted", "https://hooks.example.test:8443/?k=nf_literal_root"), webhook("bare", "https://hooks.example.test:8443")}
}

// toolServers are remote, an external ToolServer with a credential in its
// URL and an empty auth prefix, as kubectl apply leaves it; prober and
// execer, managed ones with some in their path and readiness probes, execer
// edited since it was checked and with some in its auth prefix; slashy,
// whose path reads as user info; grpc, whose probe holds none, with a
// Bearer prefix; keyed, whose prefix is one word and a space but not a
// scheme; and faraway in team-b.
func toolServers() []client.Object {
	server := func(ns, name string, spec v1alpha1.ToolServerSpec) *v1alpha1.ToolServer {
		ts := &v1alpha1.ToolServer{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Generation: 1}, Spec: spec}
		ts.Status.ObservedGeneration = 1
		ts.Status.Conditions = []metav1.Condition{{Type: v1alpha1.ConditionReady, Status: metav1.ConditionTrue, ObservedGeneration: 1, Reason: "Connected"}}
		return ts
	}
	remote := server("team-a", "remote", v1alpha1.ToolServerSpec{URL: "https://bot:tk_literal_user@mcp.example.test/mcp?key=tk_literal_query", Auth: &v1alpha1.ToolServerAuth{SecretRef: v1alpha1.SecretKeyRef{Name: "mcp", Key: "token"}, Prefix: ptr.To("")}})
	remote.Annotations = map[string]string{"cadre.dev/owner": "tools-team", "kubectl.kubernetes.io/last-applied-configuration": `{"spec":{"url":"https://mcp.example.test"}}`}
	remote.ManagedFields = []metav1.ManagedFieldsEntry{{Manager: "kubectl-client-side-apply", Operation: metav1.ManagedFieldsOperationUpdate, APIVersion: "cadre.dev/v1alpha1", FieldsType: "FieldsV1", FieldsV1: metav1.NewFieldsV1(`{"f:spec":{"f:url":{}}}`)}}
	prober := server("team-a", "prober", v1alpha1.ToolServerSpec{Managed: &v1alpha1.ManagedServer{Image: "mcp:1", Port: 8080, Path: "/mcp?key=tk_literal_path", ReadinessProbe: &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/healthz?token=tk_literal_probe_path", Port: intstr.FromInt32(8080), HTTPHeaders: []corev1.HTTPHeader{{Name: "X-Probe-Key", Value: "tk_literal_header"}, {Name: "X-Probe-Tenant", Value: "tk_literal_tenant"}}}},
	}}})
	prober.Status.URL = "http://prober.team-a.svc:8080/mcp?key=tk_literal_status"
	execer := server("team-a", "execer", v1alpha1.ToolServerSpec{Managed: &v1alpha1.ManagedServer{Image: "mcp:1", Port: 8080, ReadinessProbe: &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{"check", "--token=tk_literal_exec"}}},
	}}, Auth: &v1alpha1.ToolServerAuth{SecretRef: v1alpha1.SecretKeyRef{Name: "mcp", Key: "token"}, Header: "X-Api-Key", Prefix: ptr.To("tenant-42:tk_literal_prefix.")}})
	execer.Generation = 2
	slashy := server("team-a", "slashy", v1alpha1.ToolServerSpec{Managed: &v1alpha1.ManagedServer{Image: "mcp:1", Port: 8080, Path: "//bot:tk_literal_slashy@evil/mcp"}})
	slashy.Status.URL = "http://slashy.team-a.svc:8080//bot:tk_literal_slashy@evil/mcp"
	grpc := server("team-a", "grpc", v1alpha1.ToolServerSpec{Managed: &v1alpha1.ManagedServer{Image: "mcp:1", Port: 8080, ReadinessProbe: &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{GRPC: &corev1.GRPCAction{Port: 8080, Service: ptr.To("mcp.health")}},
	}}, Auth: &v1alpha1.ToolServerAuth{SecretRef: v1alpha1.SecretKeyRef{Name: "mcp", Key: "token"}, Prefix: ptr.To("Bearer ")}})
	keyed := server("team-a", "keyed", v1alpha1.ToolServerSpec{URL: "https://mcp.example.test/mcp", Auth: &v1alpha1.ToolServerAuth{SecretRef: v1alpha1.SecretKeyRef{Name: "mcp", Key: "token"}, Prefix: ptr.To("sk_tk_literal_keyed9 ")}})
	faraway := server("team-b", "faraway", v1alpha1.ToolServerSpec{URL: "https://mcp.example.test/mcp"})
	return []client.Object{remote, prober, execer, slashy, grpc, keyed, faraway}
}

// startConfig serves the API, with Teams in team-a and team-b, to callers
// named by the X-User header, in the groups of X-Groups; X-User
// agentaccess/<name> is team-a's AgentAccess of that name, with the role
// X-Groups names.
func startConfig(t *testing.T, opts ...api.Option) func(user, groups, path string) (int, string) {
	t.Helper()
	store := memory.NewBackend().Open()
	roles := []v1alpha1.TeamRole{
		{Role: v1alpha1.RoleViewer, Subjects: []v1alpha1.RoleSubject{{Kind: v1alpha1.SubjectGroup, Name: "readers"}}},
		{Role: v1alpha1.RoleMember, Subjects: []v1alpha1.RoleSubject{{Kind: v1alpha1.SubjectGroup, Name: "backend"}}},
		{Role: v1alpha1.RoleAdmin, Subjects: []v1alpha1.RoleSubject{{Kind: v1alpha1.SubjectUser, Name: "lead@example.test"}}},
	}
	for _, n := range []string{"team-a", "team-b"} {
		if err := store.CreateTeam(ctx, &v1alpha1.Team{ObjectMeta: metav1.ObjectMeta{Namespace: n, Name: v1alpha1.TeamName}, Spec: v1alpha1.TeamSpec{Roles: roles}}); err != nil {
			t.Fatal(err)
		}
	}
	m := manager.New(store, manager.Config{Agents: manager.StaticAgents{Agents: map[string]*v1alpha1.Agent{}}})
	mux := http.NewServeMux()
	api.NewServer(m, slog.New(slog.NewTextHandler(io.Discard, nil)), opts...).Register(mux)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u := r.Header.Get("X-User"); u != "" {
			id := &auth.Identity{Username: u, Groups: strings.Split(r.Header.Get("X-Groups"), ",")}
			if name, ok := strings.CutPrefix(u, "agentaccess/"); ok {
				id = auth.AgentIdentity("team-a", name, r.Header.Get("X-Groups"))
			}
			r = r.WithContext(auth.WithIdentity(r.Context(), id))
		}
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return func(user, groups, path string) (int, string) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, srv.URL+api.Base+path, nil)
		req.Header.Set("X-User", user)
		req.Header.Set("X-Groups", groups)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = res.Body.Close() }()
		out, _ := io.ReadAll(res.Body)
		return res.StatusCode, string(out)
	}
}

func TestGetAgent(t *testing.T) {
	get := startConfig(t, configCluster(t))
	for _, c := range []struct {
		name, path string
		code       int
	}{
		{"a namespace without a Team", "/namespaces/team-c/agents/writer", 404},
		{"a namespace that cannot be one", "/namespaces/Team_A/agents/writer", 400},
		{"an Agent that is not there", "/namespaces/team-a/agents/nope", 404},
		{"another namespace's Agent", "/namespaces/team-a/agents/elsewhere", 404},
		{"in its own namespace", "/namespaces/team-b/agents/elsewhere", 200},
	} {
		if code, body := get("", "", c.path); code != c.code {
			t.Errorf("%s: %d %s, want %d", c.name, code, body, c.code)
		}
	}

	code, body := get("", "", "/namespaces/team-a/agents/reviewer")
	var got api.ConfigResource
	if err := json.Unmarshal([]byte(body), &got); err != nil || code != 200 {
		t.Fatalf("reviewer: %d %s", code, body)
	}
	want := api.ConfigSummary{Name: "reviewer", Reason: "Pending", Message: "its current spec has not been checked yet"}
	if got.ConfigSummary != want || got.Kind != "Agent" || got.Generation != 2 || got.ObservedGeneration != 1 || len(got.Conditions) != 1 {
		t.Errorf("an Agent edited since it was checked: %s", body)
	}
}

// What each role sees of an Agent; nobody sees its fields' owners, nor the
// spec kubectl last applied.
func TestAgentByRole(t *testing.T) {
	policy := &authz.Policy{FoldUsernames: true}
	withRoles := startConfig(t, api.WithAccess(auth.ModeOIDC, policy), configCluster(t))
	withoutLogins := startConfig(t, configCluster(t))
	for _, c := range []struct {
		name                              string
		get                               func(user, groups, path string) (int, string)
		user, groups                      string
		spec, status, annotations, labels bool
	}{
		{"a viewer", withRoles, "eve@example.test", "readers", false, false, false, true},
		{"a member", withRoles, "dana@example.test", "backend", true, true, true, true},
		{"an admin", withRoles, "lead@example.test", "", true, true, true, true},
		{"--auth=none", withoutLogins, "", "", true, true, true, true},
	} {
		code, body := c.get(c.user, c.groups, "/namespaces/team-a/agents/writer")
		var got api.ConfigResource
		if err := json.Unmarshal([]byte(body), &got); err != nil || code != 200 {
			t.Fatalf("%s: %d %s", c.name, code, body)
		}
		if !got.Ready || len(got.Conditions) != 1 || got.Metadata.ResourceVersion == "" {
			t.Errorf("%s: no health: %s", c.name, body)
		}
		if (got.Spec != nil) != c.spec || (got.Status != nil) != c.status || (got.Metadata.Annotations != nil) != c.annotations || (got.Metadata.Labels != nil) != c.labels {
			t.Errorf("%s: spec %v, status %v, annotations %v, labels %v: %s", c.name, got.Spec != nil, got.Status != nil, got.Metadata.Annotations != nil, got.Metadata.Labels != nil, body)
		}
		if c.spec && (!strings.Contains(body, "write the docs") || !strings.Contains(body, `"cadre.dev/owner":"docs-team"`)) {
			t.Errorf("%s: spec or annotations missing: %s", c.name, body)
		}
		for _, never := range []string{"last-applied", "managedFields", "f:instructions"} {
			if strings.Contains(body, never) {
				t.Errorf("%s sees %q: %s", c.name, never, body)
			}
		}
	}
	if code, body := withRoles("mallory@example.test", "", "/namespaces/team-a/agents/writer"); code != 403 {
		t.Errorf("a stranger: %d %s", code, body)
	}
}

// A member sees that an Agent's job template sets env values, commands and
// arguments, not what they are; an admin sees them.
func TestAgentJobTemplateByRole(t *testing.T) {
	policy := &authz.Policy{FoldUsernames: true}
	get := startConfig(t, api.WithAccess(auth.ModeOIDC, policy), configCluster(t))
	literals := []string{"ghp_literal_secret", "ghp_literal_command", "ghp_literal_arg", "ghp_literal_init", "ghp_literal_init_env"}
	for _, c := range []struct {
		name, user, groups string
		masked             bool
	}{
		{"a member", "dana@example.test", "backend", true},
		{"a member AgentAccess", "agentaccess/bot", "member", true},
		{"an admin", "lead@example.test", "", false},
	} {
		code, body := get(c.user, c.groups, "/namespaces/team-a/agents/jobber")
		if code != 200 {
			t.Fatalf("%s: %d %s", c.name, code, body)
		}
		for _, l := range literals {
			if strings.Contains(body, l) == c.masked {
				t.Errorf("%s: %q shown %v: %s", c.name, l, !c.masked, body)
			}
		}
		if c.masked && (!strings.Contains(body, `{"name":"GITHUB_TOKEN","value":"(set)"}`) || !strings.Contains(body, `"command":["(set)","(set)"],"args":["(set)"]`)) {
			t.Errorf("%s: no (set) in place of the values: %s", c.name, body)
		}
		if !strings.Contains(body, `{"name":"API_KEY","valueFrom":{"secretKeyRef":{"name":"api","key":"key"}}}`) {
			t.Errorf("%s: no secret reference: %s", c.name, body)
		}
	}
}

func TestListModelProviders(t *testing.T) {
	get := startConfig(t, configCluster(t))
	code, body := get("", "", "/namespaces/team-a/modelproviders")
	var got api.ConfigList
	if err := json.Unmarshal([]byte(body), &got); err != nil || code != 200 {
		t.Fatalf("%d %s", code, body)
	}
	want := []api.ConfigSummary{
		{Name: "badurl", Ready: true},
		{Name: "claude", Ready: true},
		{Name: "fragment", Ready: true},
		{Name: "gateway", Ready: true},
		{Name: "gpt", Reason: "Pending", Message: "its current spec has not been checked yet"},
		{Name: "nohost", Ready: true},
	}
	if !slices.Equal(got.Items, want) {
		t.Errorf("got %+v, want %+v", got.Items, want)
	}
	if code, body := get("", "", "/namespaces/team-c/modelproviders"); code != 404 {
		t.Errorf("a namespace without a Team: %d %s", code, body)
	}
	policy := &authz.Policy{FoldUsernames: true}
	if code, body := startConfig(t, api.WithAccess(auth.ModeOIDC, policy), configCluster(t))("eve@example.test", "readers", "/namespaces/team-a/modelproviders"); code != 200 || !strings.Contains(body, `"gateway"`) {
		t.Errorf("a viewer: %d %s", code, body)
	}
}

func TestGetModelProvider(t *testing.T) {
	get := startConfig(t, configCluster(t))
	for _, c := range []struct {
		name, path string
		code       int
	}{
		{"a namespace without a Team", "/namespaces/team-c/modelproviders/claude", 404},
		{"a ModelProvider that is not there", "/namespaces/team-a/modelproviders/nope", 404},
		{"another namespace's ModelProvider", "/namespaces/team-a/modelproviders/vertex", 404},
		{"in its own namespace", "/namespaces/team-b/modelproviders/vertex", 200},
	} {
		if code, body := get("", "", c.path); code != c.code {
			t.Errorf("%s: %d %s, want %d", c.name, code, body, c.code)
		}
	}

	code, body := get("", "", "/namespaces/team-a/modelproviders/gpt")
	var got api.ConfigResource
	if err := json.Unmarshal([]byte(body), &got); err != nil || code != 200 {
		t.Fatalf("gpt: %d %s", code, body)
	}
	want := api.ConfigSummary{Name: "gpt", Reason: "Pending", Message: "its current spec has not been checked yet"}
	if got.ConfigSummary != want || got.Kind != "ModelProvider" || got.Generation != 2 || got.ObservedGeneration != 1 || len(got.Conditions) != 1 {
		t.Errorf("a ModelProvider edited since it was checked: %s", body)
	}
}

// What each role sees of a ModelProvider; nobody sees its fields' owners,
// nor the spec kubectl last applied.
func TestModelProviderByRole(t *testing.T) {
	policy := &authz.Policy{FoldUsernames: true}
	withRoles := startConfig(t, api.WithAccess(auth.ModeOIDC, policy), configCluster(t))
	withoutLogins := startConfig(t, configCluster(t))
	for _, c := range []struct {
		name                              string
		get                               func(user, groups, path string) (int, string)
		user, groups                      string
		spec, status, annotations, labels bool
	}{
		{"a viewer", withRoles, "eve@example.test", "readers", false, false, false, true},
		{"a viewer AgentAccess", withRoles, "agentaccess/bot", "viewer", false, false, false, true},
		{"a member", withRoles, "dana@example.test", "backend", true, true, true, true},
		{"an admin", withRoles, "lead@example.test", "", true, true, true, true},
		{"--auth=none", withoutLogins, "", "", true, true, true, true},
	} {
		code, body := c.get(c.user, c.groups, "/namespaces/team-a/modelproviders/claude")
		var got api.ConfigResource
		if err := json.Unmarshal([]byte(body), &got); err != nil || code != 200 {
			t.Fatalf("%s: %d %s", c.name, code, body)
		}
		if !got.Ready || len(got.Conditions) != 1 || got.Metadata.ResourceVersion == "" {
			t.Errorf("%s: no health: %s", c.name, body)
		}
		if (got.Spec != nil) != c.spec || (got.Status != nil) != c.status || (got.Metadata.Annotations != nil) != c.annotations || (got.Metadata.Labels != nil) != c.labels {
			t.Errorf("%s: spec %v, status %v, annotations %v, labels %v: %s", c.name, got.Spec != nil, got.Status != nil, got.Metadata.Annotations != nil, got.Metadata.Labels != nil, body)
		}
		if c.spec && (!strings.Contains(body, `"model":"claude-opus-5-5"`) || !strings.Contains(body, `"baseURL":"https://llm.example.test/v1"`) ||
			!strings.Contains(body, `"secretRef":{"name":"anthropic","key":"api-key"}`) || !strings.Contains(body, `"cadre.dev/owner":"docs-team"`)) {
			t.Errorf("%s: spec or annotations missing: %s", c.name, body)
		}
		for _, never := range []string{"last-applied", "managedFields", "f:model"} {
			if strings.Contains(body, never) {
				t.Errorf("%s sees %q: %s", c.name, never, body)
			}
		}
	}
	if code, body := withRoles("mallory@example.test", "", "/namespaces/team-a/modelproviders/claude"); code != 403 {
		t.Errorf("a stranger: %d %s", code, body)
	}
}

// A member sees that a ModelProvider's base URL has user info, query
// values or a fragment, not what they are, and sees none of a URL that
// does not parse; an admin sees them.
func TestModelProviderBaseURLByRole(t *testing.T) {
	policy := &authz.Policy{FoldUsernames: true}
	get := startConfig(t, api.WithAccess(auth.ModeOIDC, policy), configCluster(t))
	for _, p := range []struct {
		name     string
		literals []string
		masked   string
	}{
		{"gateway", []string{"sk_literal_user", "sk_literal_query", "sk_literal_bare"}, `"baseURL":"https://(set)@gw.example.test/v1?api-key=(set)\u0026(set)"`},
		{"fragment", []string{"sk_literal_fragment"}, `"baseURL":"https://gw.example.test/v1#(set)"`},
		{"badurl", []string{"sk_literal_bad"}, `"baseURL":"(set)"`},
		{"nohost", []string{"sk_literal_nohost"}, `"baseURL":"https://(set)@"`},
	} {
		for _, c := range []struct {
			name, user, groups string
			masked             bool
		}{
			{"a member", "dana@example.test", "backend", true},
			{"a member AgentAccess", "agentaccess/bot", "member", true},
			{"an admin", "lead@example.test", "", false},
		} {
			code, body := get(c.user, c.groups, "/namespaces/team-a/modelproviders/"+p.name)
			if code != 200 {
				t.Fatalf("%s, %s: %d %s", p.name, c.name, code, body)
			}
			for _, l := range p.literals {
				if strings.Contains(body, l) == c.masked {
					t.Errorf("%s, %s: %q shown %v: %s", p.name, c.name, l, c.masked, body)
				}
			}
			if c.masked && !strings.Contains(body, p.masked) {
				t.Errorf("%s, %s: no (set) in place of the values: %s", p.name, c.name, body)
			}
		}
	}
	if code, body := get("dana@example.test", "backend", "/namespaces/team-a/modelproviders/gpt"); code != 200 || strings.Contains(body, "baseURL") {
		t.Errorf("a member, no base URL: %d %s", code, body)
	}
}

func TestListToolServers(t *testing.T) {
	get := startConfig(t, configCluster(t))
	code, body := get("", "", "/namespaces/team-a/toolservers")
	var got api.ConfigList
	if err := json.Unmarshal([]byte(body), &got); err != nil || code != 200 {
		t.Fatalf("%d %s", code, body)
	}
	want := []api.ConfigSummary{
		{Name: "execer", Reason: "Pending", Message: "its current spec has not been checked yet"},
		{Name: "grpc", Ready: true},
		{Name: "keyed", Ready: true},
		{Name: "prober", Ready: true},
		{Name: "remote", Ready: true},
		{Name: "slashy", Ready: true},
	}
	if !slices.Equal(got.Items, want) {
		t.Errorf("got %+v, want %+v", got.Items, want)
	}
	if code, body := get("", "", "/namespaces/team-c/toolservers"); code != 404 {
		t.Errorf("a namespace without a Team: %d %s", code, body)
	}
	policy := &authz.Policy{FoldUsernames: true}
	withRoles := startConfig(t, api.WithAccess(auth.ModeOIDC, policy), configCluster(t))
	if code, body := withRoles("eve@example.test", "readers", "/namespaces/team-a/toolservers"); code != 200 || !strings.Contains(body, `"remote"`) {
		t.Errorf("a viewer: %d %s", code, body)
	}
	if code, body := withRoles("dana@example.test", "backend", "/namespaces/team-c/toolservers"); code != 403 || !strings.Contains(body, `"no_role"`) {
		t.Errorf("with logins, a namespace without a Team: %d %s", code, body)
	}
}

func TestGetToolServer(t *testing.T) {
	get := startConfig(t, configCluster(t))
	for _, c := range []struct {
		name, path string
		code       int
	}{
		{"a namespace without a Team", "/namespaces/team-c/toolservers/remote", 404},
		{"a ToolServer that is not there", "/namespaces/team-a/toolservers/nope", 404},
		{"another namespace's ToolServer", "/namespaces/team-a/toolservers/faraway", 404},
		{"in its own namespace", "/namespaces/team-b/toolservers/faraway", 200},
	} {
		if code, body := get("", "", c.path); code != c.code {
			t.Errorf("%s: %d %s, want %d", c.name, code, body, c.code)
		}
	}

	code, body := get("", "", "/namespaces/team-a/toolservers/execer")
	var got api.ConfigResource
	if err := json.Unmarshal([]byte(body), &got); err != nil || code != 200 {
		t.Fatalf("execer: %d %s", code, body)
	}
	want := api.ConfigSummary{Name: "execer", Reason: "Pending", Message: "its current spec has not been checked yet"}
	if got.ConfigSummary != want || got.Kind != "ToolServer" || got.Generation != 2 || got.ObservedGeneration != 1 || len(got.Conditions) != 1 {
		t.Errorf("a ToolServer edited since it was checked: %s", body)
	}
}

// What each role sees of a ToolServer; nobody sees its fields' owners, nor
// the spec kubectl last applied.
func TestToolServerByRole(t *testing.T) {
	policy := &authz.Policy{FoldUsernames: true}
	withRoles := startConfig(t, api.WithAccess(auth.ModeOIDC, policy), configCluster(t))
	withoutLogins := startConfig(t, configCluster(t))
	for _, c := range []struct {
		name                      string
		get                       func(user, groups, path string) (int, string)
		user, groups              string
		spec, status, annotations bool
	}{
		{"a viewer", withRoles, "eve@example.test", "readers", false, false, false},
		{"a viewer AgentAccess", withRoles, "agentaccess/bot", "viewer", false, false, false},
		{"a member", withRoles, "dana@example.test", "backend", true, true, true},
		{"a member AgentAccess", withRoles, "agentaccess/bot", "member", true, true, true},
		{"an admin", withRoles, "lead@example.test", "", true, true, true},
		{"--auth=none", withoutLogins, "", "", true, true, true},
	} {
		code, body := c.get(c.user, c.groups, "/namespaces/team-a/toolservers/remote")
		var got api.ConfigResource
		if err := json.Unmarshal([]byte(body), &got); err != nil || code != 200 {
			t.Fatalf("%s: %d %s", c.name, code, body)
		}
		if !got.Ready || len(got.Conditions) != 1 || got.Metadata.ResourceVersion == "" {
			t.Errorf("%s: no health: %s", c.name, body)
		}
		if (got.Spec != nil) != c.spec || (got.Status != nil) != c.status || (got.Metadata.Annotations != nil) != c.annotations {
			t.Errorf("%s: spec %v, status %v, annotations %v: %s", c.name, got.Spec != nil, got.Status != nil, got.Metadata.Annotations != nil, body)
		}
		if c.spec && (!strings.Contains(body, `"auth":{"secretRef":{"name":"mcp","key":"token"},"prefix":""}`) || !strings.Contains(body, `"cadre.dev/owner":"tools-team"`)) {
			t.Errorf("%s: spec or annotations missing: %s", c.name, body)
		}
		for _, never := range []string{"last-applied", "managedFields", "f:url"} {
			if strings.Contains(body, never) {
				t.Errorf("%s sees %q: %s", c.name, never, body)
			}
		}
	}
	if code, body := withRoles("mallory@example.test", "", "/namespaces/team-a/toolservers/remote"); code != 403 {
		t.Errorf("a stranger: %d %s", code, body)
	}
}

// A member sees that a ToolServer's URLs, managed path and readiness probe
// hold a value, not what it is; an admin sees them. A gRPC probe and a
// Bearer auth prefix are shown as they are.
func TestToolServerMaskedByRole(t *testing.T) {
	policy := &authz.Policy{FoldUsernames: true}
	get := startConfig(t, api.WithAccess(auth.ModeOIDC, policy), configCluster(t))
	for _, ts := range []struct {
		name     string
		literals []string
		masked   []string
	}{
		{"remote", []string{"tk_literal_user", "tk_literal_query"}, []string{`"url":"https://(set)@mcp.example.test/mcp?key=(set)"`}},
		{"prober", []string{"tk_literal_path", "tk_literal_probe_path", "tk_literal_header", "tk_literal_tenant", "tk_literal_status"}, []string{
			`"path":"/mcp?key=(set)"`, `"path":"/healthz?token=(set)"`, `"httpHeaders":[{"name":"X-Probe-Key","value":"(set)"},{"name":"X-Probe-Tenant","value":"(set)"}]`,
			`"url":"http://prober.team-a.svc:8080/mcp?key=(set)"`,
		}},
		{"execer", []string{"tk_literal_exec", `"check"`, "tk_literal_prefix"}, []string{`"exec":{"command":["(set)","(set)"]}`, `"prefix":"(set)"`}},
		{"slashy", []string{"tk_literal_slashy"}, []string{`"path":"//(set)@evil/mcp"`, `"url":"http://slashy.team-a.svc:8080//(set)@evil/mcp"`}},
		{"keyed", []string{"tk_literal_keyed"}, []string{`"prefix":"(set)"`}},
	} {
		for _, c := range []struct {
			name, user, groups string
			masked             bool
		}{
			{"a member", "dana@example.test", "backend", true},
			{"a member AgentAccess", "agentaccess/bot", "member", true},
			{"an admin", "lead@example.test", "", false},
		} {
			code, body := get(c.user, c.groups, "/namespaces/team-a/toolservers/"+ts.name)
			if code != 200 {
				t.Fatalf("%s, %s: %d %s", ts.name, c.name, code, body)
			}
			for _, l := range ts.literals {
				if strings.Contains(body, l) == c.masked {
					t.Errorf("%s, %s: %q shown %v: %s", ts.name, c.name, l, c.masked, body)
				}
			}
			for _, m := range ts.masked {
				if strings.Contains(body, m) != c.masked {
					t.Errorf("%s, %s: %s shown %v: %s", ts.name, c.name, m, !c.masked, body)
				}
			}
		}
	}
	if code, body := get("dana@example.test", "backend", "/namespaces/team-a/toolservers/grpc"); code != 200 || !strings.Contains(body, `"grpc":{"port":8080,"service":"mcp.health"}`) || !strings.Contains(body, `"prefix":"Bearer "`) {
		t.Errorf("a member, a gRPC probe and a Bearer prefix: %d %s", code, body)
	}
}

// A Schedule's list item says it is suspended, unless its current spec has
// not been checked, and whom its Tasks go to for a member and up.
func TestListSchedules(t *testing.T) {
	get := startConfig(t, configCluster(t))
	const (
		pending = `"ready":false,"reason":"Pending","message":"its current spec has not been checked yet"`
		viewer  = `{"items":[{"name":"daily","ready":true},{"name":"held",` + pending + `},{"name":"nightly",` + pending + `},{"name":"paused","ready":true,"reason":"Suspended"}]}` + "\n"
		member  = `{"items":[{"name":"daily","ready":true},{"name":"held",` + pending + `,"assignee":{"kind":"User","name":"dana@example.test"}},{"name":"nightly",` + pending + `},{"name":"paused","ready":true,"reason":"Suspended","assignee":{"kind":"Agent","name":"writer"}}]}` + "\n"
	)
	if code, body := get("", "", "/namespaces/team-a/schedules"); code != 200 || body != member {
		t.Errorf("--auth=none: %d %s, want %s", code, body, member)
	}
	if code, body := get("", "", "/namespaces/team-c/schedules"); code != 404 {
		t.Errorf("a namespace without a Team: %d %s", code, body)
	}
	policy := &authz.Policy{FoldUsernames: true}
	withRoles := startConfig(t, api.WithAccess(auth.ModeOIDC, policy), configCluster(t))
	for _, c := range []struct{ name, user, groups, want string }{
		{"a viewer", "eve@example.test", "readers", viewer},
		{"a viewer AgentAccess", "agentaccess/bot", "viewer", viewer},
		{"a member", "dana@example.test", "backend", member},
		{"a member AgentAccess", "agentaccess/bot", "member", member},
		{"an admin", "lead@example.test", "", member},
	} {
		if code, body := withRoles(c.user, c.groups, "/namespaces/team-a/schedules"); code != 200 || body != c.want {
			t.Errorf("%s: %d %s, want %s", c.name, code, body, c.want)
		}
	}
	if code, body := withRoles("mallory@example.test", "", "/namespaces/team-a/schedules"); code != 403 {
		t.Errorf("a stranger: %d %s", code, body)
	}
	if code, body := withRoles("dana@example.test", "backend", "/namespaces/team-c/schedules"); code != 403 || !strings.Contains(body, `"no_role"`) {
		t.Errorf("with logins, a namespace without a Team: %d %s", code, body)
	}
	boot := startConfig(t, api.WithAccess(auth.ModeOIDC, &authz.Policy{FoldUsernames: true, AdminUsers: []string{"root@example.test"}}), configCluster(t))
	for _, p := range []string{"/namespaces/team-c/schedules", "/namespaces/team-c/schedules/daily"} {
		if code, body := boot("root@example.test", "", p); code != 404 {
			t.Errorf("a bootstrap admin, a namespace without a Team, %s: %d %s", p, code, body)
		}
	}
}

// The other kinds' lists are what they were before Schedules' told more:
// the same for every role, a Ready item with no reason.
func TestOtherListsUnchanged(t *testing.T) {
	withRoles := startConfig(t, api.WithAccess(auth.ModeOIDC, &authz.Policy{FoldUsernames: true}), configCluster(t))
	withoutLogins := startConfig(t, configCluster(t))
	for plural, want := range map[string]string{
		"modelproviders": `{"items":[{"name":"badurl","ready":true},{"name":"claude","ready":true},{"name":"fragment","ready":true},{"name":"gateway","ready":true},{"name":"gpt","ready":false,"reason":"Pending","message":"its current spec has not been checked yet"},{"name":"nohost","ready":true}]}` + "\n",
		"toolservers":    `{"items":[{"name":"execer","ready":false,"reason":"Pending","message":"its current spec has not been checked yet"},{"name":"grpc","ready":true},{"name":"keyed","ready":true},{"name":"prober","ready":true},{"name":"remote","ready":true},{"name":"slashy","ready":true}]}` + "\n",
		"notifiers":      `{"items":[{"name":"badhook","ready":true},{"name":"caller","ready":false,"reason":"Pending","message":"its current spec has not been checked yet"},{"name":"hook","ready":true}]}` + "\n",
		"runners":        `{"items":[{"name":"ci","ready":false,"reason":"Pending","message":"its current spec has not been checked yet"},{"name":"pool","ready":true}]}` + "\n",
	} {
		for _, c := range []struct {
			name         string
			get          func(user, groups, path string) (int, string)
			user, groups string
		}{
			{"a viewer", withRoles, "eve@example.test", "readers"},
			{"a viewer AgentAccess", withRoles, "agentaccess/bot", "viewer"},
			{"a member", withRoles, "dana@example.test", "backend"},
			{"a member AgentAccess", withRoles, "agentaccess/bot", "member"},
			{"an admin", withRoles, "lead@example.test", ""},
			{"--auth=none", withoutLogins, "", ""},
		} {
			if code, body := c.get(c.user, c.groups, "/namespaces/team-a/"+plural); code != 200 || body != want {
				t.Errorf("%s, %s: %d %s, want %s", c.name, plural, code, body, want)
			}
		}
	}
}

func TestGetSchedule(t *testing.T) {
	get := startConfig(t, configCluster(t))
	for _, c := range []struct {
		name, path string
		code       int
	}{
		{"a namespace without a Team", "/namespaces/team-c/schedules/daily", 404},
		{"a Schedule that is not there", "/namespaces/team-a/schedules/nope", 404},
		{"another namespace's Schedule", "/namespaces/team-a/schedules/yonder", 404},
		{"in its own namespace", "/namespaces/team-b/schedules/yonder", 200},
	} {
		if code, body := get("", "", c.path); code != c.code {
			t.Errorf("%s: %d %s, want %d", c.name, code, body, c.code)
		}
	}

	code, body := get("", "", "/namespaces/team-a/schedules/nightly")
	var got api.ConfigResource
	if err := json.Unmarshal([]byte(body), &got); err != nil || code != 200 {
		t.Fatalf("nightly: %d %s", code, body)
	}
	want := api.ConfigSummary{Name: "nightly", Reason: "Pending", Message: "its current spec has not been checked yet"}
	if got.ConfigSummary != want || got.Kind != "Schedule" || got.Generation != 2 || got.ObservedGeneration != 1 || len(got.Conditions) != 1 {
		t.Errorf("a Schedule edited since it was checked: %s", body)
	}
	// Its conditions say it is suspended, and its spec whom its Tasks go to.
	code, body = get("", "", "/namespaces/team-a/schedules/paused")
	got = api.ConfigResource{}
	if err := json.Unmarshal([]byte(body), &got); err != nil || code != 200 {
		t.Fatalf("paused: %d %s", code, body)
	}
	if want := (api.ConfigSummary{Name: "paused", Ready: true}); got.ConfigSummary != want || got.Conditions[0].Reason != "Suspended" {
		t.Errorf("a suspended Schedule: %s", body)
	}
}

// What each role sees of a Schedule: its task template unmasked, as the
// Tasks it creates show it to everyone; nobody sees its fields' owners, nor
// the spec kubectl last applied.
func TestScheduleByRole(t *testing.T) {
	policy := &authz.Policy{FoldUsernames: true}
	withRoles := startConfig(t, api.WithAccess(auth.ModeOIDC, policy), configCluster(t))
	withoutLogins := startConfig(t, configCluster(t))
	for _, c := range []struct {
		name                      string
		get                       func(user, groups, path string) (int, string)
		user, groups              string
		spec, status, annotations bool
	}{
		{"a viewer", withRoles, "eve@example.test", "readers", false, false, false},
		{"a viewer AgentAccess", withRoles, "agentaccess/bot", "viewer", false, false, false},
		{"a member", withRoles, "dana@example.test", "backend", true, true, true},
		{"a member AgentAccess", withRoles, "agentaccess/bot", "member", true, true, true},
		{"an admin", withRoles, "lead@example.test", "", true, true, true},
		{"--auth=none", withoutLogins, "", "", true, true, true},
	} {
		code, body := c.get(c.user, c.groups, "/namespaces/team-a/schedules/daily")
		var got api.ConfigResource
		if err := json.Unmarshal([]byte(body), &got); err != nil || code != 200 {
			t.Fatalf("%s: %d %s", c.name, code, body)
		}
		if !got.Ready || len(got.Conditions) != 1 || got.Metadata.ResourceVersion == "" || got.Metadata.Labels["tier"] != "one" {
			t.Errorf("%s: no health: %s", c.name, body)
		}
		if (got.Spec != nil) != c.spec || (got.Status != nil) != c.status || (got.Metadata.Annotations != nil) != c.annotations {
			t.Errorf("%s: spec %v, status %v, annotations %v: %s", c.name, got.Spec != nil, got.Status != nil, got.Metadata.Annotations != nil, body)
		}
		for _, s := range []string{`"description":"post the daily thread"`, `"lastTask":"daily-202610310000"`, `"cadre.dev/owner":"ops-team"`} {
			if strings.Contains(body, s) != c.spec {
				t.Errorf("%s: %s shown %v: %s", c.name, s, !c.spec, body)
			}
		}
		for _, never := range []string{"last-applied", "managedFields", "f:cron"} {
			if strings.Contains(body, never) {
				t.Errorf("%s sees %q: %s", c.name, never, body)
			}
		}
	}
	if code, body := withRoles("mallory@example.test", "", "/namespaces/team-a/schedules/daily"); code != 403 {
		t.Errorf("a stranger: %d %s", code, body)
	}
}

// shared hands out ts or nf without copying what it points to, as a cache
// may: a member's view masks a copy of its own.
type shared struct {
	client.Reader
	ts *v1alpha1.ToolServer
	nf *v1alpha1.Notifier
}

func (s shared) Get(_ context.Context, _ client.ObjectKey, obj client.Object, _ ...client.GetOption) error {
	switch o := obj.(type) {
	case *v1alpha1.ToolServer:
		*o = *s.ts
	case *v1alpha1.Notifier:
		*o = *s.nf
	}
	return nil
}

func TestToolServerMaskLeavesTheCache(t *testing.T) {
	prober := toolServers()[1].(*v1alpha1.ToolServer)
	get := startConfig(t, api.WithAccess(auth.ModeOIDC, &authz.Policy{FoldUsernames: true}), api.WithConfig(shared{ts: prober}))
	if code, body := get("dana@example.test", "backend", "/namespaces/team-a/toolservers/prober"); code != 200 || strings.Contains(body, "tk_literal_header") {
		t.Fatalf("a member: %d %s", code, body)
	}
	code, body := get("lead@example.test", "", "/namespaces/team-a/toolservers/prober")
	for _, l := range []string{"tk_literal_path", "tk_literal_probe_path", "tk_literal_header"} {
		if code != 200 || !strings.Contains(body, l) {
			t.Errorf("an admin after a member: %q gone: %d %s", l, code, body)
		}
	}
}

func TestListNotifiers(t *testing.T) {
	get := startConfig(t, configCluster(t))
	code, body := get("", "", "/namespaces/team-a/notifiers")
	var got api.ConfigList
	if err := json.Unmarshal([]byte(body), &got); err != nil || code != 200 {
		t.Fatalf("%d %s", code, body)
	}
	want := []api.ConfigSummary{
		{Name: "badhook", Ready: true},
		{Name: "caller", Reason: "Pending", Message: "its current spec has not been checked yet"},
		{Name: "hook", Ready: true},
	}
	if !slices.Equal(got.Items, want) {
		t.Errorf("got %+v, want %+v", got.Items, want)
	}
	if code, body := get("", "", "/namespaces/team-c/notifiers"); code != 404 {
		t.Errorf("a namespace without a Team: %d %s", code, body)
	}
	policy := &authz.Policy{FoldUsernames: true}
	withRoles := startConfig(t, api.WithAccess(auth.ModeOIDC, policy), configCluster(t))
	for _, c := range []struct{ name, user, groups string }{
		{"a viewer", "eve@example.test", "readers"},
		{"a viewer AgentAccess", "agentaccess/bot", "viewer"},
		{"a member", "dana@example.test", "backend"},
		{"a member AgentAccess", "agentaccess/bot", "member"},
		{"an admin", "lead@example.test", ""},
	} {
		if code, body := withRoles(c.user, c.groups, "/namespaces/team-a/notifiers"); code != 200 || !strings.Contains(body, `"hook"`) {
			t.Errorf("%s: %d %s", c.name, code, body)
		}
	}
	if code, body := withRoles("mallory@example.test", "", "/namespaces/team-a/notifiers"); code != 403 {
		t.Errorf("a stranger: %d %s", code, body)
	}
	if code, body := withRoles("dana@example.test", "backend", "/namespaces/team-c/notifiers"); code != 403 || !strings.Contains(body, `"no_role"`) {
		t.Errorf("with logins, a namespace without a Team: %d %s", code, body)
	}
	boot := startConfig(t, api.WithAccess(auth.ModeOIDC, &authz.Policy{FoldUsernames: true, AdminUsers: []string{"root@example.test"}}), configCluster(t))
	for _, p := range []string{"/namespaces/team-c/notifiers", "/namespaces/team-c/notifiers/hook"} {
		if code, body := boot("root@example.test", "", p); code != 404 {
			t.Errorf("a bootstrap admin, a namespace without a Team, %s: %d %s", p, code, body)
		}
	}
}

func TestGetNotifier(t *testing.T) {
	get := startConfig(t, configCluster(t))
	for _, c := range []struct {
		name, path string
		code       int
	}{
		{"a namespace without a Team", "/namespaces/team-c/notifiers/hook", 404},
		{"a Notifier that is not there", "/namespaces/team-a/notifiers/nope", 404},
		{"another namespace's Notifier", "/namespaces/team-a/notifiers/distant", 404},
		{"in its own namespace", "/namespaces/team-b/notifiers/distant", 200},
	} {
		if code, body := get("", "", c.path); code != c.code {
			t.Errorf("%s: %d %s, want %d", c.name, code, body, c.code)
		}
	}

	code, body := get("", "", "/namespaces/team-a/notifiers/caller")
	var got api.ConfigResource
	if err := json.Unmarshal([]byte(body), &got); err != nil || code != 200 {
		t.Fatalf("caller: %d %s", code, body)
	}
	want := api.ConfigSummary{Name: "caller", Reason: "Pending", Message: "its current spec has not been checked yet"}
	if got.ConfigSummary != want || got.Kind != "Notifier" || got.Generation != 2 || got.ObservedGeneration != 1 || len(got.Conditions) != 1 {
		t.Errorf("a Notifier edited since it was checked: %s", body)
	}
	withRoles := startConfig(t, api.WithAccess(auth.ModeOIDC, &authz.Policy{FoldUsernames: true}), configCluster(t))
	if code, body := withRoles("mallory@example.test", "", "/namespaces/team-a/notifiers/hook"); code != 403 {
		t.Errorf("a stranger: %d %s", code, body)
	}
}

// What each role sees of a Notifier; nobody sees its fields' owners, nor
// the spec kubectl last applied.
func TestNotifierByRole(t *testing.T) {
	policy := &authz.Policy{FoldUsernames: true}
	withRoles := startConfig(t, api.WithAccess(auth.ModeOIDC, policy), configCluster(t))
	withoutLogins := startConfig(t, configCluster(t))
	for _, c := range []struct {
		name                      string
		get                       func(user, groups, path string) (int, string)
		user, groups              string
		spec, status, annotations bool
	}{
		{"a viewer", withRoles, "eve@example.test", "readers", false, false, false},
		{"a viewer AgentAccess", withRoles, "agentaccess/bot", "viewer", false, false, false},
		{"a member", withRoles, "dana@example.test", "backend", true, true, true},
		{"a member AgentAccess", withRoles, "agentaccess/bot", "member", true, true, true},
		{"an admin", withRoles, "lead@example.test", "", true, true, true},
		{"--auth=none", withoutLogins, "", "", true, true, true},
	} {
		code, body := c.get(c.user, c.groups, "/namespaces/team-a/notifiers/hook")
		var got api.ConfigResource
		if err := json.Unmarshal([]byte(body), &got); err != nil || code != 200 {
			t.Fatalf("%s: %d %s", c.name, code, body)
		}
		if !got.Ready || len(got.Conditions) != 1 || got.Metadata.ResourceVersion == "" || got.Metadata.Labels["tier"] != "one" {
			t.Errorf("%s: no health: %s", c.name, body)
		}
		if (got.Spec != nil) != c.spec || (got.Status != nil) != c.status || (got.Metadata.Annotations != nil) != c.annotations {
			t.Errorf("%s: spec %v, status %v, annotations %v: %s", c.name, got.Spec != nil, got.Status != nil, got.Metadata.Annotations != nil, body)
		}
		for _, s := range []string{`"on":["Question","Done"]`, `"secretRef":{"name":"hook","key":"signing"}`, `"lastSent":"2026-10-31T09:00:00Z"`, `"cadre.dev/owner":"ops-team"`} {
			if strings.Contains(body, s) != c.spec {
				t.Errorf("%s: %s shown %v: %s", c.name, s, !c.spec, body)
			}
		}
		for _, never := range []string{"last-applied", "managedFields", "f:on"} {
			if strings.Contains(body, never) {
				t.Errorf("%s sees %q: %s", c.name, never, body)
			}
		}
	}
	if code, body := withRoles("mallory@example.test", "", "/namespaces/team-a/notifiers/hook"); code != 403 {
		t.Errorf("a stranger: %d %s", code, body)
	}
}

// A member sees that a Notifier's webhook URL, headers and tool arguments
// hold a value, not what it is, and none of a URL that does not parse; an
// admin sees them.
func TestNotifierMaskedByRole(t *testing.T) {
	policy := &authz.Policy{FoldUsernames: true}
	get := startConfig(t, api.WithAccess(auth.ModeOIDC, policy), configCluster(t))
	for _, nf := range []struct {
		name     string
		literals []string
		masked   []string
	}{
		{"hook", []string{"nf_literal_query", "nf_literal_bare", "nf_literal_fragment", "nf_literal_header", "nf_literal_tenant"}, []string{
			`"url":"https://hooks.example.test/(set)?token=(set)\u0026(set)#(set)"`, `"headers":{"X-Api-Key":"(set)","X-Tenant":"(set)"}`,
		}},
		{"caller", []string{"nf_literal_channel", "nf_literal_arg"}, []string{`"arguments":{"channel":"(set)","token":"(set)"}`}},
		{"badhook", []string{"nf_literal_bad"}, []string{`"url":"(set)"`}},
	} {
		for _, c := range []struct {
			name, user, groups string
			masked             bool
		}{
			{"a member", "dana@example.test", "backend", true},
			{"a member AgentAccess", "agentaccess/bot", "member", true},
			{"an admin", "lead@example.test", "", false},
		} {
			code, body := get(c.user, c.groups, "/namespaces/team-a/notifiers/"+nf.name)
			if code != 200 {
				t.Fatalf("%s, %s: %d %s", nf.name, c.name, code, body)
			}
			for _, l := range nf.literals {
				if strings.Contains(body, l) == c.masked {
					t.Errorf("%s, %s: %q shown %v: %s", nf.name, c.name, l, c.masked, body)
				}
			}
			for _, m := range nf.masked {
				if strings.Contains(body, m) != c.masked {
					t.Errorf("%s, %s: %s shown %v: %s", nf.name, c.name, m, !c.masked, body)
				}
			}
		}
	}
}

// A member sees a webhook URL's scheme, host and port, not its path, and
// that the last delivery failed, not the error, which may quote the URL; an
// admin sees them.
func TestNotifierWebhookPathMaskedByRole(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	get := startConfig(t, api.WithAccess(auth.ModeOIDC, &authz.Policy{FoldUsernames: true}),
		api.WithConfig(fake.NewClientBuilder().WithScheme(scheme).WithObjects(webhooks()...).Build()))
	for _, nf := range []struct {
		name   string
		admin  []string
		member []string
	}{
		{"slack", []string{`"url":"` + slackURL + `"`, `"message":"Post \"` + slackURL + `\": context deadline exceeded"`}, []string{
			`"url":"https://hooks.slack.com/(set)"`, `"lastError":{"message":"(set)","at":"2026-10-31T09:00:00Z"}`,
		}},
		{"rooted", []string{`"url":"https://hooks.example.test:8443/?k=nf_literal_root"`}, []string{`"url":"https://hooks.example.test:8443/?k=(set)"`}},
		{"bare", []string{`"url":"https://hooks.example.test:8443"`}, []string{`"url":"https://hooks.example.test:8443"`}},
	} {
		for _, c := range []struct {
			name, user, groups string
			want               []string
		}{
			{"a member", "dana@example.test", "backend", nf.member},
			{"a member AgentAccess", "agentaccess/bot", "member", nf.member},
			{"an admin", "lead@example.test", "", nf.admin},
		} {
			code, body := get(c.user, c.groups, "/namespaces/team-a/notifiers/"+nf.name)
			if code != 200 {
				t.Fatalf("%s, %s: %d %s", nf.name, c.name, code, body)
			}
			for _, w := range c.want {
				if !strings.Contains(body, w) {
					t.Errorf("%s, %s: no %s: %s", nf.name, c.name, w, body)
				}
			}
			if c.user != "lead@example.test" && (strings.Contains(body, "XXSECRETXX") || strings.Contains(body, "nf_literal")) {
				t.Errorf("%s, %s: a literal shown: %s", nf.name, c.name, body)
			}
			if nf.name != "slack" && strings.Contains(body, "lastError") {
				t.Errorf("%s, %s: a lastError with none: %s", nf.name, c.name, body)
			}
		}
	}
}

func TestNotifierMaskLeavesTheCache(t *testing.T) {
	nfs := notifiers()
	for _, c := range []struct {
		nf       *v1alpha1.Notifier
		literals []string
	}{
		{nfs[0].(*v1alpha1.Notifier), []string{"nf_literal_query", "nf_literal_header", "nf_literal_tenant"}},
		{nfs[1].(*v1alpha1.Notifier), []string{"nf_literal_channel", "nf_literal_arg"}},
	} {
		get := startConfig(t, api.WithAccess(auth.ModeOIDC, &authz.Policy{FoldUsernames: true}), api.WithConfig(shared{nf: c.nf}))
		if code, body := get("dana@example.test", "backend", "/namespaces/team-a/notifiers/"+c.nf.Name); code != 200 || strings.Contains(body, "nf_literal") {
			t.Fatalf("a member, %s: %d %s", c.nf.Name, code, body)
		}
		code, body := get("lead@example.test", "", "/namespaces/team-a/notifiers/"+c.nf.Name)
		for _, l := range c.literals {
			if code != 200 || !strings.Contains(body, l) {
				t.Errorf("an admin after a member, %s: %q gone: %d %s", c.nf.Name, l, code, body)
			}
		}
	}
	slack := webhooks()[0].(*v1alpha1.Notifier)
	get := startConfig(t, api.WithAccess(auth.ModeOIDC, &authz.Policy{FoldUsernames: true}), api.WithConfig(shared{nf: slack}))
	if code, body := get("dana@example.test", "backend", "/namespaces/team-a/notifiers/slack"); code != 200 || strings.Contains(body, "XXSECRETXX") {
		t.Fatalf("a member, slack: %d %s", code, body)
	}
	if code, body := get("lead@example.test", "", "/namespaces/team-a/notifiers/slack"); code != 200 || strings.Count(body, "XXSECRETXX") != 2 {
		t.Errorf("an admin after a member, slack: its URL gone from the spec or lastError: %d %s", code, body)
	}
}

func TestListRunners(t *testing.T) {
	get := startConfig(t, configCluster(t))
	code, body := get("", "", "/namespaces/team-a/runners")
	var got api.ConfigList
	if err := json.Unmarshal([]byte(body), &got); err != nil || code != 200 {
		t.Fatalf("%d %s", code, body)
	}
	want := []api.ConfigSummary{
		{Name: "ci", Reason: "Pending", Message: "its current spec has not been checked yet"},
		{Name: "pool", Ready: true},
	}
	if !slices.Equal(got.Items, want) {
		t.Errorf("got %+v, want %+v", got.Items, want)
	}
	if code, body := get("", "", "/namespaces/team-c/runners"); code != 404 {
		t.Errorf("a namespace without a Team: %d %s", code, body)
	}
	policy := &authz.Policy{FoldUsernames: true}
	withRoles := startConfig(t, api.WithAccess(auth.ModeOIDC, policy), configCluster(t))
	for _, c := range []struct{ name, user, groups string }{
		{"a viewer", "eve@example.test", "readers"},
		{"a viewer AgentAccess", "agentaccess/bot", "viewer"},
		{"a member", "dana@example.test", "backend"},
		{"a member AgentAccess", "agentaccess/bot", "member"},
		{"an admin", "lead@example.test", ""},
	} {
		if code, body := withRoles(c.user, c.groups, "/namespaces/team-a/runners"); code != 200 || !strings.Contains(body, `"pool"`) {
			t.Errorf("%s: %d %s", c.name, code, body)
		}
	}
	if code, body := withRoles("mallory@example.test", "", "/namespaces/team-a/runners"); code != 403 {
		t.Errorf("a stranger: %d %s", code, body)
	}
	if code, body := withRoles("dana@example.test", "backend", "/namespaces/team-c/runners"); code != 403 || !strings.Contains(body, `"no_role"`) {
		t.Errorf("with logins, a namespace without a Team: %d %s", code, body)
	}
	boot := startConfig(t, api.WithAccess(auth.ModeOIDC, &authz.Policy{FoldUsernames: true, AdminUsers: []string{"root@example.test"}}), configCluster(t))
	for _, p := range []string{"/namespaces/team-c/runners", "/namespaces/team-c/runners/pool"} {
		if code, body := boot("root@example.test", "", p); code != 404 {
			t.Errorf("a bootstrap admin, a namespace without a Team, %s: %d %s", p, code, body)
		}
	}
}

func TestGetRunner(t *testing.T) {
	get := startConfig(t, configCluster(t))
	for _, c := range []struct {
		name, path string
		code       int
	}{
		{"a namespace without a Team", "/namespaces/team-c/runners/pool", 404},
		{"a Runner that is not there", "/namespaces/team-a/runners/nope", 404},
		{"another namespace's Runner", "/namespaces/team-a/runners/offsite", 404},
		{"in its own namespace", "/namespaces/team-b/runners/offsite", 200},
	} {
		if code, body := get("", "", c.path); code != c.code {
			t.Errorf("%s: %d %s, want %d", c.name, code, body, c.code)
		}
	}

	code, body := get("", "", "/namespaces/team-a/runners/ci")
	var got api.ConfigResource
	if err := json.Unmarshal([]byte(body), &got); err != nil || code != 200 {
		t.Fatalf("ci: %d %s", code, body)
	}
	want := api.ConfigSummary{Name: "ci", Reason: "Pending", Message: "its current spec has not been checked yet"}
	if got.ConfigSummary != want || got.Kind != "Runner" || got.Generation != 2 || got.ObservedGeneration != 1 || len(got.Conditions) != 1 {
		t.Errorf("a Runner edited since it was checked: %s", body)
	}
	withRoles := startConfig(t, api.WithAccess(auth.ModeOIDC, &authz.Policy{FoldUsernames: true}), configCluster(t))
	if code, body := withRoles("mallory@example.test", "", "/namespaces/team-a/runners/pool"); code != 403 {
		t.Errorf("a stranger: %d %s", code, body)
	}
}

// What each role sees of a Runner: its labels, ServiceAccount and token
// Secret unmasked, as they hold no credential; nobody sees its fields'
// owners, nor the spec kubectl last applied.
func TestRunnerByRole(t *testing.T) {
	policy := &authz.Policy{FoldUsernames: true}
	withRoles := startConfig(t, api.WithAccess(auth.ModeOIDC, policy), configCluster(t))
	withoutLogins := startConfig(t, configCluster(t))
	for _, c := range []struct {
		name                      string
		get                       func(user, groups, path string) (int, string)
		user, groups              string
		spec, status, annotations bool
	}{
		{"a viewer", withRoles, "eve@example.test", "readers", false, false, false},
		{"a viewer AgentAccess", withRoles, "agentaccess/bot", "viewer", false, false, false},
		{"a member", withRoles, "dana@example.test", "backend", true, true, true},
		{"a member AgentAccess", withRoles, "agentaccess/bot", "member", true, true, true},
		{"an admin", withRoles, "lead@example.test", "", true, true, true},
		{"--auth=none", withoutLogins, "", "", true, true, true},
	} {
		code, body := c.get(c.user, c.groups, "/namespaces/team-a/runners/pool")
		var got api.ConfigResource
		if err := json.Unmarshal([]byte(body), &got); err != nil || code != 200 {
			t.Fatalf("%s: %d %s", c.name, code, body)
		}
		if !got.Ready || len(got.Conditions) != 1 || got.Metadata.ResourceVersion == "" || got.Metadata.Labels["tier"] != "one" {
			t.Errorf("%s: no health: %s", c.name, body)
		}
		if (got.Spec != nil) != c.spec || (got.Status != nil) != c.status || (got.Metadata.Annotations != nil) != c.annotations {
			t.Errorf("%s: spec %v, status %v, annotations %v: %s", c.name, got.Spec != nil, got.Status != nil, got.Metadata.Annotations != nil, body)
		}
		for _, s := range []string{`"labels":{"pool":"gpu"}`, `"tokenSecret":"pool-token"`, `"mintedAt":"2026-10-31T09:00:00Z"`, `"cadre.dev/owner":"ops-team"`} {
			if strings.Contains(body, s) != c.spec {
				t.Errorf("%s: %s shown %v: %s", c.name, s, !c.spec, body)
			}
		}
		for _, never := range []string{"last-applied", "managedFields", "f:labels"} {
			if strings.Contains(body, never) {
				t.Errorf("%s sees %q: %s", c.name, never, body)
			}
		}
	}
	code, body := withRoles("dana@example.test", "backend", "/namespaces/team-a/runners/ci")
	if code != 200 || !strings.Contains(body, `"serviceAccount":{"name":"ci-runner"}`) {
		t.Errorf("a member, a Runner on a ServiceAccount: %d %s", code, body)
	}
}

// No role is shown a Runner's token hash, listed or read.
func TestRunnerTokenHashNeverServed(t *testing.T) {
	policy := &authz.Policy{FoldUsernames: true}
	withRoles := startConfig(t, api.WithAccess(auth.ModeOIDC, policy), configCluster(t))
	withoutLogins := startConfig(t, configCluster(t))
	for _, c := range []struct {
		name         string
		get          func(user, groups, path string) (int, string)
		user, groups string
	}{
		{"a viewer", withRoles, "eve@example.test", "readers"},
		{"a viewer AgentAccess", withRoles, "agentaccess/bot", "viewer"},
		{"a member", withRoles, "dana@example.test", "backend"},
		{"a member AgentAccess", withRoles, "agentaccess/bot", "member"},
		{"an admin", withRoles, "lead@example.test", ""},
		{"--auth=none", withoutLogins, "", ""},
	} {
		for _, p := range []string{"/namespaces/team-a/runners", "/namespaces/team-a/runners/pool"} {
			code, body := c.get(c.user, c.groups, p)
			if code != 200 || strings.Contains(body, runnerHash) || strings.Contains(body, "tokenHash") {
				t.Errorf("%s, %s: %d %s", c.name, p, code, body)
			}
		}
	}
}

// Without WithConfig, an Agent is not served, and Agents are still listed.
func TestAgentsWithoutConfig(t *testing.T) {
	get := startConfig(t)
	if code, body := get("", "", "/namespaces/team-a/agents/writer"); code != 404 {
		t.Errorf("an Agent: %d %s", code, body)
	}
	if code, body := get("", "", "/namespaces/team-a/agents"); code != 200 || body != `{"items":[]}`+"\n" {
		t.Errorf("the Agents: %d %q", code, body)
	}
}
