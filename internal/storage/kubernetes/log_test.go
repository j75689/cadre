package kubernetes

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/oklog/ulid/v2"

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

// refuseDeletes answers with status and code the DELETE of an entry, or
// with dirs that of a directory key ("…/").
type refuseDeletes struct {
	http.RoundTripper
	dirs   bool
	status int
	code   string
}

func (r refuseDeletes) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method != http.MethodDelete || strings.HasSuffix(req.URL.Path, "/") != r.dirs {
		return r.RoundTripper.RoundTrip(req)
	}
	return &http.Response{StatusCode: r.status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("<Error><Code>" + r.code + "</Code></Error>")), Request: req}, nil
}

// A delete the store refuses is an error, not a deleted stream.
func TestDeleteStreamFailsWhenTheStoreRefuses(t *testing.T) {
	ctx := context.Background()
	l, _ := fakeLog(t, false)
	if err := l.Append(ctx, "a/thread", "0001", nil); err != nil {
		t.Fatal(err)
	}
	for _, rt := range []refuseDeletes{
		{http.DefaultTransport, false, http.StatusForbidden, "AccessDenied"},
		{http.DefaultTransport, true, http.StatusForbidden, "AccessDenied"},
	} {
		l.s3 = s3.New(l.s3.Options(), func(o *s3.Options) { o.HTTPClient = &http.Client{Transport: rt} })
		if err := l.DeleteStream(ctx, "a"); err == nil {
			t.Errorf("delete refused by the store (directories: %v): got nil", rt.dirs)
		}
	}
}

