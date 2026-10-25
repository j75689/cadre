package kubernetes

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/j75689/cadre/internal/s3fake"
	"github.com/j75689/cadre/internal/storage"
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

// A stream named after another's entry does not collide with it, even on a
// filesystem-backed store: entries live under the reserved segment. A 412 is
// still "taken" only if the entry exists: a key whose path something else
// made a directory is a collision, not an entry.
func TestAppendTellsATakenKeyFromACollision(t *testing.T) {
	ctx := context.Background()
	l, fake := fakeLog(t, false)
	fake.Filer = true
	if err := l.Append(ctx, "a/runs", "R1", []byte("record")); err != nil {
		t.Fatal(err)
	}
	if err := l.Append(ctx, "a/runs", "R1", []byte("again")); !errors.Is(err, storage.ErrAlreadyExists) {
		t.Errorf("append a taken key: %v, want ErrAlreadyExists", err)
	}
	if err := l.Append(ctx, "a/runs/R2/events", "0001", []byte("event")); err != nil {
		t.Fatal(err)
	}
	if err := l.Append(ctx, "a/runs", "R2", []byte("record")); err != nil {
		t.Errorf("append an entry named like a nested stream: %v", err)
	}
	// An object below the entry's path, from another writer.
	foreign := "cadre/a/runs/" + storage.EntrySegment + "/R3/x"
	if _, err := l.s3.PutObject(ctx, &s3.PutObjectInput{Bucket: &l.bucket, Key: &foreign, Body: strings.NewReader("x")}); err != nil {
		t.Fatal(err)
	}
	err := l.Append(ctx, "a/runs", "R3", []byte("record"))
	if !errors.Is(err, storage.ErrStreamCollision) || errors.Is(err, storage.ErrAlreadyExists) {
		t.Errorf("append over a directory: %v, want ErrStreamCollision", err)
	}
}

// A HEAD that fails for a moment after a 412 is retried, so an ordinary race
// for a key still ends as ErrAlreadyExists. The SDK retries a 500 three times
// by itself, so four failures are needed to reach the driver's own retry.
func TestTakenRetriesATransientHead(t *testing.T) {
	ctx := context.Background()
	l, fake := fakeLog(t, false)
	if err := l.Append(ctx, "a/thread", "0001", []byte("x")); err != nil {
		t.Fatal(err)
	}
	fake.HeadFailures.Store(4)
	if err := l.Append(ctx, "a/thread", "0001", []byte("y")); !errors.Is(err, storage.ErrAlreadyExists) {
		t.Fatalf("append a taken key while HEAD fails four times: %v, want ErrAlreadyExists", err)
	}
	// A HEAD that keeps failing ends in an error, never in "already exists".
	fake.HeadFailures.Store(1000)
	short, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if err := l.Append(short, "a/thread", "0001", []byte("y")); err == nil || errors.Is(err, storage.ErrAlreadyExists) {
		t.Fatalf("append a taken key while HEAD keeps failing: %v, want an error that is not ErrAlreadyExists", err)
	}
}
