package chaos_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	stdlog "log"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	ctrl "sigs.k8s.io/controller-runtime/pkg/manager"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	"github.com/j75689/cadre/internal/api"
	"github.com/j75689/cadre/internal/controller"
	"github.com/j75689/cadre/internal/httpjson"
	"github.com/j75689/cadre/internal/manager"
	"github.com/j75689/cadre/internal/mcp"
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
	managerConfig = manager.Config{Lease: 3 * time.Second, Heartbeat: time.Second, StepIdle: stepIdle}
	loopConfig    = manager.LoopConfig{Reclaim: 200 * time.Millisecond, Dispatch: 300 * time.Millisecond}
)

// server is one cadre-server instance: its own driver, informer, Task
// Manager, loops and listener.
type server struct {
	name   string
	id     string // this instance's, in instanceHeader
	srv    *httptest.Server
	driver *kubernetes.Driver
	cancel context.CancelFunc
	loops  chan struct{}
	// lost is sent why the controllers stopped while the server lived (a
	// leader that missed its renewal): cadre-server exits then, so the
	// server is to die and be started again.
	lost chan error
	hub  *api.Hub
	// streams is how many watch streams it serves.
	streams atomic.Int64
}

// instanceHeader names the instance that answered, so a watch client knows
// which stream a kill ends.
const instanceHeader = "X-Chaos-Instance"

// instances numbers the instances started.
var instances atomic.Int64

// watchConfig pings often, so pings are among the writes the chaos cuts,
// and serves fewer streams than there are browsers, so some are refused and
// poll.
var watchConfig = api.WatchConfig{MaxStreams: 3, Ping: 2 * time.Second}

// drainTimeout is how long a drain waits for the streams to end, as
// cadre-server's shutdown waits for its handlers.
const drainTimeout = 10 * time.Second

// startServer starts an instance; publicURL is where runners reach the
// servers, for the per-run MCP endpoint.
func startServer(name string, cfg kubernetes.Config, rnd *lockedRand, publicURL string) (*server, error) {
	ctx, cancel := context.WithCancel(context.Background())
	drv, err := kubernetes.Open(ctx, cfg)
	if err != nil {
		cancel()
		return nil, err
	}
	// Each server runs its own controllers, as cadre-server does: they
	// mark the Agent Ready, and claims read it.
	// The controllers elect a leader, as cadre-server's replicas do, with
	// a short Lease so a killed server's place is taken within the chaos.
	// A killed server does not hand its Lease over: it expires, as a
	// crashed pod's does.
	ctrls, err := controller.NewManager(cfg.REST, controller.LeaderElection(leaderNS), controller.LeaderLease(leaderLease),
		func(o *ctrl.Options) { o.LeaderElectionReleaseOnCancel = false })
	if err != nil {
		cancel()
		_ = drv.Close()
		return nil, err
	}
	if err := controller.Setup(ctrls, controller.Options{ServerURL: publicURL, Lease: managerConfig.Lease, Resync: jobResync}, controller.SkipNameValidation()); err != nil {
		cancel()
		_ = drv.Close()
		return nil, err
	}
	lost := make(chan error, 1)
	go func() {
		err := ctrls.Start(ctx)
		if ctx.Err() == nil {
			lost <- err
		}
	}()
	mcfg := managerConfig
	mcfg.Agents = controller.Agents(ctrls)
	m := manager.New(&chaosStore{Driver: drv, rnd: rnd}, mcfg)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	// The hub is fed as cadre-server's is: task hints from the driver's
	// informer, run hints from its log.
	hub := api.NewHub(drv)
	hub.TailRuns(drv)
	mux := http.NewServeMux()
	runner.NewServer(m, log, publicURL).Register(mux)
	api.NewServer(m, log, api.WithWatch(hub, watchConfig)).Register(mux)
	mcp.NewServer(m, log).Register(mux)
	s := &server{name: name, id: fmt.Sprintf("%s#%d", name, instances.Add(1)), driver: drv, cancel: cancel, loops: make(chan struct{}), lost: lost, hub: hub}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(instanceHeader, s.id)
		if api.IsWatch(r) {
			s.streams.Add(1)
			defer s.streams.Add(-1)
		}
		mux.ServeHTTP(w, r)
	}))
	go func() {
		defer close(s.loops)
		m.Run(ctx, loopConfig, log)
	}()
	return s, nil
}

// kill stops the instance abruptly: in-flight requests are cut, the loops
// stop and the informer is closed. With drain, it first ends its watch
// streams as cadre-server's shutdown does (hub.Close), and waits up to
// drainTimeout for them to close.
func (s *server) kill(drain bool) {
	if drain {
		s.hub.Close()
		for deadline := time.Now().Add(drainTimeout); s.streams.Load() > 0 && time.Now().Before(deadline); {
			time.Sleep(10 * time.Millisecond)
		}
	}
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
		// A kill cuts the watch streams it relays: that is no news.
		ErrorLog: stdlog.New(io.Discard, "", 0),
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
