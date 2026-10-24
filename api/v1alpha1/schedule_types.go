package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// LabelSchedule labels a Task with the Schedule that created it.
const LabelSchedule = "cadre.dev/schedule"

// AnnotationScheduledAt holds the tick, in RFC 3339, a scheduled Task was
// created for.
const AnnotationScheduledAt = "cadre.dev/scheduled-at"

// ScheduleSpec says when to create a task, and which.
//
// +kubebuilder:validation:XValidation:rule="!has(self.task.parentRef)",message="a scheduled task has no parent"
type ScheduleSpec struct {
	// Cron is a standard five-field cron expression, or a descriptor such
	// as @daily, in Timezone.
	// +kubebuilder:validation:MinLength=1
	Cron string `json:"cron"`

	// Timezone is the IANA time zone Cron is read in; default UTC.
	// +optional
	Timezone string `json:"timezone,omitempty"`

	// Jitter delays each tick's task by up to this long, by an amount fixed
	// per Schedule and tick, so that schedules on the same cron do not all
	// fire at once. Keep it shorter than the interval between ticks.
	// +optional
	Jitter *metav1.Duration `json:"jitter,omitempty"`

	// Suspend stops new tasks. A tick that passes while suspended never
	// fires, even once resumed.
	// +optional
	Suspend bool `json:"suspend,omitempty"`

	// Task is the spec of every task the Schedule creates.
	Task TaskSpec `json:"task"`
}

// ScheduleStatus is a Schedule's status.
type ScheduleStatus struct {
	ConfigStatus `json:",inline"`

	// LastScheduleTime is the last tick a task was created for.
	// +optional
	LastScheduleTime *metav1.Time `json:"lastScheduleTime,omitempty"`

	// NextScheduleTime is when the next task is due, jitter included.
	// +optional
	NextScheduleTime *metav1.Time `json:"nextScheduleTime,omitempty"`

	// LastTask is the name of the last task created.
	// +optional
	LastTask string `json:"lastTask,omitempty"`

	// ActiveSince is when the Schedule was last seen unsuspended after
	// being suspended, or its creation: no tick before it fires. It is
	// cleared while suspended.
	// +optional
	ActiveSince *metav1.Time `json:"activeSince,omitempty"`
}

// Schedule creates a Task on a cron schedule, once per tick.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced
// +kubebuilder:validation:XValidation:rule="size(self.metadata.name) <= 50",message="a Schedule's name is at most 50 characters, so that its tasks' names (the name, a dash and 12 digits) fit 63"
// +kubebuilder:printcolumn:name="Cron",type=string,JSONPath=`.spec.cron`
// +kubebuilder:printcolumn:name="Timezone",type=string,JSONPath=`.spec.timezone`
// +kubebuilder:printcolumn:name="Suspended",type=boolean,JSONPath=`.spec.suspend`
// +kubebuilder:printcolumn:name="Next",type=string,JSONPath=`.status.nextScheduleTime`
// +kubebuilder:printcolumn:name="Last task",type=string,JSONPath=`.status.lastTask`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type Schedule struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ScheduleSpec   `json:"spec"`
	Status ScheduleStatus `json:"status,omitempty"`
}

// ScheduleList is a list of Schedules.
//
// +kubebuilder:object:root=true
type ScheduleList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Schedule `json:"items"`
}
