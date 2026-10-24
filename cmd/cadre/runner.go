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
	"github.com/j75689/cadre/internal/runner"
)

func newRunnerCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{Use: "runner", Short: "Runners"}
	var (
		o         fakerunner.Options
		outcome   string
		reason    string
		tokenFile string
	)
	// In a Job (CADRE_RUNNER set) the fake claims under the Job's name, the
	// Job's agent's tasks, once, as any Job's runner must; CADRE_SERVER and
	// CADRE_NAMESPACE are the global flags' defaults already.
	host, _ := os.Hostname()
	job, inJob := runner.ReadJobEnv(os.Getenv)
	if inJob {
		host = job.Runner
	}
	o.Agent = job.Agent
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
	f.BoolVar(&o.Once, "once", inJob, "stop after one run, or the first claim that finds no work; on in a Job")
	f.DurationVar(&o.Poll, "poll", time.Second, "wait between claims when there is no work")
	f.StringVar(&tokenFile, "token-file", "", "a file holding this runner's token, read before every request: a minted token from its Runner's <name>-token Secret, or a ServiceAccount token for the audience cadre; by default "+client.DefaultTokenFile+" when it exists, else none")
	cmd.AddCommand(fake)
	return cmd
}
