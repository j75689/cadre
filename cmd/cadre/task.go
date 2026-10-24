package main

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	"github.com/j75689/cadre/internal/client"
	"github.com/j75689/cadre/internal/httpjson"
	"github.com/j75689/cadre/internal/manager"
	"github.com/j75689/cadre/internal/runes"
)

func newTaskCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{Use: "task", Short: "Create, inspect and steer tasks"}
	cmd.AddCommand(newTaskCreate(g), newTaskList(g), newTaskGet(g), newTaskComment(g), newTaskReply(g), newTaskDecide(g, true), newTaskDecide(g, false), newTaskCancel(g))
	return cmd
}

func newTaskCreate(g *globals) *cobra.Command {
	var (
		spec          v1alpha1.TaskSpec
		name, agent   string
		human, parent string
		priority      string
	)
	cmd := &cobra.Command{
		Use:   "create TITLE",
		Short: "Create a task",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			spec.Title = args[0]
			spec.Priority = v1alpha1.Priority(priority)
			switch {
			case agent != "" && human != "":
				return fmt.Errorf("--agent and --user are exclusive")
			case agent != "":
				spec.Assignee = &v1alpha1.Assignee{Kind: v1alpha1.AssigneeAgent, Name: agent}
			case human != "":
				spec.Assignee = &v1alpha1.Assignee{Kind: v1alpha1.AssigneeUser, Name: human}
			}
			if parent != "" {
				spec.ParentRef = &v1alpha1.TaskReference{Name: parent}
			}
			t, err := g.client().CreateTask(cmd.Context(), g.namespace, name, spec)
			if err != nil {
				return err
			}
			outf(cmd.OutOrStdout(), "created #%d %s\n", t.Status.Number, t.Name)
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&name, "name", "", "task name; a ULID when empty")
	f.StringVarP(&spec.Description, "description", "d", "", "what the work is")
	f.StringVar(&agent, "agent", "", "assign to this Agent")
	f.StringVar(&human, "user", "", "assign to this user")
	f.StringVarP(&priority, "priority", "p", "", "low, medium, high or urgent")
	f.StringVar(&parent, "parent", "", "make it a subtask of this task")
	f.StringSliceVarP(&spec.Labels, "label", "l", nil, "a label; repeatable")
	return cmd
}

func newTaskList(g *globals) *cobra.Command {
	var phases []string
	var limit int
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List the board",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ps := make([]v1alpha1.Phase, 0, len(phases))
			for _, p := range phases {
				ps = append(ps, v1alpha1.Phase(p))
			}
			list, err := g.client().ListTasks(cmd.Context(), g.namespace, ps, 0, limit)
			if err != nil {
				return err
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			outln(w, "NUMBER\tNAME\tPHASE\tPRIORITY\tASSIGNEE\tTITLE")
			for _, t := range list.Items {
				outf(w, "#%d\t%s\t%s\t%s\t%s\t%s\n", t.Status.Number, t.Name, phaseOf(&t), orDash(string(t.Spec.Priority)), assignee(&t), t.Spec.Title)
			}
			return w.Flush()
		},
	}
	cmd.Flags().StringSliceVar(&phases, "phase", nil, "only these phases; repeatable")
	cmd.Flags().IntVar(&limit, "limit", 100, "at most this many tasks")
	return cmd
}

