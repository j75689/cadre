package controller

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	"github.com/j75689/cadre/internal/runner"
)

// cadre-server watches Jobs everywhere (an informer cannot be granted per
// namespace) but caches only its own; creating and deleting them, and
// reading the ServiceAccounts they run as, it may do only where
// config/rbac/workloads.yaml is bound, in each Team namespace.
//
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch
// +kubebuilder:rbac:groups=cadre.dev,resources=tasks,verbs=get;list;watch

// Job labels and the runner's environment.
const (
	LabelAgent  = "cadre.dev/agent"
	LabelRunner = "cadre.dev/runner"

	// TokenPath is where a Job's pod finds its token for the audience cadre.
	TokenPath = "/var/run/secrets/cadre/token"

	defaultActiveDeadline = time.Hour
	defaultMaxPending     = 2
	// ttlAfterFinished is how long a finished Job is kept, for its logs.
	ttlAfterFinished = 10 * time.Minute
	// tokenLifetime is the projected token's; the kubelet renews it.
	tokenLifetime = 10 * time.Minute
)

// jobReconciler starts Jobs for runner agents that have a job: each Job's
// pod is a runner that claims one run of the agent's, works it and exits.
// It starts as many as there are claimable tasks with no Job waiting for
// them, within the Team's free slots and the job's maxPending, and deletes
// the Jobs of an agent that lost its job, and a Job that claimed a run and
// still runs a lease after the run is no longer its own.
//
// Which Jobs claimed a run it learns from the Running tasks, whose
// status.run.runnerName is the Job's name, and remembers for the moment a
// run ends. A Job whose pod has run twice the lease with no run naming it
// is deleted whether it is remembered or not, so a restart, which forgets,
// leaves no stuck Job counted as waiting for work.
type jobReconciler struct {
	base
	serverURL string
	lease     time.Duration
	events    events.EventRecorder

	mu      sync.Mutex
	claimed map[types.NamespacedName]claim // Jobs seen running a run
	// created are the Jobs it created that the cache may not show yet,
	// by agent: they wait for work all the same.
	created map[types.NamespacedName]map[string]time.Time
}

// claim is a Job seen running a run: its agent, and, once it no longer
// runs one, when that was first seen. The Job is given a lease from then
// to exit on its own.
type claim struct {
	agent string
	gone  time.Time
}

// createdFor is how long a created Job the cache does not show yet still
// counts as waiting for work.
const createdFor = time.Minute

func (r *jobReconciler) setup(mgr ctrl.Manager) error {
	r.events = mgr.GetEventRecorder("cadre-jobs")
	return ctrl.NewControllerManagedBy(mgr).For(&v1alpha1.Agent{}).Named("agent-jobs").WithOptions(r.options()).
		Owns(&batchv1.Job{}).
		Watches(&v1alpha1.Task{}, handler.EnqueueRequestsFromMapFunc(assignee), builder.WithPredicates(jobRelevant)).
		// A run's end frees its Team slot after the Task's write: the
		// agents with Jobs may start another then.
		Watches(&v1alpha1.Team{}, handler.EnqueueRequestsFromMapFunc(r.jobAgents)).
		Complete(r)
}

// jobRelevant passes the task changes that change an agent's Jobs: its
// phase, its assignee, the runner holding its run; not a heartbeat.
var jobRelevant = predicate.Funcs{UpdateFunc: func(e event.UpdateEvent) bool {
	o, n := e.ObjectOld.(*v1alpha1.Task), e.ObjectNew.(*v1alpha1.Task)
	return o.Status.Phase != n.Status.Phase || !equality.Semantic.DeepEqual(o.Spec.Assignee, n.Spec.Assignee) || runnerOf(o) != runnerOf(n)
}}

func runnerOf(t *v1alpha1.Task) string {
	if t.Status.Run == nil {
		return ""
	}
	return t.Status.Run.RunnerName
}

