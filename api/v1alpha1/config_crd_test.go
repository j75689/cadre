package v1alpha1_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
)

func key() *v1alpha1.Auth {
	return &v1alpha1.Auth{SecretRef: v1alpha1.SecretKeyRef{Name: "anthropic", Key: "api-key"}}
}

// expect creates obj and checks the API server accepts or rejects it.
func expect(t *testing.T, c client.Client, name string, obj client.Object, valid bool) {
	t.Helper()
	obj.SetNamespace(ns)
	err := c.Create(context.Background(), obj)
	switch {
	case valid && err != nil:
		t.Errorf("%s: rejected: %v", name, err)
	case !valid && !apierrors.IsInvalid(err):
		t.Errorf("%s: got %v, want Invalid", name, err)
	}
}

func TestConfigCRDs(t *testing.T) {
	c := startAPIServer(t)
	n := 0
	meta := func() metav1.ObjectMeta {
		n++
		return metav1.ObjectMeta{Name: "x" + string(rune('a'+n%26)) + string(rune('a'+n/26))}
	}

	for name, tc := range map[string]struct {
		spec  v1alpha1.ModelProviderSpec
		valid bool
	}{
		"anthropic with a key":    {v1alpha1.ModelProviderSpec{Type: v1alpha1.ProviderAnthropic, Model: "claude-opus-5-5", Auth: key()}, true},
		"anthropic without a key": {v1alpha1.ModelProviderSpec{Type: v1alpha1.ProviderAnthropic, Model: "claude-opus-5-5"}, false},
		"openai with a base URL":  {v1alpha1.ModelProviderSpec{Type: v1alpha1.ProviderOpenAI, Model: "m", Auth: key(), BaseURL: "https://llm.internal/v1"}, true},
		"base URL not a URL":      {v1alpha1.ModelProviderSpec{Type: v1alpha1.ProviderOpenAI, Model: "m", Auth: key(), BaseURL: "llm.internal"}, false},
		"bedrock with identity":   {v1alpha1.ModelProviderSpec{Type: v1alpha1.ProviderBedrock, Model: "m", WorkloadIdentity: true, Region: "us-east-1"}, true},
		"bedrock without region":  {v1alpha1.ModelProviderSpec{Type: v1alpha1.ProviderBedrock, Model: "m", WorkloadIdentity: true}, false},
		"vertex with project":     {v1alpha1.ModelProviderSpec{Type: v1alpha1.ProviderVertex, Model: "m", WorkloadIdentity: true, Project: "p", Location: "us-east5"}, true},
		"vertex without location": {v1alpha1.ModelProviderSpec{Type: v1alpha1.ProviderVertex, Model: "m", WorkloadIdentity: true, Project: "p"}, false},
		"vertex with nothing":     {v1alpha1.ModelProviderSpec{Type: v1alpha1.ProviderVertex, Model: "m"}, false},
		"anthropic with a region": {v1alpha1.ModelProviderSpec{Type: v1alpha1.ProviderAnthropic, Model: "m", Auth: key(), Region: "us-east-1"}, false},
		"openai with a project":   {v1alpha1.ModelProviderSpec{Type: v1alpha1.ProviderOpenAI, Model: "m", Auth: key(), Project: "p"}, false},
		"bedrock with a base URL": {v1alpha1.ModelProviderSpec{Type: v1alpha1.ProviderBedrock, Model: "m", WorkloadIdentity: true, Region: "r", BaseURL: "https://x"}, false},
		"anthropic with identity": {v1alpha1.ModelProviderSpec{Type: v1alpha1.ProviderAnthropic, Model: "m", Auth: key(), WorkloadIdentity: true}, false},
		"unknown type":            {v1alpha1.ModelProviderSpec{Type: "azure", Model: "m", Auth: key()}, false},
		"no model":                {v1alpha1.ModelProviderSpec{Type: v1alpha1.ProviderAnthropic, Auth: key()}, false},
	} {
		expect(t, c, "ModelProvider "+name, &v1alpha1.ModelProvider{ObjectMeta: meta(), Spec: tc.spec}, tc.valid)
	}

	for name, tc := range map[string]struct {
		spec  v1alpha1.ToolServerSpec
		valid bool
	}{
		"external":           {v1alpha1.ToolServerSpec{URL: "https://api.example.com/mcp/", Auth: &v1alpha1.ToolServerAuth{SecretRef: v1alpha1.SecretKeyRef{Name: "github", Key: "token"}}, Permissions: map[string]v1alpha1.Permission{"create_pull_request": v1alpha1.PermissionAsk, "*": v1alpha1.PermissionAllow}}, true},
		"api key header":     {v1alpha1.ToolServerSpec{URL: "https://x", Auth: &v1alpha1.ToolServerAuth{SecretRef: v1alpha1.SecretKeyRef{Name: "s", Key: "k"}, Header: "X-API-Key", Prefix: new(string)}}, true},
		"bad header name":    {v1alpha1.ToolServerSpec{URL: "https://x", Auth: &v1alpha1.ToolServerAuth{SecretRef: v1alpha1.SecretKeyRef{Name: "s", Key: "k"}, Header: "X API Key"}}, false},
		"managed":            {v1alpha1.ToolServerSpec{Managed: &v1alpha1.ManagedServer{Image: "example/search:1.0", Port: 8080}}, true},
		"both":               {v1alpha1.ToolServerSpec{URL: "https://x", Managed: &v1alpha1.ManagedServer{Image: "i", Port: 1}}, false},
		"neither":            {v1alpha1.ToolServerSpec{}, false},
		"unknown permission": {v1alpha1.ToolServerSpec{URL: "https://x", Permissions: map[string]v1alpha1.Permission{"*": "maybe"}}, false},
		"unknown transport":  {v1alpha1.ToolServerSpec{URL: "https://x", Transport: "stdio"}, false},
		"managed port of 0":  {v1alpha1.ToolServerSpec{Managed: &v1alpha1.ManagedServer{Image: "i"}}, false},
		"managed with auth":  {v1alpha1.ToolServerSpec{Managed: &v1alpha1.ManagedServer{Image: "i", Port: 1}, Auth: &v1alpha1.ToolServerAuth{SecretRef: v1alpha1.SecretKeyRef{Name: "s", Key: "k"}}}, false},
		"managed bad path":   {v1alpha1.ToolServerSpec{Managed: &v1alpha1.ManagedServer{Image: "i", Port: 1, Path: "mcp"}}, false},
	} {
		expect(t, c, "ToolServer "+name, &v1alpha1.ToolServer{ObjectMeta: meta(), Spec: tc.spec}, tc.valid)
	}

	task := v1alpha1.TaskSpec{Title: "Today's thread", Assignee: &v1alpha1.Assignee{Kind: v1alpha1.AssigneeAgent, Name: "writer"}}
	withParent := task
	withParent.ParentRef = &v1alpha1.TaskReference{Name: "epic"}
	for name, tc := range map[string]struct {
		name  string
		spec  v1alpha1.ScheduleSpec
		valid bool
	}{
		"valid":         {"", v1alpha1.ScheduleSpec{Cron: "0 9 * * *", Timezone: "Asia/Taipei", Jitter: &metav1.Duration{Duration: time.Minute}, Task: task}, true},
		"with a parent": {"", v1alpha1.ScheduleSpec{Cron: "0 9 * * *", Task: withParent}, false},
		"no cron":       {"", v1alpha1.ScheduleSpec{Task: task}, false},
		"no title":      {"", v1alpha1.ScheduleSpec{Cron: "@daily", Task: v1alpha1.TaskSpec{}}, false},
		"name of 50":    {strings.Repeat("s", 50), v1alpha1.ScheduleSpec{Cron: "@daily", Task: task}, true},
		"name over 50":  {strings.Repeat("s", 51), v1alpha1.ScheduleSpec{Cron: "@daily", Task: task}, false},
	} {
		m := meta()
		if tc.name != "" {
			m.Name = tc.name
		}
		expect(t, c, "Schedule "+name, &v1alpha1.Schedule{ObjectMeta: m, Spec: tc.spec}, tc.valid)
	}

	for name, tc := range map[string]struct {
		spec  v1alpha1.AgentSpec
		valid bool
	}{
		"builtin":               {v1alpha1.AgentSpec{ModelRef: "claude", ToolRefs: []string{"github"}, Runtime: v1alpha1.AgentRuntime{Type: v1alpha1.RuntimeBuiltin}}, true},
		"runner with selector":  {v1alpha1.AgentSpec{ModelRef: "claude", Runtime: v1alpha1.AgentRuntime{Type: v1alpha1.RuntimeRunner, Selector: map[string]string{"pool": "claude-code"}}}, true},
		"runner, no selector":   {v1alpha1.AgentSpec{ModelRef: "claude", Runtime: v1alpha1.AgentRuntime{Type: v1alpha1.RuntimeRunner}}, false},
		"builtin with selector": {v1alpha1.AgentSpec{ModelRef: "claude", Runtime: v1alpha1.AgentRuntime{Type: v1alpha1.RuntimeBuiltin, Selector: map[string]string{"a": "b"}}}, false},
		"reserved label prefix": {v1alpha1.AgentSpec{ModelRef: "claude", Runtime: v1alpha1.AgentRuntime{Type: v1alpha1.RuntimeRunner, Selector: map[string]string{"cadre.dev/runtime": "builtin"}}}, false},
		"no model":              {v1alpha1.AgentSpec{Runtime: v1alpha1.AgentRuntime{Type: v1alpha1.RuntimeBuiltin}}, false},
		"builtin in steps":      {v1alpha1.AgentSpec{ModelRef: "claude", Runtime: v1alpha1.AgentRuntime{Type: v1alpha1.RuntimeBuiltin, Execution: v1alpha1.RunModeStep}}, true},
		"runner in steps":       {v1alpha1.AgentSpec{ModelRef: "claude", Runtime: v1alpha1.AgentRuntime{Type: v1alpha1.RuntimeRunner, Selector: map[string]string{"a": "b"}, Execution: v1alpha1.RunModeStep}}, false},
		// No schema default: a default of step would make every runner
		// agent invalid.
		"runner by default":  {v1alpha1.AgentSpec{ModelRef: "claude", Runtime: v1alpha1.AgentRuntime{Type: v1alpha1.RuntimeRunner, Selector: map[string]string{"a": "b"}}}, true},
		"builtin with a job": {v1alpha1.AgentSpec{ModelRef: "claude", Runtime: v1alpha1.AgentRuntime{Type: v1alpha1.RuntimeBuiltin, Job: &v1alpha1.AgentJob{RunnerRef: "r"}}}, false},
		"runner with a job":  {v1alpha1.AgentSpec{ModelRef: "claude", Runtime: v1alpha1.AgentRuntime{Type: v1alpha1.RuntimeRunner, Selector: map[string]string{"a": "b"}, Job: &v1alpha1.AgentJob{RunnerRef: "r"}}}, true},
	} {
		expect(t, c, "Agent "+name, &v1alpha1.Agent{ObjectMeta: meta(), Spec: tc.spec}, tc.valid)
	}
	// A Job carries its agent's name in a label, which holds 63 characters.
	long := strings.Repeat("a", 64)
	jobSpec := v1alpha1.AgentSpec{ModelRef: "claude", Runtime: v1alpha1.AgentRuntime{Type: v1alpha1.RuntimeRunner, Selector: map[string]string{"a": "b"}, Job: &v1alpha1.AgentJob{RunnerRef: "r"}}}
	expect(t, c, "Agent with a job and a long name", &v1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: long, Namespace: "default"}, Spec: jobSpec}, false)
	jobSpec.Runtime.Job = nil
	expect(t, c, "Agent without a job and a long name", &v1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: long, Namespace: "default"}, Spec: jobSpec}, true)

	for name, tc := range map[string]struct {
		spec  v1alpha1.RunnerSpec
		valid bool
	}{
		"minted token":             {v1alpha1.RunnerSpec{Labels: map[string]string{"pool": "gpu"}}, true},
		"service account":          {v1alpha1.RunnerSpec{ServiceAccount: &v1alpha1.ServiceAccountRef{Name: "ci"}, Labels: map[string]string{"pool": "ci"}}, true},
		"service account, no name": {v1alpha1.RunnerSpec{ServiceAccount: &v1alpha1.ServiceAccountRef{}, Labels: map[string]string{"pool": "ci"}}, false},
		"no labels":                {v1alpha1.RunnerSpec{}, false},
		"reserved label prefix":    {v1alpha1.RunnerSpec{Labels: map[string]string{"cadre.dev/runtime": "builtin"}}, false},
	} {
		expect(t, c, "Runner "+name, &v1alpha1.Runner{ObjectMeta: meta(), Spec: tc.spec}, tc.valid)
	}

	// Defaults: an Agent applied without a runtime is builtin (sent as raw
	// YAML would be: the typed client always sends a runtime object); a
	// ToolServer without a transport speaks streamable HTTP.
	raw := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": v1alpha1.GroupVersion.String(), "kind": "Agent",
		"metadata": map[string]any{"name": "no-runtime", "namespace": ns},
		"spec":     map[string]any{"modelRef": "claude"},
	}}
	if err := c.Create(context.Background(), raw); err != nil {
		t.Fatalf("agent without runtime: %v", err)
	}
	if typ, _, _ := unstructured.NestedString(raw.Object, "spec", "runtime", "type"); typ != string(v1alpha1.RuntimeBuiltin) {
		t.Errorf("agent applied without runtime has runtime type %q, want builtin", typ)
	}
	a := &v1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "defaults", Namespace: ns}, Spec: v1alpha1.AgentSpec{ModelRef: "claude"}}
	if err := c.Create(context.Background(), a); err != nil || a.Spec.Runtime.Type != v1alpha1.RuntimeBuiltin || a.Spec.Effort != v1alpha1.EffortHigh {
		t.Errorf("agent defaults: %+v, %v", a.Spec, err)
	}
	ts := &v1alpha1.ToolServer{ObjectMeta: metav1.ObjectMeta{Name: "defaults", Namespace: ns}, Spec: v1alpha1.ToolServerSpec{URL: "https://x"}}
	if err := c.Create(context.Background(), ts); err != nil || ts.Spec.Transport != v1alpha1.TransportStreamableHTTP {
		t.Errorf("toolserver defaults: %+v, %v", ts.Spec, err)
	}
	authed := &v1alpha1.ToolServer{ObjectMeta: metav1.ObjectMeta{Name: "auth-defaults", Namespace: ns}, Spec: v1alpha1.ToolServerSpec{
		URL: "https://x", Auth: &v1alpha1.ToolServerAuth{SecretRef: v1alpha1.SecretKeyRef{Name: "s", Key: "k"}},
	}}
	if err := c.Create(context.Background(), authed); err != nil || authed.Spec.Auth.Header != "Authorization" || authed.Spec.Auth.Prefix == nil || *authed.Spec.Auth.Prefix != "Bearer " {
		t.Errorf("toolserver auth defaults: %+v, %v", authed.Spec.Auth, err)
	}
}

