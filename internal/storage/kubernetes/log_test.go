package kubernetes

import (
	"context"
	"errors"
	"testing"

	"github.com/j75689/cadre/internal/s3fake"
)

func fakeLog(t *testing.T, ignoreIfNoneMatch bool) (*objectLog, *s3fake.Server) {
	t.Helper()
	fake := s3fake.New()
	t.Cleanup(fake.Close)
	fake.IgnoreIfNoneMatch = ignoreIfNoneMatch
	fake.CreateBucket("probe-test")
	l, err := newObjectLog(context.Background(), S3Config{Endpoint: fake.URL, Bucket: "probe-test", Prefix: "cadre/", AccessKey: "fake", SecretKey: "fake", PathStyle: true})
	if err != nil {
		t.Fatalf("new log: %v", err)
	}
	return l, fake
}

func TestProbeAcceptsAnEnforcingStore(t *testing.T) {
	l, fake := fakeLog(t, false)
	if err := l.probe(context.Background()); err != nil {
		t.Fatalf("probe: %v", err)
	}
	if keys := fake.Keys("probe-test"); len(keys) != 0 {
		t.Fatalf("probe left keys behind: %v", keys)
	}
}

func TestProbeRejectsAStoreThatIgnoresIfNoneMatch(t *testing.T) {
	l, fake := fakeLog(t, true)
	if err := l.probe(context.Background()); !errors.Is(err, errNotEnforced) {
		t.Fatalf("probe: got %v, want errNotEnforced", err)
	}
	if keys := fake.Keys("probe-test"); len(keys) != 0 {
		t.Fatalf("probe left keys behind: %v", keys)
	}
}

func TestProbeFailsWithoutBucket(t *testing.T) {
	l, _ := fakeLog(t, false)
	l.bucket = "missing"
	if err := l.probe(context.Background()); err == nil {
		t.Fatal("probe of a missing bucket succeeded")
	}
}
