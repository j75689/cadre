package v1alpha1_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
)

const ns = "default"

// startAPIServer applies the generated CRDs to an envtest API server. Outside
// CI it skips when the envtest binaries are not installed; `make test`
// installs them. In CI a missing install fails, so coverage cannot silently
// disappear.
func startAPIServer(t *testing.T) client.Client {
	t.Helper()
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		if os.Getenv("CI") != "" {
			t.Fatal("KUBEBUILDER_ASSETS is not set in CI; run make test")
		}
		t.Skip("KUBEBUILDER_ASSETS is not set; run make test")
	}
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("start envtest: %v", err)
	}
	t.Cleanup(func() {
		if err := env.Stop(); err != nil {
			t.Errorf("stop envtest: %v", err)
		}
	})
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add to scheme: %v", err)
	}
	// For the tests that change a CRD.
	if err := apiextensionsv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add to scheme: %v", err)
	}
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	return c
}

func TestCRDs(t *testing.T) {
	c := startAPIServer(t)
	ctx := context.Background()

	t.Run("team must be named team", func(t *testing.T) {
		bad := &v1alpha1.Team{ObjectMeta: metav1.ObjectMeta{Name: "backend", Namespace: ns}}
		if err := c.Create(ctx, bad); !apierrors.IsInvalid(err) {
			t.Fatalf("create Team named backend: got %v, want Invalid", err)
		}
	})

	t.Run("team defaults and status subresource", func(t *testing.T) {
		team := &v1alpha1.Team{ObjectMeta: metav1.ObjectMeta{Name: v1alpha1.TeamName, Namespace: ns}}
		if err := c.Create(ctx, team); err != nil {
			t.Fatalf("create Team: %v", err)
		}
		if team.Spec.MaxConcurrentRuns != 1 {
			t.Fatalf("maxConcurrentRuns = %d, want default 1", team.Spec.MaxConcurrentRuns)
		}
		team.Status.NextNumber = 7
		if err := c.Status().Update(ctx, team); err != nil {
			t.Fatalf("update Team status: %v", err)
		}
		got := &v1alpha1.Team{}
		if err := c.Get(ctx, client.ObjectKeyFromObject(team), got); err != nil {
			t.Fatalf("get Team: %v", err)
		}
		if got.Status.NextNumber != 7 {
			t.Fatalf("nextNumber = %d, want 7", got.Status.NextNumber)
		}
	})

	t.Run("team rejects negative concurrency", func(t *testing.T) {
		team := &v1alpha1.Team{
			ObjectMeta: metav1.ObjectMeta{Name: v1alpha1.TeamName, Namespace: "kube-public"},
			Spec:       v1alpha1.TeamSpec{MaxConcurrentRuns: -1},
		}
		if err := c.Create(ctx, team); !apierrors.IsInvalid(err) {
			t.Fatalf("create Team with maxConcurrentRuns -1: got %v, want Invalid", err)
		}
	})

	t.Run("team archiveAfter must be a positive duration", func(t *testing.T) {
		// The typed client cannot even encode these, so send raw objects.
		for _, d := range []string{"30d", "-1h", "0s"} {
			team := &unstructured.Unstructured{Object: map[string]any{
				"apiVersion": v1alpha1.GroupVersion.String(),
				"kind":       "Team",
				"metadata":   map[string]any{"name": v1alpha1.TeamName, "namespace": "kube-public"},
				"spec":       map[string]any{"archiveAfter": d},
			}}
			if err := c.Create(ctx, team); !apierrors.IsInvalid(err) {
				t.Errorf("archiveAfter %q: got %v, want Invalid", d, err)
			}
		}
	})

	t.Run("task validation", func(t *testing.T) {
		for name, spec := range map[string]v1alpha1.TaskSpec{
			"missing title":     {},
			"unknown priority":  {Title: "x", Priority: "whenever"},
			"unknown assignee":  {Title: "x", Assignee: &v1alpha1.Assignee{Kind: "Robot", Name: "r"}},
			"own parent":        {Title: "x", ParentRef: &v1alpha1.TaskReference{Name: "invalid"}},
			"parent not a name": {Title: "x", ParentRef: &v1alpha1.TaskReference{Name: "Not_A_Name"}},
			"parent a number":   {Title: "x", ParentRef: &v1alpha1.TaskReference{Name: "#1"}},
		} {
			task := &v1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: "invalid", Namespace: ns}, Spec: spec}
			if err := c.Create(ctx, task); !apierrors.IsInvalid(err) {
				t.Errorf("%s: got %v, want Invalid", name, err)
			}
		}
	})

	t.Run("task spec and status are written separately", func(t *testing.T) {
		task := &v1alpha1.Task{
			ObjectMeta: metav1.ObjectMeta{Name: "fix-login-flake", Namespace: ns},
			Spec:       v1alpha1.TaskSpec{Title: "Fix the flaky login test"},
		}
		if err := c.Create(ctx, task); err != nil {
			t.Fatalf("create Task: %v", err)
		}
		if task.Spec.Priority != v1alpha1.PriorityMedium {
			t.Fatalf("priority = %q, want default medium", task.Spec.Priority)
		}

		// A write to the main resource must not touch status.
		task.Status.Phase = v1alpha1.PhaseDone
		if err := c.Update(ctx, task); err != nil {
			t.Fatalf("update Task: %v", err)
		}
		if task.Status.Phase != "" {
			t.Fatalf("phase = %q after a spec update, want it untouched", task.Status.Phase)
		}

		lease := metav1.NewTime(time.Now().Add(time.Minute).Truncate(time.Second))
		task.Status = v1alpha1.TaskStatus{
			Number: 1,
			Phase:  v1alpha1.PhaseRunning,
			Run: &v1alpha1.Run{
				ID: "01JB0000000000000000000000", Mode: v1alpha1.RunModeRuntime,
				Runner: "runner-7f3a", LeaseUntil: lease,
			},
			Outbox: []v1alpha1.OutboxItem{{
				ID: "01JB0000000000000000000001", Kind: v1alpha1.OutboxCreateSubtask,
				Spec: &v1alpha1.TaskSpec{Title: "child"},
			}},
		}
		if err := c.Status().Update(ctx, task); err != nil {
			t.Fatalf("update Task status: %v", err)
		}
		got := &v1alpha1.Task{}
		if err := c.Get(ctx, client.ObjectKeyFromObject(task), got); err != nil {
			t.Fatalf("get Task: %v", err)
		}
		if got.Status.Run == nil || got.Status.Run.ID != task.Status.Run.ID || !got.Status.Run.LeaseUntil.Equal(&lease) {
			t.Fatalf("run = %+v, want %+v", got.Status.Run, task.Status.Run)
		}

		for name, it := range map[string]v1alpha1.OutboxItem{
			"id not a ULID":           {ID: "not-a-ulid", Kind: v1alpha1.OutboxCreateSubtask, Spec: &v1alpha1.TaskSpec{Title: "x"}},
			"CreateSubtask, no spec":  {ID: "01JB0000000000000000000002", Kind: v1alpha1.OutboxCreateSubtask},
			"RunEnded, no run":        {ID: "01JB0000000000000000000002", Kind: v1alpha1.OutboxRunEnded},
			"RunEnded, unknown cause": {ID: "01JB0000000000000000000002", Kind: v1alpha1.OutboxRunEnded, Run: &v1alpha1.RunRecord{ID: "01JB0000000000000000000002", Mode: v1alpha1.RunModeRuntime, Reason: "Bored", EndedAt: lease}},
		} {
			bad := got.DeepCopy()
			bad.Status.Outbox = []v1alpha1.OutboxItem{it}
			if err := c.Status().Update(ctx, bad); !apierrors.IsInvalid(err) {
				t.Errorf("outbox %s: got %v, want Invalid", name, err)
			}
		}

		got.Status.Outbox = append(got.Status.Outbox, v1alpha1.OutboxItem{
			ID: task.Status.Run.ID, Kind: v1alpha1.OutboxRunEnded,
			Run: &v1alpha1.RunRecord{
				ID: task.Status.Run.ID, Mode: v1alpha1.RunModeRuntime, Reason: v1alpha1.RunFinished,
				Outcome: v1alpha1.RunOutcomeDone, EndedAt: lease,
			},
		})
		if err := c.Status().Update(ctx, got); err != nil {
			t.Fatalf("valid RunEnded item: %v", err)
		}
	})
}
