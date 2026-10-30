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
	"strings"
	"syscall"
	"time"
	_ "time/tzdata" // Schedules' time zones, in an image without zoneinfo
	"unicode"

	"github.com/go-logr/logr"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrl "sigs.k8s.io/controller-runtime/pkg/client/config"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	"github.com/j75689/cadre/internal/api"
	"github.com/j75689/cadre/internal/auth"
	"github.com/j75689/cadre/internal/authz"
	"github.com/j75689/cadre/internal/controller"
	"github.com/j75689/cadre/internal/httpjson"
	"github.com/j75689/cadre/internal/manager"
	"github.com/j75689/cadre/internal/mcp"
	"github.com/j75689/cadre/internal/runner"
	"github.com/j75689/cadre/internal/storage/kubernetes"
	"github.com/j75689/cadre/internal/web"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "cadre-server:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		listen    = flag.String("listen", ":8080", "address to serve /api/v1, /runner/v1, /mcp/run (the per-run MCP endpoint), /mcp (the board MCP endpoint for outside agents), the board UI and the health probes on")
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
		showVer   = flag.Bool("version", false, "print the version and exit")
		external  = flag.String("external-url", "", "the URL people's browsers reach this server at (an ingress, say): logins come back to <URL>/auth/callback, cookies are Secure when it is https, and changes made with a session must come from it; default --public-url")
		authMode  = flag.String("auth", "", "how people log in to the API and the UI: oidc, or none (anyone may act as anyone, for local development only); default oidc in a cluster, none outside one")
		oidcCfg   oidcFlags
		admins    = flag.String("admin-users", "", "comma-separated usernames who are admin on every team and may create teams, with --auth=oidc")
		adminGrps = flag.String("admin-groups", "", "comma-separated groups whose members are admin on every team and may create teams, with --auth=oidc")
		streams   = flag.Int("watch-max-streams", api.DefaultWatchMaxStreams, "how many live-update streams (GET /api/v1/namespaces/{ns}/watch) this replica serves at once; one more is answered 503, and its board polls instead")
		s3cfg     kubernetes.S3Config
	)
	flag.StringVar(&oidcCfg.issuer, "oidc-issuer", "", "the OIDC provider's issuer URL")
	flag.StringVar(&oidcCfg.clientID, "oidc-client-id", "", "the OIDC client the web UI logs in with; ID tokens are checked for it")
	flag.StringVar(&oidcCfg.secretFile, "oidc-client-secret-file", "", "a file holding that client's secret")
	flag.StringVar(&oidcCfg.cliClientID, "oidc-cli-client-id", "", "the public OIDC client cadre login uses, if not the web UI's; ID tokens for it are accepted too")
	flag.StringVar(&oidcCfg.scopes, "oidc-scopes", "openid profile email groups", "the scopes asked for, space- or comma-separated")
	flag.StringVar(&oidcCfg.groupsClaim, "oidc-groups-claim", "groups", "the ID token claim listing a user's groups")
	flag.BoolVar(&oidcCfg.offlineAccess, "oidc-offline-access", true, "let cadre login ask for offline_access, and keep a refresh token, where the provider offers it")
	flag.StringVar(&oidcCfg.usernameClaim, "oidc-username-claim", "email", "the ID token claim naming a user")
	flag.StringVar(&oidcCfg.sessionKeyFile, "session-key-file", "", "a file holding the 32-byte key (raw or hex) that encrypts session cookies, the same on every replica; required with --auth=oidc")
	flag.StringVar(&s3cfg.Endpoint, "s3-endpoint", "", "S3-compatible endpoint; empty for AWS S3")
	flag.StringVar(&s3cfg.Region, "s3-region", "us-east-1", "S3 region")
	flag.StringVar(&s3cfg.Bucket, "s3-bucket", "", "bucket for the log (required)")
	flag.StringVar(&s3cfg.Prefix, "s3-prefix", "cadre/", "prefix for every object key")
	flag.BoolVar(&s3cfg.PathStyle, "s3-path-style", false, "use path-style addressing (most self-hosted stores)")
	flag.Parse()
	if *showVer {
		fmt.Println(version)
		return nil
	}
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
	if err := validateStreams(*streams); err != nil {
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
	if *external == "" {
		*external = *publicURL
	}
	// One TokenReview cache serves runners and agents on the board: both
	// send ServiceAccount tokens for the audience cadre.
	reviews := runner.Cached(controller.TokenReviews(ctrls.GetClient()))
	agents := controller.AgentAccesses(ctrls.GetClient(), ctrls.GetAPIReader(), reviews, log)
	authn, err := newAuthenticator(ctx, *authMode, inCluster, oidcCfg, *external, agents, log)
	if err != nil {
		return err
	}
	policy, err := newPolicy(authn, *admins, *adminGrps, oidcCfg.usernameClaim, log)
	if err != nil {
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
	if err := warmConfig(ctx, ctrls.GetCache()); err != nil {
		return err
	}
	// Schedules create tasks through the store, which exists only now that
	// the manager runs: their controller joins it late, as controller-runtime
	// allows, and starts once this replica leads.
	if err := controller.SetupSchedules(ctrls, store, controller.Options{}); err != nil {
		return fmt.Errorf("controllers: %w", err)
	}

	forwarder := mcp.NewForwarder(controller.ToolServerSources(ctrls), log)
	m := manager.New(store, manager.Config{
		Lease: *lease, Heartbeat: *heartbeat, StepIdle: *stepIdle, Agents: controller.Agents(ctrls), Tools: forwarder,
		Notifiers: controller.Notifiers(ctrls), ExternalURL: *external, Version: version,
	})
	var runnerOpts []runner.Option
	if *auth == "required" {
		auth := controller.Runners(ctrls.GetClient(), reviews, *builtin)
		runnerOpts = append(runnerOpts, runner.WithAuth(auth, *builtin))
	} else {
		log.Warn("runners are not authenticated (--runner-auth=none): any client may claim and finish runs")
	}
	hub := api.NewHub(store)
	if err := api.ConfigEvents(ctx, ctrls.GetCache(), hub); err != nil {
		return err
	}
	apiOpts := []api.Option{
		api.WithAgentAccesses(controller.AgentAccessStore(ctrls.GetClient(), ctrls.GetAPIReader(), ctrls.GetEventRecorder("cadre-server"))),
		api.WithConfig(ctrls.GetClient()),
		api.WithWatch(hub, api.WatchConfig{MaxStreams: *streams}),
	}
	mux := newMux(m, log, *publicURL, authn, policy, runnerOpts, apiOpts, mcp.WithForwarder(forwarder))

	srv := &http.Server{
		Addr:              *listen,
		Handler:           httpjson.LogRequests(boundRequests(mux, requestTimeout, mcpRequestTimeout), log),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      mcpRequestTimeout + 5*time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	// Shutdown waits for handlers, and a watch stream's ends only when told.
	srv.RegisterOnShutdown(hub.Close)
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

// newMux routes everything cadre-server serves: the runner protocol, the
// per-run MCP endpoint, the board MCP endpoint for agents outside Cadre, the
// API, the health probes and the board UI.
func newMux(m *manager.Manager, log *slog.Logger, publicURL string, authn auth.Authenticator, policy *authz.Policy, runnerOpts []runner.Option, apiOpts []api.Option, mcpOpts ...mcp.Option) http.Handler {
	mux := http.NewServeMux()
	authn.Register(mux, api.Base)
	runner.NewServer(m, log, publicURL, runnerOpts...).Register(mux)
	mcp.NewServer(m, log, mcpOpts...).Register(mux)
	mcp.NewBoard(m, log, policy).Register(mux, authn)
	api.NewServer(m, log, append([]api.Option{api.WithAccess(authn.Info().Mode, policy)}, apiOpts...)...).Register(mux)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		if err := m.Ready(r.Context()); err != nil {
			log.Warn("not ready", "err", err)
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	// The board UI, at its own paths only: a wrong method on a route above
	// stays the mux's 405.
	web.Register(mux)
	// People are authenticated on the API only: runners and the per-run
	// MCP endpoint check their own tokens.
	return authn.Wrap(mux, api.Base)
}

// newPolicy is the roles policy with logins, nil without them (everyone
// may do anything).
func newPolicy(authn auth.Authenticator, users, groups, usernameClaim string, log *slog.Logger) (*authz.Policy, error) {
	// Comma-separated: a group's name may hold spaces.
	split := func(s string) []string {
		var out []string
		for _, f := range strings.Split(s, ",") {
			if f = strings.TrimSpace(f); f != "" {
				out = append(out, f)
			}
		}
		return out
	}
	p := &authz.Policy{AdminUsers: split(users), AdminGroups: split(groups), FoldUsernames: usernameClaim == "email"}
	for _, u := range p.AdminUsers {
		if strings.ContainsFunc(u, unicode.IsSpace) {
			return nil, fmt.Errorf("--admin-users %q: a username has no spaces; separate names with commas", u)
		}
	}
	if authn.Info().Mode != auth.ModeOIDC {
		if len(p.AdminUsers) > 0 || len(p.AdminGroups) > 0 {
			return nil, errors.New("--admin-users and --admin-groups need --auth=oidc: without logins everyone is an admin")
		}
		return nil, nil
	}
	if len(p.AdminUsers) == 0 && len(p.AdminGroups) == 0 {
		log.Warn("no bootstrap admins (--admin-users, --admin-groups): only people named in a Team's roles can do anything, and nobody can create a team through the API")
	}
	return p, nil
}

// oidcFlags are the --oidc-* flags.
type oidcFlags struct {
	issuer, clientID, secretFile, cliClientID, scopes, groupsClaim, usernameClaim, sessionKeyFile string
	offlineAccess                                                                                 bool
}

// newAuthenticator builds the --auth mode: oidc by default in a cluster,
// none outside one. externalURL is where browsers reach the server; agents
// authenticates agents outside Cadre, with oidc only: without logins
// nobody is authenticated, and an AgentAccess grants nothing.
func newAuthenticator(ctx context.Context, mode string, inCluster bool, f oidcFlags, externalURL string, agents auth.Agents, log *slog.Logger) (auth.Authenticator, error) {
	if mode == "" {
		mode = auth.ModeNone
		if inCluster {
			mode = auth.ModeOIDC
		}
	}
	switch mode {
	case auth.ModeNone:
		log.Warn("people are not authenticated (--auth=none): anyone may act as anyone through the API, the UI and the board MCP endpoint, /mcp")
		return auth.None{}, nil
	case auth.ModeOIDC:
	default:
		return nil, fmt.Errorf("--auth %q: must be oidc or none", mode)
	}
	if f.issuer == "" || f.clientID == "" || f.sessionKeyFile == "" {
		return nil, errors.New("--auth=oidc needs --oidc-issuer, --oidc-client-id and --session-key-file")
	}
	keyData, err := os.ReadFile(f.sessionKeyFile)
	if err != nil {
		return nil, fmt.Errorf("--session-key-file: %w", err)
	}
	key, err := auth.ParseSessionKey(keyData)
	if err != nil {
		return nil, fmt.Errorf("--session-key-file: %w", err)
	}
	var secret string
	if f.secretFile != "" {
		data, err := os.ReadFile(f.secretFile)
		if err != nil {
			return nil, fmt.Errorf("--oidc-client-secret-file: %w", err)
		}
		secret = strings.TrimSpace(string(data))
	}
	return auth.NewOIDC(ctx, auth.Config{
		Issuer: f.issuer, ClientID: f.clientID, ClientSecret: secret, CLIClientID: f.cliClientID, OfflineAccess: f.offlineAccess,
		Scopes:        strings.FieldsFunc(f.scopes, func(r rune) bool { return r == ' ' || r == ',' }),
		UsernameClaim: f.usernameClaim, GroupsClaim: f.groupsClaim,
		PublicURL: externalURL, SessionKey: key, Agents: agents, Log: log,
	})
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
// mcpBound for one that may make a tool call. A watch stream has none: it
// bounds its own lifetime.
func boundRequests(next http.Handler, d, mcpBound time.Duration) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if api.IsWatch(r) {
			next.ServeHTTP(w, r)
			return
		}
		bound := d
		// The board endpoint has the per-run one's bound, as both are MCP.
		if r.URL.Path == mcp.Path || r.URL.Path == mcp.BoardPath || api.IsApproval(r) {
			bound = mcpBound
		}
		ctx, cancel := context.WithTimeout(r.Context(), bound)
		defer cancel()
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// warmConfig starts c's informers for the configuration kinds the API
// reads. The cache starts one at its first read, and on a replica that does
// not lead no controller has: they start now, not on a request.
func warmConfig(ctx context.Context, c cache.Informers) error {
	for _, obj := range []client.Object{&v1alpha1.Agent{}, &v1alpha1.ModelProvider{}, &v1alpha1.ToolServer{}, &v1alpha1.Schedule{}, &v1alpha1.Notifier{}, &v1alpha1.Runner{}} {
		if _, err := c.GetInformer(ctx, obj); err != nil {
			return fmt.Errorf("cache %T: %w", obj, err)
		}
	}
	return nil
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

// validateStreams checks --watch-max-streams.
func validateStreams(n int) error {
	if n < 1 {
		return fmt.Errorf("--watch-max-streams %d: must be at least 1", n)
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
