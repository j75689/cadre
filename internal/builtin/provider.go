package builtin

import (
	"context"
	"encoding/json"
	"errors"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	"github.com/j75689/cadre/internal/runner"
)

// A Provider is one model API, such as anthropic or openai. Bedrock and
// Vertex serve the same Messages API as anthropic: they are the anthropic
// provider with a different client, not providers of their own.
type Provider interface {
	// Start opens a conversation for one run. It fails with an error
	// marked Permanent when the run cannot start at all, a missing
	// credential say.
	Start(ctx context.Context, s Setup) (Conversation, error)
}

// Setup is what a conversation starts from.
type Setup struct {
	Model  runner.ClaimedModel
	Effort v1alpha1.Effort
	// Credential is the value of the ModelProvider's Secret key, empty
	// when it has none. It lives for the run only and is never logged.
	Credential string
	System     string
	// Prompt renders the first user message, the task and its thread, for
	// a model whose context window is contextTokens (0 if unknown): the
	// provider calls it once it knows the window.
	Prompt func(contextTokens int64) string
	// Tools are the run's MCP tools, sorted by name.
	Tools []Tool
}

// Tool is a tool offered to the model.
type Tool struct {
	Name        string
	Description string
	InputSchema json.RawMessage // a JSON object schema
}

// A Conversation keeps its provider's own history types, append-only, so
// what the model returned (thinking blocks included) goes back unchanged.
type Conversation interface {
	// Turn sends the conversation and appends the model's reply to it.
	// On an error nothing is appended, so a Turn may be retried.
	Turn(ctx context.Context) (*Reply, error)
	// AddResults appends the results of the last reply's tool calls, all
	// in one message, in the order of the calls.
	AddResults(results []ToolResult)
	// ContextLimit is the model's context window in tokens; 0 if unknown.
	ContextLimit() int64
}

// Stop is why the model stopped.
type Stop string

// Stops.
const (
	StopEndTurn   Stop = "end_turn"
	StopToolUse   Stop = "tool_use"
	StopMaxTokens Stop = "max_tokens"
	StopRefusal   Stop = "refusal"
	// StopContextFull is the model's context window filling up.
	StopContextFull Stop = "context_full"
	// StopPaused is a turn the API kept pausing, past what the provider
	// resumes.
	StopPaused Stop = "paused"
)

// Reply is one model response.
type Reply struct {
	Stop  Stop
	Text  string // the reply's text, joined
	Calls []ToolCall
	Usage runner.Usage
	// ContextTokens is the size of the conversation after this reply.
	ContextTokens int64
	// RefusalCategory says why a refusal happened, when the API says.
	RefusalCategory string
}

// ToolCall is a call the model asked for.
type ToolCall struct {
	ID    string
	Name  string
	Input json.RawMessage
}

// ToolResult is what a call returned.
type ToolResult struct {
	CallID  string
	Text    string
	IsError bool
}

// Permanent marks an error that retrying cannot fix: a bad request, a
// rejected credential, an unknown model. Other provider errors are retried
// with backoff for a while.
func Permanent(err error) error { return permanent{err} }

type permanent struct{ error }

func (p permanent) Unwrap() error { return p.error }

// IsPermanent reports whether err was marked Permanent.
func IsPermanent(err error) bool { return isPermanent(err) }

func isPermanent(err error) bool {
	var p permanent
	return errors.As(err, &p)
}
