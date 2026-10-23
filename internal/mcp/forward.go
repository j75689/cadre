package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	"github.com/j75689/cadre/internal/manager"
	"github.com/j75689/cadre/internal/storage"
)

// Sources gives the endpoint what it needs to forward an agent's
// ToolServers. The tool list is built from cached reads of the Agent and its
// ToolServers, which may be a little stale; a call reads its ToolServer
// fresh, and the Secret it authenticates with. Secret values are read per
// call and never cached or logged.
type Sources interface {
	CachedAgent(ctx context.Context, namespace, name string) (*v1alpha1.Agent, error)
	CachedToolServer(ctx context.Context, namespace, name string) (*v1alpha1.ToolServer, error)
	ToolServer(ctx context.Context, namespace, name string) (*v1alpha1.ToolServer, error)
	SecretValue(ctx context.Context, namespace string, ref v1alpha1.SecretKeyRef) (string, error)
}

// Forwarding limits. Listing a run's tools is bounded by
// discoveryTimeout in all, so a ToolServer that does not answer delays a
// request by at most that, and a failed listing is remembered for
// failureTTL so it does not delay the next ones.
const (
	discoveryTTL   = time.Minute
	failureTTL     = 15 * time.Second
	forwardTimeout = 60 * time.Second
	maxResultBytes = 1 << 20
	maxToolName    = 64
)

var discoveryTimeout = 10 * time.Second // a variable for tests

// toolName is the model APIs' rule for tool names.
var toolName = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

var unsafeChars = regexp.MustCompile(`[^a-zA-Z0-9_-]`)

// exposedName is how a ToolServer's tool is named to the agent:
// <toolserver>__<tool>, both reduced to the model APIs' characters (a
// ToolServer's "." becomes "_"). ToolServer names cannot contain "_", so
// distinct ToolServers keep distinct prefixes and no exposed name collides
// with a core tool.
func exposedName(server, tool string) string {
	return unsafeChars.ReplaceAllString(server, "_") + "__" + unsafeChars.ReplaceAllString(tool, "_")
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

// discovery caches each ToolServer's tool list for a minute, and a failure
// to list for failureTTL, so a stateless endpoint does not list a server's
// tools on every request. An entry holds for one generation of the
// ToolServer.
type discovery struct {
	mu    sync.Mutex
	lists map[string]discovered // by namespace/name
}

type discovered struct {
	generation int64
	tools      []*sdk.Tool
	failed     bool
	at         time.Time
}

func (d discovered) fresh(generation int64, now time.Time) bool {
	ttl := discoveryTTL
	if d.failed {
		ttl = failureTTL
	}
	return d.generation == generation && now.Sub(d.at) < ttl
}

// forwarded is one forwarded tool, as registered for a request.
type forwarded struct {
	server *v1alpha1.ToolServer
	remote *sdk.Tool
	schema map[string]any // the remote input schema, checked
	ask    bool           // the call must ask a human first
}

// Forwarder reaches an agent's ToolServers: it lists their tools for the
// per-run endpoint and makes the calls, both those a run makes and those a
// human approves.
type Forwarder struct {
	sources   Sources
	log       *slog.Logger
	discovery discovery
}

// NewForwarder returns a Forwarder reading Agents, ToolServers and their
// credentials from src.
func NewForwarder(src Sources, log *slog.Logger) *Forwarder {
	return &Forwarder{sources: src, log: log}
}

// forwardedTools returns the agent's ToolServers' tools this run may call.
// A ToolServer that is missing, not Ready, managed or unreachable
// contributes none; that is logged, never fatal for the core tools. The
// ToolServers are listed concurrently, within discoveryTimeout in all.
func (f *Forwarder) forwardedTools(ctx context.Context, t *v1alpha1.Task) map[string]forwarded {
	out := map[string]forwarded{}
	if f.sources == nil || t.Spec.Assignee == nil {
		return out
	}
	agent, err := f.sources.CachedAgent(ctx, t.Namespace, t.Spec.Assignee.Name)
	if err != nil {
		f.log.Warn("forward tools: read agent", "task", t.Namespace+"/"+t.Name, "err", err)
		return out
	}
	ctx, cancel := context.WithTimeout(ctx, discoveryTimeout)
	defer cancel()
	type listed struct {
		ts    *v1alpha1.ToolServer
		tools []*sdk.Tool
	}
	lists := make([]listed, len(agent.Spec.ToolRefs))
	var wg sync.WaitGroup
	for i, ref := range agent.Spec.ToolRefs {
		ts, err := f.sources.CachedToolServer(ctx, t.Namespace, ref)
		if err != nil || ts.Spec.URL == "" || !ts.Status.ReadyFor(ts.Generation) {
			if err != nil && !isNotFound(err) {
				f.log.Warn("forward tools: read ToolServer", "toolServer", t.Namespace+"/"+ref, "err", err)
			}
			continue
		}
		wg.Go(func() { lists[i] = listed{ts: ts, tools: f.discover(ctx, ts)} })
	}
	wg.Wait()

	collided := map[string]bool{}
	for _, l := range lists {
		for _, tool := range l.tools {
			perm := permissionFor(l.ts.Spec.Permissions, tool.Name)
			if perm == v1alpha1.PermissionDeny {
				continue
			}
			name := exposedName(l.ts.Name, tool.Name)
			switch {
			case len(name) > maxToolName || !toolName.MatchString(name):
				f.log.Warn("forward tools: exposed name over 64 characters, skipped", "toolServer", l.ts.Name, "tool", tool.Name)
				continue
			case collided[name]:
				continue
			}
			if _, dup := out[name]; dup {
				// Neither is offered: which one a call means would be a guess.
				f.log.Warn("forward tools: name collision, skipped", "toolServer", l.ts.Name, "tool", tool.Name, "as", name)
				delete(out, name)
				collided[name] = true
				continue
			}
			schema, err := objectSchema(tool.InputSchema)
			if err != nil {
				f.log.Warn("forward tools: unusable input schema, skipped", "toolServer", l.ts.Name, "tool", tool.Name, "err", err)
				continue
			}
			out[name] = forwarded{server: l.ts, remote: tool, schema: schema, ask: perm == v1alpha1.PermissionAsk}
		}
	}
	return out
}

// objectSchema checks that a remote tool's input schema is what MCP
// requires, a JSON object schema of type "object"; a tool without one takes
// any object.
func objectSchema(schema any) (map[string]any, error) {
	if schema == nil {
		return map[string]any{"type": "object"}, nil
	}
	data, err := json.Marshal(schema)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("not a JSON object: %w", err)
	}
	if m["type"] != "object" {
		return nil, fmt.Errorf("type is %v, not \"object\"", m["type"])
	}
	return m, nil
}

