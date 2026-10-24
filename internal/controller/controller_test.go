package controller_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	"github.com/j75689/cadre/internal/controller"
	"github.com/j75689/cadre/internal/testenv"
)

type env struct {
	t *testing.T
	c client.Client
}

func start(t *testing.T) *env {
	t.Helper()
	cfg := testenv.APIServer(t)
	mgr, err := controller.NewManager(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := controller.Setup(mgr, controller.Options{Resync: 200 * time.Millisecond}, controller.SkipNameValidation()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- mgr.Start(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("manager: %v", err)
		}
	})
	c, err := client.New(cfg, client.Options{Scheme: mgr.GetScheme()})
	if err != nil {
		t.Fatal(err)
	}
	return &env{t: t, c: c}
}

func (e *env) create(objs ...client.Object) {
	e.t.Helper()
	for _, o := range objs {
		if err := e.c.Create(context.Background(), o); err != nil {
			e.t.Fatalf("create %T %s: %v", o, o.GetName(), err)
		}
	}
}

// ready waits until obj's Ready condition has the given status and reason.
func (e *env) ready(obj client.Object, status metav1.ConditionStatus, reason string) {
	e.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		if err := e.c.Get(context.Background(), client.ObjectKeyFromObject(obj), obj); err == nil {
			conds := conditions(obj)
			c := meta.FindStatusCondition(conds, v1alpha1.ConditionReady)
			if c != nil && c.Status == status && c.Reason == reason && c.ObservedGeneration == obj.GetGeneration() {
				return
			}
			last = fmt.Sprintf("%+v", c)
		}
		time.Sleep(50 * time.Millisecond)
	}
	e.t.Fatalf("%T %s: Ready %s/%s never reached; last %s", obj, obj.GetName(), status, reason, last)
}

func conditions(obj client.Object) []metav1.Condition {
	switch o := obj.(type) {
	case *v1alpha1.Agent:
		return o.Status.Conditions
	case *v1alpha1.ModelProvider:
		return o.Status.Conditions
	case *v1alpha1.ToolServer:
		return o.Status.Conditions
	}
	return nil
}

func meta2(ns, name string) metav1.ObjectMeta { return metav1.ObjectMeta{Namespace: ns, Name: name} }

func TestReadiness(t *testing.T) {
	e := start(t)
	const ns = "team-a"
	e.create(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}},
		&v1alpha1.Team{ObjectMeta: meta2(ns, v1alpha1.TeamName)},
	)
	agent := &v1alpha1.Agent{ObjectMeta: meta2(ns, "coder"), Spec: v1alpha1.AgentSpec{ModelRef: "claude", ToolRefs: []string{"github"}}}
	e.create(agent)
	e.ready(agent, metav1.ConditionFalse, "ModelProviderNotFound")

	mp := &v1alpha1.ModelProvider{ObjectMeta: meta2(ns, "claude"), Spec: v1alpha1.ModelProviderSpec{
		Type: v1alpha1.ProviderAnthropic, Model: "claude-opus-5-5",
		Auth: &v1alpha1.Auth{SecretRef: v1alpha1.SecretKeyRef{Name: "anthropic", Key: "api-key"}},
	}}
	e.create(mp)
	e.ready(mp, metav1.ConditionFalse, "SecretNotFound")
	e.ready(agent, metav1.ConditionFalse, "ModelProviderNotReady")

	// A Secret without the key, then with it: found on the periodic
	// re-check, since Secrets are not watched.
	secret := &corev1.Secret{ObjectMeta: meta2(ns, "anthropic"), Data: map[string][]byte{"other": []byte("x")}}
	e.create(secret)
	e.ready(mp, metav1.ConditionFalse, "SecretKeyNotFound")
	secret.Data = map[string][]byte{"api-key": []byte("not-a-real-key")}
	if err := e.c.Update(context.Background(), secret); err != nil {
		t.Fatal(err)
	}
	e.ready(mp, metav1.ConditionTrue, "SecretFound")
	e.ready(agent, metav1.ConditionFalse, "ToolServerNotFound")

	managed := &v1alpha1.ToolServer{ObjectMeta: meta2(ns, "github"), Spec: v1alpha1.ToolServerSpec{Managed: &v1alpha1.ManagedServer{Image: "example/mcp:1.0", Port: 8080}}}
	e.create(managed)
	e.ready(managed, metav1.ConditionFalse, "Deploying")
	e.ready(agent, metav1.ConditionFalse, "ToolServerNotReady")

	if err := e.c.Delete(context.Background(), managed); err != nil {
		t.Fatal(err)
	}
	external := &v1alpha1.ToolServer{ObjectMeta: meta2(ns, "github"), Spec: v1alpha1.ToolServerSpec{URL: "https://mcp.example.com/"}}
	e.create(external)
	e.ready(external, metav1.ConditionTrue, "External")
	e.ready(agent, metav1.ConditionTrue, "Resolved")

	// A spec change is reflected for the new generation.
	agent.Spec.ToolRefs = append(agent.Spec.ToolRefs, "missing")
	if err := e.c.Update(context.Background(), agent); err != nil {
		t.Fatal(err)
	}
	e.ready(agent, metav1.ConditionFalse, "ToolServerNotFound")

	// Outside Team namespaces nothing is reconciled. (One manager per test
	// binary: controller-runtime rejects a second controller of a name.)
	e.create(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "elsewhere"}})
	stranger := &v1alpha1.Agent{ObjectMeta: meta2("elsewhere", "coder"), Spec: v1alpha1.AgentSpec{ModelRef: "claude"}}
	e.create(stranger)
	time.Sleep(time.Second)
	if err := e.c.Get(context.Background(), client.ObjectKeyFromObject(stranger), stranger); err != nil {
		t.Fatal(err)
	}
	if len(stranger.Status.Conditions) != 0 {
		t.Fatalf("an Agent outside any Team namespace got status %+v", stranger.Status)
	}
}
