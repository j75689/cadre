// Package anthropic is the builtin loop's Provider for the Anthropic
// Messages API. Bedrock and Vertex serve the same API; they will reuse this
// provider with the SDK's Mantle and Vertex clients.
package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	sdk "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/anthropics/anthropic-sdk-go/packages/param"
	"github.com/anthropics/anthropic-sdk-go/shared/constant"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	"github.com/j75689/cadre/internal/builtin"
	"github.com/j75689/cadre/internal/provider"
	"github.com/j75689/cadre/internal/runner"
)

// Request settings.
const (
	// maxTokens is the output cap when the Models API cannot say the
	// model's own; it leaves room for a long turn, and responses are
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
	c, err := p.open(s)
	if err != nil {
		return nil, err
	}
	if err := c.describe(ctx); err != nil {
		return nil, err
	}
	c.params.Messages = []sdk.BetaMessageParam{sdk.NewBetaUserMessage(sdk.NewBetaTextBlock(s.Prompt(c.limit)))}
	return c, nil
}

// sdkModule is the SDK whose request building a recorded conversation
// depends on.
const sdkModule = "github.com/anthropics/anthropic-sdk-go"

// Begin describes the model and renders the first message, as Start does,
// and returns them as the conversation's opening.
func (p *Provider) Begin(ctx context.Context, s builtin.Setup) (*builtin.Opening, error) {
	c, err := p.open(s)
	if err != nil {
		return nil, err
	}
	if err := c.describe(ctx); err != nil {
		return nil, err
	}
	return &builtin.Opening{
		Provider: v1alpha1.ProviderAnthropic, SDK: provider.ModuleVersion(sdkModule), Model: s.Model.Model,
		System: s.System, Prompt: s.Prompt(c.limit), Tools: s.Tools, Settings: c.settings(),
	}, nil
}

// Resume rebuilds a conversation from its opening and its recorded steps:
// the same parameters, settings and history it had live. A recorded turn
// goes back as it was recorded, byte for byte, never decoded into the
// SDK's types and encoded again: a tool call's input keeps its keys' order
// and its numbers, and a block this SDK does not know stays as it came.
func (p *Provider) Resume(_ context.Context, s builtin.Setup, o *builtin.Opening, steps []builtin.Recorded) (builtin.StepConversation, error) {
	s.Model.Model, s.System, s.Tools = o.Model, o.System, o.Tools
	c, err := p.open(s)
	if err != nil {
		return nil, err
	}
	if err := provider.CheckOpening(o, v1alpha1.ProviderAnthropic, sdkModule, c.log); err != nil {
		return nil, err
	}
	c.apply(o.Settings)
	c.params.Messages = []sdk.BetaMessageParam{sdk.NewBetaUserMessage(sdk.NewBetaTextBlock(o.Prompt))}
	for i, st := range steps {
		switch st.Kind {
		case builtin.RecordedTurn:
			for _, raw := range st.Messages {
				if !json.Valid(raw) {
					return nil, builtin.Permanent(fmt.Errorf("recorded step %d: a message that is not JSON", i))
				}
				c.params.Messages = append(c.params.Messages, param.Override[sdk.BetaMessageParam](raw))
			}
			if st.Settings != nil {
				c.apply(*st.Settings)
			}
		case builtin.RecordedResults:
			c.AddResults(st.Results)
		default:
			return nil, builtin.Permanent(fmt.Errorf("recorded step %d is of kind %q", i, st.Kind))
		}
	}
	return c, nil
}

// open builds a conversation from s, with its client and the parameters
// every request carries, and no messages.
func (p *Provider) open(s builtin.Setup) (*conversation, error) {
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
	return &conversation{
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
	}, nil
}

// settings are the conversation's settings, to record.
func (c *conversation) settings() builtin.Settings {
	return builtin.Settings{
		ContextLimit: c.limit, MaxTokens: c.params.MaxTokens, Effort: string(c.effort),
		Thinking: c.thinking, Fallbacks: c.fallbacks, Probe: c.probe, Started: c.started,
	}
}

// apply takes recorded settings back.
func (c *conversation) apply(s builtin.Settings) {
	c.limit, c.params.MaxTokens, c.effort = s.ContextLimit, s.MaxTokens, sdk.BetaOutputConfigEffort(s.Effort)
	c.thinking, c.fallbacks, c.probe, c.started = s.Thinking, s.Fallbacks, s.Probe, s.Started
}

