package anthropic_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	"github.com/j75689/cadre/internal/builtin"
	"github.com/j75689/cadre/internal/provider/anthropic"
	"github.com/j75689/cadre/internal/provider/providertest"
)

// A tool call's input with keys out of order, a number no float64 holds,
// and characters JSON encoders like to escape.
const awkward = `{"zeta":"a < b && c > d","alpha":12345678901234567891,"mid":[1,2.50,{"y":true,"x":null}]}`

// withFutureBlock streams a message whose first block is of a type this
// SDK does not model, then text and a tool call.
func withFutureBlock(w http.ResponseWriter, _ map[string]any) {
	w.Header().Set("Content-Type", "text/event-stream")
	ev := func(name, data string) { _, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, data) }
	ev("message_start", `{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-opus-5-5","content":[],"stop_reason":null,"usage":{"input_tokens":12,"output_tokens":1}}}`)
	ev("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"future_block","data":"kept"}}`)
	ev("content_block_stop", `{"type":"content_block_stop","index":0}`)
	ev("content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"text","text":"Posting."}}`)
	ev("content_block_stop", `{"type":"content_block_stop","index":1}`)
	ev("content_block_start", `{"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"tu_thread_post","name":"thread_post","input":{}}}`)
	ev("content_block_delta", `{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"body\":\"hi\"}"}}`)
	ev("content_block_stop", `{"type":"content_block_stop","index":2}`)
	ev("message_delta", `{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":25}}`)
	ev("message_stop", `{"type":"message_stop"}`)
}

// A conversation run a step at a time sends the requests it sends live,
// byte for byte, so the prompt cache holds across workers: the thinking
// block's signature, a tool call's input as the model wrote it, the tool
// results, the settings.
func TestAResumedConversationSendsTheSameRequests(t *testing.T) {
	replies := func() []func(w http.ResponseWriter, body map[string]any) {
		return []func(w http.ResponseWriter, body map[string]any){
			sse("tool_use", "Posting <now> & then.", [2]string{"thread_post", awkward}, [2]string{"other", `{"b":2,"a":1}`}),
			sse("tool_use", "Again.", [2]string{"thread_post", `{"body":"x"}`}),
			sse("end_turn", "Done."),
		}
	}
	results := [][]builtin.ToolResult{
		{{CallID: "tu_thread_post", Text: "posted <ok>"}, {CallID: "tu_other", Text: "no such tool", IsError: true}},
		{{CallID: "tu_thread_post", Text: "posted"}},
	}
	live := newAPI(t, replies()...)
	providertest.Live(ctx, t, &anthropic.Provider{Endpoint: live.URL}, setup(""), results...)
	steps := newAPI(t, replies()...)
	o := providertest.StepByStep(ctx, t, &anthropic.Provider{Endpoint: steps.URL}, setup(""), results...)
	if o.Provider != v1alpha1.ProviderAnthropic || o.Model != "claude-opus-5-5" || o.Prompt != "# Task #1" || !strings.HasPrefix(o.SDK, "github.com/anthropics/anthropic-sdk-go@v") {
		t.Fatalf("opening %+v", o)
	}
	providertest.Same(t, live.bodies, steps.bodies)
	if !strings.Contains(string(steps.bodies[1]), "12345678901234567891") {
		t.Fatalf("the large integer did not survive: %s", steps.bodies[1])
	}
}

// A content block of a type this SDK does not model goes back as it came,
// live and resumed alike.
func TestAnUnknownBlockIsResumedAsItCame(t *testing.T) {
	replies := func() []func(w http.ResponseWriter, body map[string]any) {
		return []func(w http.ResponseWriter, body map[string]any){
			withFutureBlock, sse("end_turn", "Done."),
		}
	}
	results := []builtin.ToolResult{{CallID: "tu_thread_post", Text: "posted"}}
	live := newAPI(t, replies()...)
	providertest.Live(ctx, t, &anthropic.Provider{Endpoint: live.URL}, setup(""), results)
	steps := newAPI(t, replies()...)
	providertest.StepByStep(ctx, t, &anthropic.Provider{Endpoint: steps.URL}, setup(""), results)
	providertest.Same(t, live.bodies, steps.bodies)
	if strings.Contains(string(steps.bodies[1]), "null") || !strings.Contains(string(steps.bodies[1]), `"future_block"`) {
		t.Fatalf("the unknown block did not go back as it came: %s", steps.bodies[1])
	}
}

// What the first request learns (a model that rejects adaptive thinking,
// with no Models API to say so) is recorded with the turn, and every later
// step sends the request without it, as the live conversation does.
func TestALearnedSettingIsResumed(t *testing.T) {
	replies := func() []func(w http.ResponseWriter, body map[string]any) {
		return []func(w http.ResponseWriter, body map[string]any){
			fail(400, "thinking: adaptive thinking is not supported on this model"),
			sse("tool_use", "Posting.", [2]string{"thread_post", `{"body":"hi"}`}), sse("end_turn", "Done."),
		}
	}
	results := []builtin.ToolResult{{CallID: "tu_thread_post", Text: "posted", IsError: true}}
	gateway := func(a *api) builtin.Setup { a.noModels = true; return setup(a.URL) }
	live := newAPI(t, replies()...)
	providertest.Live(ctx, t, &anthropic.Provider{}, gateway(live), results)
	steps := newAPI(t, replies()...)
	providertest.StepByStep(ctx, t, &anthropic.Provider{}, gateway(steps), results)
	providertest.Same(t, live.bodies, steps.bodies)
	if strings.Contains(string(steps.bodies[2]), `"thinking":{`) {
		t.Fatalf("the last request still asks for thinking: %s", steps.bodies[2])
	}
}

// A conversation another provider opened is not resumed.
func TestAnotherProvidersOpening(t *testing.T) {
	o := &builtin.Opening{Provider: v1alpha1.ProviderOpenAI, Model: "m", Prompt: "p"}
	if _, err := (&anthropic.Provider{}).Resume(ctx, setup("http://127.0.0.1:1"), o, nil); !builtin.IsPermanent(err) {
		t.Fatalf("resume an openai opening: %v, want a permanent error", err)
	}
	if _, err := (&anthropic.Provider{}).Resume(ctx, setup("http://127.0.0.1:1"), &builtin.Opening{Provider: v1alpha1.ProviderAnthropic},
		[]builtin.Recorded{{Kind: "dream"}}); !builtin.IsPermanent(err) {
		t.Fatalf("resume a step of no known kind: %v, want a permanent error", err)
	}
}
