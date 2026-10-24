// Command cadre-runner runs builtin agents' tasks: Cadre's own agent loop,
// a whole run at a time (runtime mode) or a step at a time (step mode). It
// runs in the cluster, because it reads each run's model
// credential from the ModelProvider's Secret with its own ServiceAccount;
// credentials never travel over the runner protocol.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"

	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	"github.com/j75689/cadre/internal/builtin"
	cadreclient "github.com/j75689/cadre/internal/client"
	"github.com/j75689/cadre/internal/provider/anthropic"
	"github.com/j75689/cadre/internal/provider/openai"
	"github.com/j75689/cadre/internal/runner"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "cadre-runner:", err)
		os.Exit(1)
	}
}

// defaults are the flags' defaults: a runner started for a Job (CADRE_RUNNER
// set) claims under the Job's name, from the server and in the namespace
// its Job says, one whole run, and exits; explicit flags still win.
type defaults struct {
	server, runner, namespaces, mode, agent string
	once                                    bool
}

func flagDefaults(getenv func(string) string, host string) defaults {
	d := defaults{server: "http://cadre-server:8080", runner: host, mode: "step"}
	if env, ok := runner.ReadJobEnv(getenv); ok {
		d.runner, d.mode, d.once, d.agent = env.Runner, "runtime", true, env.Agent
		if env.Server != "" {
			d.server = env.Server
		}
		d.namespaces = env.Namespace
	}
	return d
}

func run() error {
	host, _ := os.Hostname()
	d := flagDefaults(os.Getenv, host)
	var (
		server     = flag.String("server", d.server, "cadre-server's URL; in a Job, "+runner.EnvServer)
		name       = flag.String("runner", d.runner, "this runner's name, shown in status.run.runner; in a Job, "+runner.EnvRunner)
		namespaces = flag.String("namespaces", d.namespaces, "comma-separated Team namespaces to take work from; empty for every one; in a Job, "+runner.EnvNamespace)
		maxTurns   = flag.Int("max-turns", builtin.DefaultMaxTurns, "model responses per run before it hands the task to a human")
		once       = flag.Bool("once", d.once, "stop after one run (one step in step mode), or the first claim that finds no work; on in a Job")
		mode       = flag.String("mode", d.mode, "step: work the steps of builtin agents' runs, a step at a time; runtime: work whole runs of builtin agents set to execution: runtime; runtime in a Job")
		parallel   = flag.Int("parallel", 1, "in step mode, how many steps to work at once")
		stepTime   = flag.Duration("step-timeout", builtin.DefaultStepTimeout, "in step mode, the longest one step may take before it is given back")
		tokenFile  = flag.String("token-file", "", "the token to authenticate to cadre-server with, read before every request; by default the projected ServiceAccount token at "+cadreclient.DefaultTokenFile+" when it exists, else none")
	)
	flag.Parse()
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	switch {
	case *mode != "runtime" && *mode != "step":
		return fmt.Errorf("--mode must be runtime or step, not %q", *mode)
	case *parallel < 1:
		return fmt.Errorf("--parallel must be at least 1")
	case *parallel > 1 && *mode != "step":
		return fmt.Errorf("--parallel is for --mode=step")
	}
	if strings.TrimSpace(*name) == "" {
		return fmt.Errorf("--runner is empty and the host name could not be read; name this runner")
	}

	rest, err := ctrl.GetConfig()
	if err != nil {
		return fmt.Errorf("kubernetes config: %w", err)
	}
	// A direct client: no informer, so Secrets are read fresh and never
	// cached, and the ServiceAccount needs get on them only.
	kube, err := client.New(rest, client.Options{})
	if err != nil {
		return fmt.Errorf("kubernetes client: %w", err)
	}

	o := builtin.Options{
		Runner:   *name,
		Secrets:  secrets{kube},
		Log:      log,
		MaxTurns: *maxTurns,
		Once:     *once,
		Agent:    d.agent,

		StepTimeout: *stepTime,
		Providers: map[v1alpha1.ProviderType]builtin.Provider{
			v1alpha1.ProviderAnthropic: &anthropic.Provider{Log: log},
			v1alpha1.ProviderOpenAI:    &openai.Provider{Log: log},
		},
	}
	for _, ns := range strings.Split(*namespaces, ",") {
		if ns = strings.TrimSpace(ns); ns != "" {
			o.Namespaces = append(o.Namespaces, ns)
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	log.Info("cadre-runner started", "server", *server, "runner", *name, "namespaces", o.Namespaces, "mode", *mode)
	hc, authenticated := cadreclient.RunnerHTTP(*tokenFile)
	if !authenticated {
		log.Warn("no runner token: requests are unauthenticated, which only a cadre-server with --runner-auth=none accepts", "tokenFile", cadreclient.DefaultTokenFile)
	}
	c := cadreclient.New(*server, hc)
	if *mode == "runtime" {
		return builtin.Run(ctx, c, o)
	}
	// Each worker has a name of its own: a worker is its step's holder by
	// identity and name, so two sharing both could take each other's step.
	var wg sync.WaitGroup
	errs := make([]error, *parallel)
	for i := range *parallel {
		w := o
		if *parallel > 1 {
			w.Runner = fmt.Sprintf("%s-%d", *name, i)
		}
		wg.Go(func() { errs[i] = builtin.RunSteps(ctx, c, w) })
	}
	wg.Wait()
	return errors.Join(errs...)
}

// secrets reads one Secret key from the API server for each run. The value
// goes to the run only; it is never cached or logged.
type secrets struct{ c client.Reader }

func (s secrets) Value(ctx context.Context, namespace string, ref v1alpha1.SecretKeyRef) (string, error) {
	var sec corev1.Secret
	if err := s.c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: ref.Name}, &sec); err != nil {
		return "", err
	}
	v, ok := sec.Data[ref.Key]
	if !ok {
		return "", fmt.Errorf("secret %s/%s has no key %s", namespace, ref.Name, ref.Key)
	}
	return string(v), nil
}
