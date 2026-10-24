package main

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
)

func TestSecrets(t *testing.T) {
	sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "anthropic", Namespace: "team-a"}, Data: map[string][]byte{"api-key": []byte("not-a-real-key")}}
	s := secrets{fake.NewClientBuilder().WithObjects(sec).Build()}
	ctx := context.Background()
	if v, err := s.Value(ctx, "team-a", v1alpha1.SecretKeyRef{Name: "anthropic", Key: "api-key"}); err != nil || v != "not-a-real-key" {
		t.Fatalf("value %q, %v", v, err)
	}
	if _, err := s.Value(ctx, "team-a", v1alpha1.SecretKeyRef{Name: "anthropic", Key: "other"}); err == nil {
		t.Fatal("a missing key must fail")
	}
	if _, err := s.Value(ctx, "team-b", v1alpha1.SecretKeyRef{Name: "anthropic", Key: "api-key"}); err == nil {
		t.Fatal("a Secret in another namespace must not be found")
	}
}

// A runner started for a Job takes its name, server and namespace from the
// Job's environment, and works one whole run of the Job's agent.
func TestFlagDefaults(t *testing.T) {
	env := func(vars map[string]string) func(string) string { return func(k string) string { return vars[k] } }
	if d := flagDefaults(env(nil), "host-1"); d != (defaults{server: "http://cadre-server:8080", runner: "host-1", mode: "step"}) {
		t.Errorf("outside a Job: %+v", d)
	}
	d := flagDefaults(env(map[string]string{
		"CADRE_SERVER": "http://cadre.test", "CADRE_NAMESPACE": "team-a", "CADRE_RUNNER": "coder-01jb", "CADRE_AGENT": "coder",
	}), "host-1")
	if d != (defaults{server: "http://cadre.test", runner: "coder-01jb", namespaces: "team-a", mode: "runtime", agent: "coder", once: true}) {
		t.Errorf("in a Job: %+v", d)
	}
}
