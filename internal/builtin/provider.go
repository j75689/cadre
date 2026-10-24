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
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"inputSchema"` // a JSON object schema
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
	CallID  string `json:"callId"`
	Text    string `json:"text"`
	IsError bool   `json:"isError,omitempty"`
}

// A StepProvider can also run a conversation a step at a time, each step
// perhaps on another worker: the conversation is rebuilt from what its
// steps recorded, and sends the same requests it would have sent live.
type StepProvider interface {
	Provider
	// Begin describes the model and builds the conversation's opening,
	// sending nothing: step 0's record.
	Begin(ctx context.Context, s Setup) (*Opening, error)
	// Resume rebuilds the conversation from its opening and the steps
	// recorded since, in order. Of s it takes the model's endpoint and
	// the credential; the rest is the opening's.
	Resume(ctx context.Context, s Setup, o *Opening, steps []Recorded) (StepConversation, error)
}

// Opening is what a conversation's first request is built from, the same
// way every time: the inputs, not the request.
type Opening struct {
	Provider v1alpha1.ProviderType `json:"provider"`
	SDK      string                `json:"sdk"` // the provider's SDK module@version
	Model    string                `json:"model"`
	System   string                `json:"system"`
	Prompt   string                `json:"prompt"` // the first user message, as rendered
	Tools    []Tool                `json:"tools"`
	Settings Settings              `json:"settings"`
}

// Settings are what a provider learned about the model, from its Models API
// or its first request, and the optional request features it uses.
type Settings struct {
	ContextLimit int64  `json:"contextLimit,omitempty"`
	MaxTokens    int64  `json:"maxTokens,omitempty"`
	Effort       string `json:"effort,omitempty"` // "" when left out
	Thinking     bool   `json:"thinking,omitempty"`
	Fallbacks    bool   `json:"fallbacks,omitempty"`
	StreamUsage  bool   `json:"streamUsage,omitempty"`
	// Probe is a model the Models API could not describe, whose features
	// the first request probes.
	Probe bool `json:"probe,omitempty"`
	// Started is a conversation whose first request was answered: its
	// features are settled.
	Started bool `json:"started,omitempty"`
}

// Recorded is what one step added to a conversation: a turn's messages, as
// the provider sends them back, and its settings when the turn changed
// them; or the results of the turn's calls, perhaps none (a provider
// answers some calls itself when the results are added).
type Recorded struct {
	Kind     RecordKind        `json:"kind"`
	Messages []json.RawMessage `json:"messages,omitempty"`
	Settings *Settings         `json:"settings,omitempty"`
	Results  []ToolResult      `json:"results,omitempty"`
}

// RecordKind says what a step recorded.
type RecordKind string

// What a step records.
const (
	RecordedTurn    RecordKind = "turn"
	RecordedResults RecordKind = "results"
)

// ResultsRecord is the record of a step that ran a turn's calls.
func ResultsRecord(results []ToolResult) Recorded {
	return Recorded{Kind: RecordedResults, Results: results}
}

// A StepConversation is a Conversation whose turns can be recorded.
type StepConversation interface {
	Conversation
	// LastTurn is what the last Turn added, to record.
	LastTurn() Recorded
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
