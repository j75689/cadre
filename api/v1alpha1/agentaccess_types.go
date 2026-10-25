package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// LabelAgentAccess names the AgentAccess a minted token Secret belongs to,
// so the Secrets can be found.
const LabelAgentAccess = "cadre.dev/agentaccess"

// AgentRole is the role an AgentAccess acts with on its Team's board: at
// most member, so an agent can never manage the Team.
// +kubebuilder:validation:Enum=member;viewer
type AgentRole string

// AgentAccessSpec says how an agent outside Cadre authenticates to the
// board, and with which role.
type AgentAccessSpec struct {
	// Role is the agent's role on this namespace's Team, and on no other:
	// viewer or member.
	Role AgentRole `json:"role"`

	// ServiceAccount, for an agent in the cluster, is the ServiceAccount it
	// authenticates as, with a projected token for the audience cadre. It
	// is the ServiceAccount of that name in the AgentAccess's own
	// namespace. Without it Cadre mints a token for the agent into the
	// Secret <name>-token.
	// +optional
	ServiceAccount *ServiceAccountRef `json:"serviceAccount,omitempty"`
}

// AgentAccessStatus is an AgentAccess's status.
type AgentAccessStatus struct {
	ConfigStatus `json:",inline"`
	MintedToken  `json:",inline"`
}

// AgentAccess lets an agent outside Cadre, a bot or a scheduled job say,
// use its namespace's board, /mcp and the API, as agent:<namespace>/<name>
// with the role it names.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced
// +kubebuilder:printcolumn:name="Role",type=string,JSONPath=`.spec.role`
// +kubebuilder:printcolumn:name="ServiceAccount",type=string,JSONPath=`.spec.serviceAccount.name`
// +kubebuilder:printcolumn:name="Token",type=string,JSONPath=`.status.tokenSecret`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type AgentAccess struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   AgentAccessSpec   `json:"spec"`
	Status AgentAccessStatus `json:"status,omitempty"`
}

// AgentAccessList is a list of AgentAccesses.
//
// +kubebuilder:object:root=true
type AgentAccessList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AgentAccess `json:"items"`
}
