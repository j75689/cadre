package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Priority orders tasks on the board.
// +kubebuilder:validation:Enum=low;medium;high;urgent
type Priority string

// Task priorities.
const (
	PriorityLow    Priority = "low"
	PriorityMedium Priority = "medium"
	PriorityHigh   Priority = "high"
	PriorityUrgent Priority = "urgent"
)

// AssigneeKind says whether a task is assigned to an agent or a human.
// +kubebuilder:validation:Enum=Agent;User
type AssigneeKind string

// Assignee kinds.
const (
	AssigneeAgent AssigneeKind = "Agent"
	AssigneeUser  AssigneeKind = "User"
)

// Assignee names who works on a task.
type Assignee struct {
	// Kind is Agent or User.
	Kind AssigneeKind `json:"kind"`

	// Name is the Agent's name in the task's namespace, or the user's name.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
}

// TaskReference points at another Task in the same namespace.
type TaskReference struct {
	// Name is the referenced Task's metadata.name, a DNS subdomain.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	Name string `json:"name"`
}

// TaskSpec is the human intent of a task. Humans and GitOps write it; the
// Task Manager never does.
type TaskSpec struct {
	// Title is the one-line summary shown on the board.
	// +kubebuilder:validation:MinLength=1
	Title string `json:"title"`

	// Description is the full statement of the work.
	// +optional
	Description string `json:"description,omitempty"`

	// Assignee is who works on the task. An unassigned task is never claimed.
	// +optional
	Assignee *Assignee `json:"assignee,omitempty"`

	// Priority orders the task on the board.
	// +kubebuilder:default=medium
	// +optional
	Priority Priority `json:"priority,omitempty"`

	// ParentRef makes this task a subtask of another task.
	// +optional
	ParentRef *TaskReference `json:"parentRef,omitempty"`

	// Labels are free-form tags for filtering the board.
	// +listType=set
	// +optional
	Labels []string `json:"labels,omitempty"`
}

// Phase is where a task is in its life. There are exactly seven.
// +kubebuilder:validation:Enum=Todo;Running;Waiting;Review;Done;Cancelled;Failed
type Phase string

// Task phases.
const (
	PhaseTodo      Phase = "Todo"
	PhaseRunning   Phase = "Running"
	PhaseWaiting   Phase = "Waiting"
	PhaseReview    Phase = "Review"
	PhaseDone      Phase = "Done"
	PhaseCancelled Phase = "Cancelled"
	PhaseFailed    Phase = "Failed"
)

// WaitingReason says what a Waiting task waits for.
// +kubebuilder:validation:Enum=Question;ToolApproval;Review
type WaitingReason string

// Waiting reasons.
const (
	WaitingQuestion     WaitingReason = "Question"
	WaitingToolApproval WaitingReason = "ToolApproval"
	WaitingReview       WaitingReason = "Review"
)

// Waiting describes what a Waiting task waits for. It is set only while the
// phase is Waiting.
type Waiting struct {
	// Reason is what the task waits for.
	Reason WaitingReason `json:"reason"`

	// Since is when the task started waiting.
	Since metav1.Time `json:"since"`

	// ThreadSeq is the thread entry that asked for the human, if any. Thread
	// sequences start at 1, so an unset ThreadSeq means none, or one not
	// written yet: see ItemID.
	// +kubebuilder:validation:Minimum=1
	// +optional
	ThreadSeq int64 `json:"threadSeq,omitempty"`

	// ItemID is the outbox item that writes the entry asking for the
	// human, when the run ended before it was written; ThreadSeq is set
	// once it is.
	// +optional
	ItemID string `json:"itemId,omitempty"`
}

// RunMode is who drives the agent loop of a run.
// +kubebuilder:validation:Enum=runtime;step
type RunMode string

// Run modes.
const (
	RunModeRuntime RunMode = "runtime"
	RunModeStep    RunMode = "step"
)

