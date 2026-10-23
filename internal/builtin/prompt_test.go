package builtin

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/j75689/cadre/internal/manager"
	"github.com/j75689/cadre/internal/runner"
)

func TestFirstPromptShowsCallsClipped(t *testing.T) {
	at := time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC)
	agent := manager.Author{Kind: manager.AuthorAgent, Name: "coder"}
	big := strings.Repeat("é", maxShown) // twice maxShown bytes
	args, _ := json.Marshal(map[string]string{"title": "fix login"})
	thread := []manager.ThreadEntry{
		{Seq: 1, Author: agent, Kind: manager.EntryToolCall, Body: "Calls github__create_pull_request", At: at,
			Call: &manager.ToolCall{ID: "c1", Name: "github__create_pull_request", Arguments: args}},
		{Seq: 2, Author: manager.Author{Kind: manager.AuthorUser, Name: "alice"}, Kind: manager.EntryApproved, Body: "go ahead", At: at, Call: &manager.ToolCall{ID: "c1"}},
		{Seq: 3, Author: manager.Author{Kind: manager.AuthorSystem, Name: "cadre"}, Kind: manager.EntryToolResult, Body: big, At: at, Call: &manager.ToolCall{ID: "c1", IsError: true}},
	}
	files := []manager.FileInfo{{Path: "notes.md", Version: 2, Size: 10}}
	p := firstPrompt(runner.ClaimedTask{Number: 7, Title: "Open a PR", Labels: []string{"backend"}}, thread, files)
	for _, want := range []string{
		"# Task #7: Open a PR", "Labels: backend",
		`Calls github__create_pull_request (call c1) with arguments:` + "\n" + `{"title":"fix login"}`,
		"Call c1: go ahead", "Call c1 failed:", "[clipped: 8192 of 16384 bytes shown",
		"- notes.md (version 2, 10 bytes)",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt lacks %q", want)
		}
	}
	if !utf8.ValidString(p) || len(p) > 3*maxShown {
		t.Fatalf("prompt: %d bytes, valid UTF-8 %v", len(p), utf8.ValidString(p))
	}
}

func TestSystemPrompt(t *testing.T) {
	if got := systemPrompt(&runner.ClaimedAgent{Instructions: "Be terse."}); !strings.HasPrefix(got, preamble) || !strings.HasSuffix(got, "\n\nBe terse.") {
		t.Fatalf("system prompt: %q", got)
	}
	if got := systemPrompt(nil); got != preamble {
		t.Fatalf("without instructions: %q", got)
	}
}
