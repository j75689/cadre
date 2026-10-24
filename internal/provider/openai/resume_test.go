package openai_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	"github.com/j75689/cadre/internal/builtin"
	"github.com/j75689/cadre/internal/provider/openai"
	"github.com/j75689/cadre/internal/provider/providertest"
)

// A call's arguments with keys out of order, a number no float64 holds,
// and characters JSON encoders like to escape.
const awkward = `{"zeta":"a < b && c > d","alpha":12345678901234567891,"mid":[1,2.50]}`

// untyped streams a reply whose one call has no type, as several
// compatible servers send it.
func untyped(w http.ResponseWriter) {
	streamed(
		`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_0","function":{"name":"thread_post","arguments":"{\"body\":\"hi\"}"}}]},"finish_reason":null}]}`,
		`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
	)(w)
}

// A conversation run a step at a time sends the requests it sends live,
// byte for byte: a call's arguments as the model wrote them, a call
// without a type, the answer the provider gives itself for arguments that
// are not JSON, a step whose calls it answered all by itself.
func TestAResumedConversationSendsTheSameRequests(t *testing.T) {
	replies := func() []func(http.ResponseWriter) {
		return []func(http.ResponseWriter){
			sse("tool_calls", "Posting <now> & then.", [2]string{"thread_post", awkward}, [2]string{"thread_post", `{not json`}),
			sse("tool_calls", "Only bad.", [2]string{"thread_post", `{nope`}),
			untyped,
			sse("stop", "Done."),
		}
	}
	results := [][]builtin.ToolResult{
		{{CallID: "call_0", Text: "posted <ok>"}},
		nil, // the provider answered the one call itself
		{{CallID: "call_0", Text: "posted", IsError: true}},
	}
	a := newAPI(t, replies()...)
	providertest.Live(ctx, t, &openai.Provider{}, setup(a.URL), results...)
	b := newAPI(t, replies()...)
	o := providertest.StepByStep(ctx, t, &openai.Provider{}, setup(b.URL), results...)
	if o.Provider != v1alpha1.ProviderOpenAI || o.Model != "some-model" || !strings.HasPrefix(o.SDK, "github.com/openai/openai-go/v3@v") {
		t.Fatalf("opening %+v", o)
	}
	providertest.Same(t, a.bodies, b.bodies)
	switch {
	case !strings.Contains(string(b.bodies[1]), "not valid JSON"):
		t.Fatalf("the call with bad arguments was not answered: %s", b.bodies[1])
	case !strings.Contains(string(b.bodies[1]), `12345678901234567891`):
		t.Fatalf("the large integer did not survive: %s", b.bodies[1])
	case strings.Count(string(b.bodies[2]), "not valid JSON") != 2:
		t.Fatalf("the step answered by the provider alone was not answered: %s", b.bodies[2])
	case strings.Contains(string(b.bodies[3]), "null"):
		t.Fatalf("something went back as null: %s", b.bodies[3])
	}
	msgs := b.requests[3]["messages"].([]any)
	asst := msgs[len(msgs)-2].(map[string]any)
	if calls, _ := asst["tool_calls"].([]any); len(calls) != 1 || calls[0].(map[string]any)["type"] != "function" {
		t.Fatalf("the untyped call did not go back as a function call: %v", asst)
	}
}

// A call without a type is a function call: it is made.
func TestAnUntypedCallIsMade(t *testing.T) {
	a := newAPI(t, untyped)
	conv, err := (&openai.Provider{}).Start(ctx, setup(a.URL))
	if err != nil {
		t.Fatal(err)
	}
	r, err := conv.Turn(ctx)
	if err != nil || len(r.Calls) != 1 || r.Calls[0].ID != "call_0" || string(r.Calls[0].Input) != `{"body":"hi"}` {
		t.Fatalf("reply: %+v, %v", r, err)
	}
}

// What the first request learns (a server that takes no reasoning effort)
// is recorded with the turn and holds for every later step.
func TestALearnedSettingIsResumed(t *testing.T) {
	byParam := func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(400)
		_, _ = fmt.Fprint(w, `{"error":{"message":"Unsupported value: 'medium' is not supported with this model.","type":"invalid_request_error","param":"reasoning_effort","code":"unsupported_value"}}`)
	}
	replies := func() []func(http.ResponseWriter) {
		return []func(http.ResponseWriter){byParam, sse("tool_calls", "Posting.", [2]string{"thread_post", `{"body":"hi"}`}), sse("stop", "Done.")}
	}
	results := []builtin.ToolResult{{CallID: "call_0", Text: "posted", IsError: true}}
	a := newAPI(t, replies()...)
	providertest.Live(ctx, t, &openai.Provider{}, setup(a.URL), results)
	b := newAPI(t, replies()...)
	providertest.StepByStep(ctx, t, &openai.Provider{}, setup(b.URL), results)
	providertest.Same(t, a.bodies, b.bodies)
	if strings.Contains(string(b.bodies[2]), "reasoning_effort") {
		t.Fatalf("the last request still asks for reasoning effort: %s", b.bodies[2])
	}
}

// A conversation another provider opened is not resumed.
func TestAnotherProvidersOpening(t *testing.T) {
	o := &builtin.Opening{Provider: v1alpha1.ProviderAnthropic, Model: "m", Prompt: "p"}
	if _, err := (&openai.Provider{}).Resume(ctx, setup("http://127.0.0.1:1"), o, nil); !builtin.IsPermanent(err) {
		t.Fatalf("resume an anthropic opening: %v, want a permanent error", err)
	}
}
