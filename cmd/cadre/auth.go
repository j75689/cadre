package main

import (
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"
)

// tokenMargin is how long a printed token must still be good for: a header
// helper never hands an agent a token that dies during its next call.
const tokenMargin = time.Minute

func newAuthCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{Use: "auth", Short: "Work with your login"}
	cmd.AddCommand(&cobra.Command{
		Use:   "token",
		Short: "Print the ID token cadre login stored for the server, for an agent's Authorization header",
		Long: "Print the ID token cadre login stored for --server, to send as\n" +
			"\"Authorization: Bearer <token>\" to the server's MCP endpoint, /mcp. A token\n" +
			"that has expired, or expires within a minute, is refreshed first where the\n" +
			"login has a refresh token, and otherwise not printed: log in again.\n" +
			"An ID token lasts hours at most, so it suits an agent you run yourself; an\n" +
			"agent that runs unattended needs a token of its own.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cred, ok, err := currentLogin(cmd.Context(), g.server)
			var refused *refusedError
			switch {
			case errors.As(err, &refused):
				return fmt.Errorf("the login for %s expires within a minute, and %w; run cadre login", g.server, err)
			case err != nil:
				return err
			}
			switch {
			case !ok || cred.IDToken == "":
				return fmt.Errorf("no login for %s: run cadre login", g.server)
			case !time.Now().Add(tokenMargin).Before(cred.Expires):
				return fmt.Errorf("the login for %s has expired, or expires within a minute: run cadre login", g.server)
			}
			outln(cmd.OutOrStdout(), cred.IDToken)
			return nil
		},
	})
	return cmd
}
