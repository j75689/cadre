package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	"github.com/j75689/cadre/internal/manager"
	"github.com/j75689/cadre/internal/mcp"
	"github.com/j75689/cadre/internal/runner"
)

// session is the run's MCP session on its per-run endpoint. The token
// travels in a header, never in the URL.
type session struct {
	*sdk.ClientSession
}

func connect(ctx context.Context, m *runner.ClaimedMCP, transport http.RoundTripper) (*session, error) {
	if m == nil || m.URL == "" || m.Token == "" {
		return nil, fmt.Errorf("the claim has no MCP endpoint")
	}
	if transport == nil {
		transport = http.DefaultTransport
	}
	client := sdk.NewClient(&sdk.Implementation{Name: "cadre-runner", Version: v1alpha1.GroupVersion.Version}, nil)
	s, err := client.Connect(ctx, &sdk.StreamableClientTransport{
		Endpoint:   m.URL,
		HTTPClient: &http.Client{Transport: bearer{token: m.Token, next: transport}},
		MaxRetries: -1,
	}, nil)
	if err != nil {
		return nil, err
	}
	return &session{s}, nil
}

type bearer struct {
	token string
	next  http.RoundTripper
}

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return b.next.RoundTrip(r)
}

// tools lists the run's tools, sorted by name, so the request prefix is
// the same on every turn and the prompt cache holds.
func (s *session) tools(ctx context.Context) ([]Tool, error) {
	var out []Tool
	for t, err := range s.Tools(ctx, nil) {
		if err != nil {
			return nil, err
		}
		schema, err := json.Marshal(t.InputSchema)
		if err != nil || t.InputSchema == nil {
			schema = []byte(`{"type":"object"}`)
		}
		out = append(out, Tool{Name: t.Name, Description: t.Description, InputSchema: schema})
	}
	slices.SortFunc(out, func(a, b Tool) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

// result is a tool call's outcome: what the model reads, and whether the
// server ended the run with it.
type result struct {
	ToolResult
	runEnded bool
}

// call makes a tool call. A call the endpoint could not serve is an error
// result for the model, not a failure of the run.
func (s *session) call(ctx context.Context, c ToolCall) (result, error) {
	var args any = map[string]any{}
	if len(c.Input) > 0 {
		args = c.Input
	}
	res, err := s.CallTool(ctx, &sdk.CallToolParams{Name: c.Name, Arguments: args})
	if err != nil {
		if ctx.Err() != nil {
			return result{}, ctx.Err()
		}
		return result{ToolResult: ToolResult{CallID: c.ID, Text: "The call failed: " + err.Error(), IsError: true}}, nil
	}
	return result{ToolResult: ToolResult{CallID: c.ID, Text: resultText(res), IsError: res.IsError}, runEnded: res.Meta[mcp.MetaRunEnded] == true}, nil
}

// resultText is a result's text parts, or its structured content as JSON
// when it has no text.
func resultText(res *sdk.CallToolResult) string {
	var parts []string
	for _, c := range res.Content {
		if t, ok := c.(*sdk.TextContent); ok {
			parts = append(parts, t.Text)
		}
	}
	if len(parts) == 0 && res.StructuredContent != nil {
		if data, err := json.Marshal(res.StructuredContent); err == nil {
			return string(data)
		}
	}
	if len(parts) == 0 {
		return "(no output)"
	}
	return strings.Join(parts, "\n")
}

// structured calls a core tool and decodes its structured result.
func (s *session) structured(ctx context.Context, name string, args, out any) error {
	res, err := s.CallTool(ctx, &sdk.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		return err
	}
	if res.IsError {
		return fmt.Errorf("%s: %s", name, resultText(res))
	}
	data, err := json.Marshal(res.StructuredContent)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, out)
}

// thread reads the whole thread, oldest first, a page at a time.
func (s *session) thread(ctx context.Context) ([]manager.ThreadEntry, error) {
	var all []manager.ThreadEntry
	var after int64
	for {
		var page struct {
			Entries []manager.ThreadEntry `json:"entries"`
		}
		if err := s.structured(ctx, "thread_read", map[string]any{"after": after}, &page); err != nil {
			return nil, err
		}
		all = append(all, page.Entries...)
		if len(page.Entries) < manager.MaxThreadPage {
			return all, nil
		}
		after = page.Entries[len(page.Entries)-1].Seq
	}
}

func (s *session) files(ctx context.Context) ([]manager.FileInfo, error) {
	var out struct {
		Files []manager.FileInfo `json:"files"`
	}
	err := s.structured(ctx, "task_files_list", map[string]any{}, &out)
	return out.Files, err
}

// post writes a comment to the thread.
func (s *session) post(ctx context.Context, body string) error {
	var out struct{ Seq int64 }
	return s.structured(ctx, "thread_post", map[string]any{"body": body}, &out)
}
