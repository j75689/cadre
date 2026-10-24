package main

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	"github.com/j75689/cadre/internal/client"
	"github.com/j75689/cadre/internal/fakerunner"
)

func newRunnerCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{Use: "runner", Short: "Runners"}
	var (
		o         fakerunner.Options
		outcome   string
		reason    string
		tokenFile string
	)
	host, _ := os.Hostname()
	fake := &cobra.Command{
		Use:   "fake",
		Short: "Claim tasks and finish them with a chosen outcome, without doing any work",
		Long: `Claim tasks and finish them with a chosen outcome, without doing any work.

The runner claims from every Team namespace unless -n or CADRE_NAMESPACE is
given. It renews
its lease while it "works", stops a run at once if the run loses its task
(cancelled or reclaimed), and retries a finish safely.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			o.Outcome = v1alpha1.RunOutcome(outcome)
			o.Reason = v1alpha1.WaitingReason(reason)
			// Only an explicit -n or CADRE_NAMESPACE limits the runner; by
			// default it takes work from every Team namespace.
			if cmd.Flags().Changed("namespace") || os.Getenv("CADRE_NAMESPACE") != "" {
				o.Namespaces = []string{g.namespace}
			}
			o.Log = cmd.ErrOrStderr()
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			hc, authenticated := client.RunnerHTTP(tokenFile)
			if !authenticated {
				_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "no runner token: requests are unauthenticated, which only a cadre-server with --runner-auth=none accepts")
			}
			results, err := fakerunner.Run(ctx, client.New(g.server, hc), o)
			if err != nil {
				return err
			}
			if o.Once && len(results) == 1 && results[0].Lost {
				return fmt.Errorf("run %s lost its task", results[0].RunID)
			}
			return nil
		},
	}
	f := fake.Flags()
	f.StringVar(&o.Runner, "runner", host, "this runner's name, shown in status.run.runner")
	f.DurationVar(&o.Work, "work", 2*time.Second, "how long each run pretends to work")
	f.StringVar(&outcome, "outcome", "Done", "Done, Review, Failed or Waiting")
	f.StringVar(&reason, "reason", "Question", "with --outcome Waiting: Question, ToolApproval or Review")
	f.IntVar(&o.Subtasks, "subtasks", 0, "subtasks each run asks for")
	// The fake stands in for cadre-runner, so by default it claims builtin
	// agents as cadre-runner does.
	f.StringToStringVarP(&o.Labels, "label", "l", map[string]string{v1alpha1.LabelRuntime: string(v1alpha1.RuntimeBuiltin)},
		"claim labels, key=value; repeatable. The default claims builtin agents, as cadre-runner does")
	f.BoolVar(&o.Once, "once", false, "stop after one run")
	f.DurationVar(&o.Poll, "poll", time.Second, "wait between claims when there is no work")
	f.StringVar(&tokenFile, "token-file", "", "the token to authenticate with, read before every request; by default the projected ServiceAccount token at "+client.DefaultTokenFile+" when it exists, else none")
	cmd.AddCommand(fake)
	return cmd
}
