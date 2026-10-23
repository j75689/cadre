// Package api serves /api/v1: what the CLI, and later the web UI, use to work
// on the board. Every change goes through the Task Manager. M0 is
// unauthenticated; the author of a thread entry is whatever the client sends.
package api

import (
	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	"github.com/j75689/cadre/internal/manager"
)

// Base is the API's path prefix.
const Base = "/api/v1"

// CreateTaskRequest creates a task. Without a name the server names it after
// a fresh lowercase ULID.
type CreateTaskRequest struct {
	Name string            `json:"name,omitempty"`
	Spec v1alpha1.TaskSpec `json:"spec"`
}

// TaskList is a page of the board.
type TaskList struct {
	Items  []v1alpha1.Task `json:"items"`
	Offset int             `json:"offset"`
	Limit  int             `json:"limit"`
}

// CommentRequest writes to a task's thread, or answers a waiting task.
type CommentRequest struct {
	Author string `json:"author"`
	Body   string `json:"body"`
}

// Thread is a page of a task's thread.
type Thread struct {
	Items []manager.ThreadEntry `json:"items"`
}

// ReplyResponse is the task after a reply, and the answer it recorded.
type ReplyResponse struct {
	Task  *v1alpha1.Task       `json:"task"`
	Entry *manager.ThreadEntry `json:"entry"`
}

// Runs is a task's run history, oldest first.
type Runs struct {
	Items []v1alpha1.RunRecord `json:"items"`
}

// Events is one run's progress events, in order.
type Events struct {
	Items []manager.Event `json:"items"`
}

// TeamRequest opts a namespace in.
type TeamRequest struct {
	DisplayName       string `json:"displayName,omitempty"`
	MaxConcurrentRuns int32  `json:"maxConcurrentRuns,omitempty"`
}

// TeamList lists every Team.
type TeamList struct {
	Items []v1alpha1.Team `json:"items"`
}
