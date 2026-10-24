package v1alpha1_test

import (
	"testing"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
)

// A builtin agent runs a step at a time unless it says runtime; any other
// type runs in its runner's process.
func TestStepModeIsTheBuiltinDefault(t *testing.T) {
	for _, tc := range []struct {
		runtime v1alpha1.AgentRuntime
		step    bool
	}{
		{v1alpha1.AgentRuntime{}, true},
		{v1alpha1.AgentRuntime{Type: v1alpha1.RuntimeBuiltin}, true},
		{v1alpha1.AgentRuntime{Type: v1alpha1.RuntimeBuiltin, Execution: v1alpha1.RunModeStep}, true},
		{v1alpha1.AgentRuntime{Type: v1alpha1.RuntimeBuiltin, Execution: v1alpha1.RunModeRuntime}, false},
		{v1alpha1.AgentRuntime{Type: v1alpha1.RuntimeRunner}, false},
	} {
		if got := tc.runtime.StepMode(); got != tc.step {
			t.Errorf("%+v: step mode %v, want %v", tc.runtime, got, tc.step)
		}
	}
}
