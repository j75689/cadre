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
	"github.com/j75689/cadre/internal/httpjson"
	"github.com/j75689/cadre/internal/manager"
	"github.com/j75689/cadre/internal/storage"
	"github.com/j75689/cadre/internal/task"
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
		Tasks []v1alpha1.Task `json:"tasks"`
	}
	boardTaskIn struct {
		Namespace string `json:"namespace" jsonschema:"the team's namespace"`
		Task      string `json:"task" jsonschema:"the task's name, or #number"`
		After     int64  `json:"after,omitempty" jsonschema:"the thread entries after this sequence number; 0 reads from the start"`
	}
	boardTaskOut struct {
		Task   *v1alpha1.Task        `json:"task"`
		Thread []manager.ThreadEntry `json:"thread"`
	}
	boardCreateIn struct {
		Namespace   string             `json:"namespace" jsonschema:"the team's namespace"`
		Title       string             `json:"title" jsonschema:"one-line summary of the task"`
		Description string             `json:"description,omitempty"`
		Priority    v1alpha1.Priority  `json:"priority,omitempty" jsonschema:"low, medium, high or urgent"`
		Assignee    *v1alpha1.Assignee `json:"assignee,omitempty" jsonschema:"who works on it: kind Agent or User, and a name; unassigned when omitted"`
		Parent      string             `json:"parent,omitempty" jsonschema:"make it a subtask of this task, by name"`
	}
	boardPostIn struct {
		Namespace string `json:"namespace" jsonschema:"the team's namespace"`
		Task      string `json:"task" jsonschema:"the task's name, or #number"`
		Body      string `json:"body" jsonschema:"the message"`
	}
)

// serverFor builds the MCP server for one request, with the board's tools
// acting as the request's caller. Their results are declared any: a Task's
// schema cannot be inferred (its metadata's types have their own), so a
// result is sent as structured content without an output schema.
func (b *Board) serverFor(r *http.Request) *sdk.Server {
	id, _ := auth.FromContext(r.Context())
	server := sdk.NewServer(&sdk.Implementation{Name: "cadre", Version: v1alpha1.GroupVersion.Version}, &sdk.ServerOptions{SchemaCache: b.schemas})
	author := anonymous
	if id != nil {
		author = id.Username
	}
	as := manager.Author{Kind: manager.AuthorUser, Name: author}
	// allowed decides a call: the caller's identity, as Check reads it from
	// the context, on the namespace the call names.
	allowed := func(ctx context.Context, tool, ns string) *sdk.CallToolResult {
		if id != nil {
			ctx = auth.WithIdentity(ctx, id)
		}
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
			limit := in.Limit
			switch {
			case limit <= 0:
				limit = api.DefaultLimit
			case limit > api.MaxLimit:
				limit = api.MaxLimit
			}
			tasks, err := b.m.ListTasks(ctx, storage.Query{Namespace: in.Namespace, Phases: in.Phase, Parent: in.Parent, Offset: in.Offset, Limit: limit})
			if err != nil {
				return b.fail(err), nil, nil
			}
			if tasks == nil {
				tasks = []v1alpha1.Task{}
			}
			return nil, boardListOut{Tasks: tasks}, nil
		})

	sdk.AddTool(server, &sdk.Tool{Name: "get_task", Description: "Read a task and its thread: comments, questions, answers and tool calls, oldest first. Page the thread with after, the last sequence number read."},
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
			thread, err := b.m.ReadThread(ctx, key, in.After, 0)
			if err != nil {
				return b.fail(err), nil, nil
			}
			if thread == nil {
				thread = []manager.ThreadEntry{}
			}
			return nil, boardTaskOut{Task: t, Thread: thread}, nil
		})

	sdk.AddTool(server, &sdk.Tool{Name: "create_task", Description: "Create a task on a team's board. It is unassigned unless an assignee is given."},
		func(ctx context.Context, _ *sdk.CallToolRequest, in boardCreateIn) (*sdk.CallToolResult, any, error) {
			if res := allowed(ctx, "create_task", in.Namespace); res != nil {
				return res, nil, nil
			}
			spec := v1alpha1.TaskSpec{Title: in.Title, Description: in.Description, Priority: in.Priority, Assignee: in.Assignee}
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
			e, err := b.m.AppendThread(ctx, key, as, manager.EntryComment, in.Body)
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
			_, e, err := b.m.Answer(ctx, key, as, in.Body)
			if err != nil {
				return b.fail(err), nil, nil
			}
			return nil, e, nil
		})
	return server
}

// resolve turns a task's name, or #number, into its key: a number is
// looked up on the board, a page at a time.
func (b *Board) resolve(ctx context.Context, ns, ref string) (storage.Key, error) {
	digits, ok := strings.CutPrefix(ref, "#")
	if !ok {
		return storage.Key{Namespace: ns, Name: ref}, nil
	}
	n, err := strconv.ParseInt(digits, 10, 64)
	if err != nil || n < 1 {
		return storage.Key{}, fmt.Errorf("%q is not a task number: %w", ref, manager.ErrInvalid)
	}
	for offset := 0; ; offset += api.MaxLimit {
		tasks, err := b.m.ListTasks(ctx, storage.Query{Namespace: ns, Offset: offset, Limit: api.MaxLimit})
		if err != nil {
			return storage.Key{}, err
		}
		for _, t := range tasks {
			if t.Status.Number == n {
				return storage.Key{Namespace: ns, Name: t.Name}, nil
			}
		}
		if len(tasks) < api.MaxLimit {
			return storage.Key{}, fmt.Errorf("no task #%d: %w", n, storage.ErrNotFound)
		}
	}
}

// fail turns an error into a tool error the agent can read, classified as
// the API classifies it; storage details stay in the server log.
func (b *Board) fail(err error) *sdk.CallToolResult {
	switch httpjson.Classify(err) {
	case httpjson.CodeInvalid:
		return toolError("Invalid call: " + err.Error())
	case httpjson.CodeNotFound:
		return toolError("Not found.")
	case httpjson.CodeAlreadyExists:
		return toolError("Already exists.")
	case httpjson.CodeInvalidTransition:
		return toolError("Not allowed in the task's phase: " + task.ErrInvalidTransition.Error())
	case httpjson.CodeEntryPending:
		return toolError(manager.ErrEntryPending.Error())
	}
	b.log.Warn("board MCP tool failed", "err", err)
	return toolError("A temporary error occurred; try again.")
}
