package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Transport is how Cadre talks to an MCP server.
// +kubebuilder:validation:Enum=streamable_http
type Transport string

// Transports.
const (
	TransportStreamableHTTP Transport = "streamable_http"
)

// Permission decides what happens when an agent calls a tool.
// +kubebuilder:validation:Enum=allow;deny;ask
type Permission string

// Permissions.
const (
	PermissionAllow Permission = "allow"
	PermissionDeny  Permission = "deny"
	// PermissionAsk ends the run with Waiting{ToolApproval}; the call
	// happens once a human approves it.
	PermissionAsk Permission = "ask"
)

// ToolServerAuth says how Cadre authenticates to a ToolServer: the Secret
// key's value, after Prefix, in the Header of every request.
type ToolServerAuth struct {
	// SecretRef is the Secret key holding the credential.
	SecretRef SecretKeyRef `json:"secretRef"`

	// Header is the request header that carries the credential.
	// +kubebuilder:default=Authorization
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9-]+$`
	// +optional
	Header string `json:"header,omitempty"`

	// Prefix goes before the credential in the header; empty for a bare
	// key such as X-API-Key.
	// +kubebuilder:default="Bearer "
	// +optional
	Prefix *string `json:"prefix,omitempty"`
}

// ManagedServer is a ToolServer Cadre runs itself.
type ManagedServer struct {
	// Image is the server's container image.
	// +kubebuilder:validation:MinLength=1
	Image string `json:"image"`

	// Port is the port the server listens on.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Port int32 `json:"port"`

	// Path is the MCP endpoint's path on the server; default /mcp.
	// +kubebuilder:validation:Pattern=`^/`
	// +kubebuilder:default=/mcp
	// +optional
	Path string `json:"path,omitempty"`

	// ReadinessProbe replaces the server container's default readiness
	// probe, a TCP connect to Port, for a server with a health endpoint.
	// +kubebuilder:validation:XValidation:rule="[has(self.exec), has(self.httpGet), has(self.tcpSocket), has(self.grpc)].filter(x, x).size() == 1",message="set exactly one of exec, httpGet, tcpSocket and grpc"
	// +kubebuilder:validation:XValidation:rule="!has(self.terminationGracePeriodSeconds)",message="a readiness probe takes no terminationGracePeriodSeconds"
	// +optional
	ReadinessProbe *corev1.Probe `json:"readinessProbe,omitempty"`
}

// ToolServerSpec describes one MCP server: an external one at a URL, or one
// Cadre runs.
//
// +kubebuilder:validation:XValidation:rule="has(self.url) != has(self.managed)",message="set exactly one of url and managed"
// +kubebuilder:validation:XValidation:rule="!has(self.managed) || !has(self.auth)",message="a managed server takes no auth: it runs in the cluster, unauthenticated"
type ToolServerSpec struct {
	// Transport is how Cadre talks to the server.
	// +kubebuilder:default=streamable_http
	// +optional
	Transport Transport `json:"transport,omitempty"`

	// URL is an external server's endpoint.
	// +kubebuilder:validation:Pattern=`^https?://`
	// +optional
	URL string `json:"url,omitempty"`

	// Managed makes Cadre run the server: a Deployment and a Service.
	// +optional
	Managed *ManagedServer `json:"managed,omitempty"`

	// Auth is the credential Cadre sends to the server. It never reaches
	// the agent.
	// +optional
	Auth *ToolServerAuth `json:"auth,omitempty"`

	// Notifications lets the namespace's Notifiers call this server's
	// tools that Permissions allow, with its credential, on every event they
	// listen for. Off by default: whoever may create a Notifier would
	// otherwise drive any allowed tool, past the agents' toolRefs.
	// +optional
	Notifications bool `json:"notifications,omitempty"`

	// Permissions decide per tool whether a call is allowed, denied or must
	// be approved by a human first. "*" applies to tools not listed by name.
	// Without permissions every tool is allowed; with them the list is an
	// allowlist, so a tool matching neither a name nor "*" is denied.
	// +optional
	Permissions map[string]Permission `json:"permissions,omitempty"`
}

// ToolServer is one MCP server an agent may use.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced
// +kubebuilder:printcolumn:name="URL",type=string,JSONPath=`.spec.url`
// +kubebuilder:printcolumn:name="Managed",type=string,JSONPath=`.status.url`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type ToolServer struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ToolServerSpec   `json:"spec"`
	Status ToolServerStatus `json:"status,omitempty"`
}

// ToolServerStatus is a ToolServer's status.
type ToolServerStatus struct {
	ConfigStatus `json:",inline"`

	// URL is a managed server's endpoint, at its Service, once Cadre runs
	// it.
	// +optional
	URL string `json:"url,omitempty"`
}

// PermissionFor is what the server's permissions say of calling tool: the
// tool's own entry, else "*", else deny; without permissions, allow.
func (t *ToolServer) PermissionFor(tool string) Permission {
	perms := t.Spec.Permissions
	if len(perms) == 0 {
		return PermissionAllow
	}
	if p, ok := perms[tool]; ok {
		return p
	}
	if p, ok := perms["*"]; ok {
		return p
	}
	return PermissionDeny
}

// ForNotification says whether a Notifier may call tool on the server: it
// takes notifications, and its permissions allow the tool outright, since
// nobody approves a notification. If not, reason is the Notifier's Ready
// reason and message what to tell.
func (t *ToolServer) ForNotification(tool string) (ok bool, reason, message string) {
	if !t.Spec.Notifications {
		return false, "ToolServerNotForNotifications", "ToolServer " + t.Name + " does not take notifications: its spec.notifications is not set"
	}
	if p := t.PermissionFor(tool); p != PermissionAllow {
		return false, "ToolNotAllowed", "ToolServer " + t.Name + "'s permissions say " + string(p) + " for " + tool + ": a notification needs allow"
	}
	return true, "", ""
}

// Endpoint is where the server is reached: its url, or for a managed one
// the URL Cadre gave it.
func (t *ToolServer) Endpoint() string {
	if t.Spec.Managed != nil {
		return t.Status.URL
	}
	return t.Spec.URL
}

// ToolServerList is a list of ToolServers.
//
// +kubebuilder:object:root=true
type ToolServerList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ToolServer `json:"items"`
}
