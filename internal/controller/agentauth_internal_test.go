package controller

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	"github.com/j75689/cadre/internal/auth"
	"github.com/j75689/cadre/internal/token"
)

// A minted token is its AgentAccess's while the AgentAccess, read fresh,
// has no ServiceAccount and the token's hash; a read that fails is no
// refusal, and is not remembered as one; and a Runner of the same name,
// whose minted token has the same shape, is never looked at.
func TestMintedAgentToken(t *testing.T) {
	ctx := context.Background()
	tok, hash, err := token.Mint("team-a", "bot")
	if err != nil {
		t.Fatal(err)
	}
	runnerTok, runnerHash, err := token.Mint("team-a", "laptop")
	if err != nil {
		t.Fatal(err)
	}
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	bot := &v1alpha1.AgentAccess{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "bot"}, Spec: v1alpha1.AgentAccessSpec{Role: "viewer"}}
	bot.Status.TokenHash = hash
	laptop := &v1alpha1.Runner{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "laptop"}}
	laptop.Status.TokenHash = runnerHash
	var failing atomic.Bool
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(bot, laptop).WithIndex(&v1alpha1.AgentAccess{}, indexSA, accessSA).
		WithInterceptorFuncs(interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if failing.Load() {
				return errors.New("the API server is away")
			}
			return c.Get(ctx, key, obj, opts...)
		}}).Build()
	a := AgentAccesses(c, c, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))

	failing.Store(true)
	if _, err := a.Authenticate(ctx, tok); err == nil || errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatalf("a failed read: %v, want an error that is no refusal", err)
	}
	failing.Store(false)
	id, err := a.Authenticate(ctx, tok)
	if err != nil || id.Username != "agent:team-a/bot" || id.Agent == nil || id.Agent.Role != "viewer" {
		t.Fatalf("once the read works: %+v, %v", id, err)
	}
	for name, bad := range map[string]string{"another token": tok + "x", "a Runner's minted token": runnerTok, "no token": "x.y"} {
		if _, err := a.Authenticate(ctx, bad); !errors.Is(err, auth.ErrUnauthenticated) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// The cache answers first: what it does not have, or holds with
	// another hash, is refused without a read of the API server.
	var freshReads atomic.Int32
	fresh := fake.NewClientBuilder().WithScheme(scheme).WithObjects(bot.DeepCopy()).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			freshReads.Add(1)
			return c.Get(ctx, key, obj, opts...)
		}}).Build()
	stale := fake.NewClientBuilder().WithScheme(scheme).WithIndex(&v1alpha1.AgentAccess{}, indexSA, accessSA).Build()
	if _, err := AgentAccesses(stale, fresh, nil, nil).Authenticate(ctx, tok); !errors.Is(err, auth.ErrUnauthenticated) || freshReads.Load() != 0 {
		t.Fatalf("not in the cache: %v, %d fresh reads", err, freshReads.Load())
	}
	// In the cache but gone from the API server: refused at once.
	gone := fake.NewClientBuilder().WithScheme(scheme).Build()
	if _, err := AgentAccesses(c, gone, nil, nil).Authenticate(ctx, tok); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatalf("deleted, the cache not knowing yet: %v", err)
	}

	// One that names a ServiceAccount takes no minted token, whatever its
	// status says.
	bot.Spec.ServiceAccount = &v1alpha1.ServiceAccountRef{Name: "bot"}
	if err := c.Update(ctx, bot); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Authenticate(ctx, tok); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatalf("an AgentAccess with a ServiceAccount: %v", err)
	}
}
