package api_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	"github.com/j75689/cadre/internal/api"
	"github.com/j75689/cadre/internal/controller"
	"github.com/j75689/cadre/internal/testenv"
)

// Each configuration kind and the Team, applied, changed in its spec and
// in its status, and deleted, is one hint each time on its namespace's
// streams, and none on another's.
func TestWatchConfigHints(t *testing.T) {
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
	for _, name := range []string{"team-a", "team-b"} {
		if err := c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}); err != nil {
			t.Fatal(err)
		}
	}
	hub := api.NewHub(nil)
	if err := api.ConfigEvents(ctx, mgr.GetCache(), hub); err != nil {
		t.Fatal(err)
	}
	url := startPlain(t, hub, fastWatch)
	_, events := watch(t, url, "", "/namespaces/team-a/watch")
	_, elsewhere := watch(t, url, "", "/namespaces/team-b/watch")
	for _, s := range []<-chan sseEvent{events, elsewhere} {
		if ev := next(t, s); ev.kind != "hello" || ev.data != `{"tasks":false,"config":true,"runs":false,"lifetimeSeconds":60}` {
			t.Fatalf("first event %+v, want hello with config", ev)
		}
	}

	meta := func(name string) metav1.ObjectMeta { return metav1.ObjectMeta{Namespace: "team-a", Name: name} }
	status := `{"status":{"observedGeneration":2}}`
	for _, k := range []struct {
		kind         string
		obj          ctrlclient.Object
		spec, status string
	}{
		{"Agent", &v1alpha1.Agent{ObjectMeta: meta("writer"), Spec: v1alpha1.AgentSpec{ModelRef: "claude"}}, `{"spec":{"description":"docs"}}`, status},
		{"ModelProvider", &v1alpha1.ModelProvider{ObjectMeta: meta("claude"), Spec: v1alpha1.ModelProviderSpec{
			Type: v1alpha1.ProviderAnthropic, Model: "claude-opus-5-5",
			Auth: &v1alpha1.Auth{SecretRef: v1alpha1.SecretKeyRef{Name: "anthropic", Key: "api-key"}},
		}}, `{"spec":{"model":"claude-sonnet-5-5"}}`, status},
		{"ToolServer", &v1alpha1.ToolServer{ObjectMeta: meta("github"), Spec: v1alpha1.ToolServerSpec{URL: "https://mcp.example.com/"}}, `{"spec":{"notifications":true}}`, status},
		{"Schedule", &v1alpha1.Schedule{ObjectMeta: meta("nightly"), Spec: v1alpha1.ScheduleSpec{Cron: "0 3 * * *", Task: v1alpha1.TaskSpec{Title: "triage"}}}, `{"spec":{"suspend":true}}`, status},
		{"Notifier", &v1alpha1.Notifier{ObjectMeta: meta("chat"), Spec: v1alpha1.NotifierSpec{
			On: []v1alpha1.NotifyEvent{v1alpha1.NotifyQuestion}, Webhook: &v1alpha1.Webhook{URL: "https://hooks.example.com/x"},
		}}, `{"spec":{"on":["Question","Review"]}}`, status},
		{"Runner", &v1alpha1.Runner{ObjectMeta: meta("laptop"), Spec: v1alpha1.RunnerSpec{Labels: map[string]string{"pool": "a"}}}, `{"spec":{"labels":{"pool":"b"}}}`, status},
		{"Team", &v1alpha1.Team{ObjectMeta: meta(v1alpha1.TeamName)}, `{"spec":{"displayName":"Team A"}}`, `{"status":{"nextNumber":7}}`},
	} {
		want := fmt.Sprintf(`{"kind":%q,"name":%q}`, k.kind, k.obj.GetName())
		for _, step := range []struct {
			what  string
			write func() error
		}{
			{"apply", func() error { return c.Create(ctx, k.obj) }},
			{"spec", func() error { return c.Patch(ctx, k.obj, ctrlclient.RawPatch(types.MergePatchType, []byte(k.spec))) }},
			{"status", func() error {
				return c.Status().Patch(ctx, k.obj, ctrlclient.RawPatch(types.MergePatchType, []byte(k.status)))
			}},
			{"delete", func() error { return c.Delete(ctx, k.obj) }},
		} {
			if err := step.write(); err != nil {
				t.Fatalf("%s %s: %v", step.what, k.kind, err)
			}
			if ev := next(t, events); ev.kind != "config" || ev.data != want {
				t.Fatalf("after %s of %s: %+v, want config %s", step.what, k.kind, ev, want)
			}
			// One hint: a second would come within the flush.
			quiet(t, events, 3*fastWatch.Coalesce)
		}
	}
	quiet(t, elsewhere, 100*time.Millisecond)
}
