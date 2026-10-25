package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// LabelRunner names the Runner a minted token Secret belongs to, so the
// Secrets can be found.
const LabelRunner = "cadre.dev/runner"

// ServiceAccountRef names a ServiceAccount in the referring object's own
// namespace.
type ServiceAccountRef struct {
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
}

// RunnerSpec says who a runner is and which agents' tasks it may claim in
// its namespace.
type RunnerSpec struct {
	// ServiceAccount, for a runner in the cluster, is the ServiceAccount it
	// authenticates as, with a projected token for the audience cadre. It
	// is the ServiceAccount of that name in the Runner's own namespace: a
	// Team cannot enlist another Team's runner. Without it the runner is outside the cluster, and
	// Cadre mints a token for it into the Secret <name>-token.
	// +optional
	ServiceAccount *ServiceAccountRef `json:"serviceAccount,omitempty"`

	// Labels are what the runner's claims are matched with: an agent of
	// runtime type runner whose selector they include. The server takes
	// them from here, never from the runner's request. The cadre.dev/
	// prefix is reserved. A Runner without labels could claim nothing.
	// +kubebuilder:validation:MinProperties=1
	// +kubebuilder:validation:XValidation:rule="self.all(k, !k.startsWith('cadre.dev/'))",message="the cadre.dev/ label prefix is reserved"
	Labels map[string]string `json:"labels"`
}

// RunnerStatus is a Runner's status.
type RunnerStatus struct {
	ConfigStatus `json:",inline"`
	MintedToken  `json:",inline"`
}

// MintedToken is the token Cadre mints for an object without a
// ServiceAccount, a Runner or an AgentAccess: where it is and its hash.
type MintedToken struct {
	// TokenSecret is the Secret holding the minted token.
	// +optional
	TokenSecret string `json:"tokenSecret,omitempty"`

	// TokenHash is the SHA-256 of the minted token, hex. A token whose
	// hash is not this one is refused, so replacing the Secret's token
	// revokes the old one.
	// +optional
	TokenHash string `json:"tokenHash,omitempty"`

	// MintedAt is when the current token was minted.
	// +optional
	MintedAt *metav1.Time `json:"mintedAt,omitempty"`
}

// Runner is a runner allowed to claim tasks in its namespace: Cadre's
// tenant boundary. A runner serving several Teams has a Runner in each.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced
// +kubebuilder:validation:XValidation:rule="oldSelf.hasValue() || (size(self.metadata.name) <= 63 && !self.metadata.name.contains('.'))",message="a Runner's name is a DNS label (at most 63 characters, no dot): it names the Secret <name>-token and labels it",optionalOldSelf=true
// +kubebuilder:printcolumn:name="ServiceAccount",type=string,JSONPath=`.spec.serviceAccount.name`
// +kubebuilder:printcolumn:name="Token",type=string,JSONPath=`.status.tokenSecret`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type Runner struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   RunnerSpec   `json:"spec"`
	Status RunnerStatus `json:"status,omitempty"`
}

// RunnerList is a list of Runners.
//
// +kubebuilder:object:root=true
type RunnerList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Runner `json:"items"`
}