// Run is the control state of the run in flight. It is set only while the
// phase is Running, and its ID fences every transition the run makes.
type Run struct {
	// ID is the run's ULID; the per-run MCP token maps to it.
	// +kubebuilder:validation:Pattern=`^[0-9A-HJKMNP-TV-Z]{26}$`
	ID string `json:"id"`

	// Mode is runtime or step.
	Mode RunMode `json:"mode"`

	// Runner is who holds the run: the authenticated identity of the runner
	// in runtime mode, or of the worker holding the current step in step
	// mode. Only requests from it may renew or finish the run. It is empty
	// for a run claimed on a server that does not authenticate runners;
	// such a run is not fenced on its sender.
	// +optional
	Runner string `json:"runner,omitempty"`

	// RunnerName is the name the runner gave itself, for display.
	// +optional
	RunnerName string `json:"runnerName,omitempty"`

	// Attempt counts the workers that took the current step, in step mode:
	// a worker that dies writes nothing, but its take is counted. It is
	// reset when the step is committed.
	// +kubebuilder:validation:Minimum=0
	// +optional
	Attempt int32 `json:"attempt,omitempty"`

	// Step advances in step mode only.
	// +kubebuilder:validation:Minimum=0
	// +optional
	Step int64 `json:"step,omitempty"`

	// StepItems are the outbox items the current step recorded, in step
	// mode: a step done again after its worker died records the same
	// items, which are not recorded twice, even once applied. They are
	// cleared when the step is committed.
	// +listType=map
	// +listMapKey=id
	// +optional
	StepItems []StepItem `json:"stepItems,omitempty"`

	// StepFree is true, in step mode, while the current step waits for a
	// worker: Runner and RunnerName are then empty. It is the only sign of
	// a free step, as a run claimed without authentication has no Runner
	// either.
	// +optional
	StepFree bool `json:"stepFree,omitempty"`

	// LastRunner and LastRunnerName are the worker that held the last
	// step taken, in step mode, kept while the step is free so a run that
	// fails then records who ran it last.
	// +optional
	LastRunner string `json:"lastRunner,omitempty"`
	// +optional
	LastRunnerName string `json:"lastRunnerName,omitempty"`

	// LeaseUntil is when the run may be reclaimed unless it is renewed. In
	// step mode it is the held step's lease; for a free step it is the idle
	// deadline, by which a worker must take the step or the run fails.
	LeaseUntil metav1.Time `json:"leaseUntil"`

	// MCPTokenHash is the hex SHA-256 of the run's per-run MCP token. The
	// token itself is handed to the runner once, at claim, and never
	// stored; it is bound to this run and stops working when the run ends.
	// +kubebuilder:validation:Pattern=`^[0-9a-f]{64}$`
	// +optional
	MCPTokenHash string `json:"mcpTokenHash,omitempty"`
}

// StepItem is an outbox item a step recorded, and where its entry is
// looked for: the thread revision or file version it was recorded after.
type StepItem struct {
	ID    string `json:"id"`
	After int64  `json:"after,omitempty"`
}

// OutboxKind is the kind of side effect an outbox item asks for.
// +kubebuilder:validation:Enum=CreateSubtask;RunEnded;AppendThread;WriteFile;Notify
type OutboxKind string

// Outbox kinds.
const (
	// OutboxCreateSubtask creates the subtask in Spec.
	OutboxCreateSubtask OutboxKind = "CreateSubtask"
	// OutboxRunEnded appends Run to the task's run history.
	OutboxRunEnded OutboxKind = "RunEnded"
	// OutboxAppendThread appends a run's thread entry, its body written
	// beforehand as a blob.
	OutboxAppendThread OutboxKind = "AppendThread"
	// OutboxWriteFile writes a new version of a task file, its content
	// written beforehand as a blob.
	OutboxWriteFile OutboxKind = "WriteFile"
	// OutboxNotify tells the Notifiers listening for Notify.Event.
	OutboxNotify OutboxKind = "Notify"
)

// Notification is what a Notify item tells, and to whom it was told.
type Notification struct {
	// Event is what happened.
	Event NotifyEvent `json:"event"`
	// ThreadSeq is the thread entry the task waits on, if known yet.
	// +optional
	ThreadSeq int64 `json:"threadSeq,omitempty"`
	// WaitingItem is the outbox item of the entry the task waits on, when
	// the entry was not in the thread yet: its sequence is filled in from
	// the task's wait only while the task still waits on that item.
	// +optional
	WaitingItem string `json:"waitingItem,omitempty"`
	// At is when it happened.
	At metav1.Time `json:"at"`
	// To names the Notifiers listening when it happened: the only ones it
	// goes to.
	// +listType=set
	// +optional
	To []string `json:"to,omitempty"`
	// Delivered names the Notifiers that took it, so a pass that finds
	// one still failing does not send it to the others again.
	// +listType=set
	// +optional
	Delivered []string `json:"delivered,omitempty"`
	// SendingBy and SendingUntil are the server sending it now, and until
	// when it may: no other server sends it meanwhile.
	// +optional
	SendingBy string `json:"sendingBy,omitempty"`
	// +optional
	SendingUntil *metav1.Time `json:"sendingUntil,omitempty"`
}

