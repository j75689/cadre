// Command cadre-server runs the Task Manager: the /api/v1 and /runner/v1
// endpoints and the background loops (numbering, outbox dispatch, lease
// reclaim), and the controllers that mark Agents, ModelProviders and
// ToolServers Ready, start runner agents' Jobs and create Schedules' tasks. Any number of replicas
// may run against the same cluster and object store; the controllers run on
// the elected leader only (--leader-elect).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
	_ "time/tzdata" // Schedules' time zones, in an image without zoneinfo

	"github.com/go-logr/logr"
	ctrl "sigs.k8s.io/controller-runtime/pkg/client/config"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/j75689/cadre/internal/api"
	"github.com/j75689/cadre/internal/controller"
	"github.com/j75689/cadre/internal/httpjson"
	"github.com/j75689/cadre/internal/manager"
	"github.com/j75689/cadre/internal/mcp"
	"github.com/j75689/cadre/internal/runner"
	"github.com/j75689/cadre/internal/storage/kubernetes"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "cadre-server:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		listen    = flag.String("listen", ":8080", "address to serve /api/v1, /runner/v1, /mcp/run and the health probes on")
		publicURL = flag.String("public-url", "", "URL runners reach this server at, for their per-run MCP endpoint; required in a cluster (default outside one: http://localhost and the listen port)")
		lease     = flag.Duration("lease", manager.DefaultConfig.Lease, "how long a claim or heartbeat keeps a run alive")
		stepIdle  = flag.Duration("step-idle", manager.DefaultConfig.StepIdle, "how long a free step of a step-mode run waits for a worker before the run fails")
		heartbeat = flag.Duration("heartbeat", manager.DefaultConfig.Heartbeat, "how often runners renew; also the reclaim grace")
		reclaim   = flag.Duration("reclaim-interval", manager.DefaultLoopConfig.Reclaim, "how often expired leases are reclaimed")
		dispatch  = flag.Duration("dispatch-interval", manager.DefaultLoopConfig.Dispatch, "how often the outbox is dispatched and new tasks numbered")
		auth      = flag.String("runner-auth", "required", "required: runners authenticate with a ServiceAccount token for audience cadre; none: no runner authentication, for tests and local development only")
		builtin   = flag.String("builtin-runner", "system:serviceaccount:cadre-system:cadre-runner", "the identity of cadre-runner, the only runner that may claim builtin agents' tasks")
		elect     = flag.Bool("leader-elect", true, "run the controllers on one replica at a time, elected with a Lease; the API, the runner protocol and the MCP endpoint serve on every replica")
		electNS   = flag.String("leader-elect-namespace", "", "the namespace of the leader election Lease; by default the pod's own in a cluster (outside one, leader election needs it, or is off)")
		s3cfg     kubernetes.S3Config
	)
	flag.StringVar(&s3cfg.Endpoint, "s3-endpoint", "", "S3-compatible endpoint; empty for AWS S3")
	flag.StringVar(&s3cfg.Region, "s3-region", "us-east-1", "S3 region")
	flag.StringVar(&s3cfg.Bucket, "s3-bucket", "", "bucket for the log (required)")
	flag.StringVar(&s3cfg.Prefix, "s3-prefix", "cadre/", "prefix for every object key")
	flag.BoolVar(&s3cfg.PathStyle, "s3-path-style", false, "use path-style addressing (most self-hosted stores)")
	flag.Parse()
	if *auth != "required" && *auth != "none" {
		return fmt.Errorf("--runner-auth %q: must be required or none", *auth)
	}
	// Keys come from the environment, never flags, so they stay out of
	// process listings. Without them the AWS default chain is used.
	s3cfg.AccessKey = os.Getenv("CADRE_S3_ACCESS_KEY")
	s3cfg.SecretKey = os.Getenv("CADRE_S3_SECRET_KEY")
	if err := validateTimings(*lease, *heartbeat); err != nil {
		return err
	}

	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	// controller-runtime logs informer and watch failures; without a logger
	// it complains once and then drops them all.
	ctrllog.SetLogger(logr.FromSlogHandler(log.Handler()))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	rest, err := ctrl.GetConfig()
	if err != nil {
		return fmt.Errorf("kubernetes config: %w", err)
	}

	var ctrlOpts []controller.ManagerOption
	inCluster := os.Getenv("KUBERNETES_SERVICE_HOST") != ""
	switch {
	case *elect && (inCluster || *electNS != ""):
		ctrlOpts = append(ctrlOpts, controller.LeaderElection(*electNS))
	case *elect:
		log.Warn("outside a cluster without --leader-elect-namespace: leader election is off, so run one replica only")
	}
	ctrls, err := controller.NewManager(rest, ctrlOpts...)
	if err != nil {
		return fmt.Errorf("controllers: %w", err)
	}
	if *publicURL, err = resolvePublicURL(*publicURL, *listen, os.Getenv("KUBERNETES_SERVICE_HOST") != ""); err != nil {
		return err
	}
	// Runner agents' Jobs reach cadre-server where every runner does.
	if err := controller.Setup(ctrls, controller.Options{ServerURL: *publicURL, Lease: *lease}); err != nil {
		return fmt.Errorf("controllers: %w", err)
	}
	// One informer cache serves the controllers and the board index: the
	// controller manager starts it, and the driver adds its informers to it
	// and waits for them to sync.
	// A manager that fails to start would leave the driver waiting for a
	// cache that never syncs: its failure ends the wait, and the server.
	controllers := make(chan error, 1)
	openCtx, opened := context.WithCancelCause(ctx)
	go func() {
		err := ctrls.Start(ctx)
		opened(fmt.Errorf("controllers stopped: %w", err))
		controllers <- err
	}()
	store, err := kubernetes.Open(openCtx, kubernetes.Config{REST: rest, S3: s3cfg, Cache: ctrls.GetCache()})
	if err != nil {
		if cause := context.Cause(openCtx); cause != nil && ctx.Err() == nil {
			return cause
		}
		return err
	}
	opened(nil)
	defer func() { _ = store.Close() }()
	// Schedules create tasks through the store, which exists only now that
	// the manager runs: their controller joins it late, as controller-runtime
	// allows, and starts once this replica leads.
	if err := controller.SetupSchedules(ctrls, store, controller.Options{}); err != nil {
		return fmt.Errorf("controllers: %w", err)
	}

	forwarder := mcp.NewForwarder(controller.ToolServerSources(ctrls), log)
	m := manager.New(store, manager.Config{Lease: *lease, Heartbeat: *heartbeat, StepIdle: *stepIdle, Agents: controller.Agents(ctrls), Tools: forwarder})
	mux := http.NewServeMux()
	var runnerOpts []runner.Option
	if *auth == "required" {
		reviews := runner.Cached(controller.TokenReviews(ctrls.GetClient()))
		auth := controller.Runners(ctrls.GetClient(), reviews, *builtin)
		runnerOpts = append(runnerOpts, runner.WithAuth(auth, *builtin))
	} else {
		log.Warn("runners are not authenticated (--runner-auth=none): any client may claim and finish runs")
	}
	runner.NewServer(m, log, *publicURL, runnerOpts...).Register(mux)
	mcp.NewServer(m, log, mcp.WithForwarder(forwarder)).Register(mux)
	api.NewServer(m, log).Register(mux)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		if err := m.Ready(r.Context()); err != nil {
			log.Warn("not ready", "err", err)
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})

	srv := &http.Server{
		Addr:              *listen,
		Handler:           httpjson.LogRequests(boundRequests(mux, requestTimeout, mcpRequestTimeout), log),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      mcpRequestTimeout + 5*time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	loops := make(chan struct{})
	go func() {
		defer close(loops)
		m.Run(ctx, manager.LoopConfig{Reclaim: *reclaim, Dispatch: *dispatch}, log)
	}()
	served := make(chan error, 1)
	go func() { served <- srv.ListenAndServe() }()
	log.Info("cadre-server started", "listen", *listen)

	// Whatever ends the server (a signal, the listener failing, the
	// controllers stopping), in-flight requests are drained before exit.
	select {
	case err = <-served:
	case cerr := <-controllers:
		if cerr != nil {
			err = fmt.Errorf("controllers stopped: %w", cerr)
		}
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if serr := srv.Shutdown(shutdown); err == nil {
		err = serr
	}
	stop()
	<-loops
	if errors.Is(err, http.ErrServerClosed) {
		err = nil
	}
	return err
}

// requestTimeout bounds one request's work, comfortably above the Task
// Manager's conflict-retry budget, so a hung API server cannot pin every
// connection. Requests that may make a forwarded tool call, on the per-run
// MCP endpoint or approving a call, get longer: the call may take up to a
// minute on its ToolServer.
const (
	requestTimeout    = 30 * time.Second
	mcpRequestTimeout = 90 * time.Second
)

// boundRequests gives every request a context that ends after d, or after
// mcpBound for one that may make a tool call.
func boundRequests(next http.Handler, d, mcpBound time.Duration) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bound := d
		if r.URL.Path == mcp.Path || api.IsApproval(r) {
			bound = mcpBound
		}
		ctx, cancel := context.WithTimeout(r.Context(), bound)
		defer cancel()
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// validateTimings checks what runners are told: whole seconds, at least one,
// and a heartbeat that comes before the lease runs out.
func validateTimings(lease, heartbeat time.Duration) error {
	for name, d := range map[string]time.Duration{"--lease": lease, "--heartbeat": heartbeat} {
		if d < time.Second || d%time.Second != 0 {
			return fmt.Errorf("%s %s: must be a whole number of seconds, at least 1s", name, d)
		}
	}
	if heartbeat >= lease {
		return fmt.Errorf("--heartbeat %s must be shorter than --lease %s", heartbeat, lease)
	}
	return nil
}

// resolvePublicURL returns the URL runners reach this server at. Outside a
// cluster it defaults to localhost on the listen port; inside one there is
// no sensible default, and a wrong one would only show at a runner's first
// tool call, so the flag is required.
func resolvePublicURL(flagValue, listen string, inCluster bool) (string, error) {
	if flagValue != "" {
		return flagValue, nil
	}
	if inCluster {
		return "", errors.New("--public-url is required in a cluster: runners reach their MCP endpoint there")
	}
	_, port, err := net.SplitHostPort(listen)
	if err != nil {
		return "", fmt.Errorf("--listen %q: %w", listen, err)
	}
	return "http://" + net.JoinHostPort("localhost", port), nil
}
