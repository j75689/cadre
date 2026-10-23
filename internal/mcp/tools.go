package mcp

import (
	"context"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	"github.com/j75689/cadre/internal/httpjson"
	"github.com/j75689/cadre/internal/manager"
	"github.com/j75689/cadre/internal/storage"
)

// Messages a tool answers with when it cannot act. They are tool errors
// (isError), so the model sees them: a lost run must stop, and a malformed
// call can be corrected.
const (
	msgRunGone  = "This run is no longer active (the task was cancelled, reclaimed or finished). Stop working on this task now."
	msgAskEnded = "Your question has been posted and this run has ended; the task now waits for a human. Stop working on this task now and do not call finish."
	// msgApprovalEnded answers a call that must ask first.
	msgApprovalEnded = "This call needs a human's approval. It has been recorded and this run has ended; a new run continues with the call's result, or its denial, once a human decides. Stop working on this task now and do not call finish."
)

// Tool inputs and outputs.
type (
	postIn struct {
		Body string `json:"body" jsonschema:"the message to post to the task's thread"`
	}
	postOut struct {
		Seq int64 `json:"seq"`
	}
	askIn struct {
		Question string `json:"question" jsonschema:"the question for a human; the run ends once it is posted"`
	}
	askOut struct {
		Seq     int64  `json:"seq"`
		Message string `json:"message"`
	}
	subtaskIn struct {
		Title       string             `json:"title" jsonschema:"one-line summary of the subtask"`
		Description string             `json:"description,omitempty" jsonschema:"what the subtask is"`
		Priority    v1alpha1.Priority  `json:"priority,omitempty" jsonschema:"low, medium, high or urgent"`
		Labels      []string           `json:"labels,omitempty"`
		Assignee    *v1alpha1.Assignee `json:"assignee,omitempty" jsonschema:"who works on it: kind Agent or User, and a name; unassigned when omitted"`
	}
	readIn struct {
		Path string `json:"path" jsonschema:"the file's path"`
	}
	writeIn struct {
		Path    string `json:"path" jsonschema:"the file's path, at most 255 bytes"`
		Content string `json:"content" jsonschema:"the file's new content, UTF-8 text, at most 1 MiB"`
	}
	listOut struct {
		Files []manager.FileInfo `json:"files"`
	}
	empty struct{}
)

// Tool names follow the model APIs' rule, ^[a-zA-Z0-9_-]{1,64}$, so runners
// pass them to a model unchanged: no dots. Forwarded ToolServer tools are
// named <toolserver>__<tool>; ToolServer names cannot contain "_", so they
// never collide with these.

// addTools registers the run's tools on server. Every one resolves the run
// from the request and is fenced on it.
func (s *Server) addTools(server *sdk.Server, t *v1alpha1.Task) {
	key := storage.Key{Namespace: t.Namespace, Name: t.Name}
	runID := t.Status.Run.ID

	sdk.AddTool(server, &sdk.Tool{Name: "thread_post", Description: "Post a message to the task's thread, for the humans following the task."},
		func(ctx context.Context, _ *sdk.CallToolRequest, in postIn) (*sdk.CallToolResult, postOut, error) {
			e, err := s.m.RunPost(ctx, key, runID, in.Body)
			if err != nil {
				return s.fail(err), postOut{}, nil
			}
			return nil, postOut{Seq: e.Seq}, nil
		})

	sdk.AddTool(server, &sdk.Tool{Name: "thread_ask", Description: "Ask a human a question. The run ends as soon as the question is posted: stop working and do not call finish. A new run continues once a human answers."},
		func(ctx context.Context, _ *sdk.CallToolRequest, in askIn) (*sdk.CallToolResult, askOut, error) {
			e, err := s.m.RunAsk(ctx, key, runID, in.Question)
			if err != nil {
				return s.fail(err), askOut{}, nil
			}
			return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: msgAskEnded}}}, askOut{Seq: e.Seq, Message: msgAskEnded}, nil
		})

	sdk.AddTool(server, &sdk.Tool{Name: "task_create_subtask", Description: "Create a subtask of this task. It is unassigned unless an assignee is given."},
		func(ctx context.Context, _ *sdk.CallToolRequest, in subtaskIn) (*sdk.CallToolResult, manager.SubtaskCreated, error) {
			created, err := s.m.RunCreateSubtask(ctx, key, runID, v1alpha1.TaskSpec{
				Title: in.Title, Description: in.Description, Priority: in.Priority, Labels: in.Labels, Assignee: in.Assignee,
			})
			if err != nil {
				return s.fail(err), manager.SubtaskCreated{}, nil
			}
			return nil, *created, nil
		})

	sdk.AddTool(server, &sdk.Tool{Name: "task_files_read", Description: "Read the latest version of one of the task's files."},
		func(ctx context.Context, _ *sdk.CallToolRequest, in readIn) (*sdk.CallToolResult, manager.File, error) {
			f, err := s.m.RunReadFile(ctx, key, runID, in.Path)
			if err != nil {
				return s.fail(err), manager.File{}, nil
			}
			return nil, *f, nil
		})

	sdk.AddTool(server, &sdk.Tool{Name: "task_files_write", Description: "Write a new version of one of the task's files (UTF-8 text, at most 1 MiB)."},
		func(ctx context.Context, _ *sdk.CallToolRequest, in writeIn) (*sdk.CallToolResult, manager.FileInfo, error) {
			info, err := s.m.RunWriteFile(ctx, key, runID, in.Path, in.Content)
			if err != nil {
				return s.fail(err), manager.FileInfo{}, nil
			}
			return nil, *info, nil
		})

	sdk.AddTool(server, &sdk.Tool{Name: "task_files_list", Description: "List the task's files with their latest version and size."},
		func(ctx context.Context, _ *sdk.CallToolRequest, _ empty) (*sdk.CallToolResult, listOut, error) {
			files, err := s.m.RunListFiles(ctx, key, runID)
			if err != nil {
				return s.fail(err), listOut{}, nil
			}
			return nil, listOut{Files: files}, nil
		})
}

// fail turns an error into a tool error the model can read. It uses the
// classification the HTTP APIs use. Storage details stay in the server log.
func (s *Server) fail(err error) *sdk.CallToolResult {
	var msg string
	switch httpjson.Classify(err) {
	case httpjson.CodeRunMismatch, httpjson.CodeStepMismatch, httpjson.CodeInvalidTransition:
		msg = msgRunGone
	case httpjson.CodeInvalid:
		msg = "Invalid call: " + err.Error()
	case httpjson.CodeNotFound:
		msg = "Not found."
	case httpjson.CodeAlreadyExists:
		msg = "Already exists."
	default:
		s.log.Warn("MCP tool failed", "err", err)
		msg = "A temporary error occurred; try again."
	}
	r := &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: msg}}}
	r.IsError = true
	return r
}