// LastTurn is what the last Turn appended, as it is sent back, and the
// settings when the turn changed them (its first request settled them).
func (c *conversation) LastTurn() builtin.Recorded {
	return c.last
}

// describe reads what the model is from the Models API, once per run: its
// context window, its output cap, and whether it takes adaptive thinking and
// the asked effort. A transient failure fails Start, which the loop retries;
// a definitive one (a gateway without the Models API, a model it does not
// list) leaves the window unknown, the cap at maxTokens, and thinking and
// effort to be probed on the first request.
func (c *conversation) describe(ctx context.Context) error {
	info, err := c.client.Models.Get(ctx, c.model, sdk.ModelGetParams{})
	if err != nil {
		err = classify(err)
		if !builtin.IsPermanent(err) {
			return err
		}
		c.log.Warn("the Models API does not describe the model; probing it instead", "model", c.model, "err", err)
		c.params.MaxTokens, c.probe = maxTokens, true
		return nil
	}
	c.limit, c.params.MaxTokens = info.MaxInputTokens, info.MaxTokens
	if c.params.MaxTokens <= 0 {
		c.params.MaxTokens = maxTokens
	}
	caps := info.Capabilities
	c.thinking = caps.Thinking.Supported && caps.Thinking.Types.Adaptive.Supported
	if !effortSupported(caps.Effort, c.effort) {
		c.log.Warn("the model does not take this effort; leaving it out", "model", c.model, "effort", c.effort)
		c.effort = ""
	}
	return nil
}

func effortSupported(e sdk.EffortCapability, level sdk.BetaOutputConfigEffort) bool {
	if !e.Supported {
		return false
	}
	switch level {
	case sdk.BetaOutputConfigEffortLow:
		return e.Low.Supported
	case sdk.BetaOutputConfigEffortMedium:
		return e.Medium.Supported
	case sdk.BetaOutputConfigEffortHigh:
		return e.High.Supported
	case sdk.BetaOutputConfigEffortXhigh:
		return e.Xhigh.Supported
	case sdk.BetaOutputConfigEffortMax:
		return e.Max.Supported
	}
	return false
}

// inputSchema turns a tool's JSON object schema into the SDK's parameter,
// keeping every keyword. It is sent as JSON encoded from a map, whose keys
// encoding/json sorts: the same schema is the same bytes on every request,
// as the prompt cache needs.
func inputSchema(raw json.RawMessage) (sdk.BetaToolInputSchemaParam, error) {
	// Numbers are kept as written, not turned into float64.
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		return sdk.BetaToolInputSchemaParam{}, fmt.Errorf("input schema: %w", err)
	}
	if m == nil {
		return sdk.BetaToolInputSchemaParam{}, errors.New("input schema: not a JSON object")
	}
	m["type"] = "object"
	data, err := json.Marshal(m)
	if err != nil {
		return sdk.BetaToolInputSchemaParam{}, fmt.Errorf("input schema: %w", err)
	}
	return param.Override[sdk.BetaToolInputSchemaParam](json.RawMessage(data)), nil
}

type conversation struct {
	client sdk.Client
	log    *slog.Logger
	model  string
	params sdk.BetaMessageNewParams
	limit  int64
	// Optional request features, set from the Models API. Fallbacks, which
	// it does not describe, and thinking and effort when it cannot be read
	// (probe), are probed on the first request instead: a 400 naming one
	// gets the request again without it. Later requests never change
	// shape: the conversation carries what the first one started.
	effort              sdk.BetaOutputConfigEffort // "" when left out
	thinking, fallbacks bool
	probe               bool // the Models API could not describe the model
	started             bool // a request has been answered
	last                builtin.Recorded
}

// ContextLimit is the model's window, 0 when the Models API could not be
// read. The loop then has no ceiling of its own, and the API's
// model_context_window_exceeded stop (StopContextFull) ends the run instead.
func (c *conversation) ContextLimit() int64 { return c.limit }

// maxPauses bounds how often one turn is continued after the API paused it.
const maxPauses = 5

func (c *conversation) Turn(ctx context.Context) (*builtin.Reply, error) {
	c.last = builtin.Recorded{}
	before, from := c.settings(), len(c.params.Messages)
	r, err := c.turn(ctx)
	if err != nil {
		return nil, err
	}
	rec, err := provider.RecordTurn(c.params.Messages[from:], before, c.settings())
	if err != nil {
		c.params.Messages = c.params.Messages[:from]
		return nil, builtin.Permanent(err)
	}
	c.last = rec
	return r, nil
}

