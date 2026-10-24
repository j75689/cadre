// Package openai is the builtin loop's Provider for OpenAI-compatible Chat
// Completions endpoints: OpenAI's own API, or any server at a
// ModelProvider's baseURL that speaks it (vLLM, Ollama, LiteLLM, a
// gateway).
package openai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	oai "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/packages/param"
	"github.com/openai/openai-go/v3/shared"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	"github.com/j75689/cadre/internal/builtin"
	"github.com/j75689/cadre/internal/runes"
	"github.com/j75689/cadre/internal/runner"
)

const defaultBase = "https://api.openai.com/v1/"

// Provider is the OpenAI-compatible provider.
type Provider struct {
	Log *slog.Logger
	// HTTPClient carries the API requests; default a plain client, whose
	// requests end with the run's context.
	HTTPClient *http.Client
}

// Start opens a conversation. The chat service is built from explicit
// options only, not through the SDK's client constructor, which reads
// OPENAI_API_KEY, OPENAI_BASE_URL, organization and project ids and custom
// headers from the environment: the credential is the ModelProvider's
// Secret and nothing else.
func (p *Provider) Start(_ context.Context, s builtin.Setup) (builtin.Conversation, error) {
	if s.Credential == "" {
		return nil, builtin.Permanent(errors.New("an openai ModelProvider needs auth.secretRef"))
	}
	log := p.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	hc := p.HTTPClient
	if hc == nil {
		hc = &http.Client{}
	}
	base := s.Model.BaseURL
	if base == "" {
		base = defaultBase
	}
	chat := oai.NewChatCompletionService(option.WithHTTPClient(hc), option.WithBaseURL(base), option.WithAPIKey(s.Credential))

	tools := make([]oai.ChatCompletionToolUnionParam, 0, len(s.Tools))
	for _, t := range s.Tools {
		var schema map[string]any
		if err := json.Unmarshal(t.InputSchema, &schema); err != nil {
			return nil, builtin.Permanent(fmt.Errorf("tool %s: input schema: %w", t.Name, err))
		}
		fn := shared.FunctionDefinitionParam{Name: t.Name, Parameters: schema}
		if t.Description != "" {
			fn.Description = param.NewOpt(t.Description)
		}
		tools = append(tools, oai.ChatCompletionFunctionTool(fn))
	}
	effort := s.Effort
	if effort == "" {
		effort = v1alpha1.EffortHigh
	}
	c := &conversation{
		chat: chat, log: log, model: s.Model.Model,
		params: oai.ChatCompletionNewParams{
			Model: s.Model.Model,
			Tools: tools,
			// The window is not known here (the Models API of these
			// endpoints does not say), so the prompt is rendered for an
			// unknown window, and the output cap is the server's.
			Messages: []oai.ChatCompletionMessageParamUnion{oai.SystemMessage(s.System), oai.UserMessage(s.Prompt(0))},
		},
		effort: shared.ReasoningEffort(effort),
		usage:  true,
	}
	return c, nil
}

type conversation struct {
	chat   oai.ChatCompletionService
	log    *slog.Logger
	model  string
	params oai.ChatCompletionNewParams
	// Optional request features, probed on the first request: a 400 naming
	// one gets the request again without it, for the rest of the run.
	// Many compatible servers take neither reasoning effort nor stream
	// usage.
	effort  shared.ReasoningEffort // "" when left out
	usage   bool                   // ask for usage in the stream
	started bool                   // a request has been answered
}

// ContextLimit is unknown for these endpoints: the loop has no ceiling of
// its own, and a server's context-length error ends the run.
func (c *conversation) ContextLimit() int64 { return 0 }

func (c *conversation) Turn(ctx context.Context) (*builtin.Reply, error) {
	for {
		completion, err := c.stream(ctx)
		if err == nil {
			c.started = true
			if len(completion.Choices) == 0 {
				return nil, errors.New("the model's reply has no choices")
			}
			c.params.Messages = append(c.params.Messages, completion.Choices[0].Message.ToParam())
			return reply(completion), nil
		}
		if c.started {
			return nil, classify(err)
		}
		switch feature := rejected(err); {
		case feature == "reasoning" && c.effort != "":
			c.log.Warn("the model rejects reasoning effort; continuing without it", "model", c.model)
			c.effort = ""
		case feature == "stream_options" && c.usage:
			c.log.Warn("the server rejects stream usage; continuing without it", "model", c.model)
			c.usage = false
		default:
			return nil, classify(err)
		}
	}
}

