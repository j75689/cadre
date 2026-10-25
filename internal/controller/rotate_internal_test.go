package controller

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	"github.com/j75689/cadre/internal/api"
	"github.com/j75689/cadre/internal/storage"
	"github.com/j75689/cadre/internal/token"
)

// rotateEnv is AgentAccess bot in team-a, a Team's namespace, with a
// minted token, its Secret holding another key beside the token, in a fake
// cluster, its calls through funcs.
func rotateEnv(t *testing.T, funcs ...interceptor.Funcs) (client.Client, *events.FakeRecorder, string) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	aa := &v1alpha1.AgentAccess{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "bot", UID: "bot-uid"}, Spec: v1alpha1.AgentAccessSpec{Role: "member"}}
	old, hash, err := token.Mint("team-a", "bot")
	if err != nil {
		t.Fatal(err)
	}
	aa.Status.TokenSecret, aa.Status.TokenHash = "bot.access-token", hash
	yes := true
	sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "bot.access-token", OwnerReferences: []metav1.OwnerReference{
		{APIVersion: v1alpha1.GroupVersion.String(), Kind: "AgentAccess", Name: "bot", UID: "bot-uid", Controller: &yes},
	}}, Data: map[string][]byte{tokenKey: []byte(old), "note": []byte("kept by an operator")}}
	team := &v1alpha1.Team{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: v1alpha1.TeamName}}
	b := fake.NewClientBuilder().WithScheme(scheme).WithObjects(aa, sec, team).WithStatusSubresource(&v1alpha1.AgentAccess{})
	if len(funcs) > 0 {
		b = b.WithInterceptorFuncs(funcs[0])
	}
	return b.Build(), events.NewFakeRecorder(10), old
}

// store is the API's store on c, its rotations' hooks settable.
func store(c client.Client, rec *events.FakeRecorder) accessStore {
	return AgentAccessStore(c, c, rec).(accessStore)
}

func live(t *testing.T, c client.Client) (secretToken, statusHash, note string) {
	t.Helper()
	var sec corev1.Secret
	var aa v1alpha1.AgentAccess
	if err := errors.Join(c.Get(context.Background(), client.ObjectKey{Namespace: "team-a", Name: "bot.access-token"}, &sec),
		c.Get(context.Background(), client.ObjectKey{Namespace: "team-a", Name: "bot"}, &aa)); err != nil {
		t.Fatal(err)
	}
	return string(sec.Data[tokenKey]), aa.Status.TokenHash, string(sec.Data["note"])
}

// A rotation replaces the token, and only it, and its hash: the token it
// answers with is the live one, and an Event names who rotated it.
func TestRotate(t *testing.T) {
	t.Parallel()
	c, rec, old := rotateEnv(t)
	tok, err := store(c, rec).RotateAgentAccessToken(context.Background(), "team-a", "bot", "lead@example.test")
	if err != nil || tok == old {
		t.Fatalf("rotate: %q, %v", tok, err)
	}
	if secret, hash, note := live(t, c); secret != tok || hash != token.Hash(tok) || note != "kept by an operator" {
		t.Fatalf("after: Secret %q, hash %q, note %q", secret, hash, note)
	}
	if ev := <-rec.Events; !strings.Contains(ev, "token rotated by lead@example.test") {
		t.Errorf("event %q", ev)
	}
}

// Two rotations at once, both having read the Secret: the Secret's version
// lets one through, and the other is a conflict; the one answered is live.
func TestRotateTwiceAtOnce(t *testing.T) {
	t.Parallel()
	c, rec, _ := rotateEnv(t)
	var both sync.WaitGroup
	both.Add(2)
	s := store(c, rec)
	s.rot.afterSecretRead = func() { both.Done(); both.Wait() }
	type result struct {
		tok string
		err error
	}
	results := make(chan result, 2)
	for range 2 {
		go func() {
			tok, err := s.RotateAgentAccessToken(context.Background(), "team-a", "bot", "someone")
			results <- result{tok, err}
		}()
	}
	a, b := <-results, <-results
	if (a.err == nil) == (b.err == nil) {
		t.Fatalf("want one rotation through and one conflict: %+v, %+v", a, b)
	}
	won, lost := a, b
	if a.err != nil {
		won, lost = b, a
	}
	if !errors.Is(lost.err, storage.ErrConflict) || !strings.Contains(lost.err.Error(), "bot.access-token changed meanwhile") {
		t.Errorf("the other: %v", lost.err)
	}
	if secret, hash, _ := live(t, c); secret != won.tok || hash != token.Hash(won.tok) {
		t.Errorf("the token answered is not the live one: Secret %q, hash %q", secret, hash)
	}
}

