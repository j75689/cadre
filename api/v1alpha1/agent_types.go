package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
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
// +kubebuilder:validation:XValidation:rule="!has(self.job) || self.type == 'runner'",message="job is for runtime type runner"
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

	// Execution is how a builtin agent's runs are driven: step, a step at
	// a time by any worker (the default), or runtime, one worker for the
	// whole run. Other runtime types run in their runner's process, as
	// runtime. It has no schema default, as the default depends on the
	// type: empty is step for builtin and runtime for the others.
	// +kubebuilder:validation:Enum=runtime;step
	// +optional
	Execution RunMode `json:"execution,omitempty"`

	// Job, for runtime type runner, has cadre-server start a Kubernetes
	// Job for the agent's work: each Job's pod is a runner that claims one
	// run, works it and exits. Runners already running may claim the
	// agent's tasks too.
	// +optional
	Job *AgentJob `json:"job,omitempty"`
}

// AgentJob is how cadre-server starts runners for an agent's work.
type AgentJob struct {
	// RunnerRef names a Runner in the agent's namespace that has a
	// serviceAccount and whose labels include the agent's selector: the
	// Jobs' pods run as that ServiceAccount, whatever the template says,
	// and claim with the Runner's labels.
	// +kubebuilder:validation:MinLength=1
	RunnerRef string `json:"runnerRef"`

	// Template is the pod a Job runs: the team's runner. Its runner reads
	// CADRE_SERVER, CADRE_NAMESPACE, CADRE_RUNNER (its name, the Job's),
	// CADRE_AGENT and the token at /var/run/secrets/cadre/token, which
	// cadre-server adds, claims once (this agent's tasks only), works the
	// run and exits; cadre-runner does so by itself. The API server checks the
	// template when a Job is created; the schema here does not.
	// +kubebuilder:validation:Schemaless
	// +kubebuilder:pruning:PreserveUnknownFields
	// +kubebuilder:validation:Type=object
	Template corev1.PodTemplateSpec `json:"template"`

	// ActiveDeadline bounds a Job's run time; default one hour.
	// +optional
	ActiveDeadline *metav1.Duration `json:"activeDeadline,omitempty"`

	// MaxPending is how many of the agent's Jobs may wait for work at once;
	// default 2.
	// +kubebuilder:validation:Minimum=1
	// +optional
	MaxPending int32 `json:"maxPending,omitempty"`
}

// Builtin reports whether the agent runs Cadre's own loop; an empty type
// is the default, builtin.
func (r AgentRuntime) Builtin() bool {
	return r.Type == RuntimeBuiltin || r.Type == ""
}

// StepMode reports whether the agent's runs are driven a step at a time.
func (r AgentRuntime) StepMode() bool {
	return r.Builtin() && r.Execution != RunModeRuntime
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
// +kubebuilder:validation:XValidation:rule="!has(self.spec.runtime) || !has(self.spec.runtime.job) || size(self.metadata.name) <= 63",message="an agent with a job has a name of at most 63 characters, as its Jobs carry it in a label"
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