func newTaskGet(g *globals) *cobra.Command {
	var entries int
	cmd := &cobra.Command{
		Use:   "get NAME|#NUMBER",
		Short: "Show a task, the end of its thread and its runs",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := g.client()
			ctx := cmd.Context()
			name, err := resolve(ctx, c, g.namespace, args[0])
			if err != nil {
				return err
			}
			t, err := c.GetTask(ctx, g.namespace, name)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			printTask(out, t)

			var after int64
			if t.Status.Thread != nil {
				after = max(t.Status.Thread.Revision-int64(entries), 0)
			}
			thread, err := c.Thread(ctx, g.namespace, name, after, 0)
			if err != nil {
				return err
			}
			if len(thread.Items) > 0 {
				outln(out, "\nThread:")
				for _, e := range thread.Items {
					outf(out, "  %d  %s  %s %s (%s): %s\n", e.Seq, e.At.Format("2006-01-02 15:04"), strings.ToLower(string(e.Author.Kind)), e.Author.Name, e.Kind, e.Body)
					if e.Kind == manager.EntryToolCall && e.Call != nil && len(e.Call.Arguments) > 0 {
						outf(out, "      arguments: %s\n", clip(string(e.Call.Arguments), maxShownArguments))
					}
				}
			}
			runs, err := c.Runs(ctx, g.namespace, name)
			if err != nil {
				return err
			}
			if len(runs.Items) > 0 {
				outln(out, "\nRuns:")
				w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
				outln(w, "  RUN\tRUNNER\tENDED\tREASON\tOUTCOME")
				for _, r := range runs.Items {
					outf(w, "  %s\t%s\t%s\t%s\t%s\n", r.ID, orDash(r.Runner), r.EndedAt.Format("2006-01-02 15:04:05"), r.Reason, orDash(string(r.Outcome)))
				}
				if err := w.Flush(); err != nil {
					return err
				}
			}
			return nil
		},
	}
	cmd.Flags().IntVar(&entries, "thread", 5, "how many thread entries to show")
	return cmd
}

func printTask(out io.Writer, t *v1alpha1.Task) {
	outf(out, "#%d %s\n", t.Status.Number, t.Spec.Title)
	outf(out, "Name:      %s/%s\n", t.Namespace, t.Name)
	outf(out, "Phase:     %s\n", phaseOf(t))
	if w := t.Status.Waiting; w != nil {
		outf(out, "Waiting:   %s since %s\n", w.Reason, w.Since.Format("2006-01-02 15:04:05"))
	}
	outf(out, "Assignee:  %s\n", assignee(t))
	outf(out, "Priority:  %s\n", orDash(string(t.Spec.Priority)))
	if t.Spec.ParentRef != nil {
		outf(out, "Parent:    %s\n", t.Spec.ParentRef.Name)
	}
	if r := t.Status.Run; r != nil {
		outf(out, "Run:       %s on %s, lease until %s\n", r.ID, orDash(r.Runner), r.LeaseUntil.Format("15:04:05"))
	}
	if len(t.Status.Outbox) > 0 {
		outf(out, "Outbox:    %d pending\n", len(t.Status.Outbox))
	}
	if t.Spec.Description != "" {
		outf(out, "\n%s\n", strings.TrimSpace(t.Spec.Description))
	}
}

func newTaskComment(g *globals) *cobra.Command {
	var msg string
	cmd := &cobra.Command{
		Use:   "comment NAME|#NUMBER",
		Short: "Write to a task's thread",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := g.client()
			name, err := resolve(cmd.Context(), c, g.namespace, args[0])
			if err != nil {
				return err
			}
			entry, err := c.Comment(cmd.Context(), g.namespace, name, g.as, msg)
			if err != nil {
				return err
			}
			outf(cmd.OutOrStdout(), "comment %d on %s\n", entry.Seq, name)
			return nil
		},
	}
	cmd.Flags().StringVarP(&msg, "message", "m", "", "the comment")
	_ = cmd.MarkFlagRequired("message")
	return cmd
}

func newTaskReply(g *globals) *cobra.Command {
	var msg string
	cmd := &cobra.Command{
		Use:   "reply NAME|#NUMBER",
		Short: "Answer a waiting task and send it back to the agent",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := g.client()
			name, err := resolve(cmd.Context(), c, g.namespace, args[0])
			if err != nil {
				return err
			}
			res, err := c.Reply(cmd.Context(), g.namespace, name, g.as, msg)
			if client.IsCode(err, httpjson.CodeInvalidTransition) {
				if t, gerr := c.GetTask(cmd.Context(), g.namespace, name); gerr == nil && t.Status.Waiting != nil &&
					t.Status.Waiting.Reason == v1alpha1.WaitingToolApproval {
					return fmt.Errorf("%s is waiting for a tool approval; use approve or deny instead", name)
				}
				return fmt.Errorf("%s is not waiting for an answer; use comment instead", name)
			}
			if err != nil {
				return err
			}
			outf(cmd.OutOrStdout(), "#%d is %s\n", res.Task.Status.Number, res.Task.Status.Phase)
			return nil
		},
	}
	cmd.Flags().StringVarP(&msg, "message", "m", "", "the answer")
	_ = cmd.MarkFlagRequired("message")
	return cmd
}

