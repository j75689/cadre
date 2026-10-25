// Command cadre is the Cadre CLI: work on the board, set up a team, and run a
// fake runner that drives tasks through their life without an LLM.
package main

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/user"
	"time"

	"github.com/spf13/cobra"

	"github.com/j75689/cadre/internal/auth"
	"github.com/j75689/cadre/internal/client"
)

// globals are the flags every command shares.
type globals struct {
	server    string
	namespace string
	as        string
}

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if err := newRoot().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "cadre:", err)
		os.Exit(1)
	}
}

func newRoot() *cobra.Command {
	g := &globals{}
	root := &cobra.Command{
		Use:           "cadre",
		Short:         "Work with a Cadre board",
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().StringVar(&g.server, "server", envOr("CADRE_SERVER", "http://localhost:8080"), "cadre-server URL (env CADRE_SERVER)")
	root.PersistentFlags().StringVarP(&g.namespace, "namespace", "n", envOr("CADRE_NAMESPACE", "default"), "the team's namespace (env CADRE_NAMESPACE)")
	root.PersistentFlags().StringVar(&g.as, "as", currentUser(), "who you are on the board, for a server without logins (--auth=none); with one, you are who cadre login says")
	root.SetVersionTemplate("{{.Version}}\n")
	// --as names the author only on a server without logins: a server
	// with them takes the author from the login, and naming another would
	// mislead.
	root.PersistentPreRunE = func(cmd *cobra.Command, _ []string) error {
		if !cmd.Flags().Changed("as") || cmd.Name() == "login" {
			return nil
		}
		info, err := client.New(g.server, nil).AuthInfo(cmd.Context())
		if err == nil && info.Mode == auth.ModeOIDC {
			return errors.New("--as: on this server identity comes from login; run cadre login instead")
		}
		return nil
	}
	root.AddCommand(newTaskCmd(g), newTeamCmd(g), newAgentAccessCmd(g), newRunnerCmd(g), newLoginCmd(g), newAuthCmd(g), &cobra.Command{
		Use:   "version",
		Short: "Print the version",
		Args:  cobra.NoArgs,
		Run:   func(cmd *cobra.Command, _ []string) { outln(cmd.OutOrStdout(), version) },
	})
	return root
}

// client talks to the server, with the ID token cadre login stored for it,
// if any. One that has expired fails every request with a pointer to cadre
// login.
func (g *globals) client() *client.Client {
	creds, err := loadCredentials()
	c, ok := creds[serverKey(g.server)]
	switch {
	case err != nil || !ok:
		return client.New(g.server, nil)
	case !time.Now().Before(c.Expires):
		return client.New(g.server, &http.Client{Transport: expired{g.server}})
	}
	return client.New(g.server, &http.Client{Transport: client.Bearer(c.IDToken, nil)})
}

// expired refuses to send a request with a login that has run out.
type expired struct{ server string }

func (e expired) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, fmt.Errorf("your login to %s expired; run cadre login", e.server)
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func currentUser() string {
	if v := os.Getenv("USER"); v != "" {
		return v
	}
	if u, err := user.Current(); err == nil {
		return u.Username
	}
	return "anonymous"
}

// outf and outln print command output. Errors writing to a tabwriter surface
// at Flush; for a terminal there is nothing better to do than carry on.
func outf(w io.Writer, format string, args ...any) { _, _ = fmt.Fprintf(w, format, args...) }

func outln(w io.Writer, args ...any) { _, _ = fmt.Fprintln(w, args...) }