// ThreadAppend is a thread entry a run committed: the second phase of a
// fenced write, after its body was written as a blob.
type ThreadAppend struct {
	// Blob is the key of the body in the task's blob stream.
	// +kubebuilder:validation:MinLength=1
	Blob string `json:"blob"`
	// Kind is the entry kind: Comment, Question or ToolCall.
	// +kubebuilder:validation:MinLength=1
	Kind string `json:"kind"`
	// Author is the agent writing it.
	// +kubebuilder:validation:MinLength=1
	Author string `json:"author"`
	// RunID is the run that wrote it.
	RunID string `json:"runId"`
	// After is the thread revision when the item was committed: the entry,
	// if an earlier apply wrote it, is after it.
	// +kubebuilder:validation:Minimum=0
	// +optional
	After int64 `json:"after,omitempty"`
}

// FileWrite is a task file version a run committed: the second phase of a
// fenced write, after its content was written as a blob.
type FileWrite struct {
	// Blob is the key of the content in the task's blob stream.
	// +kubebuilder:validation:MinLength=1
	Blob string `json:"blob"`
	// Path is the file's path.
	// +kubebuilder:validation:MinLength=1
	Path string `json:"path"`
	// Size is the content's size in bytes.
	// +kubebuilder:validation:Minimum=0
	Size int64 `json:"size"`
	// After is the file's latest version when the item was committed: the
	// version, if an earlier apply wrote it, is after it.
	// +kubebuilder:validation:Minimum=0
	// +optional
	After int64 `json:"after,omitempty"`
}

// RunEndReason says why a run ended.
// +kubebuilder:validation:Enum=Finished;Interrupted;Cancelled
type RunEndReason string

// Run end reasons.
const (
	RunFinished    RunEndReason = "Finished"
	RunInterrupted RunEndReason = "Interrupted" // its lease expired and it was reclaimed
	RunCancelled   RunEndReason = "Cancelled"
)

// RunOutcome is how a finished run ended, as reported by the runner.
// +kubebuilder:validation:Enum=Done;Review;Waiting;Failed
type RunOutcome string

// Run outcomes.
const (
	RunOutcomeDone    RunOutcome = "Done"
	RunOutcomeReview  RunOutcome = "Review"
	RunOutcomeWaiting RunOutcome = "Waiting"
	RunOutcomeFailed  RunOutcome = "Failed"
)

// RunRecord is a run that has ended, as kept in the run history.
type RunRecord struct {
	// ID is the run's ULID.
	// +kubebuilder:validation:Pattern=`^[0-9A-HJKMNP-TV-Z]{26}$`
	ID string `json:"id"`

	// Mode is runtime or step.
	Mode RunMode `json:"mode"`

	// Runner is the runner, or the worker of the last step, that held the
	// run: its authenticated identity, as on Run.
	// +optional
	Runner string `json:"runner,omitempty"`

	// RunnerName is the name the runner gave itself, for display.
	// +optional
	RunnerName string `json:"runnerName,omitempty"`

	// Step is the step the run had reached.
	// +kubebuilder:validation:Minimum=0
	// +optional
	Step int64 `json:"step,omitempty"`

	// Attempt is how many workers took the step the run had reached, in
	// step mode.
	// +kubebuilder:validation:Minimum=0
	// +optional
	Attempt int32 `json:"attempt,omitempty"`

	// Reason is why the run ended.
	Reason RunEndReason `json:"reason"`

	// Outcome is set when Reason is Finished.
	// +optional
	Outcome RunOutcome `json:"outcome,omitempty"`

	// EndedAt is when the run ended.
	EndedAt metav1.Time `json:"endedAt"`
}

