package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// SecretKeyRef names one key of a Secret in the same namespace.
type SecretKeyRef struct {
	// Name is the Secret's name.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// Key is the key within the Secret.
	// +kubebuilder:validation:MinLength=1
	Key string `json:"key"`
}

// Auth says where a credential comes from.
type Auth struct {
	// SecretRef is the Secret key holding the credential.
	SecretRef SecretKeyRef `json:"secretRef"`
}

// ConditionReady is the condition type every configuration resource reports.
const ConditionReady = "Ready"

// ConfigStatus is the status of a configuration resource: whether it can be
// used, and for which generation of its spec that was decided.
type ConfigStatus struct {
	// ObservedGeneration is the spec generation the conditions describe.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions are the standard Kubernetes conditions; Ready says whether
	// the resource can be used.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}
