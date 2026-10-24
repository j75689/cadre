package openai_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	"github.com/j75689/cadre/internal/builtin"
	"github.com/j75689/cadre/internal/provider/openai"
	"github.com/j75689/cadre/internal/runner"
)

const key = "test-key-not-real"

var ctx = context.Background()

// api is a fake Chat Completions endpoint answering each request with the
// next scripted reply.
type api struct {
	*httptest.Server
	mu       sync.Mutex
	requests []map[string]any
	headers  []http.Header
	paths    []string
	replies  []func(http.ResponseWriter)
}

func newAPI(t *testing.T, replies ...func(http.ResponseWriter)) *api {
	t.Helper()
	a := &api{replies: replies}
	a.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		a.mu.Lock()
		a.requests = append(a.requests, body)
		a.headers = append(a.headers, r.Header.Clone())
		a.paths = append(a.paths, r.URL.Path)
		n := len(a.requests)
		a.mu.Unlock()
		if n > len(a.replies) {
			http.Error(w, "unscripted request", http.StatusInternalServerError)
			return
		}
		a.replies[n-1](w)
	}))
	t.Cleanup(a.Close)
	return a
}

// sse streams a reply: text, then tool calls split over two chunks each,
// then the finish reason, then usage.
func sse(finish, text string, calls ...[2]string) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "text/event-stream")
		chunk := func(choices string, usage string) {
			_, _ = fmt.Fprintf(w, `data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[%s]%s}`+"\n\n", choices, usage)
		}
		chunk(fmt.Sprintf(`{"index":0,"delta":{"role":"assistant","content":%q},"finish_reason":null}`, text), "")
		for i, c := range calls {
			half := len(c[1]) / 2
			chunk(fmt.Sprintf(`{"index":0,"delta":{"tool_calls":[{"index":%d,"id":"call_%d","type":"function","function":{"name":%q,"arguments":%q}}]},"finish_reason":null}`, i, i, c[0], c[1][:half]), "")
			chunk(fmt.Sprintf(`{"index":0,"delta":{"tool_calls":[{"index":%d,"function":{"arguments":%q}}]},"finish_reason":null}`, i, c[1][half:]), "")
		}
		chunk(fmt.Sprintf(`{"index":0,"delta":{},"finish_reason":%q}`, finish), "")
		chunk("", `,"usage":{"prompt_tokens":100,"completion_tokens":20,"total_tokens":120,"prompt_tokens_details":{"cached_tokens":60}}`)
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	}
}

func fail(status int, msg string) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = fmt.Fprintf(w, `{"error":{"message":%q,"type":"invalid_request_error","code":null}}`, msg)
	}
}

func setup(base string) builtin.Setup {
	return builtin.Setup{
		Model:      runner.ClaimedModel{Type: v1alpha1.ProviderOpenAI, Model: "some-model", BaseURL: base},
		Effort:     v1alpha1.EffortMedium,
		Credential: key,
		System:     "be careful",
		Prompt:     func(int64) string { return "# Task #1" },
		Tools: []builtin.Tool{
			{Name: "thread_post", Description: "post", InputSchema: json.RawMessage(`{"type":"object","properties":{"body":{"type":"string"}},"required":["body"]}`)},
		},
	}
}

func TestAConversation(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "from-the-environment")
	t.Setenv("OPENAI_BASE_URL", "http://127.0.0.1:1")
	t.Setenv("OPENAI_ORG_ID", "org-from-the-environment")
	a := newAPI(t, sse("tool_calls", "Posting.", [2]string{"thread_post", `{"body":"hi"}`}, [2]string{"thread_post", `{"body":"again"}`}), sse("stop", "Done."))
	conv, err := (&openai.Provider{}).Start(ctx, setup(a.URL+"/v1"))
	if err != nil {
		t.Fatal(err)
	}
	r, err := conv.Turn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if r.Stop != builtin.StopToolUse || r.Text != "Posting." || len(r.Calls) != 2 || r.Calls[0].ID != "call_0" || string(r.Calls[0].Input) != `{"body":"hi"}` || string(r.Calls[1].Input) != `{"body":"again"}` {
		t.Fatalf("reply: %+v", r)
	}
	if r.Usage.InputTokens != 40 || r.Usage.CacheReadInputTokens != 60 || r.Usage.OutputTokens != 20 || r.ContextTokens != 120 {
		t.Fatalf("usage %+v, context %d", r.Usage, r.ContextTokens)
	}
	conv.AddResults([]builtin.ToolResult{{CallID: "call_0", Text: "posted"}, {CallID: "call_1", Text: "refused", IsError: true}})
	if r, err = conv.Turn(ctx); err != nil || r.Stop != builtin.StopEndTurn || r.Text != "Done." {
		t.Fatalf("second turn: %+v, %v", r, err)
	}

	h := a.headers[0]
	if h.Get("Authorization") != "Bearer "+key || h.Get("OpenAI-Organization") != "" || a.paths[0] != "/v1/chat/completions" {
		t.Fatalf("auth %q, org %q, path %q: only the Secret and the ModelProvider's baseURL count", h.Get("Authorization"), h.Get("OpenAI-Organization"), a.paths[0])
	}
	first := a.requests[0]
	opts, _ := first["stream_options"].(map[string]any)
	if first["reasoning_effort"] != "medium" || opts["include_usage"] != true || first["stream"] != true {
		t.Fatalf("request: %v", first)
	}
	for _, field := range []string{"temperature", "top_p", "tool_choice", "max_tokens"} {
		if _, ok := first[field]; ok {
			t.Errorf("the request sends %s", field)
		}
	}
	// The history is append-only: system, user, the assistant's calls, one
	// tool message per result (a failure says so), then nothing edited.
	msgs := a.requests[1]["messages"].([]any)
	if len(msgs) != 5 {
		t.Fatalf("second request has %d messages: %v", len(msgs), msgs)
	}
	assistant := msgs[2].(map[string]any)
	tool1, tool2 := msgs[3].(map[string]any), msgs[4].(map[string]any)
	if msgs[0].(map[string]any)["role"] != "system" || len(assistant["tool_calls"].([]any)) != 2 ||
		tool1["tool_call_id"] != "call_0" || tool2["content"] != "Error: refused" {
		t.Fatalf("messages: %v", msgs)
	}
}

