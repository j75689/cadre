// Command cadre is the Cadre CLI: work on the board, set up a team, and run a
// fake runner that drives tasks through their life without an LLM.
package main

import (
	"fmt"
	"io"
	"os"
	"os/user"

	"github.com/spf13/cobra"

	"github.com/j75689/cadre/internal/client"
)

// globals are the flags every command shares.
type globals struct {
	server    string
	namespace string
	as        string
}

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
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().StringVar(&g.server, "server", envOr("CADRE_SERVER", "http://localhost:8080"), "cadre-server URL (env CADRE_SERVER)")
	root.PersistentFlags().StringVarP(&g.namespace, "namespace", "n", envOr("CADRE_NAMESPACE", "default"), "the team's namespace (env CADRE_NAMESPACE)")
	root.PersistentFlags().StringVar(&g.as, "as", currentUser(), "who you are on the board")
	root.AddCommand(newTaskCmd(g), newTeamCmd(g), newRunnerCmd(g))
	return root
}

func (g *globals) client() *client.Client { return client.New(g.server, nil) }

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
