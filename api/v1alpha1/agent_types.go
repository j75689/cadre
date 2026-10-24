package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// RuntimeType says who runs an agent's tasks.
// +kubebuilder:validation:Enum=builtin;runner
type RuntimeType string

// ReservedLabelPrefix is the label prefix Cadre keeps for itself.
const ReservedLabelPrefix = "cadre.dev/"

// LabelRuntime is the claim label that lets a runner claim builtin agents.
// cadre-runner always sends it. Any runner sending it may claim them until
// runners are authenticated; then the label is bound to cadre-runner's
// ServiceAccount.
const LabelRuntime = "cadre.dev/runtime"

// Runtime types.
const (
	// RuntimeBuiltin is Cadre's own loop, run by cadre-runner.
	RuntimeBuiltin RuntimeType = "builtin"
	// RuntimeRunner hands the tasks to any runner whose labels match the
	// selector.
	RuntimeRunner RuntimeType = "runner"
)

// Effort is how much thinking and token spend Cadre's builtin loop asks
// the model for.
// +kubebuilder:validation:Enum=low;medium;high;xhigh;max
type Effort string

// Effort levels.
const (
	EffortLow    Effort = "low"
	EffortMedium Effort = "medium"
	EffortHigh   Effort = "high"
	EffortXHigh  Effort = "xhigh"
	EffortMax    Effort = "max"
)

// AgentRuntime says who runs an agent's tasks.
//
// +kubebuilder:validation:XValidation:rule="self.type == 'runner' || !has(self.selector)",message="selector is for runtime type runner"
// +kubebuilder:validation:XValidation:rule="self.type != 'runner' || (has(self.selector) && size(self.selector) > 0)",message="runtime type runner needs a selector"
// +kubebuilder:validation:XValidation:rule="!has(self.selector) || self.selector.all(k, !k.startsWith('cadre.dev/'))",message="the cadre.dev/ label prefix is reserved"
// +kubebuilder:validation:XValidation:rule="!has(self.execution) || self.execution == 'runtime' || self.type == 'builtin'",message="step execution is for runtime type builtin"
type AgentRuntime struct {
	// Type is builtin (Cadre's loop) or runner (any runner whose labels
	// match the selector).
	// +kubebuilder:default=builtin
	Type RuntimeType `json:"type,omitempty"`

	// Selector picks the runners that may claim this agent's tasks: every
	// label must match. Labels under the cadre.dev/ prefix are reserved for
	// Cadre (cadre-runner claims builtin agents with
	// cadre.dev/runtime: builtin), so a selector may not use them.
	// +optional
	Selector map[string]string `json:"selector,omitempty"`

	// Execution is how a builtin agent's runs are driven: runtime, one
	// worker for the whole run, or step, a step at a time by any worker.
	// Other runtime types run in their runner's process, as runtime.
	// +kubebuilder:validation:Enum=runtime;step
	// +kubebuilder:default=runtime
	// +optional
	Execution RunMode `json:"execution,omitempty"`
}

// Builtin reports whether the agent runs Cadre's own loop; an empty type
// is the default, builtin.
func (r AgentRuntime) Builtin() bool {
	return r.Type == RuntimeBuiltin || r.Type == ""
}

// StepMode reports whether the agent's runs are driven a step at a time.
func (r AgentRuntime) StepMode() bool {
	return r.Builtin() && r.Execution == RunModeStep
}

// AgentSpec is an agent: what it is told, which model it uses and which
// tools it may call.
type AgentSpec struct {
	// Description says what the agent is for, on the board.
	// +optional
	Description string `json:"description,omitempty"`

	// ModelRef is the name of a ModelProvider in the same namespace.
	// +kubebuilder:validation:MinLength=1
	ModelRef string `json:"modelRef"`

	// ToolRefs are the names of ToolServers in the same namespace that the
	// agent may use.
	// +listType=set
	// +optional
	ToolRefs []string `json:"toolRefs,omitempty"`

	// Instructions are the agent's system prompt.
	// +optional
	Instructions string `json:"instructions,omitempty"`

	// Effort is how much thinking and token spend the builtin loop asks
	// the model for. It belongs to the agent, not the ModelProvider: one
	// model serves agents that need different depth. Runner agents ignore
	// it.
	// +kubebuilder:default=high
	// +optional
	Effort Effort `json:"effort,omitempty"`

	// Runtime says who runs the agent's tasks; by default Cadre's builtin
	// loop. The object default makes an Agent applied without a runtime
	// builtin too, not typeless.
	// +kubebuilder:default={type: builtin}
	// +optional
	Runtime AgentRuntime `json:"runtime,omitempty"`
}

// Agent is an agent tasks can be assigned to.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced
// +kubebuilder:printcolumn:name="Model",type=string,JSONPath=`.spec.modelRef`
// +kubebuilder:printcolumn:name="Runtime",type=string,JSONPath=`.spec.runtime.type`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type Agent struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   AgentSpec    `json:"spec"`
	Status ConfigStatus `json:"status,omitempty"`
}

// AgentList is a list of Agents.
//
// +kubebuilder:object:root=true
type AgentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Agent `json:"items"`
}