func (c *conversation) turn(ctx context.Context) (*builtin.Reply, error) {
	for pauses := 0; ; {
		msg, err := c.send(ctx)
		if err != nil {
			return nil, err
		}
		c.params.Messages = append(c.params.Messages, toParam(msg))
		if msg.StopReason != sdk.BetaStopReasonPauseTurn {
			return reply(msg), nil
		}
		if pauses++; pauses > maxPauses {
			r := reply(msg)
			r.Stop = builtin.StopPaused
			return r, nil
		}
		// The API paused a long turn; sending it back resumes it.
	}
}

// send makes one streamed request. On the run's first request it drops an
// optional feature the model rejects and tries again without it.
func (c *conversation) send(ctx context.Context) (*sdk.BetaMessage, error) {
	for {
		msg, err := c.stream(ctx)
		if err == nil {
			c.started = true
			return msg, nil
		}
		if c.started {
			return nil, classify(err)
		}
		switch feature := rejected(err); {
		case feature == "fallbacks" && c.fallbacks:
			c.log.Warn("the API rejects server-side fallbacks; continuing without them", "model", c.model)
			c.fallbacks = false
		case !c.probe:
			return nil, classify(err)
		case feature == "thinking" && c.thinking:
			c.log.Warn("the model rejects adaptive thinking; continuing without it", "model", c.model)
			c.thinking = false
		case feature == "effort" && c.effort != "":
			c.log.Warn("the model rejects effort; continuing without it", "model", c.model)
			c.effort = ""
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
	_, msg := errorBody(apiErr)
	msg = strings.ToLower(msg)
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

// classify reduces an API error for the log and marks what retrying
// cannot fix; see provider.Classify.
func classify(err error) error {
	var apiErr *sdk.Error
	if !errors.As(err, &apiErr) {
		return err
	}
	kind, msg := errorBody(apiErr)
	return provider.Classify(&provider.APIError{API: "the Messages API", Status: apiErr.StatusCode, Kind: kind, Message: msg, RequestID: apiErr.RequestID})
}

// errorBody is an API error's type and message, from its body.
func errorBody(e *sdk.Error) (kind, msg string) {
	var body struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal([]byte(e.RawJSON()), &body) != nil {
		return "", ""
	}
	return body.Error.Type, body.Error.Message
}

// AddResults appends the results in one user message. With none (a turn
// that stopped for tool use but asked for none) it appends nothing: the API
// refuses an empty message.
func (c *conversation) AddResults(results []builtin.ToolResult) {
	if len(results) == 0 {
		return
	}
	blocks := make([]sdk.BetaContentBlockParamUnion, 0, len(results))
	for _, r := range results {
		blocks = append(blocks, sdk.NewBetaToolResultBlock(r.CallID, r.Text, r.IsError))
	}
	c.params.Messages = append(c.params.Messages, sdk.NewBetaUserMessage(blocks...))
}

// toParam is msg as it goes back to the API. A tool call's input goes back
// as the model wrote it, not decoded and encoded again, which would sort
// its keys and round its large numbers: the call is made with those bytes
// too.
func toParam(msg *sdk.BetaMessage) sdk.BetaMessageParam {
	p := msg.ToParam()
	inputs := toolInputs(msg)
	for _, block := range p.Content {
		if tu := block.OfToolUse; tu != nil {
			if raw, ok := inputs[tu.ID]; ok {
				tu.Input = raw
			}
		}
	}
	return p
}

// toolInputs are the inputs of msg's tool calls as the model wrote them,
// by call id.
func toolInputs(msg *sdk.BetaMessage) map[string]json.RawMessage {
	inputs := map[string]json.RawMessage{}
	for _, block := range msg.Content {
		if block.Type == "tool_use" && json.Valid(block.Input) {
			inputs[block.ID] = block.Input
		}
	}
	return inputs
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
	inputs := toolInputs(msg)
	for _, block := range msg.Content {
		switch b := block.AsAny().(type) {
		case sdk.BetaTextBlock:
			text = append(text, b.Text)
		case sdk.BetaToolUseBlock:
			input, ok := inputs[b.ID]
			if !ok || len(input) == 0 {
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
