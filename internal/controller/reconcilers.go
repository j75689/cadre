package controller

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
)

const (
	indexModelRef = "spec.modelRef"
	indexToolRefs = "spec.toolRefs"
)

// setupIndexes lets a ModelProvider or ToolServer change find the Agents
// that reference it.
func setupIndexes(mgr ctrl.Manager) error {
	idx := mgr.GetFieldIndexer()
	if err := idx.IndexField(context.Background(), &v1alpha1.Agent{}, indexModelRef, func(o client.Object) []string {
		return []string{o.(*v1alpha1.Agent).Spec.ModelRef}
	}); err != nil {
		return err
	}
	return idx.IndexField(context.Background(), &v1alpha1.Agent{}, indexToolRefs, func(o client.Object) []string {
		return o.(*v1alpha1.Agent).Spec.ToolRefs
	})
}

type modelProviderReconciler struct{ base }

func (r *modelProviderReconciler) setup(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).For(&v1alpha1.ModelProvider{}).Named("modelprovider").Complete(r)
}

func (r *modelProviderReconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	var mp v1alpha1.ModelProvider
	if err := r.client.Get(ctx, req.NamespacedName, &mp); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}
	if ok, err := r.hasTeam(ctx, req.Namespace); !ok || err != nil {
		return reconcile.Result{RequeueAfter: r.resync}, err
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
	return ctrl.NewControllerManagedBy(mgr).For(&v1alpha1.ToolServer{}).Named("toolserver").Complete(r)
}

func (r *toolServerReconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	var ts v1alpha1.ToolServer
	if err := r.client.Get(ctx, req.NamespacedName, &ts); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}
	if ok, err := r.hasTeam(ctx, req.Namespace); !ok || err != nil {
		return reconcile.Result{RequeueAfter: r.resync}, err
	}
	ready, reason, msg := true, "External", ""
	switch {
	case ts.Spec.Managed != nil:
		ready, reason, msg = false, "ManagedNotSupported", "managed ToolServers arrive in M2"
	case ts.Spec.Auth != nil:
		var err error
		if ready, reason, msg, err = r.secretHasKey(ctx, ts.Namespace, ts.Spec.Auth.SecretRef); err != nil {
			return reconcile.Result{}, err
		}
	}
	return reconcile.Result{RequeueAfter: r.resync}, r.setReady(ctx, &ts, &ts.Status, ready, reason, msg)
}

type agentReconciler struct{ base }

func (r *agentReconciler) setup(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).For(&v1alpha1.Agent{}).Named("agent").
		Watches(&v1alpha1.ModelProvider{}, handler.EnqueueRequestsFromMapFunc(r.agentsUsing(indexModelRef))).
		Watches(&v1alpha1.ToolServer{}, handler.EnqueueRequestsFromMapFunc(r.agentsUsing(indexToolRefs))).
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
	if ok, err := r.hasTeam(ctx, req.Namespace); !ok || err != nil {
		return reconcile.Result{RequeueAfter: r.resync}, err
	}
	ready, reason, msg, err := r.resolve(ctx, &agent)
	if err != nil {
		return reconcile.Result{}, err
	}
	return reconcile.Result{}, r.setReady(ctx, &agent, &agent.Status, ready, reason, msg)
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
	case !isReady(mp.Status):
		return false, "ModelProviderNotReady", fmt.Sprintf("ModelProvider %s is not Ready", agent.Spec.ModelRef), nil
	}
	for _, name := range agent.Spec.ToolRefs {
		var ts v1alpha1.ToolServer
		switch err := r.client.Get(ctx, client.ObjectKey{Namespace: agent.Namespace, Name: name}, &ts); {
		case client.IgnoreNotFound(err) != nil:
			return false, "", "", err
		case err != nil:
			return false, "ToolServerNotFound", fmt.Sprintf("ToolServer %s does not exist", name), nil
		case !isReady(ts.Status):
			return false, "ToolServerNotReady", fmt.Sprintf("ToolServer %s is not Ready", name), nil
		}
	}
	return true, "Resolved", "", nil
}
