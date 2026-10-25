package mcp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	"github.com/j75689/cadre/internal/api"
	"github.com/j75689/cadre/internal/auth"
	"github.com/j75689/cadre/internal/authz"
	"github.com/j75689/cadre/internal/manager"
	"github.com/j75689/cadre/internal/storage"
)

// BoardPath is where the board endpoint is served: the board's tools for
// agents outside Cadre (Claude Code on someone's machine, say), apart from
// the per-run endpoint at Path.
const BoardPath = "/mcp"

// anonymous is the author of what is written through the board endpoint
// without logins: nobody is known, and a tool takes no author.
const anonymous = "anonymous"

// Board serves the board endpoint. Each request comes with the caller's
// identity, from a bearer ID token (auth.Authenticator.Bearer); every tool
// call is decided by the caller's role on the team of the namespace it
// names, as the API's routes are (api.Check), and what it writes is the
// caller's.
type Board struct {
	m       *manager.Manager
	log     *slog.Logger
	policy  *authz.Policy // nil: everyone may do anything
	mcp     http.Handler
	schemas *sdk.SchemaCache
}

// NewBoard returns the board endpoint. policy is the API's: nil without
// logins.
func NewBoard(m *manager.Manager, log *slog.Logger, policy *authz.Policy) *Board {
	b := &Board{m: m, log: log, policy: policy, schemas: sdk.NewSchemaCache()}
	b.mcp = sdk.NewStreamableHTTPHandler(b.serverFor, &sdk.StreamableHTTPOptions{Stateless: true, Logger: log})
	return b
}

// Register adds the endpoint to mux, behind authn's bearer check.
func (b *Board) Register(mux *http.ServeMux, authn auth.Authenticator) {
	mux.Handle(BoardPath, authn.Bearer(b.mcp))
}

// BoardTools is each board tool and what it does on its namespace's team:
// what the caller's role must allow. Every tool is here, and is checked
// before it acts.
var BoardTools = map[string]authz.Action{
	"list_tasks":  authz.Read,
	"get_task":    authz.Read,
	"create_task": authz.CreateTask,
	"comment":     authz.Comment,
	"reply":       authz.Reply,
}

// Tool inputs and outputs.
type (
	boardListIn struct {
		Namespace string           `json:"namespace" jsonschema:"the team's namespace"`
		Phase     []v1alpha1.Phase `json:"phase,omitempty" jsonschema:"only tasks in these phases: Todo, Running, Waiting, Review, Done, Cancelled, Failed"`
		Parent    string           `json:"parent,omitempty" jsonschema:"only the subtasks of this task, by name"`
		Offset    int              `json:"offset,omitempty"`
		Limit     int              `json:"limit,omitempty" jsonschema:"at most this many, 50 by default and 500 at most"`
	}
	boardListOut struct {
		Tasks  []v1alpha1.Task `json:"tasks"`
		Offset int             `json:"offset"`
		Limit  int             `json:"limit"` // the page size applied: a full page may have more after it
	}
	boardTaskIn struct {
		Namespace string `json:"namespace" jsonschema:"the team's namespace"`
		Task      string `json:"task" jsonschema:"the task's name, or #number"`
		After     int64  `json:"after,omitempty" jsonschema:"the thread entries after this sequence number; 0 reads from the start"`
		Limit     int    `json:"limit,omitempty" jsonschema:"at most this many thread entries, 200 at most and by default"`
	}
	boardTaskOut struct {
		Task   *v1alpha1.Task        `json:"task"`
		Thread []manager.ThreadEntry `json:"thread"`
		// Last is the last entry's sequence number read: pass it as after
		// for the next page. A full page may have more after it.
		Last int64 `json:"last"`
	}
	boardCreateIn struct {
		Namespace   string             `json:"namespace" jsonschema:"the team's namespace"`
		Title       string             `json:"title" jsonschema:"one-line summary of the task"`
		Description string             `json:"description,omitempty"`
		Priority    v1alpha1.Priority  `json:"priority,omitempty" jsonschema:"low, medium, high or urgent"`
		Assignee    *v1alpha1.Assignee `json:"assignee,omitempty" jsonschema:"who works on it: kind Agent or User, and a name; unassigned when omitted"`
		Labels      []string           `json:"labels,omitempty"`
		Parent      string             `json:"parent,omitempty" jsonschema:"make it a subtask of this task, by name"`
	}
	boardPostIn struct {
		Namespace string `json:"namespace" jsonschema:"the team's namespace"`
		Task      string `json:"task" jsonschema:"the task's name, or #number"`
		Body      string `json:"body" jsonschema:"the message"`
	}
)

