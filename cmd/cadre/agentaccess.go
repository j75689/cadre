package main

import (
	"text/tabwriter"

	"github.com/spf13/cobra"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	"github.com/j75689/cadre/internal/api"
)

func newAgentAccessCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{Use: "agentaccess", Short: "Let agents outside Cadre use the team's board (team admins)"}
	cmd.AddCommand(newAgentAccessSet(g), newAgentAccessList(g), newAgentAccessDelete(g))
	return cmd
}

// newAgentAccessSet makes an AgentAccess, or replaces its spec.
func newAgentAccessSet(g *globals) *cobra.Command {
	var role, serviceAccount string
	cmd := &cobra.Command{
		Use:   "set NAME",
		Short: "Make an AgentAccess, or replace its role and ServiceAccount",
		Long: "Make an AgentAccess, or replace its spec. Without --service-account Cadre mints a token into the Secret NAME.access-token, " +
			"which the API never serves: whoever may read Secrets in the namespace reads it with the command this prints.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			req := api.AgentAccessRequest{Spec: v1alpha1.AgentAccessSpec{Role: v1alpha1.AgentRole(role)}}
			if serviceAccount != "" {
				req.Spec.ServiceAccount = &v1alpha1.ServiceAccountRef{Name: serviceAccount}
			}
			c := g.client()
			list, err := c.ListAgentAccesses(cmd.Context(), g.namespace)
			if err != nil {
				return err
			}
			for _, aa := range list.Items {
				if aa.Name == name {
					req.ResourceVersion = aa.ResourceVersion
				}
			}
			view, err := c.PutAgentAccess(cmd.Context(), g.namespace, name, req)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if view.Spec.ServiceAccount != nil {
				outf(out, "agentaccess %s: %s, as ServiceAccount %s, with a projected token for the audience cadre\n", name, view.Spec.Role, view.Spec.ServiceAccount.Name)
				return nil
			}
			secret := name + ".access-token"
			outf(out, "agentaccess %s: %s, with a token Cadre mints into the Secret %s\n", name, view.Spec.Role, secret)
			outf(out, "read it, with access to Secrets in %s: kubectl -n %s get secret %s -o jsonpath='{.data.token}' | base64 -d\n", g.namespace, g.namespace, secret)
			return nil
		},
	}
	cmd.Flags().StringVar(&role, "role", "", "the role on the team: viewer or member")
	cmd.Flags().StringVar(&serviceAccount, "service-account", "", "authenticate as this ServiceAccount of the namespace, instead of a minted token")
	_ = cmd.MarkFlagRequired("role")
	return cmd
}

func newAgentAccessList(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List the team's AgentAccesses",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			list, err := g.client().ListAgentAccesses(cmd.Context(), g.namespace)
			if err != nil {
				return err
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			outf(w, "NAME\tROLE\tSERVICEACCOUNT\tREADY\tTOKEN SECRET\n")
			for _, aa := range list.Items {
				sa, ready := "-", "True"
				if aa.Spec.ServiceAccount != nil {
					sa = aa.Spec.ServiceAccount.Name
				}
				if !aa.Ready {
					ready = "False (" + aa.Reason + ")"
				}
				outf(w, "%s\t%s\t%s\t%s\t%s\n", aa.Name, aa.Spec.Role, sa, ready, orDash(aa.TokenSecret))
			}
			return w.Flush()
		},
	}
}

func newAgentAccessDelete(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "delete NAME",
		Short: "Delete an AgentAccess: its agent's access ends at once",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := g.client().DeleteAgentAccess(cmd.Context(), g.namespace, args[0]); err != nil {
				return err
			}
			outf(cmd.OutOrStdout(), "agentaccess %s deleted\n", args[0])
			return nil
		},
	}
}