func (c *conversation) stream(ctx context.Context) (*oai.ChatCompletion, error) {
	params := c.params
	if c.effort != "" {
		params.ReasoningEffort = c.effort
	}
	if c.usage {
		params.StreamOptions = oai.ChatCompletionStreamOptionsParam{IncludeUsage: param.NewOpt(true)}
	}
	stream := c.chat.NewStreaming(ctx, params)
	defer func() { _ = stream.Close() }()
	var acc oai.ChatCompletionAccumulator
	for stream.Next() {
		acc.AddChunk(stream.Current())
	}
	if err := stream.Err(); err != nil {
		return nil, err
	}
	return &acc.ChatCompletion, nil
}

// AddResults appends one tool message per result, the Chat Completions
// form. The API has no error flag on a tool result, so a failure says so in
// its text.
func (c *conversation) AddResults(results []builtin.ToolResult) {
	for _, r := range results {
		text := r.Text
		if r.IsError {
			text = "Error: " + text
		}
		c.params.Messages = append(c.params.Messages, oai.ToolMessage(text, r.CallID))
	}
}

func reply(completion *oai.ChatCompletion) *builtin.Reply {
	choice := completion.Choices[0]
	u := completion.Usage
	cached := u.PromptTokensDetails.CachedTokens
	out := &builtin.Reply{
		Text: choice.Message.Content,
		Usage: runner.Usage{
			InputTokens: u.PromptTokens - cached, OutputTokens: u.CompletionTokens, CacheReadInputTokens: cached,
		},
		ContextTokens: u.PromptTokens + u.CompletionTokens,
	}
	for _, call := range choice.Message.ToolCalls {
		if call.Type != "" && call.Type != "function" {
			continue
		}
		input := json.RawMessage(call.Function.Arguments)
		if !json.Valid(input) {
			input = json.RawMessage("{}")
		}
		out.Calls = append(out.Calls, builtin.ToolCall{ID: call.ID, Name: call.Function.Name, Input: input})
	}
	switch {
	case choice.Message.Refusal != "", choice.FinishReason == "content_filter":
		out.Stop = builtin.StopRefusal
		out.Text = choice.Message.Refusal
	case len(out.Calls) > 0:
		out.Stop = builtin.StopToolUse
	case choice.FinishReason == "length":
		out.Stop = builtin.StopMaxTokens
	default:
		out.Stop = builtin.StopEndTurn
	}
	return out
}

// rejected names the optional feature a first request's 400 is about.
func rejected(err error) string {
	var apiErr *oai.Error
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusBadRequest {
		return ""
	}
	_, msg := errorBody(apiErr)
	msg = strings.ToLower(msg)
	switch {
	case strings.Contains(msg, "reasoning"):
		return "reasoning"
	case strings.Contains(msg, "stream_options"), strings.Contains(msg, "include_usage"):
		return "stream_options"
	}
	return ""
}

// classify marks the errors retrying cannot fix: every 4xx but a timeout,
// a conflict and a rate limit. An API error is reduced to its status,
// error type and message; nothing else of the response is kept.
func classify(err error) error {
	var apiErr *oai.Error
	if !errors.As(err, &apiErr) {
		return err
	}
	kind, msg := errorBody(apiErr)
	reduced := apiError{status: apiErr.StatusCode, message: msg}
	if kind != "" {
		reduced.message = kind + ": " + msg
	}
	if len(reduced.message) > maxMessage {
		reduced.message = runes.Cut(reduced.message, maxMessage) + "…"
	}
	switch s := apiErr.StatusCode; {
	case s == http.StatusRequestTimeout, s == http.StatusConflict, s == http.StatusTooManyRequests, s >= 500:
		return reduced
	case s >= 400:
		return builtin.Permanent(reduced)
	}
	return reduced
}

// maxMessage bounds how much of an API error's message is kept.
const maxMessage = 300

type apiError struct {
	status  int
	message string
}

func (e apiError) Error() string {
	msg := fmt.Sprintf("the Chat Completions API answered %d %s", e.status, http.StatusText(e.status))
	if e.message != "" {
		msg += ": " + e.message
	}
	return msg
}

// errorBody is an API error's type and message, as the SDK decoded them.
func errorBody(e *oai.Error) (kind, msg string) { return e.Type, e.Message }
