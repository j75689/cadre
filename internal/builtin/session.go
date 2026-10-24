package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	"github.com/j75689/cadre/internal/manager"
	"github.com/j75689/cadre/internal/mcp"
	"github.com/j75689/cadre/internal/runner"
)

// session is the run's MCP session on its per-run endpoint. The token
// travels in a header, never in the URL, and only to the endpoint's host.
type session struct {
	*sdk.ClientSession
	timeout time.Duration
	status  *lastStatus
	// callIDs sends each model call's id with it, in _meta, as a step-mode
	// worker does: the server derives what the call records from it.
	callIDs bool
}

// errRunGone is the endpoint answering 404: the run's token no longer
// names an active run, because the run lost its task.
var errRunGone = errors.New("the run's MCP endpoint no longer knows the run")

// lastStatus notes whether the endpoint has answered 404, which is how it
// turns away a token whose run is over.
type lastStatus struct {
	gone atomic.Bool
	next http.RoundTripper
}

func (l *lastStatus) RoundTrip(r *http.Request) (*http.Response, error) {
	res, err := l.next.RoundTrip(r)
	if err == nil && res.StatusCode == http.StatusNotFound {
		l.gone.Store(true)
	}
	return res, err
}

// errTransport marks a request to the run's endpoint that did not get an
// answer: the endpoint is down, or did not answer within the call timeout.
// It is not the task's fault, and what it did is unknown.
var errTransport = errors.New("the run's MCP endpoint did not answer")

func connect(ctx context.Context, m *runner.ClaimedMCP, transport http.RoundTripper, timeout time.Duration) (*session, error) {
	if m == nil || m.URL == "" || m.Token == "" {
		return nil, fmt.Errorf("the claim has no MCP endpoint")
	}
	endpoint, err := url.Parse(m.URL)
	if err != nil {
		return nil, fmt.Errorf("MCP endpoint: %w", err)
	}
	if transport == nil {
		transport = http.DefaultTransport
	}
	status := &lastStatus{next: transport}
	client := sdk.NewClient(&sdk.Implementation{Name: "cadre-runner", Version: v1alpha1.GroupVersion.Version}, nil)
	connectCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	s, err := client.Connect(connectCtx, &sdk.StreamableClientTransport{
		Endpoint:   m.URL,
		HTTPClient: mcp.NoRedirects(mcp.Credential(endpoint.Host, "Authorization", "Bearer "+m.Token, status)),
		MaxRetries: -1,
	}, nil)
	if err != nil {
		if status.gone.Load() {
			return nil, errRunGone
		}
		return nil, err
	}
	return &session{ClientSession: s, timeout: timeout, status: status}, nil
}

// callTool makes one request, bounded by the session's timeout. An error
// the endpoint answered (an unknown tool, a malformed call) comes back as
// it is; one it did not answer is errTransport.
func (s *session) callTool(ctx context.Context, name string, args any, meta sdk.Meta) (*sdk.CallToolResult, error) {
	callCtx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	res, err := s.CallTool(callCtx, &sdk.CallToolParams{Name: name, Arguments: args, Meta: meta})
	switch {
	case err == nil, ctx.Err() != nil:
		return res, err
	case s.status.gone.Load():
		return nil, errRunGone
	case answered(err):
		return nil, err
	default:
		return nil, fmt.Errorf("%w: %s: %w", errTransport, name, err)
	}
}

// codeRejected is the SDK's own error for a request that got no usable
// answer; any other JSON-RPC error came from the server.
const codeRejected = -32005

// answered reports whether err carries a JSON-RPC error the server sent.
func answered(err error) bool {
	var wire *jsonrpc.Error
	if errors.As(err, &wire) && wire.Code != codeRejected {
		return true
	}
	switch u := err.(type) { //nolint:errorlint // it walks the wrap tree itself, to look past the SDK's own rejection
	case interface{ Unwrap() []error }:
		for _, e := range u.Unwrap() {
			if answered(e) {
				return true
			}
		}
	case interface{ Unwrap() error }:
		return answered(u.Unwrap())
	}
	return false
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

// call makes a tool call. A call the endpoint refused (an unknown tool,
// say) is an error result for the model; one it did not answer fails with
// errTransport, and the run stops.
func (s *session) call(ctx context.Context, c ToolCall) (result, error) {
	var args any = map[string]any{}
	if len(c.Input) > 0 {
		args = c.Input
	}
	var meta sdk.Meta
	if s.callIDs {
		meta = sdk.Meta{mcp.MetaCallID: c.ID}
	}
	res, err := s.callTool(ctx, c.Name, args, meta)
	if err != nil {
		if ctx.Err() != nil || errors.Is(err, errTransport) || errors.Is(err, errRunGone) {
			return result{}, err
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
	res, err := s.callTool(ctx, name, args, nil)
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

// thread reads the whole thread, oldest first, a page at a time until a
// page comes back empty, whatever page size the server keeps to.
func (s *session) thread(ctx context.Context) ([]manager.ThreadEntry, error) {
	var all []manager.ThreadEntry
	var after int64
	for {
		var page struct {
			Entries []manager.ThreadEntry `json:"entries"`
		}
		if err := s.structured(ctx, "thread_read", map[string]any{"after": after, "limit": manager.MaxThreadPage}, &page); err != nil {
			return nil, err
		}
		if len(page.Entries) == 0 {
			return all, nil
		}
		all = append(all, page.Entries...)
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
