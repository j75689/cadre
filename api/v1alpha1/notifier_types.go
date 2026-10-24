package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// NotifyEvent is what a Notifier is told of: a task starting to wait on a
// human, for one of the Waiting reasons, or ending.
// +kubebuilder:validation:Enum=Question;ToolApproval;Review;Done;Failed;Cancelled
type NotifyEvent string

// Notify events.
const (
	NotifyQuestion     NotifyEvent = "Question"
	NotifyToolApproval NotifyEvent = "ToolApproval"
	NotifyReview       NotifyEvent = "Review"
	NotifyDone         NotifyEvent = "Done"
	NotifyFailed       NotifyEvent = "Failed"
	NotifyCancelled    NotifyEvent = "Cancelled"
)

// Webhook is an HTTP endpoint that receives each notification as a JSON
// POST.
//
// +kubebuilder:validation:XValidation:rule="!has(self.headers) || !self.headers.exists(k, k.lowerAscii() == 'authorization')",message="headers are static and readable by anyone who can read the Notifier: put no credential there; set secretRef, and the receiver checks the signature"
type Webhook struct {
	// URL receives the POSTs.
	// +kubebuilder:validation:Pattern=`^https?://`
	URL string `json:"url"`

	// SecretRef is a Secret key holding a signing secret: when set, every
	// delivery carries X-Cadre-Signature: sha256=<hex HMAC-SHA256 of the
	// body under the secret>.
	// +optional
	SecretRef *SecretKeyRef `json:"secretRef,omitempty"`

	// Headers are static headers sent with every delivery.
	// +optional
	Headers map[string]string `json:"headers,omitempty"`
}

// NotifierSpec says what to notify of, and where.
type NotifierSpec struct {
	// On lists the events notified.
	// +kubebuilder:validation:MinItems=1
	// +listType=set
	On []NotifyEvent `json:"on"`

	// Webhook is where notifications go.
	Webhook Webhook `json:"webhook"`
}

// DeliveryError is a delivery that failed.
type DeliveryError struct {
	// Message says how it failed.
	Message string `json:"message"`
	// At is when.
	At metav1.Time `json:"at"`
}

// NotifierStatus is a Notifier's status.
type NotifierStatus struct {
	ConfigStatus `json:",inline"`

	// LastSent is when a notification was last delivered.
	// +optional
	LastSent *metav1.Time `json:"lastSent,omitempty"`

	// LastError is the last delivery that failed.
	// +optional
	LastError *DeliveryError `json:"lastError,omitempty"`
}

// Notifier tells a channel when a task needs a human or ends.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced
// +kubebuilder:printcolumn:name="On",type=string,JSONPath=`.spec.on`
// +kubebuilder:printcolumn:name="URL",type=string,JSONPath=`.spec.webhook.url`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Last sent",type=string,JSONPath=`.status.lastSent`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type Notifier struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   NotifierSpec   `json:"spec"`
	Status NotifierStatus `json:"status,omitempty"`
}

// Listens reports whether n is told of ev.
func (n *Notifier) Listens(ev NotifyEvent) bool {
	for _, e := range n.Spec.On {
		if e == ev {
			return true
		}
	}
	return false
}

// NotifierList is a list of Notifiers.
//
// +kubebuilder:object:root=true
type NotifierList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Notifier `json:"items"`
}
