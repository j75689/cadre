package kubernetes_test

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/oklog/ulid/v2"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	"github.com/j75689/cadre/internal/s3fake"
	"github.com/j75689/cadre/internal/storage"
	"github.com/j75689/cadre/internal/storage/conformance"
	"github.com/j75689/cadre/internal/storage/kubernetes"
	"github.com/j75689/cadre/internal/testenv"
)

func TestConformance(t *testing.T) {
	cfg := testenv.APIServer(t)
	runConformance(t, cfg, testenv.S3(t))
}

// The fake can behave like a filesystem-backed store, so the refusal paths
// SeaweedFS takes also run without one.
func TestConformanceOnAFilerFake(t *testing.T) {
	cfg := testenv.APIServer(t)
	fake := s3fake.New()
	t.Cleanup(fake.Close)
	fake.Filer = true
	fake.CreateBucket("filer")
	runConformance(t, cfg, kubernetes.S3Config{Endpoint: fake.URL, Bucket: "filer", AccessKey: "fake", SecretKey: "fake", PathStyle: true})
}

func runConformance(t *testing.T, cfg *rest.Config, s3cfg kubernetes.S3Config) {
	admin, err := client.New(cfg, client.Options{})
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	// A fresh prefix per run, so runs against a persistent store never see
	// each other's entries.
	run := "run-" + ulid.Make().String()
	var n atomic.Int32

	conformance.Run(t, func(t *testing.T) conformance.Env {
		ns := fmt.Sprintf("conformance-%d", n.Add(1))
		if err := admin.Create(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil {
			t.Fatalf("create namespace: %v", err)
		}
		logCfg := s3cfg
		logCfg.Prefix = run + "/" + ns + "/" // isolate each test's log
		return conformance.Env{
			Namespace: ns,
			Open: func() storage.Driver {
				d, err := kubernetes.Open(context.Background(), kubernetes.Config{REST: cfg, S3: logCfg})
				if err != nil {
					t.Fatalf("open driver: %v", err)
				}
				return d
			},
		}
	})
}

// Objects the API server rejects come back as storage.ErrInvalid, so callers
// can answer 400 instead of "unavailable".
func TestInvalidObjectsMapToErrInvalid(t *testing.T) {
	cfg := testenv.APIServer(t)
	d, err := kubernetes.Open(context.Background(), kubernetes.Config{REST: cfg, S3: testenv.S3(t)})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.Close() }()
	for name, tk := range map[string]*v1alpha1.Task{
		"bad priority": {ObjectMeta: metav1.ObjectMeta{Name: "t", Namespace: "default"}, Spec: v1alpha1.TaskSpec{Title: "t", Priority: "whenever"}},
		"bad name":     {ObjectMeta: metav1.ObjectMeta{Name: "Not_A_Name", Namespace: "default"}, Spec: v1alpha1.TaskSpec{Title: "t"}},
	} {
		if err := d.CreateTask(context.Background(), tk); !errors.Is(err, storage.ErrInvalid) {
			t.Errorf("%s: %v, want storage.ErrInvalid", name, err)
		}
	}
}

// A driver on a shared informer cache (the controller manager's) adds its
// informers to it, serves the index from it, notifies Task changes, and
// leaves the cache running when it closes.
func TestASharedCache(t *testing.T) {
	cfg := testenv.APIServer(t)
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	shared, err := cache.New(cfg, cache.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan error, 1)
	go func() { started <- shared.Start(ctx) }()

	d, err := kubernetes.Open(ctx, kubernetes.Config{REST: cfg, S3: testenv.S3(t), Cache: shared})
	if err != nil {
		t.Fatal(err)
	}
	seen := make(chan string, 16)
	d.NotifyTasks(func(tk *v1alpha1.Task) { seen <- tk.Name })

	tk := &v1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: "shared", Namespace: "default"}, Spec: v1alpha1.TaskSpec{Title: "t"}}
	if err := d.CreateTask(ctx, tk); err != nil {
		t.Fatal(err)
	}
	select {
	case name := <-seen:
		if name != "shared" {
			t.Fatalf("notified of %q", name)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no notification for the new task")
	}
	// The index and the shared cache's own readers see the same Task.
	var got v1alpha1.Task
	if err := shared.Get(ctx, client.ObjectKey{Namespace: "default", Name: "shared"}, &got); err != nil {
		t.Fatalf("the shared cache: %v", err)
	}
	// Subtasks are listed from a parent index on the cache, which a second
	// driver on the same cache finds there and keeps.
	child := &v1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: "child", Namespace: "default"},
		Spec: v1alpha1.TaskSpec{Title: "t", ParentRef: &v1alpha1.TaskReference{Name: "shared"}}}
	if err := d.CreateTask(ctx, child); err != nil {
		t.Fatal(err)
	}
	again, err := kubernetes.Open(ctx, kubernetes.Config{REST: cfg, S3: testenv.S3(t), Cache: shared})
	if err != nil {
		t.Fatalf("a second driver on the shared cache: %v", err)
	}
	if err := again.Close(); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		var children v1alpha1.TaskList
		err := shared.List(ctx, &children, client.InNamespace("default"), client.MatchingFields{"spec.parentRef.name": "shared"})
		if err == nil && len(children.Items) == 1 && children.Items[0].Name == "child" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("children of shared by the parent index: %v, %v", children.Items, err)
		}
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-started:
		t.Fatalf("closing the driver stopped the shared cache: %v", err)
	default:
	}
	if err := shared.Get(ctx, client.ObjectKey{Namespace: "default", Name: "shared"}, &got); err != nil {
		t.Fatalf("the shared cache after the driver closed: %v", err)
	}
	// A closed driver hears of no more changes.
	for len(seen) > 0 {
		<-seen
	}
	other, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	if err := other.Create(ctx, &v1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: "after-close", Namespace: "default"}, Spec: v1alpha1.TaskSpec{Title: "t"}}); err != nil {
		t.Fatal(err)
	}
	select {
	case name := <-seen:
		t.Fatalf("a closed driver was notified of %q", name)
	case <-time.After(time.Second):
	}
}