func isNotFound(err error) bool { return errors.Is(err, storage.ErrNotFound) }

// discover returns a ToolServer's tools, from the cache when fresh.
func (f *Forwarder) discover(ctx context.Context, ts *v1alpha1.ToolServer) []*sdk.Tool {
	key := ts.Namespace + "/" + ts.Name
	f.discovery.mu.Lock()
	d, ok := f.discovery.lists[key]
	f.discovery.mu.Unlock()
	if ok && d.fresh(ts.Generation, time.Now()) {
		return d.tools
	}
	tools, err := f.listRemote(ctx, ts)
	if err != nil {
		f.log.Warn("forward tools: list tools", "toolServer", key, "err", err)
	}
	now := time.Now()
	f.discovery.mu.Lock()
	if f.discovery.lists == nil {
		f.discovery.lists = map[string]discovered{}
	}
	for k, old := range f.discovery.lists {
		if now.Sub(old.at) >= discoveryTTL {
			delete(f.discovery.lists, k) // ToolServers no run lists any more
		}
	}
	f.discovery.lists[key] = discovered{generation: ts.Generation, tools: tools, failed: err != nil, at: now}
	f.discovery.mu.Unlock()
	return tools
}

func (f *Forwarder) listRemote(ctx context.Context, ts *v1alpha1.ToolServer) ([]*sdk.Tool, error) {
	session, err := f.connect(ctx, ts)
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
// fresh from its Secret. Every request of the session ends with ctx, even
// those the SDK makes on its own context, such as the DELETE that closes a
// failed session: a ToolServer that does not answer costs no more than ctx.
func (f *Forwarder) connect(ctx context.Context, ts *v1alpha1.ToolServer) (*sdk.ClientSession, error) {
	var transport http.RoundTripper = within{ctx: ctx, next: http.DefaultTransport}
	if a := ts.Spec.Auth; a != nil {
		value, err := f.sources.SecretValue(ctx, ts.Namespace, a.SecretRef)
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
		endpoint, err := url.Parse(ts.Spec.URL)
		if err != nil {
			return nil, fmt.Errorf("ToolServer URL: %w", err)
		}
		// A Secret written from a file often ends in a newline, which
		// would make the header invalid.
		value = strings.TrimRight(value, "\r\n")
		transport = credential{host: endpoint.Host, header: header, value: prefix + value, next: transport}
	}
	client := sdk.NewClient(&sdk.Implementation{Name: "cadre", Version: v1alpha1.GroupVersion.Version}, nil)
	return client.Connect(ctx, &sdk.StreamableClientTransport{
		Endpoint: ts.Spec.URL,
		// Redirects are not followed: the credential must not travel to
		// wherever a ToolServer points.
		HTTPClient: &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		MaxRetries: -1,
	}, nil)
}

// within ends each request when ctx ends.
type within struct {
	ctx  context.Context
	next http.RoundTripper
}

func (w within) RoundTrip(r *http.Request) (*http.Response, error) {
	if err := w.ctx.Err(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(r.Context())
	stop := context.AfterFunc(w.ctx, cancel)
	release := func() { stop(); cancel() }
	res, err := w.next.RoundTrip(r.WithContext(ctx))
	if err != nil {
		release()
		return nil, err
	}
	res.Body = releasingBody{ReadCloser: res.Body, release: release}
	return res, nil
}

// releasingBody releases its request's context once the body is closed.
type releasingBody struct {
	io.ReadCloser
	release func()
}

func (b releasingBody) Close() error {
	err := b.ReadCloser.Close()
	b.release()
	return err
}

// credential adds a ToolServer's credential to its requests, and only to
// those for the ToolServer's own host.
type credential struct {
	host, header, value string
	next                http.RoundTripper
}

func (c credential) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Host == c.host {
		r = r.Clone(r.Context())
		r.Header.Set(c.header, c.value)
	}
	return c.next.RoundTrip(r)
}

// addForwarded registers the run's forwarded tools on server. A tool the
// SDK refuses is skipped with a log; it never costs the run its other tools.
// A tool that must ask first records the call and ends the run; a human's
// approval makes it later (see manager.Approve).
func (s *Server) addForwarded(server *sdk.Server, t *v1alpha1.Task, tools map[string]forwarded) {
	runID := t.Status.Run.ID
	key := storage.Key{Namespace: t.Namespace, Name: t.Name}
	for name, fw := range tools {
		description := fw.remote.Description
		if fw.ask {
			description = strings.TrimSpace(description + "\n\n" + askNote)
		}
		var handler sdk.ToolHandler = func(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			if err := s.m.CheckRun(ctx, key, runID); err != nil {
				return s.fail(err), nil
			}
			// What the ToolServer permits now decides, not the list the
			// tool was offered from: a tool denied since is refused
			// without paging a human, and the run goes on.
			ts, perm, why := s.fwd.callable(ctx, t.Namespace, fw.server.Name, fw.remote.Name)
			if ts == nil {
				return toolError(why), nil
			}
			if perm == v1alpha1.PermissionAsk {
				_, err := s.m.RunToolCall(ctx, key, runID, manager.ToolCall{
					Name: name, ToolServer: fw.server.Name, Tool: fw.remote.Name, Arguments: req.Params.Arguments,
				})
				if err != nil {
					return s.fail(err), nil
				}
				return runEnded(msgApprovalEnded), nil
			}
			return s.fwd.forward(ctx, ts, fw.remote.Name, req.Params.Arguments), nil
		}
		func() {
			defer func() {
				if r := recover(); r != nil {
					s.log.Warn("forward tools: tool refused, skipped", "toolServer", fw.server.Name, "tool", fw.remote.Name, "err", r)
				}
			}()
			server.AddTool(&sdk.Tool{Name: name, Description: description, InputSchema: fw.schema}, handler)
		}()
	}
}

// askNote tells the agent what calling a tool that asks first does.
const askNote = "Calling this tool needs a human's approval: the call is recorded and the run ends. Stop working once you call it; a new run continues with its result once a human decides."

// callable reads a ToolServer fresh for a call of tool: it must exist, be
// Ready and permit the tool, by allow or ask, which it returns. Otherwise it
// returns nil and what to tell the agent.
func (f *Forwarder) callable(ctx context.Context, namespace, server, tool string) (*v1alpha1.ToolServer, v1alpha1.Permission, string) {
	if f == nil || f.sources == nil {
		return nil, "", "This server forwards no ToolServers."
	}
	ts, err := f.sources.ToolServer(ctx, namespace, server)
	if err != nil && !isNotFound(err) {
		f.log.Warn("forward call: read ToolServer", "toolServer", namespace+"/"+server, "err", err)
		return nil, "", msgUnreachable
	}
	if err != nil || ts.Spec.URL == "" || !ts.Status.ReadyFor(ts.Generation) {
		return nil, "", "This tool's server is gone or not Ready."
	}
	perm := permissionFor(ts.Spec.Permissions, tool)
	if perm != v1alpha1.PermissionAllow && perm != v1alpha1.PermissionAsk {
		return nil, perm, "This tool is no longer available to this run."
	}
	return ts, perm, ""
}

// CallTimeout is the longest a forwarded call may take.
func (f *Forwarder) CallTimeout() time.Duration { return forwardTimeout }

// CallTool makes a call a human approved, on its ToolServer read fresh. A
// call its ToolServer no longer permits is not made, and the result says
// so. The result's text is what the agent reads next.
func (f *Forwarder) CallTool(ctx context.Context, namespace string, call manager.ToolCall) manager.ToolResult {
	ts, _, why := f.callable(ctx, namespace, call.ToolServer, call.Tool)
	if ts == nil {
		return manager.ToolResult{Body: "The call was not made. " + why, IsError: true}
	}
	res := f.forward(ctx, ts, call.Tool, call.Arguments)
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*sdk.TextContent); ok {
			if b.Len() > 0 {
				b.WriteString("\n")
			}
			b.WriteString(tc.Text)
		}
	}
	if b.Len() == 0 && res.StructuredContent != nil {
		if data, err := json.Marshal(res.StructuredContent); err == nil {
			b.Write(data)
		}
	}
	return manager.ToolResult{Body: b.String(), IsError: res.IsError}
}