// A store that answers 404 to deleting a directory it never had (GCS) has
// still deleted the stream.
func TestDeleteStreamIgnoresAMissingDirectory(t *testing.T) {
	ctx := context.Background()
	l, fake := fakeLog(t, false)
	if err := l.Append(ctx, "a/thread", "0001", nil); err != nil {
		t.Fatal(err)
	}
	l.s3 = s3.New(l.s3.Options(), func(o *s3.Options) {
		o.HTTPClient = &http.Client{Transport: refuseDeletes{http.DefaultTransport, true, http.StatusNotFound, "NoSuchKey"}}
	})
	if err := l.DeleteStream(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	if keys := fake.Keys("probe-test"); len(keys) != 0 {
		t.Errorf("objects left: %v", keys)
	}
}

// A stream of more entries than one listing returns is deleted whole.
func TestDeleteStreamPages(t *testing.T) {
	ctx := context.Background()
	l, fake := fakeLog(t, false)
	const n = 1001
	for i := range n {
		if err := l.Append(ctx, "a/thread", storage.SeqKey(int64(i)), nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := l.DeleteStream(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	if keys := fake.Keys("probe-test"); len(keys) != 0 {
		t.Errorf("%d objects left, e.g. %s", len(keys), keys[0])
	}
}

// A stream at the top of a store with no prefix is deleted, and the walk up
// its directories ends at the store's root.
func TestDeleteStreamWithNoPrefix(t *testing.T) {
	fake := s3fake.New()
	t.Cleanup(fake.Close)
	fake.CreateBucket("b")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	l, err := newObjectLog(ctx, S3Config{Endpoint: fake.URL, Bucket: "b", AccessKey: "fake", SecretKey: "fake", PathStyle: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Append(ctx, "x", "00000001", []byte("a")); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- l.DeleteStream(ctx, "x") }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("DeleteStream still running 1 s after its context expired")
	}
	if keys := fake.Keys("b"); len(keys) != 0 {
		t.Errorf("objects left: %v", keys)
	}
}

// recordDeletes records the keys of the DELETE requests it passes on;
// DeleteStream sends them in parallel.
type recordDeletes struct {
	http.RoundTripper
	mu   sync.Mutex
	keys []string
}

func (r *recordDeletes) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method == http.MethodDelete {
		r.mu.Lock()
		r.keys = append(r.keys, strings.TrimPrefix(req.URL.Path, "/probe-test/"))
		r.mu.Unlock()
	}
	return r.RoundTripper.RoundTrip(req)
}

// A retry after a failed directory delete finds no objects left, and still
// deletes the stream's directories, which a filer store would otherwise list
// as an entry.
func TestDeleteStreamRetryDeletesTheDirectories(t *testing.T) {
	ctx := context.Background()
	l, _ := fakeLog(t, false)
	if err := l.Append(ctx, "a/thread", "0001", nil); err != nil {
		t.Fatal(err)
	}
	opts := l.s3.Options()
	l.s3 = s3.New(opts, func(o *s3.Options) {
		o.HTTPClient = &http.Client{Transport: refuseDeletes{http.DefaultTransport, true, http.StatusServiceUnavailable, "ServiceUnavailable"}}
	})
	if err := l.DeleteStream(ctx, "a/thread"); err == nil {
		t.Fatal("delete with the directory delete refused: got nil")
	}
	rec := &recordDeletes{RoundTripper: http.DefaultTransport}
	l.s3 = s3.New(opts, func(o *s3.Options) {
		o.HTTPClient = &http.Client{Transport: rec}
	})
	if err := l.DeleteStream(ctx, "a/thread"); err != nil {
		t.Fatal(err)
	}
	if want := []string{"cadre/a/thread/~e/", "cadre/a/thread/"}; !slices.Equal(rec.keys, want) {
		t.Errorf("retry deleted %v, want %v", rec.keys, want)
	}
}

// filerLogs are a log on a filer fake and, with CADRE_TEST_S3_ENDPOINT set,
// one on that store (SeaweedFS in CI), each under a prefix of its own.
func filerLogs(t *testing.T) map[string]*objectLog {
	t.Helper()
	fl, fake := fakeLog(t, false)
	fake.Filer = true
	logs := map[string]*objectLog{"filer fake": fl}
	if ep := os.Getenv("CADRE_TEST_S3_ENDPOINT"); ep != "" {
		l, err := newObjectLog(context.Background(), S3Config{
			Endpoint: ep, Bucket: os.Getenv("CADRE_TEST_S3_BUCKET"), Prefix: "test-" + ulid.Make().String() + "/", PathStyle: true,
			AccessKey: os.Getenv("CADRE_TEST_S3_ACCESS_KEY"), SecretKey: os.Getenv("CADRE_TEST_S3_SECRET_KEY"),
		})
		if err != nil {
			t.Fatalf("new log: %v", err)
		}
		logs["store"] = l
	}
	return logs
}

// A retry of a parent stream after a failed directory delete finds no
// objects of the nested streams the first call emptied, and still deletes
// their directories, which a filer store keeps.
func TestDeleteStreamRetryDeletesNestedDirectories(t *testing.T) {
	ctx := context.Background()
	for name, l := range filerLogs(t) {
		t.Run(name, func(t *testing.T) {
			for _, s := range []string{"ns/a/thread", "ns/a2/thread"} {
				if err := l.Append(ctx, s, "0001", nil); err != nil {
					t.Fatal(err)
				}
			}
			opts := l.s3.Options()
			l.s3 = s3.New(opts, func(o *s3.Options) {
				o.HTTPClient = &http.Client{Transport: refuseDeletes{http.DefaultTransport, true, http.StatusServiceUnavailable, "ServiceUnavailable"}}
			})
			if err := l.DeleteStream(ctx, "ns/a"); err == nil {
				t.Fatal("delete with the directory deletes refused: got nil")
			}
			l.s3 = s3.New(opts)
			if err := l.DeleteStream(ctx, "ns/a"); err != nil {
				t.Fatal(err)
			}
			res, err := l.s3.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: &l.bucket, Prefix: aws.String(l.prefix + "ns/"), Delimiter: aws.String("/")})
			if err != nil {
				t.Fatal(err)
			}
			var dirs []string
			for _, p := range res.CommonPrefixes {
				dirs = append(dirs, strings.TrimPrefix(aws.ToString(p.Prefix), l.prefix))
			}
			if want := []string{"ns/a2/"}; !slices.Equal(dirs, want) {
				t.Errorf("directories left under ns/: %v, want %v", dirs, want)
			}
		})
	}
}

// The directories DeleteStream deletes are all inside the stream, never the
// parent that holds its siblings, whatever the store's prefix.
func TestDeleteStreamStaysInsideTheStream(t *testing.T) {
	ctx := context.Background()
	for _, prefix := range []string{"cadre/", ""} {
		l, _ := fakeLog(t, false)
		l.prefix = prefix
		for _, s := range []string{"ns/a/thread", "ns/a/thread/files", "ns/a/runs/R1/events", "ns/a2/thread"} {
			if err := l.Append(ctx, s, "0001", nil); err != nil {
				t.Fatal(err)
			}
		}
		rec := &recordDeletes{RoundTripper: http.DefaultTransport}
		l.s3 = s3.New(l.s3.Options(), func(o *s3.Options) {
			o.HTTPClient = &http.Client{Transport: rec}
		})
		if err := l.DeleteStream(ctx, "ns/a"); err != nil {
			t.Fatal(err)
		}
		for _, k := range rec.keys {
			if !strings.HasPrefix(k, prefix+"ns/a/") {
				t.Errorf("prefix %q: deleted %s, outside the stream", prefix, k)
			}
		}
	}
}
