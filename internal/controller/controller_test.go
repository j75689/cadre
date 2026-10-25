package controller_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	"github.com/j75689/cadre/internal/controller"
	"github.com/j75689/cadre/internal/testenv"
)

type env struct {
	t      *testing.T
	c      client.Client
	cfg    *rest.Config
	lifted map[string]apiextensionsv1.ValidationRule // the name rules liftNameRule took off, by CRD
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
	return &env{t: t, c: c, cfg: cfg}
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
	case *v1alpha1.Schedule:
		return o.Status.Conditions
	case *v1alpha1.Notifier:
		return o.Status.Conditions
	case *v1alpha1.Runner:
		return o.Status.Conditions
	case *v1alpha1.AgentAccess:
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

	// Outside Team namespaces nothing is acted on, and every resource says
	// why it is not Ready; once there is a Team, they go on. (One manager
	// per test binary: controller-runtime rejects a second controller of a
	// name.)
	const elsewhere = "elsewhere"
	e.create(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: elsewhere}})
	strangers := []struct {
		obj    client.Object
		reason string // once there is a Team
	}{
		{&v1alpha1.ModelProvider{ObjectMeta: meta2(elsewhere, "claude"), Spec: v1alpha1.ModelProviderSpec{
			Type: v1alpha1.ProviderBedrock, Model: "anthropic.claude", WorkloadIdentity: true, Region: "us-east-1",
		}}, "WorkloadIdentity"},
		{&v1alpha1.ToolServer{ObjectMeta: meta2(elsewhere, "github"), Spec: v1alpha1.ToolServerSpec{URL: "https://mcp.example.com/"}}, "External"},
		{&v1alpha1.ToolServer{ObjectMeta: meta2(elsewhere, "search"), Spec: v1alpha1.ToolServerSpec{Managed: &v1alpha1.ManagedServer{Image: "example/mcp:1.0", Port: 8080}}}, "Deploying"},
		{&v1alpha1.Agent{ObjectMeta: meta2(elsewhere, "coder"), Spec: v1alpha1.AgentSpec{ModelRef: "claude", ToolRefs: []string{"github"}}}, "Resolved"},
		// Nothing it refers to changes when the Team goes: only the
		// resync brings it back.
		{&v1alpha1.Agent{ObjectMeta: meta2(elsewhere, "lonely"), Spec: v1alpha1.AgentSpec{ModelRef: "missing"}}, "ModelProviderNotFound"},
		{&v1alpha1.Runner{ObjectMeta: meta2(elsewhere, "outside"), Spec: v1alpha1.RunnerSpec{Labels: map[string]string{"pool": "a"}}}, "TokenMinted"},
	}
	for _, s := range strangers {
		e.create(s.obj)
	}
	for _, s := range strangers {
		e.ready(s.obj, metav1.ConditionFalse, "NoTeam")
		if c := meta.FindStatusCondition(conditions(s.obj), v1alpha1.ConditionReady); c.Message != "namespace elsewhere has no Team" {
			t.Errorf("%T %s: message %q", s.obj, s.obj.GetName(), c.Message)
		}
	}
	// Nor did any of them make what it would in a Team namespace.
	for name, obj := range map[string]client.Object{"search": &appsv1.Deployment{}, controller.TokenSecretName("outside"): &corev1.Secret{}} {
		if err := e.c.Get(context.Background(), client.ObjectKey{Namespace: elsewhere, Name: name}, obj); !apierrors.IsNotFound(err) {
			t.Errorf("%T %s made outside a Team namespace: %v", obj, name, err)
		}
	}
	recovered := func() {
		t.Helper()
		e.create(&v1alpha1.Team{ObjectMeta: meta2(elsewhere, v1alpha1.TeamName)})
		for _, s := range strangers {
			status := metav1.ConditionTrue
			if s.reason == "Deploying" || s.reason == "ModelProviderNotFound" {
				status = metav1.ConditionFalse
			}
			e.ready(s.obj, status, s.reason)
		}
	}
	recovered()

	// Its Team deleted, each says so within a resync, and what it made
	// stays: the Deployment, and the Runner's token, which claims nothing
	// meanwhile. With a Team again they go on as they were.
	var dep appsv1.Deployment
	var token corev1.Secret
	ctx := context.Background()
	if err := e.c.Get(ctx, client.ObjectKey{Namespace: elsewhere, Name: "search"}, &dep); err != nil {
		t.Fatal(err)
	}
	if err := e.c.Get(ctx, client.ObjectKey{Namespace: elsewhere, Name: controller.TokenSecretName("outside")}, &token); err != nil {
		t.Fatal(err)
	}
	runner := &v1alpha1.Runner{}
	if err := e.c.Get(ctx, client.ObjectKey{Namespace: elsewhere, Name: "outside"}, runner); err != nil {
		t.Fatal(err)
	}
	hash := runner.Status.TokenHash
	if err := e.c.Delete(ctx, &v1alpha1.Team{ObjectMeta: meta2(elsewhere, v1alpha1.TeamName)}); err != nil {
		t.Fatal(err)
	}
	for _, s := range strangers {
		e.ready(s.obj, metav1.ConditionFalse, "NoTeam")
	}
	for _, obj := range []client.Object{&dep, &token} {
		rv := obj.GetResourceVersion()
		if err := e.c.Get(ctx, client.ObjectKeyFromObject(obj), obj); err != nil {
			t.Fatalf("%T %s gone with the Team: %v", obj, obj.GetName(), err)
		}
		if obj.GetResourceVersion() != rv {
			t.Errorf("%T %s changed with the Team gone", obj, obj.GetName())
		}
	}
	recovered()
	// The same token, not a new one.
	if err := e.c.Get(ctx, client.ObjectKeyFromObject(runner), runner); err != nil {
		t.Fatal(err)
	}
	if runner.Status.TokenHash != hash || hash == "" {
		t.Errorf("the Runner's token hash went from %q to %q", hash, runner.Status.TokenHash)
	}
}

