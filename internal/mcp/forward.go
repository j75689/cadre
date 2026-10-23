package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"sync"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	"github.com/j75689/cadre/internal/storage"
)

// Sources gives the endpoint what it needs to forward an agent's
// ToolServers: fresh reads of the Agent and its ToolServers, and of the
// Secret a ToolServer authenticates with. Secret values are read per call
// and never cached or logged.
type Sources interface {
	Agent(ctx context.Context, namespace, name string) (*v1alpha1.Agent, error)
	ToolServer(ctx context.Context, namespace, name string) (*v1alpha1.ToolServer, error)
	SecretValue(ctx context.Context, namespace string, ref v1alpha1.SecretKeyRef) (string, error)
}

// Forwarding limits.
const (
	discoveryTTL   = time.Minute
	forwardTimeout = 60 * time.Second
	maxResultBytes = 1 << 20
	maxToolName    = 64
)

// toolName is the model APIs' rule for tool names.
var toolName = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

var unsafeChars = regexp.MustCompile(`[^a-zA-Z0-9_-]`)

// exposedName is how a ToolServer's tool is named to the agent:
// <toolserver>__<tool>, the tool name reduced to the model APIs' characters.
// ToolServer names cannot contain "_", so no exposed name collides with a
// core tool.
func exposedName(server, tool string) string {
	return server + "__" + unsafeChars.ReplaceAllString(tool, "_")
}

// permissionFor applies a ToolServer's permissions to one tool: without a
// list every tool is allowed; with one, the tool's own entry, else "*",
// else deny.
func permissionFor(perms map[string]v1alpha1.Permission, tool string) v1alpha1.Permission {
	if len(perms) == 0 {
		return v1alpha1.PermissionAllow
	}
	if p, ok := perms[tool]; ok {
		return p
	}
	if p, ok := perms["*"]; ok {
		return p
	}
	return v1alpha1.PermissionDeny
}

// discovery caches each ToolServer's tool list per generation for a minute,
// so a stateless endpoint does not list a server's tools on every request.
type discovery struct {
	mu    sync.Mutex
	lists map[string]discovered
}

type discovered struct {
	tools []*sdk.Tool
	at    time.Time
}

// forwarded is one forwarded tool, as registered for a request.
type forwarded struct {
	server *v1alpha1.ToolServer
	remote *sdk.Tool
}

// forwardedTools returns the agent's ToolServers' tools this run may call.
// A ToolServer that is missing, not Ready, managed or unreachable
// contributes none; that is logged, never fatal for the core tools. In M1
// tools that must ask a human first are not offered.
func (s *Server) forwardedTools(ctx context.Context, t *v1alpha1.Task) map[string]forwarded {
	out := map[string]forwarded{}
	if s.sources == nil || t.Spec.Assignee == nil {
		return out
	}
	agent, err := s.sources.Agent(ctx, t.Namespace, t.Spec.Assignee.Name)
	if err != nil {
		s.log.Warn("forward tools: read agent", "task", t.Namespace+"/"+t.Name, "err", err)
		return out
	}
	for _, ref := range agent.Spec.ToolRefs {
		ts, err := s.sources.ToolServer(ctx, t.Namespace, ref)
		if err != nil || ts.Spec.URL == "" || !ts.Status.ReadyFor(ts.Generation) {
			if err != nil && !isNotFound(err) {
				s.log.Warn("forward tools: read ToolServer", "toolServer", t.Namespace+"/"+ref, "err", err)
			}
			continue
		}
		for _, tool := range s.discover(ctx, ts) {
			if permissionFor(ts.Spec.Permissions, tool.Name) != v1alpha1.PermissionAllow {
				continue
			}
			name := exposedName(ts.Name, tool.Name)
			if len(name) > maxToolName || !toolName.MatchString(name) {
				s.log.Warn("forward tools: name too long, skipped", "toolServer", ts.Name, "tool", tool.Name)
				continue
			}
			if _, dup := out[name]; dup {
				s.log.Warn("forward tools: name collision, skipped", "toolServer", ts.Name, "tool", tool.Name, "as", name)
				delete(out, name) // neither is offered: which one a call means would be a guess
				continue
			}
			out[name] = forwarded{server: ts, remote: tool}
		}
	}
	return out
}

func isNotFound(err error) bool { return errors.Is(err, storage.ErrNotFound) }

// discover returns a ToolServer's tools, from the cache when fresh.
func (s *Server) discover(ctx context.Context, ts *v1alpha1.ToolServer) []*sdk.Tool {
	key := fmt.Sprintf("%s/%s/%d", ts.Namespace, ts.Name, ts.Generation)
	s.discovery.mu.Lock()
	d, ok := s.discovery.lists[key]
	s.discovery.mu.Unlock()
	if ok && time.Since(d.at) < discoveryTTL {
		return d.tools
	}
	tools, err := s.listRemote(ctx, ts)
	if err != nil {
		s.log.Warn("forward tools: list tools", "toolServer", ts.Namespace+"/"+ts.Name, "err", err)
		return nil
	}
	s.discovery.mu.Lock()
	if s.discovery.lists == nil {
		s.discovery.lists = map[string]discovered{}
	}
	s.discovery.lists[key] = discovered{tools: tools, at: time.Now()}
	s.discovery.mu.Unlock()
	return tools
}

