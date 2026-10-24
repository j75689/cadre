package controller_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	"github.com/j75689/cadre/internal/controller"
	"github.com/j75689/cadre/internal/manager"
	"github.com/j75689/cadre/internal/storage"
	"github.com/j75689/cadre/internal/storage/kubernetes"
	"github.com/j75689/cadre/internal/task"
	"github.com/j75689/cadre/internal/testenv"
)

func TestNotifiers(t *testing.T) {
	cfg := testenv.APIServer(t)
	mgr, err := controller.NewManager(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := controller.Setup(mgr, controller.Options{Resync: 200 * time.Millisecond}, controller.SkipNameValidation()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- mgr.Start(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("manager: %v", err)
		}
	})
	store, err := kubernetes.Open(ctx, kubernetes.Config{REST: cfg, S3: testenv.S3(t), Cache: mgr.GetCache()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	c, err := client.New(cfg, client.Options{Scheme: mgr.GetScheme()})
	if err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, c: c}
	const ns = "team-notify"

	// Ready, and why not.
	e.create(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "no-team"}})
	orphan := &v1alpha1.Notifier{ObjectMeta: meta2("no-team", "n"), Spec: v1alpha1.NotifierSpec{On: []v1alpha1.NotifyEvent{v1alpha1.NotifyDone}, Webhook: &v1alpha1.Webhook{URL: "https://hooks.example.test/x"}}}
	e.create(orphan)
	e.ready(orphan, metav1.ConditionFalse, "NoTeam")
	e.create(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}, &v1alpha1.Team{ObjectMeta: meta2(ns, v1alpha1.TeamName)})
	hostless := &v1alpha1.Notifier{ObjectMeta: meta2(ns, "hostless"), Spec: v1alpha1.NotifierSpec{On: []v1alpha1.NotifyEvent{v1alpha1.NotifyDone}, Webhook: &v1alpha1.Webhook{URL: "http://:8080/x"}}}
	unsigned := &v1alpha1.Notifier{ObjectMeta: meta2(ns, "unsigned"), Spec: v1alpha1.NotifierSpec{On: []v1alpha1.NotifyEvent{v1alpha1.NotifyDone},
		Webhook: &v1alpha1.Webhook{URL: "https://hooks.example.test/x", SecretRef: &v1alpha1.SecretKeyRef{Name: "hook", Key: "key"}}}}
	e.create(hostless, unsigned)
	e.ready(hostless, metav1.ConditionFalse, "InvalidURL")
	e.ready(unsigned, metav1.ConditionFalse, "SecretMissing")

	// Delivery, through the store and the controller's view of the
	// Notifiers: a signed POST, recorded on the Notifier.
	var mu sync.Mutex
	var bodies []string
	var sigs []string
	status := http.StatusOK
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		defer mu.Unlock()
		bodies, sigs = append(bodies, string(b)), append(sigs, r.Header.Get("X-Cadre-Signature"))
		w.WriteHeader(status)
	}))
	t.Cleanup(hook.Close)
	e.create(&corev1.Secret{ObjectMeta: meta2(ns, "hook"), StringData: map[string]string{"key": "not-a-real-secret"}})
	asks := &v1alpha1.Notifier{ObjectMeta: meta2(ns, "asks"), Spec: v1alpha1.NotifierSpec{On: []v1alpha1.NotifyEvent{v1alpha1.NotifyQuestion},
		Webhook: &v1alpha1.Webhook{URL: hook.URL, SecretRef: &v1alpha1.SecretKeyRef{Name: "hook", Key: "key"}}}}
	e.create(asks)
	e.ready(asks, metav1.ConditionTrue, "Ready")
	e.ready(unsigned, metav1.ConditionTrue, "Ready") // its Secret exists now

	now := time.Now()
	clock := func() time.Time { return now }
	m := manager.New(store, manager.Config{Agents: manager.ReadyBuiltin(ns, "coder", "claude"), Notifiers: controller.Notifiers(mgr), ExternalURL: "https://cadre.example.test"}, manager.WithClock(clock))
	tk, err := m.CreateTask(ctx, ns, "x", v1alpha1.TaskSpec{Title: "Ask me", Assignee: &v1alpha1.Assignee{Kind: v1alpha1.AssigneeAgent, Name: "coder"}})
	if err != nil {
		t.Fatal(err)
	}
	key := storage.Key{Namespace: ns, Name: tk.Name}
	cl := claim(t, m)
	if _, _, err := m.Finish(ctx, key, manager.FinishRequest{RunID: cl.RunID, Outcome: task.OutcomeWaiting, Reason: v1alpha1.WaitingQuestion}); err != nil {
		t.Fatal(err)
	}
	if err := m.DeliverNotifications(ctx); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	if len(bodies) != 1 || !strings.Contains(bodies[0], `"event":"Question"`) {
		t.Fatalf("bodies %v", bodies)
	}
	mac := hmac.New(sha256.New, []byte("not-a-real-secret"))
	mac.Write([]byte(bodies[0]))
	if sigs[0] != "sha256="+hex.EncodeToString(mac.Sum(nil)) {
		t.Errorf("signature %q", sigs[0])
	}
	status = http.StatusServiceUnavailable
	mu.Unlock()
	eventually(t, 10*time.Second, func() (bool, string) {
		_ = c.Get(ctx, client.ObjectKeyFromObject(asks), asks)
		return asks.Status.LastSent != nil, "no lastSent"
	})

	// A webhook that stays down for a day: the item goes, with an Event on
	// the Task naming it, and the Notifier says what failed.
	if _, err := m.Cancel(ctx, key); err != nil {
		t.Fatal(err)
	}
	if err := m.DeliverNotifications(ctx); err != nil {
		t.Fatal(err) // nobody listens for Cancelled: nothing to send
	}
	asks2 := &v1alpha1.Notifier{ObjectMeta: meta2(ns, "ends"), Spec: v1alpha1.NotifierSpec{On: []v1alpha1.NotifyEvent{v1alpha1.NotifyDone}, Webhook: &v1alpha1.Webhook{URL: hook.URL}}}
	e.create(asks2)
	e.ready(asks2, metav1.ConditionTrue, "Ready")
	tk2, err := m.CreateTask(ctx, ns, "y", v1alpha1.TaskSpec{Title: "Do it", Assignee: &v1alpha1.Assignee{Kind: v1alpha1.AssigneeAgent, Name: "coder"}})
	if err != nil {
		t.Fatal(err)
	}
	key2 := storage.Key{Namespace: ns, Name: tk2.Name}
	cl = claim(t, m)
	if _, _, err := m.Finish(ctx, key2, manager.FinishRequest{RunID: cl.RunID, Outcome: task.OutcomeDone}); err != nil {
		t.Fatal(err)
	}
	_ = m.DeliverNotifications(ctx) // the 503
	eventually(t, 10*time.Second, func() (bool, string) {
		_ = c.Get(ctx, client.ObjectKeyFromObject(asks2), asks2)
		return asks2.Status.LastError != nil && strings.Contains(asks2.Status.LastError.Message, "503"), "no lastError"
	})
	now = now.Add(manager.NotifyTTL + time.Minute)
	if err := m.DeliverNotifications(ctx); err != nil {
		t.Fatal(err)
	}
	eventually(t, 10*time.Second, func() (bool, string) {
		var list eventsv1.EventList
		_ = c.List(ctx, &list, client.InNamespace(ns))
		for _, ev := range list.Items {
			if ev.Reason == "NotificationDropped" && ev.Regarding.Name == tk2.Name && strings.Contains(ev.Note, "ends") {
				return true, ""
			}
		}
		return false, "no NotificationDropped Event"
	})
}

// claim claims the task the index has just seen.
func claim(t *testing.T, m *manager.Manager) *manager.Claimed {
	t.Helper()
	var cl *manager.Claimed
	eventually(t, 10*time.Second, func() (bool, string) {
		var err error
		cl, err = m.Claim(context.Background(), manager.ClaimRequest{Runner: "r", Labels: map[string]string{v1alpha1.LabelRuntime: string(v1alpha1.RuntimeBuiltin)}})
		return err == nil && cl != nil, fmt.Sprintf("claim: %v", err)
	})
	return cl
}