// jobAgents maps a Team to its namespace's agents that have a job.
func (r *jobReconciler) jobAgents(ctx context.Context, o client.Object) []reconcile.Request {
	var agents v1alpha1.AgentList
	if err := r.client.List(ctx, &agents, client.InNamespace(o.GetNamespace())); err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for _, a := range agents.Items {
		if a.Spec.Runtime.Job != nil {
			reqs = append(reqs, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: a.Namespace, Name: a.Name}})
		}
	}
	return reqs
}

// assignee maps a task to the agent it is assigned to.
func assignee(_ context.Context, o client.Object) []reconcile.Request {
	a := o.(*v1alpha1.Task).Spec.Assignee
	if a == nil || a.Kind != v1alpha1.AssigneeAgent {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: o.GetNamespace(), Name: a.Name}}}
}

func (r *jobReconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	var agent v1alpha1.Agent
	if err := r.client.Get(ctx, req.NamespacedName, &agent); err != nil {
		if apierrors.IsNotFound(err) {
			// A deleted agent's Jobs go with it: it owns them.
			r.mu.Lock()
			delete(r.created, req.NamespacedName)
			for key, c := range r.claimed {
				if key.Namespace == req.Namespace && c.agent == req.Name {
					delete(r.claimed, key)
				}
			}
			r.mu.Unlock()
		}
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}
	var jobs batchv1.JobList
	if err := r.client.List(ctx, &jobs, client.InNamespace(agent.Namespace), client.MatchingLabels{LabelAgent: agent.Name}); err != nil {
		return reconcile.Result{}, err
	}
	spec := agent.Spec.Runtime.Job
	if spec == nil || agent.Spec.Runtime.Type != v1alpha1.RuntimeRunner {
		for i := range jobs.Items {
			if err := r.remove(ctx, &jobs.Items[i]); err != nil {
				return reconcile.Result{}, err
			}
		}
		return reconcile.Result{}, nil
	}
	if ok, err := r.hasTeam(ctx, agent.Namespace); err != nil || !ok {
		// The agent reconciler reports it on the Agent.
		return waitForTeam(err, r.resync)
	}
	if !isReady(&agent, agent.Status) {
		return reconcile.Result{RequeueAfter: r.resync}, nil
	}

	var tasks v1alpha1.TaskList
	if err := r.client.List(ctx, &tasks, client.InNamespace(agent.Namespace)); err != nil {
		return reconcile.Result{}, err
	}
	todo := 0
	running := map[string]bool{} // runner names of the agent's runs
	for _, t := range tasks.Items {
		if a := t.Spec.Assignee; a == nil || a.Kind != v1alpha1.AssigneeAgent || a.Name != agent.Name {
			continue
		}
		switch {
		case t.Status.Phase == v1alpha1.PhaseTodo:
			todo++
		case t.Status.Phase == v1alpha1.PhaseRunning && t.Status.Run != nil:
			running[t.Status.Run.RunnerName] = true
		}
	}

	now := time.Now()
	pending := 0
	exiting := false // a Job given a lease to exit: look again then
	listed := map[string]bool{}
	for _, j := range jobs.Items {
		listed[j.Name] = true
	}
	r.mu.Lock()
	for name, at := range r.created[req.NamespacedName] {
		switch {
		case listed[name], now.Sub(at) > createdFor:
			delete(r.created[req.NamespacedName], name)
		default:
			pending++
		}
	}
	r.mu.Unlock()
	r.forgetGone(agent.Namespace, agent.Name, listed)
	for i := range jobs.Items {
		j := &jobs.Items[i]
		key := client.ObjectKeyFromObject(j)
		if finished(j) {
			r.mu.Lock()
			delete(r.claimed, key)
			r.mu.Unlock()
			continue
		}
		if err := r.markRunning(ctx, j, now); err != nil {
			return reconcile.Result{}, err
		}
		r.mu.Lock()
		c, wasClaimed := r.claimed[key]
		switch {
		case running[j.Name]:
			r.claimed[key] = claim{agent: agent.Name}
			r.mu.Unlock()
			continue
		case wasClaimed && c.gone.IsZero():
			// Its run just ended: a lease for its pod to exit.
			c.gone = now
			r.claimed[key] = c
		}
		r.mu.Unlock()
		switch {
		case wasClaimed && now.Sub(c.gone) > r.lease:
			// Its run is over or went to another runner, yet it runs on:
			// a lease later it is not coming back.
		case wasClaimed:
			exiting = true
			continue
		case stuck(j, now, r.lease):
			// The same, known without memory, so after a restart too: a
			// started runner claims at once, so a pod that ran a lease and
			// more without holding a run is broken, whether it lost its run
			// or never claimed one.
		default:
			pending++ // its runner has not claimed yet
			continue
		}
		if err := r.remove(ctx, j); err != nil {
			return reconcile.Result{}, err
		}
	}

	var team v1alpha1.Team
	if err := r.client.Get(ctx, client.ObjectKey{Namespace: agent.Namespace, Name: v1alpha1.TeamName}, &team); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}
	free := int(max(team.Spec.MaxConcurrentRuns, 1)) - len(team.Status.Slots)
	maxPending := int(spec.MaxPending)
	if maxPending <= 0 {
		maxPending = defaultMaxPending
	}
	if n := min(todo-pending, free, maxPending-pending); n > 0 {
		var rn v1alpha1.Runner
		if err := r.client.Get(ctx, client.ObjectKey{Namespace: agent.Namespace, Name: spec.RunnerRef}, &rn); err != nil {
			return reconcile.Result{}, client.IgnoreNotFound(err)
		}
		for range n {
			j, err := r.newJob(&agent, spec, &rn)
			if err != nil {
				return reconcile.Result{}, err
			}
			if err := r.client.Create(ctx, j); err != nil {
				switch {
				case apierrors.IsForbidden(err) && authzRefusal.MatchString(err.Error()):
					// The Agent's Ready condition says so (resolveJob).
					log.FromContext(ctx).Error(err, "cadre-server may not create Jobs here; bind config/rbac/workloads.yaml", "namespace", agent.Namespace)
					return reconcile.Result{RequeueAfter: r.resync}, nil
				case apierrors.IsForbidden(err):
					// A quota or an admission policy, which no binding fixes
					// and the access reviews cannot see. Not on Ready: it
					// gates these very creates, so Ready=False would stop
					// the attempt that finds the quota lifted. An Event on
					// the Agent says why no Job starts, as Kubernetes' own
					// controllers report a create they are refused.
					r.events.Eventf(&agent, nil, corev1.EventTypeWarning, "JobRefused", "CreateJob", "the API server refused a Job: %s", err.Error())
					return reconcile.Result{RequeueAfter: r.resync}, nil
				}
				return reconcile.Result{}, err
			}
			r.mu.Lock()
			if r.created[req.NamespacedName] == nil {
				r.created[req.NamespacedName] = map[string]time.Time{}
			}
			r.created[req.NamespacedName][j.Name] = now
			r.mu.Unlock()
		}
	}
	if exiting {
		return reconcile.Result{RequeueAfter: min(r.lease, r.resync)}, nil
	}
	return reconcile.Result{RequeueAfter: r.resync}, nil
}

