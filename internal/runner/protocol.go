// Package runner is the runner protocol: the /runner/v1 HTTP endpoints a
// runner uses to claim a task, keep its run alive, report progress and finish.
// The request and response types are shared by the server and by clients.
package runner

import (
	"time"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	"github.com/j75689/cadre/internal/httpjson"
)

// Paths of the protocol's endpoints.
const (
	PathClaim     = "/runner/v1/claim"
	PathHeartbeat = "/runner/v1/heartbeat"
	PathEvents    = "/runner/v1/events"
	PathFinish    = "/runner/v1/finish"
)

// TaskRef names a task. Every call after claim carries one, so the server
// fences on a fresh read of that task and never looks a run up in an index.
type TaskRef struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

// ClaimRequest asks for one task. Namespaces limits the search; empty means
// every Team namespace. Labels are matched against Agent selectors from M1
// and are ignored in M0.
type ClaimRequest struct {
	Runner     string            `json:"runner"`
	Namespaces []string          `json:"namespaces,omitempty"`
	Labels     map[string]string `json:"labels,omitempty"`
}

// ClaimedTask is the task a claim hands out.
type ClaimedTask struct {
	Namespace   string                  `json:"namespace"`
	Name        string                  `json:"name"`
	Number      int64                   `json:"number"`
	Title       string                  `json:"title"`
	Description string                  `json:"description,omitempty"`
	Assignee    *v1alpha1.Assignee      `json:"assignee,omitempty"`
	Priority    v1alpha1.Priority       `json:"priority,omitempty"`
	ParentRef   *v1alpha1.TaskReference `json:"parentRef,omitempty"`
	Labels      []string                `json:"labels,omitempty"`
}

// ClaimResponse is the 200 body of a claim; 204 means there is no work.
// M1 adds an "agent" block (instructions, model, tools) and "mcp" (url,
// token); both are additive.
type ClaimResponse struct {
	Task             ClaimedTask      `json:"task"`
	RunID            string           `json:"runId"`
	Mode             v1alpha1.RunMode `json:"mode"`
	LeaseUntil       time.Time        `json:"leaseUntil"`
	LeaseSeconds     int              `json:"leaseSeconds"`
	HeartbeatSeconds int              `json:"heartbeatSeconds"`
}

// HeartbeatRequest renews a run's lease.
type HeartbeatRequest struct {
	Task  TaskRef `json:"task"`
	RunID string  `json:"runId"`
}

// HeartbeatResponse is the 200 body of a heartbeat.
type HeartbeatResponse struct {
	LeaseUntil   time.Time `json:"leaseUntil"`
	LeaseSeconds int       `json:"leaseSeconds"`
}

// Usage is the token usage a run reports.
type Usage struct {
	InputTokens  int64 `json:"inputTokens,omitempty"`
	OutputTokens int64 `json:"outputTokens,omitempty"`
}

// EventsRequest reports a run's progress. A repeated seq is accepted.
type EventsRequest struct {
	Task          TaskRef `json:"task"`
	RunID         string  `json:"runId"`
	Seq           int64   `json:"seq"`
	Usage         Usage   `json:"usage"`
	ContextTokens int64   `json:"contextTokens,omitempty"`
	Progress      string  `json:"progress,omitempty"`
}

// Waiting says what a run that finishes Waiting waits for.
type Waiting struct {
	Reason    v1alpha1.WaitingReason `json:"reason"`
	ThreadSeq int64                  `json:"threadSeq,omitempty"`
}

// Subtask is a subtask a finishing run asks for. ID is an optional ULID; a
// runner that mints it gets exactly one subtask across retries.
type Subtask struct {
	ID   string            `json:"id,omitempty"`
	Spec v1alpha1.TaskSpec `json:"spec"`
}

// FinishRequest ends a run. Step is required in step mode and ignored in
// runtime mode; Waiting is required exactly when Outcome is Waiting.
type FinishRequest struct {
	Task     TaskRef             `json:"task"`
	RunID    string              `json:"runId"`
	Step     int64               `json:"step,omitempty"`
	Outcome  v1alpha1.RunOutcome `json:"outcome"`
	Waiting  *Waiting            `json:"waiting,omitempty"`
	Subtasks []Subtask           `json:"subtasks,omitempty"`
}

// FinishResponse is the 200 body of a finish. AlreadyFinished is true when
// this same finish had already landed, so a retry is safe.
type FinishResponse struct {
	Phase           v1alpha1.Phase `json:"phase"`
	AlreadyFinished bool           `json:"alreadyFinished,omitempty"`
}

// Error codes /runner/v1 returns. A runner that gets run_mismatch, or
// not_found for its task, stops working on that task at once.
const (
	CodeInvalid           = httpjson.CodeInvalid
	CodeNotFound          = httpjson.CodeNotFound
	CodeRunMismatch       = httpjson.CodeRunMismatch
	CodeStepMismatch      = httpjson.CodeStepMismatch
	CodeInvalidTransition = httpjson.CodeInvalidTransition
	CodeUnavailable       = httpjson.CodeUnavailable
)

// ErrorResponse is the body of every error.
type ErrorResponse = httpjson.ErrorResponse

// ErrorBody describes an error.
type ErrorBody = httpjson.ErrorBody