// maxShownArguments is how much of a call's arguments "task get" shows.
const maxShownArguments = 2000

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return runes.Cut(s, n) + "…"
}

// newTaskDecide is "task approve" or, with approve false, "task deny".
func newTaskDecide(g *globals, approve bool) *cobra.Command {
	use, short := "approve", "Approve the tool call a task waits on: Cadre makes the call, then the agent continues"
	if !approve {
		use, short = "deny", "Deny the tool call a task waits on: the agent continues without it"
	}
	var note string
	cmd := &cobra.Command{
		Use:   use + " NAME|#NUMBER",
		Short: short,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := g.client()
			name, err := resolve(cmd.Context(), c, g.namespace, args[0])
			if err != nil {
				return err
			}
			res, err := c.Approve(cmd.Context(), g.namespace, name, g.as, approve, note)
			switch {
			case client.IsCode(err, httpjson.CodeInvalidTransition):
				return fmt.Errorf("%s is not waiting for a tool approval, or its call was already decided otherwise", name)
			case err != nil:
				return err
			}
			out := cmd.OutOrStdout()
			if r := res.Result; r != nil {
				status := "returned"
				if r.Call != nil && r.Call.IsError {
					status = "failed"
				}
				outf(out, "The call %s: %s\n", status, clip(r.Body, maxShownArguments))
			}
			outf(out, "#%d is %s\n", res.Task.Status.Number, res.Task.Status.Phase)
			return nil
		},
	}
	cmd.Flags().StringVarP(&note, "message", "m", "", "a note for the agent")
	return cmd
}

func newTaskCancel(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "cancel NAME|#NUMBER",
		Short: "Cancel a task",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := g.client()
			name, err := resolve(cmd.Context(), c, g.namespace, args[0])
			if err != nil {
				return err
			}
			t, err := c.CancelTask(cmd.Context(), g.namespace, name)
			if err != nil {
				return err
			}
			outf(cmd.OutOrStdout(), "#%d is %s\n", t.Status.Number, t.Status.Phase)
			return nil
		},
	}
}

// resolve turns "#123" into a task name by scanning the board, a page at a
// time with the page size the server applies; anything else is a name
// already. Unnumbered tasks have number 0, so #0 names nothing.
func resolve(ctx context.Context, c *client.Client, ns, ref string) (string, error) {
	digits, ok := strings.CutPrefix(ref, "#")
	if !ok {
		return ref, nil
	}
	n, err := strconv.ParseInt(digits, 10, 64)
	if err != nil || n < 1 {
		return "", fmt.Errorf("%q is not a task number", ref)
	}
	for offset := 0; ; {
		list, err := c.ListTasks(ctx, ns, nil, offset, 0)
		if err != nil {
			return "", err
		}
		for _, t := range list.Items {
			if t.Status.Number == n {
				return t.Name, nil
			}
		}
		if list.Limit == 0 || len(list.Items) < list.Limit {
			return "", fmt.Errorf("no task #%d in %s", n, ns)
		}
		offset += len(list.Items)
	}
}

func phaseOf(t *v1alpha1.Task) string {
	if t.Status.Phase == "" {
		return "-" // not numbered yet
	}
	return string(t.Status.Phase)
}

func assignee(t *v1alpha1.Task) string {
	if a := t.Spec.Assignee; a != nil {
		return strings.ToLower(string(a.Kind)) + "/" + a.Name
	}
	return "-"
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
