// Package providertest runs a provider's conversation live and a step at a
// time, for the providers' tests: a conversation rebuilt from what its steps
// recorded must send the requests it sends live, byte for byte.
package providertest

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/j75689/cadre/internal/builtin"
)

// Stored is v as a transcript keeps it: written as JSON and read back.
func Stored[T any](t *testing.T, v T) T {
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

// Live runs one conversation: a turn, then for each set of results, the
// results and another turn.
func Live(ctx context.Context, t *testing.T, p builtin.Provider, s builtin.Setup, results ...[]builtin.ToolResult) {
	t.Helper()
	conv, err := p.Start(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conv.Turn(ctx); err != nil {
		t.Fatal(err)
	}
	for _, res := range results {
		conv.AddResults(res)
		if _, err := conv.Turn(ctx); err != nil {
			t.Fatal(err)
		}
	}
}

// StepByStep runs the same conversation as Live a step at a time, each turn
// on a conversation rebuilt from the opening and what the steps before
// recorded, all through a JSON round trip, as workers do. It returns the
// opening.
func StepByStep(ctx context.Context, t *testing.T, p builtin.StepProvider, s builtin.Setup, results ...[]builtin.ToolResult) *builtin.Opening {
	t.Helper()
	o, err := p.Begin(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	o = Stored(t, o)
	var steps []builtin.Recorded
	for i := 0; ; i++ {
		conv, err := p.Resume(ctx, s, o, steps)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := conv.Turn(ctx); err != nil {
			t.Fatal(err)
		}
		steps = append(steps, Stored(t, conv.LastTurn()))
		if i == len(results) {
			return o
		}
		steps = append(steps, Stored(t, builtin.ResultsRecord(results[i])))
	}
}

// Same fails unless both runs sent the same requests.
func Same(t *testing.T, live, steps [][]byte) {
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