// remove deletes a Job and its pod, and forgets it.
func (r *jobReconciler) remove(ctx context.Context, j *batchv1.Job) error {
	r.mu.Lock()
	delete(r.claimed, client.ObjectKeyFromObject(j))
	r.mu.Unlock()
	return client.IgnoreNotFound(r.client.Delete(ctx, j, client.PropagationPolicy(metav1.DeletePropagationBackground)))
}

// AnnotationRunningSince is when a Job's pod was first seen running (ready):
// a pod still scheduling or pulling its image is not stuck, however long
// that takes. It is written once, so it outlives a restart.
const AnnotationRunningSince = "cadre.dev/running-since"

// markRunning notes when j's pod first runs.
func (r *jobReconciler) markRunning(ctx context.Context, j *batchv1.Job, now time.Time) error {
	if _, ok := j.Annotations[AnnotationRunningSince]; ok || j.Status.Ready == nil || *j.Status.Ready == 0 {
		return nil
	}
	orig := j.DeepCopy()
	if j.Annotations == nil {
		j.Annotations = map[string]string{}
	}
	j.Annotations[AnnotationRunningSince] = now.UTC().Format(time.RFC3339)
	err := r.client.Patch(ctx, j, client.MergeFrom(orig))
	if apierrors.IsForbidden(err) {
		return nil // the Agent's Ready condition says so (resolveJob)
	}
	return client.IgnoreNotFound(err)
}

