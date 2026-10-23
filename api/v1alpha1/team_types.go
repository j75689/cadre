package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TeamName is the only name a Team may have; there is one Team per namespace.
const TeamName = "team"

// TeamSpec is the human-owned configuration of a team.
type TeamSpec struct {
	// DisplayName is the name shown on the board.
	// +optional
	DisplayName string `json:"displayName,omitempty"`

	// MaxConcurrentRuns caps the number of tasks in this team that may be
	// Running at once. Claims are refused while the cap is reached.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=1
	// +optional
	MaxConcurrentRuns int32 `json:"maxConcurrentRuns,omitempty"`

	// ArchiveAfter is how long a finished task stays on the board before it is
	// archived to object storage. It must be a positive Go duration such as
	// 720h.
	// +kubebuilder:validation:XValidation:rule="duration(self) > duration('0s')",message="archiveAfter must be a positive duration such as 720h"
	// +optional
	ArchiveAfter *metav1.Duration `json:"archiveAfter,omitempty"`
}

// TeamStatus is the control state of a team, written only by the Task Manager.
type TeamStatus struct {
	// NextNumber is the human-facing number the next task will receive.
	// It is claimed with a conditional write.
	// +optional
	NextNumber int64 `json:"nextNumber,omitempty"`

	// Running is the number of runs currently holding a slot of the
	// MaxConcurrentRuns semaphore. It is a cache, recomputable from the index.
	// +kubebuilder:validation:Minimum=0
	// +optional
	Running int32 `json:"running,omitempty"`
}

// Team opts a namespace in to Cadre and holds its task counter and
// concurrency semaphore.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced
// +kubebuilder:validation:XValidation:rule="self.metadata.name == 'team'",message="a Team must be named team"
// +kubebuilder:printcolumn:name="DisplayName",type=string,JSONPath=`.spec.displayName`
// +kubebuilder:printcolumn:name="Running",type=integer,JSONPath=`.status.running`
// +kubebuilder:printcolumn:name="Max",type=integer,JSONPath=`.spec.maxConcurrentRuns`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type Team struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   TeamSpec   `json:"spec,omitempty"`
	Status TeamStatus `json:"status,omitempty"`
}

// TeamList is a list of Teams.
//
// +kubebuilder:object:root=true
type TeamList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Team `json:"items"`
}
