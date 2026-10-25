package controller

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	"github.com/j75689/cadre/internal/storage"
	"github.com/j75689/cadre/internal/token"
)

// rotateEnv is AgentAccess bot in team-a with a minted token, its Secret
// holding another key beside the token, in a fake cluster.
func rotateEnv(t *testing.T) (client.Client, *events.FakeRecorder, string) {
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
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(aa, sec).WithStatusSubresource(&v1alpha1.AgentAccess{}).Build()
	return c, events.NewFakeRecorder(10), old
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
	c, rec, old := rotateEnv(t)
	tok, err := AgentAccessStore(c, c, rec).RotateAgentAccessToken(context.Background(), "team-a", "bot", "lead@example.test")
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
	c, rec, _ := rotateEnv(t)
	var both sync.WaitGroup
	both.Add(2)
	afterSecretRead = func() { both.Done(); both.Wait() }
	t.Cleanup(func() { afterSecretRead = func() {} })
	type result struct {
		tok string
		err error
	}
	results := make(chan result, 2)
	for range 2 {
		go func() {
			tok, err := AgentAccessStore(c, c, rec).RotateAgentAccessToken(context.Background(), "team-a", "bot", "someone")
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
	c, rec, _ := rotateEnv(t)
	beforeStatusWrite = func() {
		beforeStatusWrite = func() {}
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
	t.Cleanup(func() { beforeStatusWrite = func() {} })
	tok, err := AgentAccessStore(c, c, rec).RotateAgentAccessToken(context.Background(), "team-a", "bot", "lead@example.test")
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
	c, rec, _ := rotateEnv(t)
	afterSecretRead = func() {
		afterSecretRead = func() {}
		if err := c.Delete(context.Background(), &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "bot.access-token"}}); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(func() { afterSecretRead = func() {} })
	_, err := AgentAccessStore(c, c, rec).RotateAgentAccessToken(context.Background(), "team-a", "bot", "someone")
	if !errors.Is(err, storage.ErrConflict) || errors.Is(err, storage.ErrNotFound) || !strings.Contains(err.Error(), "token Secret bot.access-token") {
		t.Fatalf("rotate: %v", err)
	}
}

// One the reconciler would not mint for, its name made before the rule,
// gets no token through the API either.
func TestRotateAnInvalidName(t *testing.T) {
	c, rec, _ := rotateEnv(t)
	_, err := AgentAccessStore(c, c, rec).RotateAgentAccessToken(context.Background(), "team-a", "bot.v2", "someone")
	if !errors.Is(err, storage.ErrInvalid) || !strings.Contains(err.Error(), "DNS labels") {
		t.Fatalf("rotate: %v", err)
	}
}
