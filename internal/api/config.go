package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	"github.com/j75689/cadre/internal/auth"
	"github.com/j75689/cadre/internal/authz"
	"github.com/j75689/cadre/internal/httpjson"
	"github.com/j75689/cadre/internal/manager"
	"github.com/j75689/cadre/internal/storage"
)

// The configuration kinds the API reads, through the controllers' cache.
// +kubebuilder:rbac:groups=cadre.dev,resources=agents;modelproviders;toolservers;schedules;notifiers;runners,verbs=get;list;watch

// lastApplied is where kubectl apply keeps the last spec it applied: the
// whole spec, so it is never served, whatever the role.
const lastApplied = "kubectl.kubernetes.io/last-applied-configuration"

// configKind is a configuration resource the API serves read-only. A kind
// is served by adding its row to configKinds.
type configKind struct {
	plural, kind string
	newObj       func() client.Object
	newList      func() client.ObjectList
	status       func(client.Object) v1alpha1.ConfigStatus
	// view is obj's spec and status as a member, or an admin, may see
	// them: without secrets, and redacted for a member where the spec may
	// hold a credential. Viewers never get here.
	view func(obj client.Object, role authz.Role) (spec, status any)
}

var configKinds = []configKind{{
	plural: "agents", kind: "Agent",
	newObj:  func() client.Object { return &v1alpha1.Agent{} },
	newList: func() client.ObjectList { return &v1alpha1.AgentList{} },
	status:  func(o client.Object) v1alpha1.ConfigStatus { return o.(*v1alpha1.Agent).Status },
	view: func(o client.Object, role authz.Role) (any, any) {
		a := o.(*v1alpha1.Agent)
		if role >= authz.Admin || a.Spec.Runtime.Job == nil {
			return a.Spec, a.Status
		}
		spec := a.Spec.DeepCopy()
		pod := &spec.Runtime.Job.Template.Spec
		maskContainers(pod.InitContainers)
		maskContainers(pod.Containers)
		return *spec, a.Status
	},
}, {
	plural: "modelproviders", kind: "ModelProvider",
	newObj:  func() client.Object { return &v1alpha1.ModelProvider{} },
	newList: func() client.ObjectList { return &v1alpha1.ModelProviderList{} },
	status:  func(o client.Object) v1alpha1.ConfigStatus { return o.(*v1alpha1.ModelProvider).Status },
	view: func(o client.Object, role authz.Role) (any, any) {
		p := o.(*v1alpha1.ModelProvider)
		if role >= authz.Admin {
			return p.Spec, p.Status
		}
		spec := p.Spec
		spec.BaseURL = maskURL(spec.BaseURL)
		return spec, p.Status
	},
}, {
	plural: "toolservers", kind: "ToolServer",
	newObj:  func() client.Object { return &v1alpha1.ToolServer{} },
	newList: func() client.ObjectList { return &v1alpha1.ToolServerList{} },
	status:  func(o client.Object) v1alpha1.ConfigStatus { return o.(*v1alpha1.ToolServer).Status.ConfigStatus },
	view: func(o client.Object, role authz.Role) (any, any) {
		ts := o.(*v1alpha1.ToolServer)
		if role >= authz.Admin {
			return ts.Spec, ts.Status
		}
		spec, status := ts.Spec.DeepCopy(), ts.Status
		spec.URL = maskURL(spec.URL)
		status.URL = maskServiceURL(status.URL)
		if a := spec.Auth; a != nil && a.Prefix != nil && !authScheme.MatchString(*a.Prefix) {
			*a.Prefix = set
		}
		if m := spec.Managed; m != nil {
			m.Path = maskURL(m.Path)
			if m.ReadinessProbe != nil {
				maskProbe(m.ReadinessProbe)
			}
		}
		return *spec, status
	},
}, {
	// A Schedule's task template is not masked: each Task it creates holds
	// it as it is, and a viewer reads those.
	plural: "schedules", kind: "Schedule",
	newObj:  func() client.Object { return &v1alpha1.Schedule{} },
	newList: func() client.ObjectList { return &v1alpha1.ScheduleList{} },
	status:  func(o client.Object) v1alpha1.ConfigStatus { return o.(*v1alpha1.Schedule).Status.ConfigStatus },
	view: func(o client.Object, _ authz.Role) (any, any) {
		s := o.(*v1alpha1.Schedule)
		return s.Spec, s.Status
	},
}, {
	plural: "notifiers", kind: "Notifier",
	newObj:  func() client.Object { return &v1alpha1.Notifier{} },
	newList: func() client.ObjectList { return &v1alpha1.NotifierList{} },
	status:  func(o client.Object) v1alpha1.ConfigStatus { return o.(*v1alpha1.Notifier).Status.ConfigStatus },
	view: func(o client.Object, role authz.Role) (any, any) {
		n := o.(*v1alpha1.Notifier)
		if role >= authz.Admin {
			return n.Spec, n.Status
		}
		spec := n.Spec.DeepCopy()
		if wh := spec.Webhook; wh != nil {
			wh.URL = maskWebhookURL(wh.URL)
			maskValues(wh.Headers)
		}
		if ts := spec.ToolServer; ts != nil {
			maskValues(ts.Arguments)
		}
		status := n.Status.DeepCopy()
		if e := status.LastError; e != nil { // its message may quote the URL
			e.Message = set
		}
		return *spec, *status
	},
}, {
	// A Runner's token hash is nobody's to see: its Secret's name and when
	// it was minted are enough to tell a rotation.
	plural: "runners", kind: "Runner",
	newObj:  func() client.Object { return &v1alpha1.Runner{} },
	newList: func() client.ObjectList { return &v1alpha1.RunnerList{} },
	status:  func(o client.Object) v1alpha1.ConfigStatus { return o.(*v1alpha1.Runner).Status.ConfigStatus },
	view: func(o client.Object, _ authz.Role) (any, any) {
		rn := o.(*v1alpha1.Runner)
		status := rn.Status
		status.TokenHash = ""
		return rn.Spec, status
	},
}}

