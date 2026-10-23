package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// RuntimeType says who runs an agent's tasks.
// +kubebuilder:validation:Enum=builtin;runner
type RuntimeType string

// Runtime types.
const (
	// RuntimeBuiltin is Cadre's own loop, run by cadre-runner.
	RuntimeBuiltin RuntimeType = "builtin"
	// RuntimeRunner hands the tasks to any runner whose labels match the
	// selector.
	RuntimeRunner RuntimeType = "runner"
)

// AgentRuntime says who runs an agent's tasks.
//
// +kubebuilder:validation:XValidation:rule="self.type == 'runner' || !has(self.selector)",message="selector is for runtime type runner"
// +kubebuilder:validation:XValidation:rule="self.type != 'runner' || (has(self.selector) && size(self.selector) > 0)",message="runtime type runner needs a selector"
type AgentRuntime struct {
	// Type is builtin (Cadre's loop) or runner (any runner whose labels
	// match the selector).
	// +kubebuilder:default=builtin
	Type RuntimeType `json:"type,omitempty"`

	// Selector picks the runners that may claim this agent's tasks: every
	// label must match.
	// +optional
	Selector map[string]string `json:"selector,omitempty"`
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

	// Runtime says who runs the agent's tasks; by default Cadre's builtin
	// loop.
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
