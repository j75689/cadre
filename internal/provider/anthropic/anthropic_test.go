package anthropic_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	"github.com/j75689/cadre/internal/builtin"
	"github.com/j75689/cadre/internal/provider/anthropic"
	"github.com/j75689/cadre/internal/runner"
)

const key = "test-key-not-real"

var ctx = context.Background()

// api is a fake Messages API: it answers each streamed request with the
// next scripted reply and records what it was sent.
type api struct {
	*httptest.Server
	mu       sync.Mutex
	noModels bool   // answer the Models API with 404, as a gateway might
	models   []int  // statuses to answer the Models API with first, then 200
	thinking bool   // the model takes adaptive thinking
	efforts  string // the effort levels the model takes, as JSON fields
	requests []map[string]any
	headers  []http.Header
	replies  []func(w http.ResponseWriter, body map[string]any)
}

func newAPI(t *testing.T, replies ...func(w http.ResponseWriter, body map[string]any)) *api {
	t.Helper()
	a := &api{replies: replies, thinking: true,
		efforts: `"low":{"supported":true},"medium":{"supported":true},"high":{"supported":true},"xhigh":{"supported":true},"max":{"supported":true}`}
	a.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/models/") {
			a.mu.Lock()
			var status int
			if len(a.models) > 0 {
				status, a.models = a.models[0], a.models[1:]
			}
			a.mu.Unlock()
			switch {
			case a.noModels:
				fail(404, "model not found")(w, nil)
				return
			case status != 0:
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				_, _ = io.WriteString(w, `{"type":"error","error":{"type":"overloaded_error","message":"overloaded"}}`)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"id":%q,"type":"model","display_name":"x","created_at":"2026-01-01T00:00:00Z","max_input_tokens":1000000,"max_tokens":128000,
				"capabilities":{"thinking":{"supported":%t,"types":{"adaptive":{"supported":%t},"enabled":{"supported":false}}},"effort":{"supported":true,%s}}}`,
				strings.TrimPrefix(r.URL.Path, "/v1/models/"), a.thinking, a.thinking, a.efforts)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		a.mu.Lock()
		a.requests = append(a.requests, body)
		a.headers = append(a.headers, r.Header.Clone())
		n := len(a.requests)
		a.mu.Unlock()
		if n > len(a.replies) {
			http.Error(w, "unscripted request", http.StatusInternalServerError)
			return
		}
		a.replies[n-1](w, body)
	}))
	t.Cleanup(a.Close)
	return a
}

// sse writes a streamed message: a thinking block, text, and tool calls.
func sse(stop string, text string, calls ...[2]string) func(http.ResponseWriter, map[string]any) {
	return func(w http.ResponseWriter, _ map[string]any) {
		w.Header().Set("Content-Type", "text/event-stream")
		ev := func(name, data string) { _, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, data) }
		ev("message_start", `{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-opus-5-5","content":[],"stop_reason":null,"usage":{"input_tokens":12,"output_tokens":1,"cache_read_input_tokens":300,"cache_creation_input_tokens":40}}}`)
		ev("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}`)
		ev("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig-1"}}`)
		ev("content_block_stop", `{"type":"content_block_stop","index":0}`)
		ev("content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`)
		ev("content_block_delta", fmt.Sprintf(`{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":%q}}`, text))
		ev("content_block_stop", `{"type":"content_block_stop","index":1}`)
		for i, c := range calls {
			idx := i + 2
			ev("content_block_start", fmt.Sprintf(`{"type":"content_block_start","index":%d,"content_block":{"type":"tool_use","id":%q,"name":%q,"input":{}}}`, idx, "tu_"+c[0], c[0]))
			ev("content_block_delta", fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":{"type":"input_json_delta","partial_json":%q}}`, idx, c[1]))
			ev("content_block_stop", fmt.Sprintf(`{"type":"content_block_stop","index":%d}`, idx))
		}
		ev("message_delta", fmt.Sprintf(`{"type":"message_delta","delta":{"stop_reason":%q},"usage":{"output_tokens":25}}`, stop))
		ev("message_stop", `{"type":"message_stop"}`)
	}
}

func fail(status int, msg string) func(http.ResponseWriter, map[string]any) {
	return func(w http.ResponseWriter, _ map[string]any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = fmt.Fprintf(w, `{"type":"error","error":{"type":"invalid_request_error","message":%q}}`, msg)
	}
}

