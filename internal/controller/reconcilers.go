package controller

import (
	"context"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
)

const (
	indexModelRef = "spec.modelRef"
	indexToolRefs = "spec.toolRefs"
	// indexRunnerSA finds the Runners naming a ServiceAccount.
	indexRunnerSA = "spec.serviceAccount.name"
	// indexAccessSA finds the AgentAccesses naming a ServiceAccount.
	indexAccessSA = "spec.serviceAccount.name"
	// indexJobRunner finds the Agents whose Jobs run as a Runner.
	indexJobRunner = "spec.runtime.job.runnerRef"
)

// setupIndexes lets a ModelProvider or ToolServer change find the Agents
// that reference it, and a ServiceAccount's token find its Runners and
// AgentAccesses.
func setupIndexes(mgr ctrl.Manager) error {
	idx := mgr.GetFieldIndexer()
	if err := idx.IndexField(context.Background(), &v1alpha1.Agent{}, indexModelRef, func(o client.Object) []string {
		return []string{o.(*v1alpha1.Agent).Spec.ModelRef}
	}); err != nil {
		return err
	}
	if err := idx.IndexField(context.Background(), &v1alpha1.Agent{}, indexToolRefs, func(o client.Object) []string {
		return o.(*v1alpha1.Agent).Spec.ToolRefs
	}); err != nil {
		return err
	}
	if err := idx.IndexField(context.Background(), &v1alpha1.Agent{}, indexJobRunner, func(o client.Object) []string {
		if job := o.(*v1alpha1.Agent).Spec.Runtime.Job; job != nil {
			return []string{job.RunnerRef}
		}
		return nil
	}); err != nil {
		return err
	}
	if err := idx.IndexField(context.Background(), &v1alpha1.Runner{}, indexRunnerSA, runnerSA); err != nil {
		return err
	}
	return idx.IndexField(context.Background(), &v1alpha1.AgentAccess{}, indexAccessSA, accessSA)
}

// accessSA indexes an AgentAccess by the ServiceAccount it names.
func accessSA(o client.Object) []string {
	if sa := o.(*v1alpha1.AgentAccess).Spec.ServiceAccount; sa != nil {
		return []string{sa.Name}
	}
	return nil
}

// runnerSA indexes a Runner by the ServiceAccount it names.
func runnerSA(o client.Object) []string {
	if sa := o.(*v1alpha1.Runner).Spec.ServiceAccount; sa != nil {
		return []string{sa.Name}
	}
	return nil
}

type modelProviderReconciler struct{ base }

func (r *modelProviderReconciler) setup(mgr ctrl.Manager) error {
	// Status writes do not change the generation, so they do not bring the
	// object back for a second Secret read; the periodic re-check does.
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.ModelProvider{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Named("modelprovider").WithOptions(r.options()).Complete(r)
}

func (r *modelProviderReconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	var mp v1alpha1.ModelProvider
	if err := r.client.Get(ctx, req.NamespacedName, &mp); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}
	if ok, err := r.hasTeam(ctx, req.Namespace); err != nil || !ok {
		return r.noTeam(ctx, &mp, &mp.Status, err)
	}
	ready, reason, msg := true, "WorkloadIdentity", ""
	if mp.Spec.Auth != nil {
		var err error
		if ready, reason, msg, err = r.secretHasKey(ctx, mp.Namespace, mp.Spec.Auth.SecretRef); err != nil {
			return reconcile.Result{}, err
		}
	}
	return reconcile.Result{RequeueAfter: r.resync}, r.setReady(ctx, &mp, &mp.Status, ready, reason, msg)
}

type toolServerReconciler struct{ base }

