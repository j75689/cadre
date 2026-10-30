// Package api serves /api/v1: what the CLI, and later the web UI, use to work
// on the board. Every change goes through the Task Manager. M0 is
// unauthenticated; the author of a thread entry is whatever the client sends.
package api

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

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

// Page sizes for listing tasks.
const (
	DefaultLimit = 100 // when the request gives no limit
	MaxLimit     = 500 // the most one page returns
)

// TaskList is a page of the board. Offset and Limit are the values that
// applied.
type TaskList struct {
	Items  []v1alpha1.Task `json:"items"`
	Offset int             `json:"offset"`
	Limit  int             `json:"limit"`
}

// CommentRequest writes to a task's thread, or answers a waiting task.
// Author is used only by a server run with --auth=none: with a login, the
// author is the caller's username, whatever the request says.
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

// ApproveRequest approves or denies the tool call a task waits on. Approve
// is required, so a request cannot deny by leaving it out. Author, as in
// CommentRequest, counts only with --auth=none.
type ApproveRequest struct {
	Author  string `json:"author"`
	Approve *bool  `json:"approve"`
	Note    string `json:"note,omitempty"`
}

// ApproveResponse is the task after the decision, the decision entry, and
// the call's result entry (none for a denied call).
type ApproveResponse struct {
	Task     *v1alpha1.Task       `json:"task"`
	Decision *manager.ThreadEntry `json:"decision"`
	Result   *manager.ThreadEntry `json:"result,omitempty"`
}

// Runs is a task's run history, oldest first.
type Runs struct {
	Items []v1alpha1.RunRecord `json:"items"`
}

// Events is one run's progress events, in order.
type Events struct {
	Items []manager.Event `json:"items"`
}

// AgentList is a namespace's Agents, by name, for picking an assignee.
type AgentList struct {
	Items []AgentSummary `json:"items"`
}

// AgentSummary is an Agent and whether it can take work: Ready for its
// current spec, and if not, why.
type AgentSummary = ConfigSummary

// ConfigSummary is a configuration resource and whether it can be used:
// Ready for its current spec, and if not, why.
type ConfigSummary struct {
	Name    string `json:"name"`
	Ready   bool   `json:"ready"`
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
}

// ConfigList is a namespace's resources of one configuration kind, by name.
type ConfigList struct {
	Items []ConfigSummary `json:"items"`
}

// ConfigResource is one configuration resource as the caller may see it:
// a viewer its health and metadata, a member and an admin its spec and
// status too, a member's with credentials a spec may hold redacted.
type ConfigResource struct {
	ConfigSummary
	Kind               string             `json:"kind"`
	Generation         int64              `json:"generation"`
	ObservedGeneration int64              `json:"observedGeneration"`
	Conditions         []metav1.Condition `json:"conditions"`
	Metadata           ConfigMeta         `json:"metadata"`
	Spec               any                `json:"spec,omitempty"`
	Status             any                `json:"status,omitempty"`
}

// ConfigMeta is a configuration resource's metadata as the API shows it:
// never its fields' owners, nor the last spec kubectl applied; annotations
// for a member and up.
type ConfigMeta struct {
	UID               string            `json:"uid"`
	ResourceVersion   string            `json:"resourceVersion"`
	CreationTimestamp metav1.Time       `json:"creationTimestamp"`
	Labels            map[string]string `json:"labels,omitempty"`
	Annotations       map[string]string `json:"annotations,omitempty"`
}

// TeamRequest opts a namespace in, or edits its Team: its spec, replaced
// whole. An edit carries the resourceVersion of the Team it was made from,
// and fails with 409 conflict if the Team changed since; without one, the
// request creates the Team, and is 409 already_exists if there is one.
type TeamRequest struct {
	v1alpha1.TeamSpec
	ResourceVersion string `json:"resourceVersion,omitempty"`
}

// AgentAccessRequest makes an AgentAccess, or edits one: its spec,
// replaced whole. An edit carries the resourceVersion of the AgentAccess it
// was made from, and fails with 409 conflict if it changed since; without
// one, the request makes it, and is 409 already_exists if there is one.
type AgentAccessRequest struct {
	ResourceVersion string                   `json:"resourceVersion,omitempty"`
	Spec            v1alpha1.AgentAccessSpec `json:"spec"`
}

// AgentAccessList is a namespace's AgentAccesses, by name.
type AgentAccessList struct {
	Items []AgentAccessView `json:"items"`
}

// AgentAccessView is an AgentAccess as the API shows it: its spec, whether
// it is Ready for it and if not why, and where a minted token is kept. The
// token is never shown, nor its hash: it is read from the Secret, by
// whoever may read Secrets in the namespace.
type AgentAccessView struct {
	Name            string                   `json:"name"`
	ResourceVersion string                   `json:"resourceVersion"`
	Spec            v1alpha1.AgentAccessSpec `json:"spec"`
	Ready           bool                     `json:"ready"`
	Reason          string                   `json:"reason,omitempty"`
	Message         string                   `json:"message,omitempty"`
	TokenSecret     string                   `json:"tokenSecret,omitempty"`
	MintedAt        *metav1.Time             `json:"mintedAt,omitempty"`
}

// AgentAccessToken is a token just minted for an AgentAccess, replacing its
// last: shown in this one answer and never again, and kept in TokenSecret
// for whoever may read Secrets there.
type AgentAccessToken struct {
	Token       string `json:"token"`
	TokenSecret string `json:"tokenSecret"`
}

// TeamList lists every Team.
type TeamList struct {
	Items []v1alpha1.Team `json:"items"`
}

// WatchHello opens a watch stream: which hints it carries, and how long it
// stays open before the browser reconnects.
type WatchHello struct {
	Tasks           bool `json:"tasks"`
	Config          bool `json:"config"`
	Runs            bool `json:"runs"`
	LifetimeSeconds int  `json:"lifetimeSeconds"`
}

// TaskHint says a Task changed in a way the board shows: read it again. It
// carries no spec and no thread.
type TaskHint struct {
	Name   string         `json:"name"`
	Number int64          `json:"number"`
	Phase  v1alpha1.Phase `json:"phase"`
	Parent string         `json:"parent,omitempty"`
}

// ConfigHint says a configuration resource or the Team of a namespace was
// made, changed or deleted: read it again. It carries no spec and no status.
type ConfigHint struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
}

// WatchEnd closes a watch stream, with one of EndLifetime, EndShutdown or
// EndNoRole.
type WatchEnd struct {
	Reason string `json:"reason"`
}