func (s *Server) listRemote(ctx context.Context, ts *v1alpha1.ToolServer) ([]*sdk.Tool, error) {
	ctx, cancel := context.WithTimeout(ctx, forwardTimeout)
	defer cancel()
	session, err := s.connect(ctx, ts)
	if err != nil {
		return nil, err
	}
	defer func() { _ = session.Close() }()
	var tools []*sdk.Tool
	for tool, err := range session.Tools(ctx, nil) {
		if err != nil {
			return nil, err
		}
		tools = append(tools, tool)
	}
	return tools, nil
}

// connect opens an MCP session on a ToolServer, with its credential read
// fresh from its Secret.
func (s *Server) connect(ctx context.Context, ts *v1alpha1.ToolServer) (*sdk.ClientSession, error) {
	transport := http.DefaultTransport
	if a := ts.Spec.Auth; a != nil {
		value, err := s.sources.SecretValue(ctx, ts.Namespace, a.SecretRef)
		if err != nil {
			return nil, fmt.Errorf("read the ToolServer's credential: %w", err)
		}
		header, prefix := "Authorization", "Bearer "
		if a.Header != "" {
			header = a.Header
		}
		if a.Prefix != nil {
			prefix = *a.Prefix
		}
		transport = credential{header: header, value: prefix + value, next: transport}
	}
	client := sdk.NewClient(&sdk.Implementation{Name: "cadre", Version: v1alpha1.GroupVersion.Version}, nil)
	return client.Connect(ctx, &sdk.StreamableClientTransport{
		Endpoint: ts.Spec.URL, HTTPClient: &http.Client{Transport: transport}, MaxRetries: -1,
	}, nil)
}

// credential adds a ToolServer's credential to every request.
type credential struct {
	header, value string
	next          http.RoundTripper
}

func (c credential) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set(c.header, c.value)
	return c.next.RoundTrip(r)
}

// addForwarded registers the run's forwarded tools on server.
func (s *Server) addForwarded(server *sdk.Server, t *v1alpha1.Task, tools map[string]forwarded) {
	runID := t.Status.Run.ID
	key := storage.Key{Namespace: t.Namespace, Name: t.Name}
	for name, f := range tools {
		schema := f.remote.InputSchema
		if schema == nil {
			schema = map[string]any{"type": "object"}
		}
		server.AddTool(&sdk.Tool{Name: name, Description: f.remote.Description, InputSchema: schema},
			func(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
				if err := s.m.CheckRun(ctx, key, runID); err != nil {
					return s.fail(err), nil
				}
				return s.forward(ctx, f, req.Params.Arguments), nil
			})
	}
}

// forward makes the call on the ToolServer and shapes its result for the
// model: text and structured content up to 1 MiB; images and other media
// are dropped with a note in M1.
func (s *Server) forward(ctx context.Context, f forwarded, args json.RawMessage) *sdk.CallToolResult {
	ctx, cancel := context.WithTimeout(ctx, forwardTimeout)
	defer cancel()
	session, err := s.connect(ctx, f.server)
	if err != nil {
		s.log.Warn("forward call: connect", "toolServer", f.server.Name, "err", err)
		return toolError("The tool's server could not be reached; try again later.")
	}
	defer func() { _ = session.Close() }()
	var arguments any = map[string]any{}
	if len(args) > 0 {
		arguments = args
	}
	res, err := session.CallTool(ctx, &sdk.CallToolParams{Name: f.remote.Name, Arguments: arguments})
	if err != nil {
		s.log.Warn("forward call", "toolServer", f.server.Name, "tool", f.remote.Name, "err", err)
		return toolError("The tool call failed: " + err.Error())
	}
	return shapeResult(res)
}

func toolError(msg string) *sdk.CallToolResult {
	r := &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: msg}}}
	r.IsError = true
	return r
}

// shapeResult keeps a forwarded result within what M1 hands a model.
func shapeResult(res *sdk.CallToolResult) *sdk.CallToolResult {
	out := &sdk.CallToolResult{IsError: res.IsError}
	budget := maxResultBytes
	var notes []string
	for _, c := range res.Content {
		tc, ok := c.(*sdk.TextContent)
		if !ok {
			notes = append(notes, fmt.Sprintf("[a %T was dropped: only text is forwarded]", c))
			continue
		}
		text := tc.Text
		if len(text) > budget {
			text = text[:budget]
			notes = append(notes, fmt.Sprintf("[the result was truncated at %d bytes]", maxResultBytes))
		}
		budget -= len(text)
		out.Content = append(out.Content, &sdk.TextContent{Text: text})
	}
	if res.StructuredContent != nil {
		if data, err := json.Marshal(res.StructuredContent); err == nil && len(data) <= maxResultBytes {
			out.StructuredContent = res.StructuredContent
		} else {
			notes = append(notes, "[the structured result was dropped: over the size limit]")
		}
	}
	for _, n := range notes {
		out.Content = append(out.Content, &sdk.TextContent{Text: n})
	}
	return out
}
