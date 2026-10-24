package anthropic_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/j75689/cadre/internal/builtin"
	"github.com/j75689/cadre/internal/provider/anthropic"
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
func stepByStep(t *testing.T, p *anthropic.Provider, s builtin.Setup, results []builtin.ToolResult) {
	t.Helper()
	o, err := p.Begin(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	o = stored(t, o)
	if o.Provider != "anthropic" || o.Model != "claude-opus-5-5" || o.Prompt != "# Task #1" || !strings.HasPrefix(o.SDK, "github.com/anthropics/anthropic-sdk-go@") {
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

// A conversation run a step at a time sends the requests it sends live,
// byte for byte, so the prompt cache holds across workers: the thinking
// block's signature, the tool result, the settings.
func TestAResumedConversationSendsTheSameRequests(t *testing.T) {
	replies := func() []func(w http.ResponseWriter, body map[string]any) {
		return []func(w http.ResponseWriter, body map[string]any){sse("tool_use", "Posting.", [2]string{"thread_post", `{"body":"hi"}`}), sse("end_turn", "Done.")}
	}
	results := []builtin.ToolResult{{CallID: "tu_thread_post", Text: "posted"}}

	live := newAPI(t, replies()...)
	conv, err := (&anthropic.Provider{Endpoint: live.URL}).Start(ctx, setup(""))
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

	steps := newAPI(t, replies()...)
	stepByStep(t, &anthropic.Provider{Endpoint: steps.URL}, setup(""), results)
	same(t, live.bodies, steps.bodies)
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
	conv, err := (&anthropic.Provider{}).Start(ctx, gateway(live))
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

	steps := newAPI(t, replies()...)
	stepByStep(t, &anthropic.Provider{}, gateway(steps), results)
	same(t, live.bodies, steps.bodies)
	if strings.Contains(string(steps.bodies[2]), `"thinking":{`) {
		t.Fatalf("the last request still asks for thinking: %s", steps.bodies[2])
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
