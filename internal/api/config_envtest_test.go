package api_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	"github.com/j75689/cadre/internal/api"
	"github.com/j75689/cadre/internal/controller"
	"github.com/j75689/cadre/internal/manager"
	"github.com/j75689/cadre/internal/storage/memory"
	"github.com/j75689/cadre/internal/testenv"
)

// An Agent applied with kubectl is read from the controllers' cache, with
// the controller's verdict, and without what kubectl and the API server
// keep in its metadata.
func TestAgentFromTheCache(t *testing.T) {
	cfg := testenv.APIServer(t)
	mgr, err := controller.NewManager(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := controller.Setup(mgr, controller.Options{Resync: 200 * time.Millisecond}, controller.SkipNameValidation()); err != nil {
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
	agent := &v1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "writer", Annotations: map[string]string{
			"cadre.dev/owner": "docs-team",
			"kubectl.kubernetes.io/last-applied-configuration": `{"spec":{"modelRef":"claude"}}`,
		}},
		Spec: v1alpha1.AgentSpec{ModelRef: "claude"},
	}
	for _, o := range []ctrlclient.Object{
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}},
		&v1alpha1.Team{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: v1alpha1.TeamName}},
		agent,
	} {
		if err := c.Create(ctx, o); err != nil {
			t.Fatal(err)
		}
	}
	if len(agent.ManagedFields) == 0 {
		t.Fatal("the API server kept no fields' owners: nothing to leave out")
	}

	store := memory.NewBackend().Open()
	if err := store.CreateTeam(ctx, &v1alpha1.Team{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: v1alpha1.TeamName}}); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	api.NewServer(manager.New(store, manager.Config{}), slog.New(slog.NewTextHandler(io.Discard, nil)), api.WithConfig(mgr.GetClient())).Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	var body string
	deadline := time.Now().Add(10 * time.Second)
	for {
		res, err := http.Get(srv.URL + api.Base + "/namespaces/" + ns + "/agents/writer")
		if err != nil {
			t.Fatal(err)
		}
		out, _ := io.ReadAll(res.Body)
		_ = res.Body.Close()
		body = string(out)
		var got api.ConfigResource
		if res.StatusCode == http.StatusOK && json.Unmarshal(out, &got) == nil && got.Reason == "ModelProviderNotFound" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the controller's verdict never showed: %d %s", res.StatusCode, body)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !strings.Contains(body, `"cadre.dev/owner":"docs-team"`) || !strings.Contains(body, `"modelRef":"claude"`) || !strings.Contains(body, string(agent.UID)) {
		t.Errorf("an admin's view lacks the annotations, spec or uid: %s", body)
	}
	for _, never := range []string{"last-applied", "managedFields", "f:spec"} {
		if strings.Contains(body, never) {
			t.Errorf("served %q: %s", never, body)
		}
	}
}