// liftNameRule takes a CRD's name rule off (off), or puts it back, so that
// a test can make an object as it was made before the rule.
func (e *env) liftNameRule(crd string, off bool) {
	e.t.Helper()
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := apiextensionsv1.AddToScheme(scheme); err != nil {
		e.t.Fatal(err)
	}
	crds, err := client.New(e.cfg, client.Options{Scheme: scheme})
	if err != nil {
		e.t.Fatal(err)
	}
	var def apiextensionsv1.CustomResourceDefinition
	if err := crds.Get(ctx, client.ObjectKey{Name: crd}, &def); err != nil {
		e.t.Fatal(err)
	}
	schema := def.Spec.Versions[0].Schema.OpenAPIV3Schema
	var kept apiextensionsv1.ValidationRules
	for _, r := range schema.XValidations {
		if strings.Contains(r.Rule, "oldSelf.hasValue()") {
			if e.lifted == nil {
				e.lifted = map[string]apiextensionsv1.ValidationRule{}
			}
			e.lifted[crd] = r
		} else {
			kept = append(kept, r)
		}
	}
	if !off {
		rule, ok := e.lifted[crd]
		if !ok {
			e.t.Fatalf("%s: the name rule was not lifted, so there is none to put back", crd)
		}
		kept = append(kept, rule)
	}
	schema.XValidations = kept
	if err := crds.Update(ctx, &def); err != nil {
		e.t.Fatal(err)
	}
}

// createDespiteTheRule creates obj, retrying until the API server, its
// CRD's name rule just lifted, takes it.
func (e *env) createDespiteTheRule(obj client.Object) {
	e.t.Helper()
	eventually(e.t, 10*time.Second, func() (bool, string) {
		err := e.c.Create(context.Background(), obj.DeepCopyObject().(client.Object))
		return err == nil, fmt.Sprint(err)
	})
}
