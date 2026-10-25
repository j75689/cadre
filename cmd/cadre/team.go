package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	"github.com/j75689/cadre/internal/api"
)

func newTeamCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{Use: "team", Short: "Set up a team"}
	cmd.AddCommand(newTeamInit(g), newTeamSet(g))
	return cmd
}

const roleUsage = "a role and its subjects, as admin=user:alice@example.test,group:platform; repeatable"

func newTeamInit(g *globals) *cobra.Command {
	var req api.TeamRequest
	var roles []string
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Opt the namespace in to Cadre",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var err error
			if req.Roles, err = parseRoles(roles); err != nil {
				return err
			}
			team, err := g.client().InitTeam(cmd.Context(), g.namespace, req)
			if err != nil {
				return err
			}
			outf(cmd.OutOrStdout(), "team %s ready, %d concurrent runs\n", team.Namespace, team.Spec.MaxConcurrentRuns)
			return nil
		},
	}
	cmd.Flags().StringVar(&req.DisplayName, "display-name", "", "name shown on the board")
	cmd.Flags().Int32Var(&req.MaxConcurrentRuns, "max-concurrent-runs", 1, "how many of the team's tasks may run at once")
	cmd.Flags().StringArrayVar(&roles, "role", nil, roleUsage)
	return cmd
}

// newTeamSet edits the Team: what each flag given says, the rest as it is.
// The roles given replace all of the Team's.
func newTeamSet(g *globals) *cobra.Command {
	var (
		displayName  string
		maxRuns      int32
		archiveAfter time.Duration
		roles        []string
	)
	cmd := &cobra.Command{
		Use:   "set",
		Short: "Edit the team: its display name, run cap, archiving and roles",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c := g.client()
			team, err := c.GetTeam(cmd.Context(), g.namespace)
			if err != nil {
				return err
			}
			s := team.Spec
			f := cmd.Flags()
			if f.Changed("display-name") {
				s.DisplayName = displayName
			}
			if f.Changed("max-concurrent-runs") {
				s.MaxConcurrentRuns = maxRuns
			}
			if f.Changed("archive-after") {
				// 0 is the default: never archived.
				s.ArchiveAfter = nil
				if archiveAfter != 0 {
					s.ArchiveAfter = &metav1.Duration{Duration: archiveAfter}
				}
			}
			if f.Changed("role") {
				if s.Roles, err = parseRoles(roles); err != nil {
					return err
				}
			}
			if _, err := c.UpdateTeam(cmd.Context(), g.namespace, api.TeamRequest{TeamSpec: s, ResourceVersion: team.ResourceVersion}); err != nil {
				return err
			}
			// What was sent: the reply shows the roles to admins only, and
			// the edit may have made the caller no longer one.
			outf(cmd.OutOrStdout(), "team %s updated: %d concurrent runs, %d roles\n", g.namespace, s.MaxConcurrentRuns, len(s.Roles))
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&displayName, "display-name", "", "name shown on the board")
	f.Int32Var(&maxRuns, "max-concurrent-runs", 0, "how many of the team's tasks may run at once; unchanged when not given")
	f.DurationVar(&archiveAfter, "archive-after", 0, "how long a finished task stays on the board, as 720h; 0 for never")
	f.StringArrayVar(&roles, "role", nil, roleUsage+"; replaces all the team's roles (to remove them all, use the API or kubectl)")
	return cmd
}

// parseRoles reads --role flags, role=kind:name,kind:name each; one role
// named twice gathers its subjects.
func parseRoles(flags []string) ([]v1alpha1.TeamRole, error) {
	var out []v1alpha1.TeamRole
	at := map[v1alpha1.RoleName]int{}
	for _, f := range flags {
		role, subjects, ok := strings.Cut(f, "=")
		if !ok || subjects == "" {
			return nil, fmt.Errorf("--role %q: want role=user:name,group:name", f)
		}
		r := v1alpha1.RoleName(role)
		i, seen := at[r]
		if !seen {
			i = len(out)
			at[r] = i
			out = append(out, v1alpha1.TeamRole{Role: r})
		}
		for _, s := range strings.Split(subjects, ",") {
			kind, name, ok := strings.Cut(s, ":")
			if !ok || name == "" || (kind != string(v1alpha1.SubjectUser) && kind != string(v1alpha1.SubjectGroup)) {
				return nil, fmt.Errorf("--role %q: subject %q is not user:name or group:name", f, s)
			}
			out[i].Subjects = append(out[i].Subjects, v1alpha1.RoleSubject{Kind: v1alpha1.SubjectKind(kind), Name: name})
		}
	}
	return out, nil
}