func setup(base string) builtin.Setup {
	return builtin.Setup{
		Model:      runner.ClaimedModel{Type: v1alpha1.ProviderAnthropic, Model: "claude-opus-5-5", BaseURL: base},
		Effort:     v1alpha1.EffortXHigh,
		Credential: key,
		System:     "be careful",
		Prompt:     func(int64) string { return "# Task #1" },
		Tools: []builtin.Tool{
			{Name: "thread_post", Description: "post", InputSchema: json.RawMessage(`{"type":"object","properties":{"body":{"type":"string"},"kind":{"enum":["note","summary"]},"ref":{"$ref":"#/$defs/ref"}},"required":["body"],"additionalProperties":false,"$defs":{"ref":{"type":"string","minLength":1}}}`)},
		},
	}
}

func TestAConversation(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "from-the-environment")
	t.Setenv("ANTHROPIC_BASE_URL", "http://127.0.0.1:1")
	a := newAPI(t, sse("tool_use", "Posting.", [2]string{"thread_post", `{"body":"hi"}`}), sse("end_turn", "Done."))
	s := setup("")
	var window int64
	s.Prompt = func(w int64) string { window = w; return "# Task #1" }
	conv, err := (&anthropic.Provider{Endpoint: a.URL}).Start(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	if conv.ContextLimit() != 1000000 || window != 1000000 {
		t.Fatalf("context limit %d, want the Models API's", conv.ContextLimit())
	}
	r, err := conv.Turn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if r.Stop != builtin.StopToolUse || r.Text != "Posting." || len(r.Calls) != 1 || r.Calls[0].Name != "thread_post" || string(r.Calls[0].Input) != `{"body":"hi"}` {
		t.Fatalf("reply: %+v", r)
	}
	if r.Usage.InputTokens != 12 || r.Usage.OutputTokens != 25 || r.Usage.CacheReadInputTokens != 300 || r.ContextTokens != 12+300+40+25 {
		t.Fatalf("usage %+v, context %d", r.Usage, r.ContextTokens)
	}
	conv.AddResults([]builtin.ToolResult{{CallID: "tu_thread_post", Text: "posted"}})
	if r, err = conv.Turn(ctx); err != nil || r.Stop != builtin.StopEndTurn || r.Text != "Done." {
		t.Fatalf("second turn: %+v, %v", r, err)
	}

	first, second := a.requests[0], a.requests[1]
	if h := a.headers[0]; h.Get("X-Api-Key") != key || !strings.Contains(h.Get("Anthropic-Beta"), "server-side-fallback-2026-07-01") {
		t.Fatalf("headers: key %q, beta %q", h.Get("X-Api-Key"), h.Get("Anthropic-Beta"))
	}
	for _, field := range []string{"temperature", "top_p", "top_k", "tool_choice"} {
		if _, ok := first[field]; ok {
			t.Errorf("the request sends %s", field)
		}
	}
	thinking, _ := first["thinking"].(map[string]any)
	output, _ := first["output_config"].(map[string]any)
	if thinking["type"] != "adaptive" || output["effort"] != "xhigh" || first["fallbacks"] != "default" || first["cache_control"] == nil || first["max_tokens"] != float64(128000) {
		t.Fatalf("request: %v", first)
	}
	tool := first["tools"].([]any)[0].(map[string]any)
	schema := tool["input_schema"].(map[string]any)
	// The schema goes out whole: every keyword the tool's server wrote.
	var want map[string]any
	_ = json.Unmarshal(setup("").Tools[0].InputSchema, &want)
	if tool["name"] != "thread_post" || !reflect.DeepEqual(schema, want) {
		t.Fatalf("tool schema %v, want %v", schema, want)
	}
	// The history goes back append-only: the thinking block with its
	// signature unchanged, then one user message with the result.
	msgs := second["messages"].([]any)
	assistant := msgs[1].(map[string]any)["content"].([]any)
	results := msgs[2].(map[string]any)["content"].([]any)
	if len(msgs) != 3 || assistant[0].(map[string]any)["signature"] != "sig-1" || results[0].(map[string]any)["tool_use_id"] != "tu_thread_post" {
		t.Fatalf("second request's messages: %v", msgs)
	}
}