func (r *toolServerReconciler) setup(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.ToolServer{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		// A managed server's Deployment says when it is Available.
		Owns(&appsv1.Deployment{}).
		Named("toolserver").WithOptions(r.options()).Complete(r)
}

func (r *toolServerReconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	var ts v1alpha1.ToolServer
	if err := r.client.Get(ctx, req.NamespacedName, &ts); err != nil {
		// A deleted managed server's Deployment and Service go with it:
		// it owns them.
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}
	if ok, err := r.hasTeam(ctx, req.Namespace); err != nil || !ok {
		return r.noTeam(ctx, &ts, &ts.Status.ConfigStatus, err)
	}
	if ts.Spec.Managed != nil {
		return r.reconcileManaged(ctx, &ts)
	}
	// Once managed (it has a URL of Cadre's), now external: what Cadre ran
	// for it goes, and the URL with it.
	var changed bool
	var err error
	if ts.Status.URL != "" {
		switch changed, err = r.unmanage(ctx, &ts); {
		case apierrors.IsForbidden(err):
			return reconcile.Result{RequeueAfter: r.resync}, r.writeStatus(ctx, &ts, &ts.Status.ConfigStatus, false, "Forbidden", forbiddenMessage(err, forbiddenUnmanage), false)
		case err != nil:
			return reconcile.Result{}, err
		}
	}
	ready, reason, msg := true, "External", ""
	if ts.Spec.Auth != nil {
		if ready, reason, msg, err = r.secretHasKey(ctx, ts.Namespace, ts.Spec.Auth.SecretRef); err != nil {
			return reconcile.Result{}, err
		}
	}
	return reconcile.Result{RequeueAfter: r.resync}, r.writeStatus(ctx, &ts, &ts.Status.ConfigStatus, ready, reason, msg, changed)
}

type agentReconciler struct {
	base
	jobs *jobAccess
}

func (r *agentReconciler) setup(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).For(&v1alpha1.Agent{}).Named("agent").WithOptions(r.options()).
		Watches(&v1alpha1.ModelProvider{}, handler.EnqueueRequestsFromMapFunc(r.agentsUsing(indexModelRef))).
		Watches(&v1alpha1.ToolServer{}, handler.EnqueueRequestsFromMapFunc(r.agentsUsing(indexToolRefs))).
		Watches(&v1alpha1.Runner{}, handler.EnqueueRequestsFromMapFunc(r.agentsUsing(indexJobRunner))).
		Complete(r)
}

// agentsUsing maps a ModelProvider or ToolServer to the Agents that
// reference it by name.
func (r *agentReconciler) agentsUsing(index string) handler.MapFunc {
	return func(ctx context.Context, o client.Object) []reconcile.Request {
		var agents v1alpha1.AgentList
		if err := r.client.List(ctx, &agents, client.InNamespace(o.GetNamespace()), client.MatchingFields{index: o.GetName()}); err != nil {
			return nil
		}
		reqs := make([]reconcile.Request, 0, len(agents.Items))
		for _, a := range agents.Items {
			reqs = append(reqs, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: a.Namespace, Name: a.Name}})
		}
		return reqs
	}
}

func (r *agentReconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	var agent v1alpha1.Agent
	if err := r.client.Get(ctx, req.NamespacedName, &agent); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}
	if ok, err := r.hasTeam(ctx, req.Namespace); err != nil || !ok {
		return r.noTeam(ctx, &agent, &agent.Status, err)
	}
	ready, reason, msg, err := r.resolve(ctx, &agent)
	if err != nil {
		if reason != "" {
			// A check that could not be made, with a reason to say so:
			// written, then returned, so that the backoff applies.
			if werr := r.setReady(ctx, &agent, &agent.Status, false, reason, msg); werr != nil {
				return reconcile.Result{}, werr
			}
		}
		return reconcile.Result{}, err
	}
	// Checked again at resync: its Runner's ServiceAccount is read, not
	// watched, and its Team may go, which brings no event for it. One that
	// relies on a shared Job access answer checks again when it expires, so
	// that every Agent in the namespace sees a change within a resync.
	return reconcile.Result{RequeueAfter: r.recheck(&agent)}, r.setReady(ctx, &agent, &agent.Status, ready, reason, msg)
}

// recheck is how soon an Agent is reconciled again: at resync, or sooner
// when the Job access answer it was resolved with expires first.
func (r *agentReconciler) recheck(agent *v1alpha1.Agent) time.Duration {
	if agent.Spec.Runtime.Job == nil {
		return r.resync
	}
	until, ok := r.jobs.expiry(agent.Namespace)
	if !ok {
		return r.resync
	}
	// Never zero, which would mean no requeue at all.
	return max(min(r.resync, until.Sub(r.jobs.now())), 100*time.Millisecond)
}

// resolve checks the Agent's references in order and names the first one
// that is missing or not Ready.
func (r *agentReconciler) resolve(ctx context.Context, agent *v1alpha1.Agent) (bool, string, string, error) {
	var mp v1alpha1.ModelProvider
	switch err := r.client.Get(ctx, client.ObjectKey{Namespace: agent.Namespace, Name: agent.Spec.ModelRef}, &mp); {
	case client.IgnoreNotFound(err) != nil:
		return false, "", "", err
	case err != nil:
		return false, "ModelProviderNotFound", fmt.Sprintf("ModelProvider %s does not exist", agent.Spec.ModelRef), nil
	case !isReady(&mp, mp.Status):
		return false, "ModelProviderNotReady", fmt.Sprintf("ModelProvider %s is not Ready", agent.Spec.ModelRef), nil
	}
	for _, name := range agent.Spec.ToolRefs {
		var ts v1alpha1.ToolServer
		switch err := r.client.Get(ctx, client.ObjectKey{Namespace: agent.Namespace, Name: name}, &ts); {
		case client.IgnoreNotFound(err) != nil:
			return false, "", "", err
		case err != nil:
			return false, "ToolServerNotFound", fmt.Sprintf("ToolServer %s does not exist", name), nil
		case !isReady(&ts, ts.Status.ConfigStatus):
			return false, "ToolServerNotReady", fmt.Sprintf("ToolServer %s is not Ready", name), nil
		}
	}
	if job := agent.Spec.Runtime.Job; job != nil {
		return r.resolveJob(ctx, agent, job)
	}
	return true, "Resolved", "", nil
}

