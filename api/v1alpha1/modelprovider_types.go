package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ProviderType is the kind of model API a ModelProvider speaks.
// +kubebuilder:validation:Enum=anthropic;openai;bedrock;vertex
type ProviderType string

// Provider types.
const (
	ProviderAnthropic ProviderType = "anthropic"
	ProviderOpenAI    ProviderType = "openai" // any OpenAI-compatible endpoint
	ProviderBedrock   ProviderType = "bedrock"
	ProviderVertex    ProviderType = "vertex"
)

// ModelProviderSpec says how to reach one model.
//
// +kubebuilder:validation:XValidation:rule="!(self.type in ['anthropic', 'openai']) || has(self.auth)",message="anthropic and openai providers need auth"
// +kubebuilder:validation:XValidation:rule="!(self.type in ['bedrock', 'vertex']) || has(self.auth) || (has(self.workloadIdentity) && self.workloadIdentity)",message="bedrock and vertex providers need auth or workloadIdentity"
// +kubebuilder:validation:XValidation:rule="!has(self.baseURL) || self.type in ['anthropic', 'openai']",message="baseURL is for anthropic and openai providers"
// +kubebuilder:validation:XValidation:rule="!(has(self.workloadIdentity) && self.workloadIdentity) || self.type in ['bedrock', 'vertex']",message="workloadIdentity is for bedrock and vertex providers"
// +kubebuilder:validation:XValidation:rule="self.type != 'bedrock' || has(self.region)",message="bedrock providers need region"
// +kubebuilder:validation:XValidation:rule="self.type != 'vertex' || (has(self.project) && has(self.location))",message="vertex providers need project and location"
// +kubebuilder:validation:XValidation:rule="!has(self.region) || self.type == 'bedrock'",message="region is for bedrock providers"
// +kubebuilder:validation:XValidation:rule="!(has(self.project) || has(self.location)) || self.type == 'vertex'",message="project and location are for vertex providers"
type ModelProviderSpec struct {
	// Type is the model API: anthropic, openai (any OpenAI-compatible
	// endpoint), bedrock or vertex.
	Type ProviderType `json:"type"`

	// Model is the model to use, as the provider names it.
	// +kubebuilder:validation:MinLength=1
	Model string `json:"model"`

	// Auth is the API key. Bedrock and Vertex can use workload identity
	// instead.
	// +optional
	Auth *Auth `json:"auth,omitempty"`

	// BaseURL overrides the provider's endpoint: an OpenAI-compatible
	// server, or a gateway in front of the provider.
	// +kubebuilder:validation:Pattern=`^https?://`
	// +optional
	BaseURL string `json:"baseURL,omitempty"`

	// WorkloadIdentity authenticates to Bedrock or Vertex as the
	// ServiceAccount, with no key.
	// +optional
	WorkloadIdentity bool `json:"workloadIdentity,omitempty"`

	// Region is the AWS region, for bedrock.
	// +kubebuilder:validation:MinLength=1
	// +optional
	Region string `json:"region,omitempty"`

	// Project is the Google Cloud project, for vertex.
	// +kubebuilder:validation:MinLength=1
	// +optional
	Project string `json:"project,omitempty"`

	// Location is the Google Cloud location, such as us-east5, for vertex.
	// +kubebuilder:validation:MinLength=1
	// +optional
	Location string `json:"location,omitempty"`
}

// ModelProvider is a model, its endpoint and its credentials.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced
// +kubebuilder:printcolumn:name="Type",type=string,JSONPath=`.spec.type`
// +kubebuilder:printcolumn:name="Model",type=string,JSONPath=`.spec.model`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type ModelProvider struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ModelProviderSpec `json:"spec"`
	Status ConfigStatus      `json:"status,omitempty"`
}

// ModelProviderList is a list of ModelProviders.
//
// +kubebuilder:object:root=true
type ModelProviderList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ModelProvider `json:"items"`
}
