package v1alpha1_test

import (
	"context"
	"testing"

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
	} {
		expect(t, c, "ToolServer "+name, &v1alpha1.ToolServer{ObjectMeta: meta(), Spec: tc.spec}, tc.valid)
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
	} {
		expect(t, c, "Agent "+name, &v1alpha1.Agent{ObjectMeta: meta(), Spec: tc.spec}, tc.valid)
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