// Through a gateway there are no server-side fallbacks; a model rejecting
// thinking or effort gets the request again without it, and the run
// remembers.
func TestOptionalFeatures(t *testing.T) {
	a := newAPI(t, fail(400, "thinking type adaptive is not supported by this model"), fail(400, "effort is not supported"), sse("end_turn", "ok"), sse("end_turn", "again"))
	a.noModels = true // a gateway without the Models API
	s := setup(a.URL)
	s.Model.BaseURL = a.URL // a gateway
	conv, err := (&anthropic.Provider{}).Start(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 2 {
		if r, err := conv.Turn(ctx); err != nil || r.Stop != builtin.StopEndTurn {
			t.Fatalf("turn %d: %+v, %v", i, r, err)
		}
	}
	last := a.requests[3]
	if _, ok := last["thinking"]; ok {
		t.Error("thinking was sent again after the model rejected it")
	}
	if _, ok := last["output_config"]; ok {
		t.Error("effort was sent again after the model rejected it")
	}
	if _, ok := a.requests[0]["fallbacks"]; ok {
		t.Error("fallbacks were sent through a gateway")
	}
}

func TestErrors(t *testing.T) {
	a := newAPI(t, fail(401, "invalid x-api-key"))
	conv, err := (&anthropic.Provider{}).Start(ctx, setup(a.URL))
	if err != nil {
		t.Fatal(err)
	}
	_, err = conv.Turn(ctx)
	if !builtin.IsPermanent(err) {
		t.Fatalf("a 401: %v, want a permanent error", err)
	}
	// What is logged is the status, the error's type and message: not the
	// URL, and not the raw body.
	if msg := err.Error(); !strings.Contains(msg, "401 Unauthorized: invalid_request_error: invalid x-api-key") || strings.Contains(msg, a.URL) || strings.Contains(msg, "{") {
		t.Fatalf("error text %q", msg)
	}

	s := setup("")
	s.Credential = ""
	if _, err := (&anthropic.Provider{}).Start(ctx, s); !builtin.IsPermanent(err) {
		t.Fatalf("without a credential: %v, want a permanent error", err)
	}

	overloaded := func(w http.ResponseWriter, _ map[string]any) {
		w.WriteHeader(529)
		_, _ = io.WriteString(w, `{"type":"error","error":{"type":"overloaded_error","message":"overloaded"}}`)
	}
	a = newAPI(t, overloaded, overloaded, overloaded)
	conv, _ = (&anthropic.Provider{}).Start(ctx, setup(a.URL))
	if _, err := conv.Turn(ctx); err == nil || builtin.IsPermanent(err) {
		t.Fatalf("overloaded: %v, want a retriable error", err)
	}
}

func TestRefusal(t *testing.T) {
	refused := func(w http.ResponseWriter, _ map[string]any) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"x\",\"content\":[],\"stop_reason\":null,\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\n"+
			"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"refusal\",\"stop_details\":{\"type\":\"refusal\",\"category\":\"cyber\"}},\"usage\":{\"output_tokens\":0}}\n\n"+
			"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}
	a := newAPI(t, refused)
	conv, _ := (&anthropic.Provider{}).Start(ctx, setup(a.URL))
	r, err := conv.Turn(ctx)
	if err != nil || r.Stop != builtin.StopRefusal || r.RefusalCategory != "cyber" {
		t.Fatalf("refusal: %+v, %v", r, err)
	}
}

// Only the first request probes optional features: later, a 400 that
// mentions one is an ordinary error, and the request keeps its shape.
func TestFeaturesAreProbedOnTheFirstRequestOnly(t *testing.T) {
	a := newAPI(t, sse("end_turn", "ok"), fail(400, "thinking blocks cannot be modified"))
	a.noModels = true
	conv, _ := (&anthropic.Provider{Endpoint: a.URL}).Start(ctx, setup(""))
	if _, err := conv.Turn(ctx); err != nil {
		t.Fatal(err)
	}
	conv.AddResults(nil)
	if _, err := conv.Turn(ctx); !builtin.IsPermanent(err) {
		t.Fatalf("a later 400: %v, want a permanent error", err)
	}
	if len(a.requests) != 2 {
		t.Fatalf("%d requests, want no retry without thinking", len(a.requests))
	}
}

// Without the Models API the output cap falls back to 64000.
func TestLimitsWithoutTheModelsAPI(t *testing.T) {
	a := newAPI(t, sse("end_turn", "ok"))
	a.noModels = true
	conv, _ := (&anthropic.Provider{Endpoint: a.URL}).Start(ctx, setup(""))
	if _, err := conv.Turn(ctx); err != nil {
		t.Fatal(err)
	}
	if conv.ContextLimit() != 0 || a.requests[0]["max_tokens"] != float64(64000) {
		t.Fatalf("limit %d, max_tokens %v", conv.ContextLimit(), a.requests[0]["max_tokens"])
	}
}

// A turn the API keeps pausing is resumed five times, then ends paused.
func TestPausedTurns(t *testing.T) {
	var replies []func(http.ResponseWriter, map[string]any)
	for range 6 {
		replies = append(replies, sse("pause_turn", "working"))
	}
	a := newAPI(t, replies...)
	conv, _ := (&anthropic.Provider{Endpoint: a.URL}).Start(ctx, setup(""))
	r, err := conv.Turn(ctx)
	if err != nil || r.Stop != builtin.StopPaused || len(a.requests) != 6 {
		t.Fatalf("reply %+v, %v after %d requests", r, err, len(a.requests))
	}
}

// What the model takes comes from the Models API, with no probing: a model
// without adaptive thinking or the asked effort gets neither, from the
// first request on, and a 400 then is just an error.
func TestCapabilitiesFromTheModelsAPI(t *testing.T) {
	a := newAPI(t, sse("end_turn", "ok"))
	a.thinking = false
	a.efforts = `"low":{"supported":true},"medium":{"supported":true},"high":{"supported":true},"xhigh":{"supported":false},"max":{"supported":false}`
	conv, err := (&anthropic.Provider{Endpoint: a.URL}).Start(ctx, setup(""))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conv.Turn(ctx); err != nil {
		t.Fatal(err)
	}
	first := a.requests[0]
	if _, ok := first["thinking"]; ok {
		t.Error("thinking was asked of a model without adaptive thinking")
	}
	if _, ok := first["output_config"]; ok {
		t.Error("xhigh effort was asked of a model without it")
	}

	b := newAPI(t, fail(400, "thinking is not supported"))
	conv, _ = (&anthropic.Provider{Endpoint: b.URL}).Start(ctx, setup(""))
	if _, err := conv.Turn(ctx); !builtin.IsPermanent(err) || len(b.requests) != 1 {
		t.Fatalf("a 400 with the Models API known: %v after %d requests; want no probing", err, len(b.requests))
	}
}

// A transient Models API failure fails Start for the loop to retry; it
// does not leave the run without its limits.
func TestTheModelsAPIIsRetried(t *testing.T) {
	a := newAPI(t)
	a.models = []int{529, 529, 529} // the SDK tries three times itself
	if _, err := (&anthropic.Provider{Endpoint: a.URL}).Start(ctx, setup("")); err == nil || builtin.IsPermanent(err) {
		t.Fatalf("Start on an overloaded Models API: %v, want a retriable error", err)
	}
	conv, err := (&anthropic.Provider{Endpoint: a.URL}).Start(ctx, setup(""))
	if err != nil || conv.ContextLimit() != 1000000 {
		t.Fatalf("Start again: %v, limit %v", err, conv)
	}
}

// An error event in the middle of a stream comes with HTTP 200; its type
// decides whether retrying can help.
func TestMidStreamErrors(t *testing.T) {
	streamed := func(kind string) func(http.ResponseWriter, map[string]any) {
		return func(w http.ResponseWriter, _ map[string]any) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"x\",\"content\":[],\"stop_reason\":null,\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\n"+
				"event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\""+kind+"\",\"message\":\"went wrong\"}}\n\n")
		}
	}
	for kind, permanent := range map[string]bool{"invalid_request_error": true, "authentication_error": true, "overloaded_error": false, "api_error": false} {
		a := newAPI(t, streamed(kind))
		conv, _ := (&anthropic.Provider{Endpoint: a.URL}).Start(ctx, setup(""))
		_, err := conv.Turn(ctx)
		if err == nil || builtin.IsPermanent(err) != permanent {
			t.Errorf("%s in the stream: %v, want permanent %v", kind, err, permanent)
		}
	}
}