// The reconciler writing the status between the Secret and the status
// writes, from the new Secret: the rotation is done all the same, and says
// so, Event included.
func TestRotateWithAResyncBetween(t *testing.T) {
	t.Parallel()
	c, rec, _ := rotateEnv(t)
	s := store(c, rec)
	s.rot.beforeStatusWrite = func() {
		s.rot.beforeStatusWrite = nil
		var sec corev1.Secret
		var aa v1alpha1.AgentAccess
		if err := errors.Join(c.Get(context.Background(), client.ObjectKey{Namespace: "team-a", Name: "bot.access-token"}, &sec),
			c.Get(context.Background(), client.ObjectKey{Namespace: "team-a", Name: "bot"}, &aa)); err != nil {
			t.Error(err)
			return
		}
		aa.Status.TokenHash = token.Hash(string(sec.Data[tokenKey]))
		if err := c.Status().Update(context.Background(), &aa); err != nil {
			t.Error(err)
		}
	}
	tok, err := s.RotateAgentAccessToken(context.Background(), "team-a", "bot", "lead@example.test")
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if secret, hash, _ := live(t, c); secret != tok || hash != token.Hash(tok) {
		t.Errorf("after: Secret %q, hash %q", secret, hash)
	}
	select {
	case ev := <-rec.Events:
		if !strings.Contains(ev, "TokenRotated") {
			t.Errorf("event %q", ev)
		}
	default:
		t.Error("no Event")
	}
}

// A Secret deleted by hand while a rotation runs is a conflict naming the
// Secret, not the AgentAccess not found.
func TestRotateWithTheSecretGone(t *testing.T) {
	t.Parallel()
	c, rec, _ := rotateEnv(t)
	s := store(c, rec)
	s.rot.afterSecretRead = func() {
		s.rot.afterSecretRead = nil
		if err := c.Delete(context.Background(), &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "bot.access-token"}}); err != nil {
			t.Error(err)
		}
	}
	_, err := s.RotateAgentAccessToken(context.Background(), "team-a", "bot", "someone")
	if !errors.Is(err, storage.ErrConflict) || errors.Is(err, storage.ErrNotFound) || !strings.Contains(err.Error(), "token Secret bot.access-token") {
		t.Fatalf("rotate: %v", err)
	}
}

// One the reconciler would not mint for, its name made before the rule,
// gets no token through the API either.
func TestRotateAnInvalidName(t *testing.T) {
	t.Parallel()
	c, rec, _ := rotateEnv(t)
	_, err := store(c, rec).RotateAgentAccessToken(context.Background(), "team-a", "bot.v2", "someone")
	if !errors.Is(err, storage.ErrInvalid) || !strings.Contains(err.Error(), "DNS labels") {
		t.Fatalf("rotate: %v", err)
	}
}

// A second rotation made, whole, while the first waits to write the
// status: the first, its token no longer the Secret's, is a conflict, and
// leaves the second's hash in place, so the second's token works.
func TestRotateOvertaken(t *testing.T) {
	t.Parallel()
	c, rec, _ := rotateEnv(t)
	first := store(c, rec)
	var second string
	first.rot.beforeStatusWrite = func() {
		var err error
		if second, err = store(c, rec).RotateAgentAccessToken(context.Background(), "team-a", "bot", "other"); err != nil {
			t.Errorf("the second rotation: %v", err)
		}
	}
	if _, err := first.RotateAgentAccessToken(context.Background(), "team-a", "bot", "someone"); !errors.Is(err, storage.ErrConflict) {
		t.Fatalf("the first rotation: %v, want a conflict", err)
	}
	if secret, hash, _ := live(t, c); secret != second || hash != token.Hash(second) {
		t.Fatalf("after: Secret %q, hash %q, want the second's %q", secret, hash, second)
	}
}

// A status others keep changing, past the backoff: a conflict that says
// the new token is in the Secret, unshown, and the old one works until the
// reconciler's next pass; the rotation took effect, so its Event is there.
func TestRotateStatusKeepsChanging(t *testing.T) {
	t.Parallel()
	gr := schema.GroupResource{Group: "cadre.dev", Resource: "agentaccesses"}
	c, rec, _ := rotateEnv(t, interceptor.Funcs{SubResourceUpdate: func(context.Context, client.Client, string, client.Object, ...client.SubResourceUpdateOption) error {
		return apierrors.NewConflict(gr, "bot", errors.New("changed"))
	}})
	start := time.Now()
	_, err := store(c, rec).RotateAgentAccessToken(context.Background(), "team-a", "bot", "someone")
	if !errors.Is(err, storage.ErrConflict) || !strings.Contains(err.Error(), "the new token is in the Secret bot.access-token") || !strings.Contains(err.Error(), "the reconciler's next pass") {
		t.Fatalf("rotate: %v", err)
	}
	if took := time.Since(start); took < time.Second || took > 6*time.Second {
		t.Errorf("gave up after %s, want about 2s", took)
	}
	select {
	case ev := <-rec.Events:
		if !strings.Contains(ev, "TokenRotated") || !strings.Contains(ev, "next pass") {
			t.Errorf("event %q", ev)
		}
	default:
		t.Error("no Event for a rotation that took effect")
	}
}

