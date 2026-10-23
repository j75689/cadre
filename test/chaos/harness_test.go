package chaos_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/oklog/ulid/v2"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	"github.com/j75689/cadre/internal/api"
	"github.com/j75689/cadre/internal/httpjson"
	"github.com/j75689/cadre/internal/manager"
	"github.com/j75689/cadre/internal/runner"
	"github.com/j75689/cadre/internal/s3fake"
	"github.com/j75689/cadre/internal/storage"
	"github.com/j75689/cadre/internal/storage/kubernetes"
)

// startAPIServer starts envtest with the CRDs. Outside CI it skips when the
// envtest binaries are not installed; `make test` installs them.
func startAPIServer(t *testing.T) *rest.Config {
	t.Helper()
	// controller-runtime complains with a stack trace if no logger is set.
	ctrllog.SetLogger(logr.Discard())
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
	t.Cleanup(func() { _ = env.Stop() })
	return cfg
}

// s3Config returns a real S3-compatible endpoint when CADRE_TEST_S3_* is set,
// and an in-process fake otherwise. The prefix is fresh per run.
func s3Config(t *testing.T) kubernetes.S3Config {
	prefix := "chaos-" + ulid.Make().String() + "/"
	if ep := os.Getenv("CADRE_TEST_S3_ENDPOINT"); ep != "" {
		return kubernetes.S3Config{
			Endpoint: ep, Bucket: os.Getenv("CADRE_TEST_S3_BUCKET"), Prefix: prefix, PathStyle: true,
			AccessKey: os.Getenv("CADRE_TEST_S3_ACCESS_KEY"), SecretKey: os.Getenv("CADRE_TEST_S3_SECRET_KEY"),
		}
	}
	fake := s3fake.New()
	t.Cleanup(fake.Close)
	fake.CreateBucket("chaos")
	return kubernetes.S3Config{Endpoint: fake.URL, Bucket: "chaos", Prefix: prefix, PathStyle: true, AccessKey: "fake", SecretKey: "fake"}
}

// lockedRand is a seeded source shared by goroutines.
type lockedRand struct {
	mu sync.Mutex
	r  *rand.Rand
}

func (l *lockedRand) Float64() float64 { l.mu.Lock(); defer l.mu.Unlock(); return l.r.Float64() }
func (l *lockedRand) IntN(n int) int   { l.mu.Lock(); defer l.mu.Unlock(); return l.r.IntN(n) }

// between returns a random duration in [lo, hi).
func (l *lockedRand) between(lo, hi time.Duration) time.Duration {
	return lo + time.Duration(l.Float64()*float64(hi-lo))
}

var errInjected = errors.New("injected: connection reset after the write")

// chaosStore delays some status writes, makes some commit and then report an
// error, and makes some fail without effect.
type chaosStore struct {
	storage.Driver
	rnd *lockedRand
}

func (c *chaosStore) UpdateTaskStatus(ctx context.Context, t *v1alpha1.Task) error {
	r := c.rnd.Float64()
	if r < 0.25 {
		time.Sleep(c.rnd.between(0, 300*time.Millisecond))
	}
	switch {
	case r >= 0.25 && r < 0.29:
		return errInjected // never reaches the server
	case r >= 0.29 && r < 0.34:
		if err := c.Driver.UpdateTaskStatus(ctx, t); err != nil {
			return err
		}
		return errInjected // landed, but the caller cannot tell
	}
	return c.Driver.UpdateTaskStatus(ctx, t)
}

func (c *chaosStore) UpdateTeamStatus(ctx context.Context, team *v1alpha1.Team) error {
	if c.rnd.Float64() < 0.2 {
		time.Sleep(c.rnd.between(0, 200*time.Millisecond))
	}
	return c.Driver.UpdateTeamStatus(ctx, team)
}

// timings are short so a minute covers many leases.
var (
	managerConfig = manager.Config{Lease: 3 * time.Second, Heartbeat: time.Second}
	loopConfig    = manager.LoopConfig{Reclaim: 200 * time.Millisecond, Dispatch: 300 * time.Millisecond}
)

// server is one cadre-server instance: its own driver, informer, Task
// Manager, loops and listener.
type server struct {
	name   string
	srv    *httptest.Server
	driver *kubernetes.Driver
	cancel context.CancelFunc
	loops  chan struct{}
}

func startServer(name string, cfg kubernetes.Config, rnd *lockedRand) (*server, error) {
	ctx, cancel := context.WithCancel(context.Background())
	drv, err := kubernetes.Open(ctx, cfg)
	if err != nil {
		cancel()
		return nil, err
	}
	m := manager.New(&chaosStore{Driver: drv, rnd: rnd}, managerConfig)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	mux := http.NewServeMux()
	runner.NewServer(m, log).Register(mux)
	api.NewServer(m, log).Register(mux)
	s := &server{name: name, srv: httptest.NewServer(mux), driver: drv, cancel: cancel, loops: make(chan struct{})}
	go func() {
		defer close(s.loops)
		m.Run(ctx, loopConfig, log)
	}()
	return s, nil
}

// kill stops the instance abruptly: in-flight requests are cut, the loops
// stop and the informer is closed.
func (s *server) kill() {
	s.cancel()
	s.srv.CloseClientConnections()
	s.srv.Close()
	<-s.loops
	_ = s.driver.Close()
}

// proxy spreads requests over the live servers, like a Service in front of
// the Deployment.
type proxy struct {
	*httptest.Server
	mu    sync.Mutex
	live  map[string]*url.URL
	rnd   *lockedRand
	count atomic.Int64
}

func newProxy(rnd *lockedRand) *proxy {
	p := &proxy{live: map[string]*url.URL{}, rnd: rnd}
	rp := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			if target := p.pick(); target != nil {
				r.SetURL(target)
			}
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			httpjson.WriteError(w, http.StatusServiceUnavailable, httpjson.CodeUnavailable, err.Error())
		},
	}
	p.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.count.Add(1)
		if p.pick() == nil {
			httpjson.WriteError(w, http.StatusServiceUnavailable, httpjson.CodeUnavailable, "no live server")
			return
		}
		rp.ServeHTTP(w, r)
	}))
	return p
}

func (p *proxy) pick() *url.URL {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.live) == 0 {
		return nil
	}
	i := p.rnd.IntN(len(p.live))
	for _, u := range p.live {
		if i == 0 {
			return u
		}
		i--
	}
	return nil
}

func (p *proxy) set(name, rawURL string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if rawURL == "" {
		delete(p.live, name)
		return
	}
	u, _ := url.Parse(rawURL)
	p.live[name] = u
}
