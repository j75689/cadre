// Command cadre-server runs the Task Manager: the /api/v1 and /runner/v1
// endpoints and the background loops (numbering, outbox dispatch, lease
// reclaim), and the controllers that mark Agents, ModelProviders and
// ToolServers Ready. Any number of replicas may run against the same cluster and
// object store.
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
		heartbeat = flag.Duration("heartbeat", manager.DefaultConfig.Heartbeat, "how often runners renew; also the reclaim grace")
		reclaim   = flag.Duration("reclaim-interval", manager.DefaultLoopConfig.Reclaim, "how often expired leases are reclaimed")
		dispatch  = flag.Duration("dispatch-interval", manager.DefaultLoopConfig.Dispatch, "how often the outbox is dispatched and new tasks numbered")
		auth      = flag.String("runner-auth", "required", "required: runners authenticate with a ServiceAccount token for audience cadre; none: no runner authentication, for tests and local development only")
		builtin   = flag.String("builtin-runner", "system:serviceaccount:cadre-system:cadre-runner", "the identity of cadre-runner, the only runner that may claim builtin agents' tasks")
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
	store, err := kubernetes.Open(ctx, kubernetes.Config{REST: rest, S3: s3cfg})
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	ctrls, err := controller.NewManager(rest)
	if err != nil {
		return fmt.Errorf("controllers: %w", err)
	}
	if err := controller.Setup(ctrls, controller.Options{}); err != nil {
		return fmt.Errorf("controllers: %w", err)
	}

	forwarder := mcp.NewForwarder(controller.ToolServerSources(ctrls), log)
	m := manager.New(store, manager.Config{Lease: *lease, Heartbeat: *heartbeat, Agents: controller.Agents(ctrls), Tools: forwarder})
	mux := http.NewServeMux()
	if *publicURL, err = resolvePublicURL(*publicURL, *listen, os.Getenv("KUBERNETES_SERVICE_HOST") != ""); err != nil {
		return err
	}
	var runnerOpts []runner.Option
	if *auth == "required" {
		reviews := runner.Cached(controller.TokenReviews(ctrls.GetClient()))
		auth := controller.Runners(ctrls.GetClient(), ctrls.GetAPIReader(), reviews, *builtin)
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
	controllers := make(chan error, 1)
	go func() { controllers <- ctrls.Start(ctx) }()
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
