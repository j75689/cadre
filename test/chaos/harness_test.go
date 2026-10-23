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
	"sync"
	"sync/atomic"
	"time"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	"github.com/j75689/cadre/internal/api"
	"github.com/j75689/cadre/internal/controller"
	"github.com/j75689/cadre/internal/httpjson"
	"github.com/j75689/cadre/internal/manager"
	"github.com/j75689/cadre/internal/runner"
	"github.com/j75689/cadre/internal/storage"
	"github.com/j75689/cadre/internal/storage/kubernetes"
)

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
	return c.fault(func() error { return c.Driver.UpdateTaskStatus(ctx, t) })
}

// CreateTask and Append get the same faults: an ambiguous create or append
// is how a duplicate history entry or an orphaned subtask would arise.
func (c *chaosStore) CreateTask(ctx context.Context, t *v1alpha1.Task) error {
	return c.fault(func() error { return c.Driver.CreateTask(ctx, t) })
}

func (c *chaosStore) Append(ctx context.Context, stream, key string, data []byte) error {
	return c.fault(func() error { return c.Driver.Append(ctx, stream, key, data) })
}

// fault delays some writes, fails some before they happen, and reports some
// as failed after they happened.
func (c *chaosStore) fault(write func() error) error {
	r := c.rnd.Float64()
	if r < 0.25 {
		time.Sleep(c.rnd.between(0, 300*time.Millisecond))
	}
	switch {
	case r >= 0.25 && r < 0.29:
		return errInjected
	case r >= 0.29 && r < 0.34:
		if err := write(); err != nil {
			return err
		}
		return errInjected
	}
	return write()
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
	// Each server runs its own controllers, as cadre-server does: they
	// mark the Agent Ready, and claims read it.
	ctrls, err := controller.NewManager(cfg.REST)
	if err != nil {
		cancel()
		_ = drv.Close()
		return nil, err
	}
	if err := controller.Setup(ctrls, controller.Options{}, controller.SkipNameValidation()); err != nil {
		cancel()
		_ = drv.Close()
		return nil, err
	}
	go func() { _ = ctrls.Start(ctx) }()
	mcfg := managerConfig
	mcfg.Agents = controller.Agents(ctrls)
	m := manager.New(&chaosStore{Driver: drv, rnd: rnd}, mcfg)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	mux := http.NewServeMux()
	runner.NewServer(m, log, "http://cadre.test").Register(mux)
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
