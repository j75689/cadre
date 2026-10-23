// Package anthropic is the builtin loop's Provider for the Anthropic
// Messages API. Bedrock and Vertex serve the same API; they will reuse this
// provider with the SDK's Mantle and Vertex clients.
package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"unicode/utf8"

	sdk "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/anthropics/anthropic-sdk-go/packages/param"
	"github.com/anthropics/anthropic-sdk-go/shared/constant"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	"github.com/j75689/cadre/internal/builtin"
	"github.com/j75689/cadre/internal/runner"
)

// Request settings.
const (
	// maxTokens leaves the model room for a long turn; responses are
	// streamed, so a long one cannot hit an HTTP timeout.
	maxTokens   = 64000
	defaultBase = "https://api.anthropic.com"
)

// Provider is the Anthropic provider.
type Provider struct {
	Log *slog.Logger
	// HTTPClient carries the API requests; default a plain client, whose
	// requests end with the run's context.
	HTTPClient *http.Client
	// Endpoint is Anthropic's own API, used when a ModelProvider names no
	// baseURL; default https://api.anthropic.com. It is for tests.
	Endpoint string
}

// Start opens a conversation. The client takes its credential from the
// ModelProvider's Secret only: the SDK's environment defaults (API key,
// auth token, profiles, base URL from the environment) are turned off, so
// nothing in the runner's environment can stand in for it.
func (p *Provider) Start(ctx context.Context, s builtin.Setup) (builtin.Conversation, error) {
	if s.Credential == "" {
		return nil, builtin.Permanent(errors.New("an anthropic ModelProvider needs auth.secretRef"))
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
		base = p.Endpoint
	}
	if base == "" {
		base = defaultBase
	}
	client := sdk.NewClient(option.WithoutEnvironmentDefaults(), option.WithHTTPClient(hc), option.WithBaseURL(base), option.WithAPIKey(s.Credential))

	tools := make([]sdk.BetaToolUnionParam, 0, len(s.Tools))
	for _, t := range s.Tools {
		schema, err := inputSchema(t.InputSchema)
		if err != nil {
			return nil, builtin.Permanent(fmt.Errorf("tool %s: %w", t.Name, err))
		}
		tool := sdk.BetaToolParam{Name: t.Name, InputSchema: schema}
		if t.Description != "" {
			tool.Description = param.NewOpt(t.Description)
		}
		tools = append(tools, sdk.BetaToolUnionParam{OfTool: &tool})
	}
	effort := s.Effort
	if effort == "" {
		effort = v1alpha1.EffortHigh
	}
	c := &conversation{
		client: client, log: log, model: s.Model.Model,
		params: sdk.BetaMessageNewParams{
			Model:     s.Model.Model,
			MaxTokens: maxTokens,
			System:    []sdk.BetaTextBlockParam{{Text: s.System}},
			Tools:     tools,
			// One breakpoint, placed by the API on the last cacheable
			// block: tools, system and the history so far are a stable
			// prefix on every turn.
			CacheControl: sdk.NewBetaCacheControlEphemeralParam(),
		},
		effort:   sdk.BetaOutputConfigEffort(effort),
		thinking: true,
		// Server-side fallbacks re-serve a refused request on another
		// model; they exist on Anthropic's own API only, so not through a
		// gateway.
		fallbacks: s.Model.Type == v1alpha1.ProviderAnthropic && s.Model.BaseURL == "",
	}
	c.limit = c.contextWindow(ctx)
	c.params.Messages = []sdk.BetaMessageParam{sdk.NewBetaUserMessage(sdk.NewBetaTextBlock(s.Prompt(c.limit)))}
	return c, nil
}

// inputSchema turns a tool's JSON object schema into the SDK's parameter,
// keeping every keyword.
func inputSchema(raw json.RawMessage) (sdk.BetaToolInputSchemaParam, error) {
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return sdk.BetaToolInputSchemaParam{}, fmt.Errorf("input schema: %w", err)
	}
	schema := sdk.BetaToolInputSchemaParam{Properties: m["properties"]}
	if req, ok := m["required"].([]any); ok {
		for _, r := range req {
			if name, ok := r.(string); ok {
				schema.Required = append(schema.Required, name)
			}
		}
	}
	delete(m, "type")
	delete(m, "properties")
	delete(m, "required")
	if len(m) > 0 {
		schema.ExtraFields = m
	}
	return schema, nil
}

