package kubernetes_test

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
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