// set stands in for a value a member may know is there but not read.
const set = "(set)"

// authScheme is a ToolServer auth prefix a member sees: none, or a scheme
// such as "Bearer ". Another may hold part of the credential.
var authScheme = regexp.MustCompile(`^([A-Za-z]+ )?$`)

// maskContainers hides what in cs may hold a credential: literal env
// values, commands and arguments. An env var's valueFrom stays, as it
// names a Secret, not its value.
func maskContainers(cs []corev1.Container) {
	for i := range cs {
		c := &cs[i]
		for j := range c.Env {
			if c.Env[j].Value != "" {
				c.Env[j].Value = set
			}
		}
		for j := range c.Command {
			c.Command[j] = set
		}
		for j := range c.Args {
			c.Args[j] = set
		}
	}
}

// maskProbe hides what in p may hold a credential: an exec probe's
// command, and an HTTP probe's header values and its path's query. Header
// names stay; a TCP or gRPC probe holds none.
func maskProbe(p *corev1.Probe) {
	if p.Exec != nil {
		for i := range p.Exec.Command {
			p.Exec.Command[i] = set
		}
	}
	if h := p.HTTPGet; h != nil {
		h.Path = maskURL(h.Path)
		for i := range h.HTTPHeaders {
			h.HTTPHeaders[i].Value = set
		}
	}
}

// maskValues hides m's values, which may hold a credential; its keys stay.
func maskValues(m map[string]string) {
	for k := range m {
		m[k] = set
	}
}

// maskWebhookURL masks a webhook URL as maskURL does, and its path too, as
// a chat service's holds its token (https://hooks.slack.com/services/...):
// only its scheme, host and port are shown. A path of "/" holds none.
func maskWebhookURL(u string) string {
	parsed, err := url.Parse(u)
	if err != nil {
		return set
	}
	if p := parsed.EscapedPath(); p != "" && p != "/" {
		parsed.Path, parsed.RawPath = "/"+set, "/"+set // so its parentheses are not escaped
	}
	return maskURL(parsed.String())
}

// maskServiceURL masks a managed server's URL, its Service's address then
// its path, as its path is masked: a path such as //user:pw@host/mcp
// parses as user info alone, not after a host.
func maskServiceURL(u string) string {
	addr, ok := strings.CutPrefix(u, "http://")
	i := strings.IndexByte(addr, '/')
	if !ok || i < 0 {
		return maskURL(u)
	}
	return u[:len("http://")+i] + maskURL(addr[i:])
}

// maskURL hides what in u may hold a credential: its user info, its
// query's values, or a query part with no value, which may be a key
// itself, and its fragment. A URL that does not parse is hidden whole.
func maskURL(u string) string {
	if u == "" {
		return u
	}
	parsed, err := url.Parse(u)
	if err != nil {
		return set
	}
	user := parsed.User != nil
	if user { // a stand-in, so the "//" before it is written with no host too
		parsed.User = url.User("u")
	}
	if parsed.Fragment != "" {
		parsed.Fragment, parsed.RawFragment = set, ""
	}
	if parsed.RawQuery != "" {
		parts := strings.Split(parsed.RawQuery, "&")
		for i, p := range parts {
			if key, _, ok := strings.Cut(p, "="); ok {
				parts[i] = key + "=" + set
			} else {
				parts[i] = set
			}
		}
		parsed.RawQuery = strings.Join(parts, "&")
	}
	out := parsed.String()
	if user { // url.User would escape set's parentheses
		out = strings.Replace(out, "//u@", "//"+set+"@", 1)
	}
	return out
}