// cadre-server refused the Secret's write: a missing verb on Secrets, not
// on AgentAccesses.
func TestRotateSecretsForbidden(t *testing.T) {
	t.Parallel()
	gr := schema.GroupResource{Resource: "secrets"}
	c, rec, _ := rotateEnv(t, interceptor.Funcs{Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
		if _, ok := obj.(*corev1.Secret); ok {
			return apierrors.NewForbidden(gr, obj.GetName(), errors.New("no"))
		}
		return c.Update(ctx, obj, opts...)
	}})
	_, err := store(c, rec).RotateAgentAccessToken(context.Background(), "team-a", "bot", "someone")
	if !errors.Is(err, api.ErrNotPermitted) || !strings.Contains(err.Error(), "update secrets") {
		t.Fatalf("rotate: %v", err)
	}
}

// No token is minted outside a Team's namespace, as the reconciler mints
// none there.
func TestRotateWithoutATeam(t *testing.T) {
	t.Parallel()
	c, rec, _ := rotateEnv(t)
	if err := c.Delete(context.Background(), &v1alpha1.Team{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: v1alpha1.TeamName}}); err != nil {
		t.Fatal(err)
	}
	if _, err := store(c, rec).RotateAgentAccessToken(context.Background(), "team-a", "bot", "someone"); !errors.Is(err, storage.ErrInvalid) || !strings.Contains(err.Error(), "has no Team") {
		t.Fatalf("rotate: %v", err)
	}
}

// The status read again with its hash gone, cleared by the reconciler
// say: an answer that leaves tokenHash out must not leave this rotation's
// hash in place of it. The rotation writes it again, and the live status
// holds it.
func TestRotateRereadsTheStatusWhole(t *testing.T) {
	t.Parallel()
	gr := schema.GroupResource{Group: "cadre.dev", Resource: "agentaccesses"}
	var once sync.Once
	c, rec, _ := rotateEnv(t, interceptor.Funcs{
		// As a real client reads: JSON decoded into the object as it is,
		// so what the answer leaves out (an empty tokenHash) stays.
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			aa, ok := obj.(*v1alpha1.AgentAccess)
			if !ok {
				return c.Get(ctx, key, obj, opts...)
			}
			var cur v1alpha1.AgentAccess
			if err := c.Get(ctx, key, &cur, opts...); err != nil {
				return err
			}
			data, err := json.Marshal(&cur)
			if err != nil {
				return err
			}
			return json.Unmarshal(data, aa)
		},
		SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			conflicted := false
			once.Do(func() {
				conflicted = true
				// The reconciler clears the hash, as for a Secret it does
				// not own, between the read and this write.
				var aa v1alpha1.AgentAccess
				if err := c.Get(ctx, client.ObjectKeyFromObject(obj), &aa); err != nil {
					t.Error(err)
					return
				}
				aa.Status.TokenHash, aa.Status.TokenSecret = "", ""
				if err := c.SubResource("status").Update(ctx, &aa); err != nil {
					t.Error(err)
				}
			})
			if conflicted {
				return apierrors.NewConflict(gr, obj.GetName(), errors.New("changed"))
			}
			return c.SubResource(sub).Update(ctx, obj, opts...)
		}})
	tok, err := store(c, rec).RotateAgentAccessToken(context.Background(), "team-a", "bot", "someone")
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if secret, hash, _ := live(t, c); secret != tok || hash != token.Hash(tok) {
		t.Fatalf("after: Secret %q, hash %q: the live status must hold the hash", secret, hash)
	}
}

// An AgentAccess switched to a ServiceAccount while the rotation waits to
// write its hash takes no minted token: the rotation is refused, not
// answered with a token checkMinted would refuse.
func TestRotateSwitchedToAServiceAccount(t *testing.T) {
	t.Parallel()
	c, rec, _ := rotateEnv(t)
	s := store(c, rec)
	s.rot.beforeStatusWrite = func() {
		var aa v1alpha1.AgentAccess
		if err := c.Get(context.Background(), client.ObjectKey{Namespace: "team-a", Name: "bot"}, &aa); err != nil {
			t.Error(err)
			return
		}
		aa.Spec.ServiceAccount = &v1alpha1.ServiceAccountRef{Name: "bot"}
		if err := c.Update(context.Background(), &aa); err != nil {
			t.Error(err)
		}
	}
	if _, err := s.RotateAgentAccessToken(context.Background(), "team-a", "bot", "someone"); !errors.Is(err, storage.ErrInvalid) || !strings.Contains(err.Error(), "switched to a ServiceAccount meanwhile") {
		t.Fatalf("rotate: %v", err)
	}
}
