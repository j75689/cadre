package kubernetes_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/j75689/cadre/internal/s3fake"
	"github.com/j75689/cadre/internal/storage"
	"github.com/j75689/cadre/internal/storage/conformance"
	"github.com/j75689/cadre/internal/storage/kubernetes"
)

// s3Config returns a real S3-compatible endpoint when CADRE_TEST_S3_* is set,
// and the in-process fake otherwise.
func s3Config(t *testing.T) kubernetes.S3Config {
	if ep := os.Getenv("CADRE_TEST_S3_ENDPOINT"); ep != "" {
		return kubernetes.S3Config{
			Endpoint:  ep,
			Bucket:    os.Getenv("CADRE_TEST_S3_BUCKET"),
			AccessKey: os.Getenv("CADRE_TEST_S3_ACCESS_KEY"),
			SecretKey: os.Getenv("CADRE_TEST_S3_SECRET_KEY"),
			PathStyle: true,
		}
	}
	fake := s3fake.New()
	t.Cleanup(fake.Close)
	const bucket = "cadre-test"
	fake.CreateBucket(bucket)
	return kubernetes.S3Config{Endpoint: fake.URL, Bucket: bucket, AccessKey: "fake", SecretKey: "fake", PathStyle: true}
}

func startAPIServer(t *testing.T) *rest.Config {
	t.Helper()
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		if os.Getenv("CI") != "" {
			t.Fatal("KUBEBUILDER_ASSETS is not set in CI; run make test")
		}
		t.Skip("KUBEBUILDER_ASSETS is not set; run make test")
	}
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("start envtest: %v", err)
	}
	t.Cleanup(func() { _ = env.Stop() })
	return cfg
}

func TestConformance(t *testing.T) {
	cfg := startAPIServer(t)
	s3cfg := s3Config(t)
	admin, err := client.New(cfg, client.Options{})
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	run := fmt.Sprintf("run-%d", os.Getpid())
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