// stuck reports whether j's pod has run for longer than twice the lease,
// a lease and its grace, while no run names it.
func stuck(j *batchv1.Job, now time.Time, lease time.Duration) bool {
	since, err := time.Parse(time.RFC3339, j.Annotations[AnnotationRunningSince])
	return err == nil && j.Status.Active > 0 && now.Sub(since) > 2*lease
}

// forgetGone drops what it remembers of agent's Jobs that are gone.
func (r *jobReconciler) forgetGone(ns, agent string, listed map[string]bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for key, c := range r.claimed {
		if key.Namespace == ns && c.agent == agent && !listed[key.Name] {
			delete(r.claimed, key)
		}
	}
}

func finished(j *batchv1.Job) bool {
	for _, c := range j.Status.Conditions {
		if (c.Type == batchv1.JobComplete || c.Type == batchv1.JobFailed) && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

// newJob is a Job for one of agent's runs. The pod runs as the Runner's
// ServiceAccount, whatever the template says, with the token for the
// audience cadre and the server's address added to every container.
func (r *jobReconciler) newJob(agent *v1alpha1.Agent, spec *v1alpha1.AgentJob, rn *v1alpha1.Runner) (*batchv1.Job, error) {
	prefix := agent.Name
	if len(prefix) > 36 { // the name stays a valid label value: 36 + 1 + 26
		prefix = prefix[:36]
	}
	name := strings.TrimRight(prefix, "-.") + "-" + strings.ToLower(ulid.Make().String())
	labels := map[string]string{LabelAgent: agent.Name, LabelRunner: rn.Name}

	tmpl := spec.Template.DeepCopy()
	if tmpl.Labels == nil {
		tmpl.Labels = map[string]string{}
	}
	for k, v := range labels {
		tmpl.Labels[k] = v
	}
	pod := &tmpl.Spec
	pod.ServiceAccountName = rn.Spec.ServiceAccount.Name
	pod.DeprecatedServiceAccount = ""
	pod.RestartPolicy = corev1.RestartPolicyNever
	lifetime := int64(tokenLifetime / time.Second)
	pod.Volumes = append(pod.Volumes, corev1.Volume{Name: "cadre-token", VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{
		Sources: []corev1.VolumeProjection{{ServiceAccountToken: &corev1.ServiceAccountTokenProjection{
			Audience: RunnerAudience, ExpirationSeconds: &lifetime, Path: "token",
		}}},
	}}})
	env := []corev1.EnvVar{
		{Name: runner.EnvServer, Value: r.serverURL},
		{Name: runner.EnvNamespace, Value: agent.Namespace},
		{Name: runner.EnvRunner, Value: name},
		{Name: runner.EnvAgent, Value: agent.Name},
	}
	for i := range pod.Containers {
		c := &pod.Containers[i]
		c.Env = append(c.Env, env...)
		c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{Name: "cadre-token", MountPath: TokenPath[:strings.LastIndex(TokenPath, "/")], ReadOnly: true})
	}

	deadline := int64(defaultActiveDeadline / time.Second)
	if spec.ActiveDeadline != nil && spec.ActiveDeadline.Duration > 0 {
		deadline = int64(spec.ActiveDeadline.Duration / time.Second)
	}
	backoff, ttl := int32(0), int32(ttlAfterFinished/time.Second)
	j := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: agent.Namespace, Labels: labels},
		Spec: batchv1.JobSpec{
			Template: *tmpl, BackoffLimit: &backoff, TTLSecondsAfterFinished: &ttl, ActiveDeadlineSeconds: &deadline,
		},
	}
	if err := controllerutil.SetControllerReference(agent, j, r.client.Scheme()); err != nil {
		return nil, err
	}
	return j, nil
}
