package builtin

import (
	"fmt"
	"strings"

	"github.com/j75689/cadre/internal/manager"
	"github.com/j75689/cadre/internal/runner"
)

// preamble comes before the agent's instructions in every run's system
// prompt. It is fixed text, so the prompt cache holds across runs.
const preamble = `You are an agent working on one task of a team's board in Cadre. The first message gives you the task and its thread so far: comments from humans and agents, questions and answers, and tool calls with their approvals and results.

Work on the task with your tools. Post short progress notes with thread_post when it helps the humans following the task, and keep your work in task files (task_files_write) when it is longer than a comment. Split off work that belongs in its own task with task_create_subtask.

When you need a human's decision, ask with thread_ask and stop: the run ends when the question is posted, and a new run continues once a human answers. A tool that needs a human's approval ends the run the same way. When the task is done, reply with a short summary of what you did; it is posted to the thread and the task goes to review.`

// systemPrompt is the run's system prompt.
func systemPrompt(agent *runner.ClaimedAgent) string {
	if agent == nil || strings.TrimSpace(agent.Instructions) == "" {
		return preamble
	}
	return preamble + "\n\n" + agent.Instructions
}

// maxShown bounds what the prompt shows of one call's arguments or result,
// so a huge result cannot fill the context before the run starts.
const maxShown = 8 << 10

// clip cuts s to maxShown bytes on a rune boundary, with a note.
func clip(s string) string {
	if len(s) <= maxShown {
		return s
	}
	cut := maxShown
	for cut > 0 && (s[cut]&0xC0) == 0x80 {
		cut--
	}
	return s[:cut] + fmt.Sprintf("\n[clipped: %d of %d bytes shown; thread_read has the whole entry]", cut, len(s))
}

// defaultPromptBudget bounds the first message when the model's context
// window is unknown.
const defaultPromptBudget = 200 << 10

// promptBudget is how many bytes the first message may take: about a
// quarter of a window of window tokens (at some four bytes a token), or
// defaultPromptBudget when the window is unknown.
func promptBudget(window int64) int {
	if window <= 0 {
		return defaultPromptBudget
	}
	return int(min(window, 1<<30))
}

// firstPrompt renders the task, its thread oldest first, and its files, in
// about budget bytes. When the thread does not fit, the newest entries are
// kept, and a note says how many older ones thread_read still has.
func firstPrompt(t runner.ClaimedTask, thread []manager.ThreadEntry, files []manager.FileInfo, budget int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Task #%d: %s\n", t.Number, t.Title)
	if t.Priority != "" {
		fmt.Fprintf(&b, "Priority: %s\n", t.Priority)
	}
	if len(t.Labels) > 0 {
		fmt.Fprintf(&b, "Labels: %s\n", strings.Join(t.Labels, ", "))
	}
	if t.ParentRef != nil {
		fmt.Fprintf(&b, "Subtask of: %s\n", t.ParentRef.Name)
	}
	if d := strings.TrimSpace(t.Description); d != "" {
		fmt.Fprintf(&b, "\n%s\n", clip(d))
	}
	head := b.String()
	tail := filesSection(files)

	entries := make([]string, len(thread))
	for i, e := range thread {
		entries[i] = entry(e)
	}
	left := budget - len(head) - len(tail) - 200 // the section heading and the note
	first := len(entries)
	for first > 0 && len(entries[first-1]) <= left {
		first--
		left -= len(entries[first])
	}
	b.WriteString("\n## Thread\n")
	switch {
	case len(thread) == 0:
		b.WriteString("\nThe thread is empty.\n")
	case first > 0:
		fmt.Fprintf(&b, "\nThe %d oldest entries, up to %d, are left out to keep this message short; read them with thread_read.\n", first, thread[first-1].Seq)
	}
	for _, e := range entries[first:] {
		b.WriteString(e)
	}
	b.WriteString(tail)
	return b.String()
}

// entry renders one thread entry.
func entry(e manager.ThreadEntry) string {
	var b strings.Builder
	fmt.Fprintf(&b, "\n### %d · %s · %s %s · %s\n", e.Seq, e.Kind, strings.ToLower(string(e.Author.Kind)), e.Author.Name, e.At.Format("2006-01-02 15:04 UTC"))
	switch {
	case e.Kind == manager.EntryToolCall && e.Call != nil:
		fmt.Fprintf(&b, "Calls %s (call %s) with arguments:\n%s\n", e.Call.Name, e.Call.ID, clip(string(e.Call.Arguments)))
	case e.Kind == manager.EntryToolResult && e.Call != nil:
		status := "returned"
		if e.Call.IsError {
			status = "failed"
		}
		fmt.Fprintf(&b, "Call %s %s:\n%s\n", e.Call.ID, status, clip(e.Body))
	case (e.Kind == manager.EntryApproved || e.Kind == manager.EntryDenied) && e.Call != nil:
		fmt.Fprintf(&b, "Call %s: %s\n", e.Call.ID, clip(e.Body))
	default:
		fmt.Fprintf(&b, "%s\n", clip(e.Body))
	}
	return b.String()
}

// filesSection renders the task's files.
func filesSection(files []manager.FileInfo) string {
	var b strings.Builder
	b.WriteString("\n## Files\n")
	if len(files) == 0 {
		b.WriteString("\nThe task has no files yet.\n")
	} else {
		b.WriteString("\n")
		for _, f := range files {
			fmt.Fprintf(&b, "- %s (version %d, %d bytes)\n", f.Path, f.Version, f.Size)
		}
	}
	return b.String()
}
