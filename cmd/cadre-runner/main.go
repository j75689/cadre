// Command cadre-runner runs builtin agents' tasks: Cadre's own agent loop,
// in runtime mode. It runs in the cluster, because it reads each run's model
// credential from the ModelProvider's Secret with its own ServiceAccount;
// credentials never travel over the runner protocol.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	"github.com/j75689/cadre/internal/builtin"
	cadreclient "github.com/j75689/cadre/internal/client"
	"github.com/j75689/cadre/internal/provider/anthropic"
	"github.com/j75689/cadre/internal/provider/openai"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "cadre-runner:", err)
		os.Exit(1)
	}
}

func run() error {
	host, _ := os.Hostname()
	var (
		server     = flag.String("server", "http://cadre-server:8080", "cadre-server's URL")
		name       = flag.String("runner", host, "this runner's name, shown in status.run.runner")
		namespaces = flag.String("namespaces", "", "comma-separated Team namespaces to take work from; empty for every one")
		maxTurns   = flag.Int("max-turns", builtin.DefaultMaxTurns, "model responses per run before it hands the task to a human")
		once       = flag.Bool("once", false, "stop after one run")
	)
	flag.Parse()
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
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
	log.Info("cadre-runner started", "server", *server, "runner", *name, "namespaces", o.Namespaces)
	return builtin.Run(ctx, cadreclient.New(*server, nil), o)
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
