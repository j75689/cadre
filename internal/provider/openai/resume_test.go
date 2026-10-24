package openai_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/j75689/cadre/internal/builtin"
	"github.com/j75689/cadre/internal/provider/openai"
)

// stored is v as a transcript keeps it: written as JSON and read back.
func stored[T any](t *testing.T, v T) T {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out T
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// stepByStep runs the conversation a step at a time, each step on a
// conversation rebuilt from what the steps before recorded, as workers do.
func stepByStep(t *testing.T, s builtin.Setup, results []builtin.ToolResult) {
	t.Helper()
	p := &openai.Provider{}
	o, err := p.Begin(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	o = stored(t, o)
	if o.Provider != "openai" || o.Model != "some-model" || !strings.HasPrefix(o.SDK, "github.com/openai/openai-go/v3@") {
		t.Fatalf("opening %+v", o)
	}
	var steps []builtin.Recorded
	for _, res := range [][]builtin.ToolResult{results, nil} {
		conv, err := p.Resume(ctx, s, o, steps)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := conv.Turn(ctx); err != nil {
			t.Fatal(err)
		}
		steps = append(steps, stored(t, conv.LastTurn()))
		if res != nil {
			steps = append(steps, stored(t, builtin.Recorded{Results: res}))
		}
	}
}

func live(t *testing.T, s builtin.Setup, results []builtin.ToolResult) {
	t.Helper()
	conv, err := (&openai.Provider{}).Start(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conv.Turn(ctx); err != nil {
		t.Fatal(err)
	}
	conv.AddResults(results)
	if _, err := conv.Turn(ctx); err != nil {
		t.Fatal(err)
	}
}

func same(t *testing.T, live, steps [][]byte) {
	t.Helper()
	if len(live) != len(steps) {
		t.Fatalf("%d requests live, %d step by step", len(live), len(steps))
	}
	for i := range live {
		if !bytes.Equal(live[i], steps[i]) {
			t.Fatalf("request %d differs:\nlive  %s\nsteps %s", i, live[i], steps[i])
		}
	}
}

// A conversation run a step at a time sends the requests it sends live,
// byte for byte, including the answer it gives itself for a call whose
// arguments are not JSON.
func TestAResumedConversationSendsTheSameRequests(t *testing.T) {
	replies := func() []func(http.ResponseWriter) {
		return []func(http.ResponseWriter){sse("tool_calls", "Posting.", [2]string{"thread_post", `{"body":"hi"}`}, [2]string{"thread_post", `{not json`}), sse("stop", "Done.")}
	}
	results := []builtin.ToolResult{{CallID: "call_0", Text: "posted"}}
	a := newAPI(t, replies()...)
	live(t, setup(a.URL), results)
	b := newAPI(t, replies()...)
	stepByStep(t, setup(b.URL), results)
	same(t, a.bodies, b.bodies)
	if !strings.Contains(string(b.bodies[1]), "not valid JSON") {
		t.Fatalf("the call with bad arguments was not answered: %s", b.bodies[1])
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
	live(t, setup(a.URL), results)
	b := newAPI(t, replies()...)
	stepByStep(t, setup(b.URL), results)
	same(t, a.bodies, b.bodies)
	if strings.Contains(string(b.bodies[2]), "reasoning_effort") {
		t.Fatalf("the last request still asks for reasoning effort: %s", b.bodies[2])
	}
}