type conversation struct {
	client sdk.Client
	log    *slog.Logger
	model  string
	params sdk.BetaMessageNewParams
	limit  int64
	// Optional request features. A model that rejects one gets the
	// request again without it, and the run remembers.
	effort              sdk.BetaOutputConfigEffort // "" once rejected
	thinking, fallbacks bool
}

// contextWindow reads the model's window from the Models API, once per
// run; 0 when it cannot be read (a gateway without the Models API, say).
func (c *conversation) contextWindow(ctx context.Context) int64 {
	info, err := c.client.Models.Get(ctx, c.model, sdk.ModelGetParams{})
	if err != nil {
		c.log.Warn("read the model's context window", "model", c.model, "err", classify(err))
		return 0
	}
	return info.MaxInputTokens
}

func (c *conversation) ContextLimit() int64 { return c.limit }

// maxPauses bounds how often one turn is continued after the API paused it.
const maxPauses = 5

func (c *conversation) Turn(ctx context.Context) (*builtin.Reply, error) {
	for pauses := 0; ; {
		msg, err := c.send(ctx)
		if err != nil {
			return nil, err
		}
		c.params.Messages = append(c.params.Messages, msg.ToParam())
		if msg.StopReason == sdk.BetaStopReasonPauseTurn && pauses < maxPauses {
			pauses++
			continue // the API paused a long turn; sending it back resumes it
		}
		return reply(msg), nil
	}
}

// send makes one streamed request, dropping an optional feature the model
// rejects and trying once more without it.
func (c *conversation) send(ctx context.Context) (*sdk.BetaMessage, error) {
	for {
		msg, err := c.stream(ctx)
		if err == nil {
			return msg, nil
		}
		switch feature := rejected(err); {
		case feature == "thinking" && c.thinking:
			c.log.Warn("the model rejects adaptive thinking; continuing without it", "model", c.model)
			c.thinking = false
		case feature == "effort" && c.effort != "":
			c.log.Warn("the model rejects effort; continuing without it", "model", c.model)
			c.effort = ""
		case feature == "fallbacks" && c.fallbacks:
			c.log.Warn("the API rejects server-side fallbacks; continuing without them", "model", c.model)
			c.fallbacks = false
		default:
			return nil, classify(err)
		}
	}
}

func (c *conversation) stream(ctx context.Context) (*sdk.BetaMessage, error) {
	params := c.params
	if c.effort != "" {
		params.OutputConfig = sdk.BetaOutputConfigParam{Effort: c.effort}
	}
	if c.thinking {
		params.Thinking = sdk.BetaThinkingConfigParamUnion{OfAdaptive: &sdk.BetaThinkingConfigAdaptiveParam{}}
	}
	if c.fallbacks {
		params.Fallbacks = sdk.BetaFallbacksParamUnion{OfDefault: constant.ValueOf[constant.Default]()}
		params.Betas = []sdk.AnthropicBeta{sdk.AnthropicBetaServerSideFallback2026_07_01}
	}
	stream := c.client.Beta.Messages.NewStreaming(ctx, params)
	defer func() { _ = stream.Close() }()
	var msg sdk.BetaMessage
	for stream.Next() {
		if err := msg.Accumulate(stream.Current()); err != nil {
			return nil, fmt.Errorf("read the model's reply: %w", err)
		}
	}
	if err := stream.Err(); err != nil {
		return nil, err
	}
	return &msg, nil
}

// rejected names the optional feature a 400 is about, if it is about one.
func rejected(err error) string {
	var apiErr *sdk.Error
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusBadRequest {
		return ""
	}
	msg := strings.ToLower(message(apiErr))
	switch {
	case strings.Contains(msg, "thinking"):
		return "thinking"
	case strings.Contains(msg, "fallback"):
		return "fallbacks"
	case strings.Contains(msg, "effort"):
		return "effort"
	}
	return ""
}