// forward makes the call on the ToolServer and shapes its result for the
// model: text and structured content up to 1 MiB; images and other media
// are dropped with a note in M1.
func (f *Forwarder) forward(ctx context.Context, ts *v1alpha1.ToolServer, tool string, args json.RawMessage) *sdk.CallToolResult {
	ctx, cancel := context.WithTimeout(ctx, forwardTimeout)
	defer cancel()
	session, err := f.connect(ctx, ts)
	if err != nil {
		f.log.Warn("forward call: connect", "toolServer", ts.Name, "err", err)
		return toolError(msgUnreachable)
	}
	defer func() { _ = session.Close() }()
	var arguments any = map[string]any{}
	if len(args) > 0 {
		arguments = args
	}
	res, err := session.CallTool(ctx, &sdk.CallToolParams{Name: tool, Arguments: arguments})
	if err != nil {
		f.log.Warn("forward call", "toolServer", ts.Name, "tool", tool, "err", err)
		return toolError("The tool call failed: " + err.Error())
	}
	return shapeResult(res)
}

const msgUnreachable = "The tool's server could not be reached; try again later."

func toolError(msg string) *sdk.CallToolResult {
	r := &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: msg}}}
	r.IsError = true
	return r
}

// shapeResult keeps a forwarded result within what M1 hands a model: text
// up to maxResultBytes in all, cut on a rune boundary.
func shapeResult(res *sdk.CallToolResult) *sdk.CallToolResult {
	out := &sdk.CallToolResult{IsError: res.IsError}
	budget := maxResultBytes
	var notes []string
	truncated := false
	for _, c := range res.Content {
		tc, ok := c.(*sdk.TextContent)
		if !ok {
			notes = append(notes, fmt.Sprintf("[a %T was dropped: only text is forwarded]", c))
			continue
		}
		if truncated {
			continue
		}
		text := tc.Text
		if len(text) > budget {
			cut := budget
			for cut > 0 && !utf8.RuneStart(text[cut]) {
				cut--
			}
			text = text[:cut]
			truncated = true
			notes = append(notes, fmt.Sprintf("[the result was truncated at %d bytes]", maxResultBytes))
		}
		budget -= len(text)
		if text != "" {
			out.Content = append(out.Content, &sdk.TextContent{Text: text})
		}
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