func TestTeamRolesCRD(t *testing.T) {
	c := startAPIServer(t)
	ctx := context.Background()
	nsObj := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": "roles-valid"}}}
	if err := c.Create(ctx, nsObj); err != nil {
		t.Fatal(err)
	}
	subject := func(kind v1alpha1.SubjectKind, name string) []v1alpha1.RoleSubject {
		return []v1alpha1.RoleSubject{{Kind: kind, Name: name}}
	}
	valid := &v1alpha1.Team{ObjectMeta: metav1.ObjectMeta{Namespace: "roles-valid", Name: v1alpha1.TeamName}, Spec: v1alpha1.TeamSpec{Roles: []v1alpha1.TeamRole{
		{Role: v1alpha1.RoleAdmin, Subjects: subject(v1alpha1.SubjectUser, "lead@example.test")},
		{Role: v1alpha1.RoleMember, Subjects: subject(v1alpha1.SubjectGroup, "backend")},
		{Role: v1alpha1.RoleViewer, Subjects: subject(v1alpha1.SubjectGroup, "everyone")},
	}}}
	if err := c.Create(ctx, valid); err != nil {
		t.Errorf("valid roles: %v", err)
	}
	for name, roles := range map[string][]v1alpha1.TeamRole{
		"an unknown role":   {{Role: "owner", Subjects: subject(v1alpha1.SubjectUser, "x")}},
		"an unknown kind":   {{Role: v1alpha1.RoleMember, Subjects: subject("team", "x")}},
		"a subject unnamed": {{Role: v1alpha1.RoleMember, Subjects: subject(v1alpha1.SubjectGroup, "")}},
		"no subjects":       {{Role: v1alpha1.RoleMember}},
	} {
		team := &v1alpha1.Team{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: v1alpha1.TeamName}, Spec: v1alpha1.TeamSpec{Roles: roles}}
		if err := c.Create(ctx, team, client.DryRunAll); !apierrors.IsInvalid(err) {
			t.Errorf("%s: got %v, want Invalid", name, err)
		}
	}
}

