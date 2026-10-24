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
	"slices"
	"strings"

	oai "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/packages/param"
	"github.com/openai/openai-go/v3/packages/ssestream"
	"github.com/openai/openai-go/v3/shared"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	"github.com/j75689/cadre/internal/builtin"
	"github.com/j75689/cadre/internal/provider"
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
	c, err := p.open(s)
	if err != nil {
		return nil, err
	}
	// The window is not known here (the Models API of these endpoints does
	// not say), so the prompt is rendered for an unknown window.
	c.params.Messages = []oai.ChatCompletionMessageParamUnion{oai.SystemMessage(s.System), oai.UserMessage(s.Prompt(0))}
	return c, nil
}

// sdkModule is the SDK whose request building a recorded conversation
// depends on.
const sdkModule = "github.com/openai/openai-go/v3"

// Begin renders the first message, as Start does, and returns it with the
// rest of the conversation's opening.
func (p *Provider) Begin(_ context.Context, s builtin.Setup) (*builtin.Opening, error) {
	c, err := p.open(s)
	if err != nil {
		return nil, err
	}
	return &builtin.Opening{
		Provider: v1alpha1.ProviderOpenAI, SDK: provider.ModuleVersion(sdkModule), Model: s.Model.Model,
		System: s.System, Prompt: s.Prompt(0), Tools: s.Tools, Settings: c.settings(),
	}, nil
}

// Resume rebuilds a conversation from its opening and its recorded steps:
// the same parameters, settings and history it had live.
func (p *Provider) Resume(_ context.Context, s builtin.Setup, o *builtin.Opening, steps []builtin.Recorded) (builtin.StepConversation, error) {
	s.Model.Model, s.System, s.Tools = o.Model, o.System, o.Tools
	c, err := p.open(s)
	if err != nil {
		return nil, err
	}
	c.apply(o.Settings)
	c.params.Messages = []oai.ChatCompletionMessageParamUnion{oai.SystemMessage(o.System), oai.UserMessage(o.Prompt)}
	for i, st := range steps {
		for _, raw := range st.Messages {
			var m oai.ChatCompletionMessageParamUnion
			if err := json.Unmarshal(raw, &m); err != nil {
				return nil, builtin.Permanent(fmt.Errorf("recorded step %d: %w", i, err))
			}
			c.params.Messages = append(c.params.Messages, m)
			if m.OfAssistant != nil {
				c.pending(m.OfAssistant.ToolCalls)
			}
		}
		if st.Settings != nil {
			c.apply(*st.Settings)
		}
		if len(st.Results) > 0 {
			c.AddResults(st.Results)
		}
	}
	return c, nil
}

// open builds a conversation from s, with its chat service and the
// parameters every request carries, and no messages.
func (p *Provider) open(s builtin.Setup) (*conversation, error) {
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
	return &conversation{
		chat: chat, log: log,
		// The output cap is the server's.
		params: oai.ChatCompletionNewParams{Model: s.Model.Model, Tools: tools},
		effort: shared.ReasoningEffort(effort),
		usage:  true,
	}, nil
}

// settings are the conversation's settings, to record.
func (c *conversation) settings() builtin.Settings {
	return builtin.Settings{Effort: string(c.effort), StreamUsage: c.usage, Started: c.started}
}

// apply takes recorded settings back.
func (c *conversation) apply(s builtin.Settings) {
	c.effort, c.usage, c.started = shared.ReasoningEffort(s.Effort), s.StreamUsage, s.Started
}

// LastTurn is what the last Turn appended, as it is sent back, and the
// settings when the turn changed them.
func (c *conversation) LastTurn() builtin.Recorded { return c.last }

type conversation struct {
	chat   oai.ChatCompletionService
	log    *slog.Logger
	params oai.ChatCompletionNewParams
	// Optional request features, probed on the first request: a 400 naming
	// one gets the request again without it, for the rest of the run.
	// Many compatible servers take neither reasoning effort nor stream
	// usage.
	effort  shared.ReasoningEffort // "" when left out
	usage   bool                   // ask for usage in the stream
	started bool                   // a request has been answered
	// unanswered are the results the provider gives itself, for calls of
	// the last reply it did not hand to the loop (arguments that are not
	// JSON, a kind of tool it does not know), keyed by call id; order is
	// the reply's call order, so AddResults keeps it.
	unanswered map[string]string
	order      []string
	zeroUsage  bool // warned about a server that reports no usage
	last       builtin.Recorded
}