// WithConfig serves the configuration resources of a Team's namespace,
// read-only, as r reads them: the controllers' cache. Without it the
// routes are not served, and GET /agents still lists Agents.
func WithConfig(r client.Reader) Option {
	return func(s *Server) { s.config = r }
}

// configTeam is ns's Team, which a namespace's configuration needs to be
// served at all: without it, nobody has a role there.
func (s *Server) configTeam(ctx context.Context, ns string) (*v1alpha1.Team, error) {
	if errs := validation.IsDNS1123Label(ns); len(errs) > 0 {
		return nil, fmt.Errorf("namespace %q: %s: %w", ns, strings.Join(errs, "; "), manager.ErrInvalid)
	}
	team, err := s.m.Team(ctx, ns)
	if errors.Is(err, storage.ErrNotFound) {
		return nil, fmt.Errorf("namespace %s has no Team: %w", ns, err)
	}
	return team, err
}

// listConfig lists a namespace's resources of kind k, by name, with
// whether each is Ready.
func (s *Server) listConfig(k configKind) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ns := r.PathValue("ns")
		if _, err := s.configTeam(r.Context(), ns); err != nil {
			httpjson.Fail(w, s.log, err)
			return
		}
		list := k.newList()
		if err := s.config.List(r.Context(), list, client.InNamespace(ns)); err != nil {
			httpjson.Fail(w, s.log, err)
			return
		}
		objs, err := meta.ExtractList(list)
		if err != nil {
			httpjson.Fail(w, s.log, err)
			return
		}
		items := make([]ConfigSummary, 0, len(objs))
		for _, o := range objs {
			items = append(items, configSummary(k, o.(client.Object)))
		}
		slices.SortFunc(items, func(a, b ConfigSummary) int { return strings.Compare(a.Name, b.Name) })
		httpjson.Write(w, http.StatusOK, ConfigList{Items: items})
	}
}

// getConfig answers one resource of kind k as the caller may see it.
func (s *Server) getConfig(k configKind) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ns, name := r.PathValue("ns"), r.PathValue("name")
		team, err := s.configTeam(r.Context(), ns)
		if err != nil {
			httpjson.Fail(w, s.log, err)
			return
		}
		obj := k.newObj()
		switch err := s.config.Get(r.Context(), client.ObjectKey{Namespace: ns, Name: name}, obj); {
		case apierrors.IsNotFound(err):
			httpjson.Fail(w, s.log, fmt.Errorf("%s %s: %w", k.kind, name, storage.ErrNotFound))
			return
		case err != nil:
			httpjson.Fail(w, s.log, err)
			return
		}
		httpjson.Write(w, http.StatusOK, s.configView(r.Context(), k, obj, team))
	}
}

func configSummary(k configKind, o client.Object) ConfigSummary {
	out := ConfigSummary{Name: o.GetName()}
	out.Ready, out.Reason, out.Message = readiness(k.status(o), o.GetGeneration())
	return out
}

// configView is o as the caller may see it, by their role on team: a
// viewer its health only, a member and an admin its spec and status too,
// as k's view gives them. Fields' owners and the last applied spec are
// nobody's to see.
func (s *Server) configView(ctx context.Context, k configKind, o client.Object, team *v1alpha1.Team) ConfigResource {
	role := authz.Admin // without logins everyone may do anything
	if s.policy != nil {
		id, _ := auth.FromContext(ctx)
		role, _ = s.policy.Role(id, team)
	}
	st := k.status(o)
	out := ConfigResource{
		ConfigSummary: configSummary(k, o), Kind: k.kind,
		Generation: o.GetGeneration(), ObservedGeneration: st.ObservedGeneration, Conditions: st.Conditions,
		Metadata: projectMeta(o, role),
	}
	if out.Conditions == nil {
		out.Conditions = []metav1.Condition{}
	}
	if role >= authz.Member {
		out.Spec, out.Status = k.view(o, role)
	}
	return out
}

// projectMeta is o's metadata as role may see it: never its fields'
// owners, nor the last applied spec; annotations for a member and up.
func projectMeta(o client.Object, role authz.Role) ConfigMeta {
	out := ConfigMeta{UID: string(o.GetUID()), ResourceVersion: o.GetResourceVersion(), CreationTimestamp: o.GetCreationTimestamp(), Labels: o.GetLabels()}
	if role < authz.Member {
		return out
	}
	for key, v := range o.GetAnnotations() {
		if key == lastApplied {
			continue
		}
		if out.Annotations == nil {
			out.Annotations = map[string]string{}
		}
		out.Annotations[key] = v
	}
	return out
}