// A server that rejects reasoning effort or stream usage gets the first
// request again without it; later, a 400 is just an error.
func TestOptionalFeatures(t *testing.T) {
	a := newAPI(t, fail(400, "Unrecognized request argument supplied: reasoning_effort"), fail(400, "stream_options is not supported"), sse("stop", "ok"), fail(400, "reasoning is wrong"))
	conv, _ := (&openai.Provider{}).Start(ctx, setup(a.URL))
	if r, err := conv.Turn(ctx); err != nil || r.Text != "ok" {
		t.Fatalf("first turn: %+v, %v", r, err)
	}
	if _, ok := a.requests[2]["reasoning_effort"]; ok {
		t.Error("reasoning_effort was sent after the server rejected it")
	}
	if _, ok := a.requests[2]["stream_options"]; ok {
		t.Error("stream_options were sent after the server rejected them")
	}
	conv.AddResults(nil)
	if _, err := conv.Turn(ctx); !builtin.IsPermanent(err) || len(a.requests) != 4 {
		t.Fatalf("a later 400: %v after %d requests", err, len(a.requests))
	}
}

func TestEndingsAndErrors(t *testing.T) {
	refusal := func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, `data: {"id":"c","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"role":"assistant","refusal":"I can't help with that."},"finish_reason":null}]}`+"\n\n"+
			`data: {"id":"c","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`+"\n\n"+"data: [DONE]\n\n")
	}
	for name, tc := range map[string]struct {
		reply func(http.ResponseWriter)
		stop  builtin.Stop
	}{
		"refusal":        {refusal, builtin.StopRefusal},
		"content filter": {sse("content_filter", ""), builtin.StopRefusal},
		"length":         {sse("length", "cut"), builtin.StopMaxTokens},
	} {
		a := newAPI(t, tc.reply)
		conv, _ := (&openai.Provider{}).Start(ctx, setup(a.URL))
		if r, err := conv.Turn(ctx); err != nil || r.Stop != tc.stop {
			t.Errorf("%s: %+v, %v; want %s", name, r, err, tc.stop)
		}
	}

	a := newAPI(t, fail(401, "Incorrect API key provided"))
	conv, _ := (&openai.Provider{}).Start(ctx, setup(a.URL))
	_, err := conv.Turn(ctx)
	if !builtin.IsPermanent(err) || !strings.Contains(err.Error(), "401 Unauthorized: invalid_request_error: Incorrect API key provided") || strings.Contains(err.Error(), a.URL) {
		t.Fatalf("a 401: %v", err)
	}
	s := setup(a.URL)
	s.Credential = ""
	if _, err := (&openai.Provider{}).Start(ctx, s); !builtin.IsPermanent(err) {
		t.Fatalf("without a credential: %v", err)
	}
	overloaded := func(w http.ResponseWriter) {
		http.Error(w, `{"error":{"message":"busy","type":"server_error"}}`, http.StatusServiceUnavailable)
	}
	b := newAPI(t, overloaded, overloaded, overloaded)
	conv, _ = (&openai.Provider{}).Start(ctx, setup(b.URL))
	if _, err := conv.Turn(ctx); err == nil || builtin.IsPermanent(err) {
		t.Fatalf("a 503: %v, want a retriable error", err)
	}
}

// Once a request has been answered, a 400 naming a feature is just an
// error: the conversation does not change shape mid-run.
func TestFeaturesAreProbedOnTheFirstRequestOnly(t *testing.T) {
	a := newAPI(t, sse("stop", "ok"), fail(400, "reasoning_effort is not supported with this conversation"))
	conv, _ := (&openai.Provider{}).Start(ctx, setup(a.URL))
	if _, err := conv.Turn(ctx); err != nil {
		t.Fatal(err)
	}
	conv.AddResults(nil)
	if _, err := conv.Turn(ctx); !builtin.IsPermanent(err) || len(a.requests) != 2 {
		t.Fatalf("a later 400: %v after %d requests; want no retry", err, len(a.requests))
	}
}