// ContextLimit is unknown for these endpoints: the loop has no ceiling of
// its own, and a server's context-length error ends the run as a full
// window (StopContextFull).
func (c *conversation) ContextLimit() int64 { return 0 }

func (c *conversation) Turn(ctx context.Context) (*builtin.Reply, error) {
	before, from := c.settings(), len(c.params.Messages)
	r, err := c.turn(ctx)
	if err != nil {
		return nil, err
	}
	var rec builtin.Recorded
	for _, m := range c.params.Messages[from:] {
		raw, err := json.Marshal(m)
		if err != nil {
			c.params.Messages = c.params.Messages[:from]
			return nil, builtin.Permanent(fmt.Errorf("record the model's reply: %w", err))
		}
		rec.Messages = append(rec.Messages, raw)
	}
	if now := c.settings(); now != before {
		rec.Settings = &now
	}
	c.last = rec
	return r, nil
}

func (c *conversation) turn(ctx context.Context) (*builtin.Reply, error) {
	for {
		completion, err := c.stream(ctx)
		if err == nil {
			c.started = true
			if len(completion.Choices) == 0 {
				return nil, errors.New("the model's reply has no choices")
			}
			c.params.Messages = append(c.params.Messages, completion.Choices[0].Message.ToParam())
			return c.reply(completion), nil
		}
		e, ok := apiError(err)
		if ok && contextFull(e) {
			return &builtin.Reply{Stop: builtin.StopContextFull}, nil
		}
		if !ok || c.started {
			return nil, classified(e, ok, err)
		}
		switch feature := rejected(e); {
		case feature == "reasoning" && c.effort != "":
			c.log.Warn("the model rejects reasoning effort; continuing without it", "model", c.params.Model)
			c.effort = ""
		case feature == "stream_options" && c.usage:
			c.log.Warn("the server rejects stream usage; continuing without it", "model", c.params.Model)
			c.usage = false
		default:
			return nil, provider.Classify(e)
		}
	}
}

func classified(e *provider.APIError, ok bool, err error) error {
	if !ok {
		return err
	}
	return provider.Classify(e)
}

// errChunks is a stream whose chunks do not fit together.
var errChunks = errors.New("the model's streamed reply did not fit together")

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
		if !acc.AddChunk(stream.Current()) {
			return nil, errChunks
		}
	}
	if err := stream.Err(); err != nil {
		return nil, err
	}
	return &acc.ChatCompletion, nil
}

// AddResults appends one tool message per call of the last reply, in the
// reply's order: the loop's results, and the provider's own for the calls
// it did not hand on. The API has no error flag on a tool result, so a
// failure says so in its text.
func (c *conversation) AddResults(results []builtin.ToolResult) {
	byID := make(map[string]string, len(results)+len(c.unanswered))
	for _, r := range results {
		text := r.Text
		if r.IsError {
			text = "Error: " + text
		}
		byID[r.CallID] = text
	}
	for id, text := range c.unanswered {
		byID[id] = text
	}
	for _, id := range c.order {
		if text, ok := byID[id]; ok {
			c.params.Messages = append(c.params.Messages, oai.ToolMessage(text, id))
			delete(byID, id)
		}
	}
	// Results for calls the reply did not have (none are expected) still
	// go back, so nothing the loop did is lost.
	for _, r := range results {
		if text, ok := byID[r.CallID]; ok {
			c.params.Messages = append(c.params.Messages, oai.ToolMessage(text, r.CallID))
		}
	}
	c.unanswered, c.order = nil, nil
}

// pending notes the calls of a reply in order, and answers itself the ones
// it does not hand to the loop: a kind of tool it does not offer, arguments
// that are not JSON.
func (c *conversation) pending(calls []oai.ChatCompletionMessageToolCallUnionParam) {
	c.unanswered, c.order = map[string]string{}, nil
	for _, call := range calls {
		switch {
		case call.OfFunction != nil:
			id := call.OfFunction.ID
			c.order = append(c.order, id)
			if !json.Valid([]byte(call.OfFunction.Function.Arguments)) {
				c.unanswered[id] = "Error: the call's arguments are not valid JSON, so it was not made. Call it again with JSON arguments."
			}
		case call.OfCustom != nil:
			id := call.OfCustom.ID
			c.order = append(c.order, id)
			c.unanswered[id] = fmt.Sprintf("Error: tools of type %q are not offered; call one of the functions you were given.", "custom")
		}
	}
}

