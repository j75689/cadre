package main

import (
	"github.com/spf13/cobra"

	"github.com/j75689/cadre/internal/api"
)

func newTeamCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{Use: "team", Short: "Set up a team"}
	var req api.TeamRequest
	initCmd := &cobra.Command{
		Use:   "init",
		Short: "Opt the namespace in to Cadre",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			team, err := g.client().InitTeam(cmd.Context(), g.namespace, req)
			if err != nil {
				return err
			}
			outf(cmd.OutOrStdout(), "team %s ready, %d concurrent runs\n", team.Namespace, team.Spec.MaxConcurrentRuns)
			return nil
		},
	}
	initCmd.Flags().StringVar(&req.DisplayName, "display-name", "", "name shown on the board")
	initCmd.Flags().Int32Var(&req.MaxConcurrentRuns, "max-concurrent-runs", 1, "how many of the team's tasks may run at once")
	cmd.AddCommand(initCmd)
	return cmd
}
