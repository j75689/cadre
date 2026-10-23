package main

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	"github.com/j75689/cadre/internal/fakerunner"
)

func newRunnerCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{Use: "runner", Short: "Runners"}
	var (
		o       fakerunner.Options
		outcome string
		reason  string
	)
	host, _ := os.Hostname()
	fake := &cobra.Command{
		Use:   "fake",
		Short: "Claim tasks and finish them with a chosen outcome, without doing any work",
		Long: `Claim tasks and finish them with a chosen outcome, without doing any work.

The runner claims from every Team namespace unless -n is given. It renews
its lease while it "works", stops a run at once if the run loses its task
(cancelled or reclaimed), and retries a finish safely.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			o.Outcome = v1alpha1.RunOutcome(outcome)
			o.Reason = v1alpha1.WaitingReason(reason)
			// Only an explicit -n limits the runner; by default it takes
			// work from every Team namespace.
			if cmd.Flags().Changed("namespace") {
				o.Namespaces = []string{g.namespace}
			}
			o.Log = cmd.ErrOrStderr()
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			results, err := fakerunner.Run(ctx, g.client(), o)
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
	f.BoolVar(&o.Once, "once", false, "stop after one run")
	f.DurationVar(&o.Poll, "poll", time.Second, "wait between claims when there is no work")
	cmd.AddCommand(fake)
	return cmd
}