func (c *conversation) reply(completion *oai.ChatCompletion) *builtin.Reply {
	choice := completion.Choices[0]
	u := completion.Usage
	if u.PromptTokens == 0 && u.CompletionTokens == 0 && !c.zeroUsage {
		c.zeroUsage = true
		c.log.Warn("the server reports no token usage; the run's events will show none", "model", c.params.Model)
	}
	cached := u.PromptTokensDetails.CachedTokens
	out := &builtin.Reply{
		Text: choice.Message.Content,
		Usage: runner.Usage{
			InputTokens: u.PromptTokens - cached, OutputTokens: u.CompletionTokens, CacheReadInputTokens: cached,
		},
		ContextTokens: u.PromptTokens + u.CompletionTokens,
	}
	switch {
	case choice.Message.Refusal != "", choice.FinishReason == "content_filter":
		out.Stop, out.Text = builtin.StopRefusal, choice.Message.Refusal
		return out
	case choice.FinishReason == "length":
		// A reply cut off at its output limit: its last call's arguments
		// may be cut too, so none of its calls is made.
		out.Stop = builtin.StopMaxTokens
		return out
	}
	c.pending(choice.Message.ToParam().OfAssistant.ToolCalls)
	for _, call := range choice.Message.ToolCalls {
		switch _, answered := c.unanswered[call.ID]; {
		case !slices.Contains(c.order, call.ID):
			// A kind of call the SDK has no parameter for.
			c.order = append(c.order, call.ID)
			c.unanswered[call.ID] = fmt.Sprintf("Error: tools of type %q are not offered; call one of the functions you were given.", call.Type)
		case !answered:
			out.Calls = append(out.Calls, builtin.ToolCall{ID: call.ID, Name: call.Function.Name, Input: json.RawMessage(call.Function.Arguments)})
		}
	}
	if len(choice.Message.ToolCalls) > 0 {
		out.Stop = builtin.StopToolUse
	} else {
		out.Stop = builtin.StopEndTurn
	}
	return out
}

// apiError reduces an error from the API, a status error or an error event
// inside a stream, to what may be logged.
func apiError(err error) (*provider.APIError, bool) {
	var status *oai.Error
	if errors.As(err, &status) {
		return &provider.APIError{API: api, Status: status.StatusCode, Kind: status.Type, Code: status.Code, Message: status.Message, Param: status.Param}, true
	}
	var stream *ssestream.StreamError
	if errors.As(err, &stream) {
		var body struct {
			Error struct {
				Type    string          `json:"type"`
				Code    json.RawMessage `json:"code"`
				Message string          `json:"message"`
				Param   string          `json:"param"`
			} `json:"error"`
		}
		_ = json.Unmarshal(stream.Event.Data, &body)
		code := strings.Trim(string(body.Error.Code), `"`)
		if code == "null" {
			code = ""
		}
		return &provider.APIError{API: api, Kind: body.Error.Type, Code: code, Message: body.Error.Message, Param: body.Error.Param}, true
	}
	return nil, false
}

const api = "the Chat Completions API"

// contextFull reports whether e says the conversation no longer fits the
// model's window.
func contextFull(e *provider.APIError) bool {
	msg := strings.ToLower(e.Message)
	return e.Code == "context_length_exceeded" || strings.Contains(msg, "maximum context length") || strings.Contains(msg, "context length exceeded")
}

// rejected names the optional feature a first request's 400 is about.
func rejected(e *provider.APIError) string {
	if e.Status != http.StatusBadRequest {
		return ""
	}
	msg := strings.ToLower(e.Message)
	switch {
	case e.Param == "reasoning_effort", strings.Contains(msg, "reasoning"):
		return "reasoning"
	case strings.HasPrefix(e.Param, "stream_options"), strings.Contains(msg, "stream_options"), strings.Contains(msg, "include_usage"):
		return "stream_options"
	}
	return ""
}
