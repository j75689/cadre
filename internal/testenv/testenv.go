// Package testenv starts what integration tests need: an envtest API server
// with Cadre's CRDs, and an S3-compatible store. It is imported by tests
// only.
package testenv

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/go-logr/logr"
	"github.com/oklog/ulid/v2"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/j75689/cadre/internal/s3fake"
	"github.com/j75689/cadre/internal/storage/kubernetes"
)

// APIServer starts envtest with the CRDs from config/crd/bases. Outside CI
// it skips when the envtest binaries are not installed (`make test` installs
// them); in CI a missing install fails, so coverage cannot silently vanish.
func APIServer(t *testing.T) *rest.Config {
	t.Helper()
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		if os.Getenv("CI") != "" {
			t.Fatal("KUBEBUILDER_ASSETS is not set in CI; run make test")
		}
		t.Skip("KUBEBUILDER_ASSETS is not set; run make test")
	}
	// controller-runtime complains with a stack trace if no logger is set.
	ctrllog.SetLogger(logr.Discard())
	_, file, _, _ := runtime.Caller(0)
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join(filepath.Dir(file), "..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("start envtest: %v", err)
	}
	t.Cleanup(func() { _ = env.Stop() })
	return cfg
}

// S3 returns a real S3-compatible store when CADRE_TEST_S3_* is set, and an
// in-process fake otherwise. Every call gets a fresh key prefix, so runs
// against a persistent store never see each other's objects.
func S3(t *testing.T) kubernetes.S3Config {
	t.Helper()
	prefix := "test-" + ulid.Make().String() + "/"
	if ep := os.Getenv("CADRE_TEST_S3_ENDPOINT"); ep != "" {
		return kubernetes.S3Config{
			Endpoint: ep, Bucket: os.Getenv("CADRE_TEST_S3_BUCKET"), Prefix: prefix, PathStyle: true,
			AccessKey: os.Getenv("CADRE_TEST_S3_ACCESS_KEY"), SecretKey: os.Getenv("CADRE_TEST_S3_SECRET_KEY"),
		}
	}
	fake := s3fake.New()
	t.Cleanup(fake.Close)
	fake.CreateBucket("test")
	return kubernetes.S3Config{Endpoint: fake.URL, Bucket: "test", Prefix: prefix, PathStyle: true, AccessKey: "fake", SecretKey: "fake"}
}