// OutboxItem is a side effect written in the same conditional write as the
// state change that caused it. The dispatcher applies it idempotently and then
// clears it with another conditional write.
//
// +kubebuilder:validation:XValidation:rule="self.kind != 'CreateSubtask' || has(self.spec)",message="a CreateSubtask item needs spec"
// +kubebuilder:validation:XValidation:rule="self.kind != 'RunEnded' || has(self.run)",message="a RunEnded item needs run"
// +kubebuilder:validation:XValidation:rule="self.kind != 'AppendThread' || has(self.thread)",message="an AppendThread item needs thread"
// +kubebuilder:validation:XValidation:rule="self.kind != 'WriteFile' || has(self.file)",message="a WriteFile item needs file"
// +kubebuilder:validation:XValidation:rule="self.kind != 'Notify' || has(self.notify)",message="a Notify item needs notify"
type OutboxItem struct {
	// ID is a ULID and the item's idempotency key.
	//
	// For CreateSubtask it is minted when the item is written, and the
	// subtask's metadata.name is derived from it deterministically (the
	// lowercase ULID, a valid DNS-1123 label), so re-applying the item is a
	// create that already exists and leaves no trace.
	//
	// For RunEnded it is the ended run's id: a run ends once, and the
	// dispatcher appends the record to the run history keyed on it.
	// +kubebuilder:validation:Pattern=`^[0-9A-HJKMNP-TV-Z]{26}$`
	ID string `json:"id"`

	// Kind is the side effect asked for.
	Kind OutboxKind `json:"kind"`

	// Spec is the subtask to create, for kind CreateSubtask.
	// +optional
	Spec *TaskSpec `json:"spec,omitempty"`

	// Run is the ended run to record, for kind RunEnded.
	// +optional
	Run *RunRecord `json:"run,omitempty"`

	// Thread is the entry to append, for kind AppendThread.
	// +optional
	Thread *ThreadAppend `json:"thread,omitempty"`

	// File is the version to write, for kind WriteFile.
	// +optional
	File *FileWrite `json:"file,omitempty"`

	// Notify is the notification, for kind Notify.
	// +optional
	Notify *Notification `json:"notify,omitempty"`

	// Seq is where an AppendThread or WriteFile item writes, its thread
	// sequence or file version, reserved here before it is written so
	// every dispatcher applying the item writes the same key.
	// +kubebuilder:validation:Minimum=0
	// +optional
	Seq int64 `json:"seq,omitempty"`
}

// ThreadStatus points at the task's thread in the log.
type ThreadStatus struct {
	// Revision advances with every append, so watching the Task is enough to
	// learn the thread changed.
	// +kubebuilder:validation:Minimum=0
	// +optional
	Revision int64 `json:"revision,omitempty"`

	// Store is the URI of the thread in object storage.
	// +optional
	Store string `json:"store,omitempty"`
}

// RunsStatus points at the task's run history in the log.
type RunsStatus struct {
	// Count is the number of runs started so far.
	// +kubebuilder:validation:Minimum=0
	// +optional
	Count int64 `json:"count,omitempty"`

	// LastID is the ULID of the most recent run.
	// +optional
	LastID string `json:"lastId,omitempty"`

	// Store is the URI of the run history in object storage.
	// +optional
	Store string `json:"store,omitempty"`
}

// TaskStatus is the control state of a task. Only the Task Manager writes it,
// through the status subresource. It holds control state and pointers only.
type TaskStatus struct {
	// Number is the human-facing number, claimed from the Team's counter.
	// +optional
	Number int64 `json:"number,omitempty"`

	// Phase is where the task is in its life.
	// +optional
	Phase Phase `json:"phase,omitempty"`

	// Waiting is set only while the phase is Waiting.
	// +optional
	Waiting *Waiting `json:"waiting,omitempty"`

	// Run is set only while the phase is Running.
	// +optional
	Run *Run `json:"run,omitempty"`

	// Outbox holds side effects not yet applied.
	// +listType=map
	// +listMapKey=id
	// +optional
	Outbox []OutboxItem `json:"outbox,omitempty"`

	// Thread points at the task's thread.
	// +optional
	Thread *ThreadStatus `json:"thread,omitempty"`

	// Runs points at the task's run history.
	// +optional
	Runs *RunsStatus `json:"runs,omitempty"`

	// Conditions are the standard Kubernetes conditions.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// Task is a work item on the board.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced
// +kubebuilder:validation:XValidation:rule="!has(self.spec.parentRef) || self.spec.parentRef.name != self.metadata.name",message="a Task cannot be its own parent"
// +kubebuilder:printcolumn:name="Number",type=integer,JSONPath=`.status.number`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Assignee",type=string,JSONPath=`.spec.assignee.name`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type Task struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   TaskSpec   `json:"spec"`
	Status TaskStatus `json:"status,omitempty"`
}

// TaskList is a list of Tasks.
//
// +kubebuilder:object:root=true
type TaskList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Task `json:"items"`
}