func TestNotifierCRD(t *testing.T) {
	c := startAPIServer(t)
	n := 0
	for name, tc := range map[string]struct {
		spec  v1alpha1.NotifierSpec
		valid bool
	}{
		"valid": {v1alpha1.NotifierSpec{On: []v1alpha1.NotifyEvent{v1alpha1.NotifyQuestion, v1alpha1.NotifyDone},
			Webhook: v1alpha1.Webhook{URL: "https://hooks.example.test/x", SecretRef: &v1alpha1.SecretKeyRef{Name: "s", Key: "k"}, Headers: map[string]string{"X-Team": "a"}}}, true},
		"no events":              {v1alpha1.NotifierSpec{Webhook: v1alpha1.Webhook{URL: "https://hooks.example.test/x"}}, false},
		"an unknown event":       {v1alpha1.NotifierSpec{On: []v1alpha1.NotifyEvent{"Running"}, Webhook: v1alpha1.Webhook{URL: "https://hooks.example.test/x"}}, false},
		"not a URL":              {v1alpha1.NotifierSpec{On: []v1alpha1.NotifyEvent{v1alpha1.NotifyDone}, Webhook: v1alpha1.Webhook{URL: "hooks.example.test"}}, false},
		"an Authorization":       {v1alpha1.NotifierSpec{On: []v1alpha1.NotifyEvent{v1alpha1.NotifyDone}, Webhook: v1alpha1.Webhook{URL: "https://h.example.test", Headers: map[string]string{"authorization": "Bearer x"}}}, false},
		"credentials in the URL": {v1alpha1.NotifierSpec{On: []v1alpha1.NotifyEvent{v1alpha1.NotifyDone}, Webhook: v1alpha1.Webhook{URL: "https://user:token@h.example.test/x"}}, false},
		"an @ in the path":       {v1alpha1.NotifierSpec{On: []v1alpha1.NotifyEvent{v1alpha1.NotifyDone}, Webhook: v1alpha1.Webhook{URL: "https://h.example.test/u/@team?to=a@b"}}, true},
		"twice the same one":     {v1alpha1.NotifierSpec{On: []v1alpha1.NotifyEvent{v1alpha1.NotifyDone, v1alpha1.NotifyDone}, Webhook: v1alpha1.Webhook{URL: "https://h.example.test"}}, false},
	} {
		n++
		expect(t, c, "Notifier "+name, &v1alpha1.Notifier{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("n%d", n)}, Spec: tc.spec}, tc.valid)
	}
}
