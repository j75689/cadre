// Command cadre-server runs the Task Manager: the /runner/v1 endpoints and
// the background loops (numbering, outbox dispatch, lease reclaim). Any number
// of replicas may run against the same cluster and object store.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	ctrl "sigs.k8s.io/controller-runtime/pkg/client/config"

	"github.com/j75689/cadre/internal/manager"
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
		listen    = flag.String("listen", ":8080", "address to serve /runner/v1 and /healthz on")
		lease     = flag.Duration("lease", manager.DefaultConfig.Lease, "how long a claim or heartbeat keeps a run alive")
		heartbeat = flag.Duration("heartbeat", manager.DefaultConfig.Heartbeat, "how often runners renew; also the reclaim grace")
		reclaim   = flag.Duration("reclaim-interval", 5*time.Second, "how often expired leases are reclaimed")
		dispatch  = flag.Duration("dispatch-interval", 10*time.Second, "how often the outbox is dispatched and new tasks numbered")
		s3cfg     kubernetes.S3Config
	)
	flag.StringVar(&s3cfg.Endpoint, "s3-endpoint", "", "S3-compatible endpoint; empty for AWS S3")
	flag.StringVar(&s3cfg.Region, "s3-region", "us-east-1", "S3 region")
	flag.StringVar(&s3cfg.Bucket, "s3-bucket", "", "bucket for the log (required)")
	flag.StringVar(&s3cfg.Prefix, "s3-prefix", "cadre/", "prefix for every object key")
	flag.BoolVar(&s3cfg.PathStyle, "s3-path-style", false, "use path-style addressing (most self-hosted stores)")
	flag.Parse()
	// Keys come from the environment, never flags, so they stay out of
	// process listings. Without them the AWS default chain is used.
	s3cfg.AccessKey = os.Getenv("CADRE_S3_ACCESS_KEY")
	s3cfg.SecretKey = os.Getenv("CADRE_S3_SECRET_KEY")

	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
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

	m := manager.New(store, manager.Config{Lease: *lease, Heartbeat: *heartbeat})
	mux := http.NewServeMux()
	runner.NewServer(m, log).Register(mux)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

	srv := &http.Server{Addr: *listen, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	loops := make(chan struct{})
	go func() {
		defer close(loops)
		m.Run(ctx, manager.LoopConfig{Reclaim: *reclaim, Dispatch: *dispatch}, log)
	}()
	served := make(chan error, 1)
	go func() { served <- srv.ListenAndServe() }()
	log.Info("cadre-server started", "listen", *listen)

	select {
	case err = <-served:
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		err = srv.Shutdown(shutdown)
	}
	stop()
	<-loops
	if errors.Is(err, http.ErrServerClosed) {
		err = nil
	}
	return err
}
