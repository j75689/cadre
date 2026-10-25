package controller

import (
	"context"
	"errors"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	"github.com/j75689/cadre/internal/api"
	"github.com/j75689/cadre/internal/storage"
)

// A PUT decides from a fresh read whether it makes or edits: with a cache
// that has not seen a create yet, an edit right after it is still an edit,
// and after a delete a make is still a make.
func TestAccessStoreReadsFresh(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	cluster := fake.NewClientBuilder().WithScheme(scheme).Build()
	// The cache: writes go through, reads see nothing yet.
	lagging := fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(_ context.Context, _ client.WithWatch, key client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
			return apierrors.NewNotFound(schema.GroupResource{Group: "cadre.dev", Resource: "agentaccesses"}, key.Name)
		},
		Create: func(ctx context.Context, _ client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			return cluster.Create(ctx, obj, opts...)
		},
		Update: func(ctx context.Context, _ client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			return cluster.Update(ctx, obj, opts...)
		},
		Delete: func(ctx context.Context, _ client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			return cluster.Delete(ctx, obj, opts...)
		},
	}).Build()
	s := AgentAccessStore(lagging, cluster)
	aa := &v1alpha1.AgentAccess{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "bot"}, Spec: v1alpha1.AgentAccessSpec{Role: "member"}}
	if err := s.CreateAgentAccess(ctx, aa); err != nil {
		t.Fatal(err)
	}
	cur, err := s.GetAgentAccess(ctx, "team-a", "bot")
	if err != nil || cur.ResourceVersion != aa.ResourceVersion {
		t.Fatalf("read right after the create: %+v, %v", cur, err)
	}
	if err := s.DeleteAgentAccess(ctx, "team-a", "bot"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetAgentAccess(ctx, "team-a", "bot"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("read right after the delete: %v", err)
	}
}

// The API server's answers, as the store says them: cadre-server's own
// RBAC refusing is ErrNotPermitted, naming the verb, not a retry.
func TestAccessStoreErrors(t *testing.T) {
	gr := schema.GroupResource{Group: "cadre.dev", Resource: "agentaccesses"}
	for name, tc := range map[string]struct {
		err  error
		want error
	}{
		"forbidden":    {apierrors.NewForbidden(gr, "bot", errors.New("no")), api.ErrNotPermitted},
		"unauthorized": {apierrors.NewUnauthorized("no"), api.ErrNotPermitted},
		"conflict":     {apierrors.NewConflict(gr, "bot", errors.New("moved")), storage.ErrConflict},
		"exists":       {apierrors.NewAlreadyExists(gr, "bot"), storage.ErrAlreadyExists},
		"missing":      {apierrors.NewNotFound(gr, "bot"), storage.ErrNotFound},
		"invalid":      {apierrors.NewBadRequest("spec.role"), storage.ErrInvalid},
	} {
		if got := storeError("update", "bot", tc.err); !errors.Is(got, tc.want) {
			t.Errorf("%s: %v, want %v", name, got, tc.want)
		}
	}
	if got := storeError("delete", "bot", apierrors.NewForbidden(gr, "bot", errors.New("no"))); got == nil || !strings.Contains(got.Error(), "delete agentaccesses") {
		t.Errorf("the verb is not named: %v", got)
	}
}