// resolveJob checks the Runner an agent's Jobs run as: it must exist, run
// in the cluster as a ServiceAccount that exists, and claim with labels
// that include the agent's selector, or its Jobs could not claim its work.
func (r *agentReconciler) resolveJob(ctx context.Context, agent *v1alpha1.Agent, job *v1alpha1.AgentJob) (bool, string, string, error) {
	var rn v1alpha1.Runner
	switch err := r.client.Get(ctx, client.ObjectKey{Namespace: agent.Namespace, Name: job.RunnerRef}, &rn); {
	case client.IgnoreNotFound(err) != nil:
		return false, "", "", err
	case err != nil:
		return false, "RunnerNotFound", fmt.Sprintf("Runner %s does not exist", job.RunnerRef), nil
	case rn.Spec.ServiceAccount == nil:
		return false, "RunnerOutsideTheCluster", fmt.Sprintf("Runner %s has no serviceAccount for the Jobs to run as", job.RunnerRef), nil
	}
	// Its Jobs run as the Runner's ServiceAccount, which claims with the
	// labels of the Runner holding it: decided from the specs, as
	// Authenticate does, so an edit to a Runner never flaps the Agent.
	sharing, err := sharers(ctx, r.client, agent.Namespace, rn.Spec.ServiceAccount.Name)
	if err != nil {
		return false, "", "", err
	}
	if h := holder(sharing); h != nil && h.Name != rn.Name {
		return false, "ServiceAccountInUse", fmt.Sprintf("Runner %s: %s", rn.Name, inUse(rn.Spec.ServiceAccount.Name, h.Name)), nil
	}
	for k, v := range agent.Spec.Runtime.Selector {
		if rn.Spec.Labels[k] != v {
			return false, "RunnerLabelsMismatch", fmt.Sprintf("Runner %s's labels do not include the selector's %s=%s", job.RunnerRef, k, v), nil
		}
	}
	var sa corev1.ServiceAccount
	switch err := r.secrets.Get(ctx, client.ObjectKey{Namespace: agent.Namespace, Name: rn.Spec.ServiceAccount.Name}, &sa); {
	case apierrors.IsForbidden(err):
		return false, "Forbidden", forbiddenMessage(err, forbidden), nil
	case client.IgnoreNotFound(err) != nil:
		return false, "", "", err
	case err != nil:
		return false, "ServiceAccountNotFound", fmt.Sprintf("ServiceAccount %s of Runner %s does not exist", rn.Spec.ServiceAccount.Name, job.RunnerRef), nil
	}
	switch ok, err := r.jobs.check(ctx, agent.Namespace); {
	case err != nil:
		// Not Ready while unknown: the Job reconciler starts Jobs only
		// for Ready Agents.
		return false, "AccessUnknown", err.Error(), err
	case !ok:
		return false, "Forbidden", forbidden, nil
	}
	return true, "Resolved", "", nil
}

// forbidden says what a namespace whose Jobs cadre-server may not run
// lacks.
const forbidden = "cadre-server may not run Jobs in this namespace: bind the ClusterRole in config/rbac/workloads.yaml here"

// noTeam ends the reconcile of obj in a namespace that has no Team (yet),
// or whose Team could not be read (err): obj is Ready=False NoTeam, and
// checked again later. An error is returned alone, since controller-runtime
// ignores a requeue that comes with one.
func (b base) noTeam(ctx context.Context, obj client.Object, status *v1alpha1.ConfigStatus, err error) (reconcile.Result, error) {
	res, err := waitForTeam(err, b.resync)
	if err != nil {
		return res, err
	}
	return res, b.setReady(ctx, obj, status, false, "NoTeam", noTeamMessage(obj.GetNamespace()))
}

// noTeamMessage is the message of a Ready condition with reason NoTeam.
func noTeamMessage(ns string) string { return "namespace " + ns + " has no Team" }

// waitForTeam ends a reconcile that writes no status in a namespace that
// has no Team (yet), as noTeam does.
func waitForTeam(err error, resync time.Duration) (reconcile.Result, error) {
	if err != nil {
		return reconcile.Result{}, err
	}
	return reconcile.Result{RequeueAfter: resync}, nil
}