// classify marks the errors retrying cannot fix: every 4xx but a timeout,
// a conflict and a rate limit. Server errors, overload and network
// failures stay retriable. An API error is reduced to its status, request
// id, error type and message: the SDK's own text carries the raw response
// body, which a gateway may fill with anything, so it is never logged.
func classify(err error) error {
	var apiErr *sdk.Error
	if !errors.As(err, &apiErr) {
		return err
	}
	reduced := apiError{status: apiErr.StatusCode, requestID: apiErr.RequestID, message: message(apiErr)}
	switch s := apiErr.StatusCode; {
	case s == http.StatusRequestTimeout, s == http.StatusConflict, s == http.StatusTooManyRequests, s >= 500:
		return reduced
	case s >= 400:
		return builtin.Permanent(reduced)
	}
	return reduced
}

// apiError is an API error without its response body.
type apiError struct {
	status    int
	requestID string
	message   string
}

func (e apiError) Error() string {
	msg := fmt.Sprintf("the Messages API answered %d %s", e.status, http.StatusText(e.status))
	if e.message != "" {
		msg += ": " + e.message
	}
	if e.requestID != "" {
		msg += " (request " + e.requestID + ")"
	}
	return msg
}

// maxMessage bounds how much of an API error's message is kept.
const maxMessage = 300

// message is an API error's type and message, from its body, clipped.
func message(e *sdk.Error) string {
	var body struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal([]byte(e.RawJSON()), &body) != nil {
		return ""
	}
	msg := strings.TrimSpace(body.Error.Type + ": " + body.Error.Message)
	if len(msg) > maxMessage {
		cut := maxMessage
		for cut > 0 && !utf8.RuneStart(msg[cut]) {
			cut--
		}
		msg = msg[:cut] + "…"
	}
	return strings.TrimPrefix(msg, ": ")
}

func (c *conversation) AddResults(results []builtin.ToolResult) {
	blocks := make([]sdk.BetaContentBlockParamUnion, 0, len(results))
	for _, r := range results {
		blocks = append(blocks, sdk.NewBetaToolResultBlock(r.CallID, r.Text, r.IsError))
	}
	c.params.Messages = append(c.params.Messages, sdk.NewBetaUserMessage(blocks...))
}

// reply is what the loop needs from a message.
func reply(msg *sdk.BetaMessage) *builtin.Reply {
	u := msg.Usage
	out := &builtin.Reply{
		Usage: runner.Usage{
			InputTokens: u.InputTokens, OutputTokens: u.OutputTokens,
			CacheReadInputTokens: u.CacheReadInputTokens, CacheCreationInputTokens: u.CacheCreationInputTokens,
		},
		ContextTokens: u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens + u.OutputTokens,
	}
	var text []string
	for _, block := range msg.Content {
		switch b := block.AsAny().(type) {
		case sdk.BetaTextBlock:
			text = append(text, b.Text)
		case sdk.BetaToolUseBlock:
			input, err := json.Marshal(b.Input)
			if err != nil || b.Input == nil {
				input = []byte("{}")
			}
			out.Calls = append(out.Calls, builtin.ToolCall{ID: b.ID, Name: b.Name, Input: input})
		}
	}
	out.Text = strings.Join(text, "\n")
	switch msg.StopReason {
	case sdk.BetaStopReasonToolUse:
		out.Stop = builtin.StopToolUse
	case sdk.BetaStopReasonMaxTokens:
		out.Stop = builtin.StopMaxTokens
	case sdk.BetaStopReasonRefusal:
		out.Stop = builtin.StopRefusal
		out.RefusalCategory = string(msg.StopDetails.Category)
	case sdk.BetaStopReasonModelContextWindowExceeded:
		out.Stop = builtin.StopContextFull
	default:
		out.Stop = builtin.StopEndTurn
	}
	return out
}