// serverFor builds the MCP server for one request, with the board's tools.
// A tool reads the caller from its context: the stateless handler serves
// the request on its own context, which Bearer gave the identity, and
// api.Check reads it there too. Their results are declared any: a Task's
// schema cannot be inferred (its metadata's types have their own), so a
// result is sent as structured content without an output schema.
func (b *Board) serverFor(*http.Request) *sdk.Server {
	server := sdk.NewServer(&sdk.Implementation{Name: "cadre", Version: v1alpha1.GroupVersion.Version}, &sdk.ServerOptions{SchemaCache: b.schemas})
	// as is who writes: the caller, or anonymous without logins.
	as := func(ctx context.Context) manager.Author {
		if id, ok := auth.FromContext(ctx); ok {
			return manager.Author{Kind: manager.AuthorUser, Name: id.Username}
		}
		return manager.Author{Kind: manager.AuthorUser, Name: anonymous}
	}
	// allowed decides a call: the caller's role on the namespace it names.
	allowed := func(ctx context.Context, tool, ns string) *sdk.CallToolResult {
		var refused *api.Refused
		switch err := api.Check(ctx, b.m, b.policy, ns, BoardTools[tool]); {
		case errors.As(err, &refused):
			return toolError(refused.Message)
		case errors.Is(err, api.ErrUnauthenticated):
			return toolError("Log in first.")
		case err != nil:
			return b.fail(err)
		}
		return nil
	}

	sdk.AddTool(server, &sdk.Tool{Name: "list_tasks", Description: "List a team's tasks in board order (priority, then number), optionally by phase or parent."},
		func(ctx context.Context, _ *sdk.CallToolRequest, in boardListIn) (*sdk.CallToolResult, any, error) {
			if res := allowed(ctx, "list_tasks", in.Namespace); res != nil {
				return res, nil, nil
			}
			// As the API checks them: an unknown phase would match
			// nothing, and look like an empty board.
			if err := errors.Join(api.ValidatePhases(in.Phase), api.ValidateParent(in.Parent)); err != nil {
				return toolError("Invalid call: " + err.Error()), nil, nil
			}
			if in.Offset < 0 {
				return toolError("Invalid call: offset must not be negative"), nil, nil
			}
			limit := api.ClampLimit(in.Limit)
			tasks, err := b.m.ListTasks(ctx, storage.Query{Namespace: in.Namespace, Phases: in.Phase, Parent: in.Parent, Offset: in.Offset, Limit: limit})
			if err != nil {
				return b.fail(err), nil, nil
			}
			if tasks == nil {
				tasks = []v1alpha1.Task{}
			}
			return nil, boardListOut{Tasks: tasks, Offset: in.Offset, Limit: limit}, nil
		})

	sdk.AddTool(server, &sdk.Tool{Name: "get_task", Description: "Read a task and a page of its thread: comments, questions, answers and tool calls, oldest first. For the next page pass after, the last sequence number this one returned."},
		func(ctx context.Context, _ *sdk.CallToolRequest, in boardTaskIn) (*sdk.CallToolResult, any, error) {
			if res := allowed(ctx, "get_task", in.Namespace); res != nil {
				return res, nil, nil
			}
			key, err := b.resolve(ctx, in.Namespace, in.Task)
			if err != nil {
				return b.fail(err), nil, nil
			}
			t, err := b.m.GetTask(ctx, key)
			if err != nil {
				return b.fail(err), nil, nil
			}
			limit := in.Limit
			if limit <= 0 || limit > manager.MaxThreadPage {
				limit = manager.MaxThreadPage
			}
			thread, err := b.m.ReadThread(ctx, key, in.After, limit)
			if err != nil {
				return b.fail(err), nil, nil
			}
			last := in.After
			if len(thread) > 0 {
				last = thread[len(thread)-1].Seq
			} else {
				thread = []manager.ThreadEntry{}
			}
			return nil, boardTaskOut{Task: t, Thread: thread, Last: last}, nil
		})

	sdk.AddTool(server, &sdk.Tool{Name: "create_task", Description: "Create a task on a team's board. It is unassigned unless an assignee is given."},
		func(ctx context.Context, _ *sdk.CallToolRequest, in boardCreateIn) (*sdk.CallToolResult, any, error) {
			if res := allowed(ctx, "create_task", in.Namespace); res != nil {
				return res, nil, nil
			}
			spec := v1alpha1.TaskSpec{Title: in.Title, Description: in.Description, Priority: in.Priority, Assignee: in.Assignee, Labels: in.Labels}
			if in.Parent != "" {
				spec.ParentRef = &v1alpha1.TaskReference{Name: in.Parent}
			}
			t, err := b.m.CreateTask(ctx, in.Namespace, "", spec)
			if err != nil {
				return b.fail(err), nil, nil
			}
			return nil, t, nil
		})

	sdk.AddTool(server, &sdk.Tool{Name: "comment", Description: "Post a comment to a task's thread."},
		func(ctx context.Context, _ *sdk.CallToolRequest, in boardPostIn) (*sdk.CallToolResult, any, error) {
			if res := allowed(ctx, "comment", in.Namespace); res != nil {
				return res, nil, nil
			}
			key, err := b.resolve(ctx, in.Namespace, in.Task)
			if err != nil {
				return b.fail(err), nil, nil
			}
			e, err := b.m.AppendThread(ctx, key, as(ctx), manager.EntryComment, in.Body)
			if err != nil {
				return b.fail(err), nil, nil
			}
			return nil, e, nil
		})

	sdk.AddTool(server, &sdk.Tool{Name: "reply", Description: "Answer the question a Waiting task asks; the task then goes back to work."},
		func(ctx context.Context, _ *sdk.CallToolRequest, in boardPostIn) (*sdk.CallToolResult, any, error) {
			if res := allowed(ctx, "reply", in.Namespace); res != nil {
				return res, nil, nil
			}
			key, err := b.resolve(ctx, in.Namespace, in.Task)
			if err != nil {
				return b.fail(err), nil, nil
			}
			_, e, err := b.m.Answer(ctx, key, as(ctx), in.Body)
			if err != nil {
				return b.fail(err), nil, nil
			}
			return nil, e, nil
		})
	return server
}

// resolve turns a task's name, or #number, into its key.
func (b *Board) resolve(ctx context.Context, ns, ref string) (storage.Key, error) {
	digits, ok := strings.CutPrefix(ref, "#")
	if !ok {
		return storage.Key{Namespace: ns, Name: ref}, nil
	}
	n, err := strconv.ParseInt(digits, 10, 64)
	if err != nil || n < 1 {
		return storage.Key{}, fmt.Errorf("%q is not a task number: %w", ref, manager.ErrInvalid)
	}
	t, err := b.m.TaskByNumber(ctx, ns, n)
	if err != nil {
		return storage.Key{}, err
	}
	return storage.Key{Namespace: ns, Name: t.Name}, nil
}

// fail is failure for a board tool.
func (b *Board) fail(err error) *sdk.CallToolResult { return failure(b.log, err, false) }
