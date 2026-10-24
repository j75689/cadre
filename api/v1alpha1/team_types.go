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

	// Roles give people a role on the team's board, by user or by group.
	// Someone matching several entries has the strongest of their roles.
	// +listType=atomic
	// +optional
	Roles []TeamRole `json:"roles,omitempty"`
}

// RoleName is what a role lets its holder do on a team's board: a viewer
// reads, a member also works on tasks, an admin also manages the team.
// +kubebuilder:validation:Enum=admin;member;viewer
type RoleName string

// Roles.
const (
	RoleAdmin  RoleName = "admin"
	RoleMember RoleName = "member"
	RoleViewer RoleName = "viewer"
)

// TeamRole gives a role to its subjects.
type TeamRole struct {
	// Role is admin, member or viewer.
	Role RoleName `json:"role"`

	// Subjects hold the role.
	// +kubebuilder:validation:MinItems=1
	// +listType=atomic
	Subjects []RoleSubject `json:"subjects"`
}

// SubjectKind says what a subject names.
// +kubebuilder:validation:Enum=group;user
type SubjectKind string

// Subject kinds.
const (
	// SubjectGroup matches anyone in the group, as the login's groups
	// claim lists them.
	SubjectGroup SubjectKind = "group"
	// SubjectUser matches the login's username.
	SubjectUser SubjectKind = "user"
)

// RoleSubject is a user or a group.
type RoleSubject struct {
	// Kind is group or user.
	Kind SubjectKind `json:"kind"`

	// Name is the group's or the user's name.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
}

// TeamStatus is the control state of a team, written only by the Task Manager.
type TeamStatus struct {
	// NextNumber is the human-facing number the next task will receive.
	// It is claimed with a conditional write.
	// +optional
	NextNumber int64 `json:"nextNumber,omitempty"`

	// Slots are the runs holding a slot of the MaxConcurrentRuns semaphore,
	// keyed by run id. Taking and giving back a slot are idempotent. The cap
	// is enforced when a slot is taken, not by the schema: the reconciler may
	// briefly re-add a run that is visibly Running past the cap.
	// +listType=map
	// +listMapKey=runId
	// +optional
	Slots []Slot `json:"slots,omitempty"`

	// Running is len(Slots), written with it, for kubectl's printer column.
	// +kubebuilder:validation:Minimum=0
	// +optional
	Running int32 `json:"running,omitempty"`
}

// Slot is one run holding a slot of the Team's semaphore.
type Slot struct {
	// RunID is the run holding the slot.
	// +kubebuilder:validation:Pattern=`^[0-9A-HJKMNP-TV-Z]{26}$`
	RunID string `json:"runId"`

	// Task is the name of the task the run was claimed for.
	// +kubebuilder:validation:MinLength=1
	Task string `json:"task"`

	// AcquiredAt is when the slot was taken.
	AcquiredAt metav1.Time `json:"acquiredAt"`
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
