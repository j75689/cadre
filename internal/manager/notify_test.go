package manager

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	"github.com/j75689/cadre/internal/storage"
	"github.com/j75689/cadre/internal/task"
)

// receiver is a webhook that records what it is sent and answers status.
type receiver struct {
	*httptest.Server
	mu     sync.Mutex
	status int
	got    []delivery
}

type delivery struct {
	header http.Header
	body   []byte
}

func newReceiver(t *testing.T) *receiver {
	r := &receiver{status: http.StatusNoContent}
	r.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		r.mu.Lock()
		defer r.mu.Unlock()
		r.got = append(r.got, delivery{header: req.Header.Clone(), body: body})
		w.WriteHeader(r.status)
	}))
	t.Cleanup(r.Close)
	return r
}

func (r *receiver) answer(status int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.status = status
}

func (r *receiver) deliveries() []delivery {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]delivery(nil), r.got...)
}

func notifier(name, url string, secret *v1alpha1.SecretKeyRef, on ...v1alpha1.NotifyEvent) v1alpha1.Notifier {
	n := v1alpha1.Notifier{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Generation: 1},
		Spec:       v1alpha1.NotifierSpec{On: on, Webhook: v1alpha1.Webhook{URL: url, SecretRef: secret, Headers: map[string]string{"X-Team": "a"}}},
	}
	n.Status.ObservedGeneration = 1
	n.Status.Conditions = []metav1.Condition{{Type: v1alpha1.ConditionReady, Status: metav1.ConditionTrue, ObservedGeneration: 1, Reason: "Ready"}}
	return n
}

// notifyEnv is a manager whose namespace has Notifiers, and what it
// recorded about them.
func notifyEnv(t *testing.T, items ...v1alpha1.Notifier) (*env, *[]string) {
	t.Helper()
	e := newEnv(t, 1)
	var mu sync.Mutex
	var recorded []string
	nf := StaticNotifiers{Items: items, Secrets: map[string]string{ns + "/hook/key": "not-a-real-secret"}, Record: func(s string) {
		mu.Lock()
		defer mu.Unlock()
		recorded = append(recorded, s)
	}}
	e.m = New(e.store, Config{Lease: 30 * time.Second, Heartbeat: 10 * time.Second, Agents: testAgents, Notifiers: nf, ExternalURL: "https://cadre.example.test", Version: "v1.2.3"}, WithClock(e.clock.now))
	return e, &recorded
}

func notifyItems(tk *v1alpha1.Task) []v1alpha1.OutboxItem {
	var out []v1alpha1.OutboxItem
	for _, it := range tk.Status.Outbox {
		if it.Kind == v1alpha1.OutboxNotify {
			out = append(out, it)
		}
	}
	return out
}

func (e *env) question(t *testing.T, key storage.Key) {
	t.Helper()
	c := e.claim(t)
	if _, _, err := e.m.Finish(ctx, key, FinishRequest{RunID: c.RunID, Outcome: task.OutcomeWaiting, Reason: v1alpha1.WaitingQuestion}); err != nil {
		t.Fatal(err)
	}
}

func TestNotifyIsWrittenOnlyWhenListenedFor(t *testing.T) {
	rec := newReceiver(t)
	e, _ := notifyEnv(t, notifier("ends", rec.URL, nil, v1alpha1.NotifyDone))
	key := e.addTask(t, "x", v1alpha1.AssigneeAgent)
	e.question(t, key)
	if items := notifyItems(e.get(t, key)); len(items) != 0 {
		t.Fatalf("a Question nobody listens for wrote %+v", items)
	}
	e2, _ := notifyEnv(t, notifier("asks", rec.URL, nil, v1alpha1.NotifyQuestion))
	key = e2.addTask(t, "y", v1alpha1.AssigneeAgent)
	e2.question(t, key)
	items := notifyItems(e2.get(t, key))
	if len(items) != 1 || items[0].Notify.Event != v1alpha1.NotifyQuestion {
		t.Fatalf("a Question listened for wrote %+v", items)
	}
	// Further writes while it waits tell nothing new.
	if _, err := e2.m.AppendThread(ctx, key, Author{Kind: AuthorUser, Name: "dana"}, EntryComment, "hm"); err != nil {
		t.Fatal(err)
	}
	if items := notifyItems(e2.get(t, key)); len(items) != 1 {
		t.Fatalf("a comment while waiting added items: %+v", items)
	}
}

func TestNotifyDelivers(t *testing.T) {
	rec := newReceiver(t)
	e, recorded := notifyEnv(t, notifier("hook", rec.URL, &v1alpha1.SecretKeyRef{Name: "hook", Key: "key"}, v1alpha1.NotifyQuestion))
	key := e.addTask(t, "x", v1alpha1.AssigneeAgent)
	e.question(t, key)
	id := notifyItems(e.get(t, key))[0].ID
	if err := e.m.Dispatch(ctx); err != nil {
		t.Fatal(err)
	}
	got := rec.deliveries()
	if len(got) != 1 {
		t.Fatalf("%d deliveries", len(got))
	}
	d := got[0]
	var p Payload
	if err := json.Unmarshal(d.body, &p); err != nil {
		t.Fatal(err)
	}
	if p.ID != id || p.Event != v1alpha1.NotifyQuestion || p.Task.Name != "x" || p.Task.Title != "x" || p.Task.Phase != v1alpha1.PhaseWaiting || p.Task.Number != 1 ||
		p.Task.Assignee == nil || p.URL != "https://cadre.example.test/t/team-a/tasks/x" {
		t.Errorf("payload %+v", p)
	}
	mac := hmac.New(sha256.New, []byte("not-a-real-secret"))
	mac.Write(d.body)
	for k, want := range map[string]string{
		"Content-Type": "application/json", "Idempotency-Key": id, "User-Agent": "cadre-server/v1.2.3", "X-Team": "a",
		"X-Cadre-Signature": "sha256=" + hex.EncodeToString(mac.Sum(nil)),
	} {
		if d.header.Get(k) != want {
			t.Errorf("%s: %q, want %q", k, d.header.Get(k), want)
		}
	}
	if items := notifyItems(e.get(t, key)); len(items) != 0 {
		t.Errorf("a delivered item stayed: %+v", items)
	}
	if strings.Join(*recorded, "|") != "delivered hook: <nil>" {
		t.Errorf("recorded %v", *recorded)
	}
}

// A failure leaves the item for the next pass; one Notifier failing does
// not resend to the others.
func TestNotifyRetries(t *testing.T) {
	good, bad := newReceiver(t), newReceiver(t)
	bad.answer(http.StatusInternalServerError)
	e, _ := notifyEnv(t, notifier("good", good.URL, nil, v1alpha1.NotifyQuestion), notifier("bad", bad.URL, nil, v1alpha1.NotifyQuestion))
	key := e.addTask(t, "x", v1alpha1.AssigneeAgent)
	e.question(t, key)
	_ = e.m.Dispatch(ctx)
	items := notifyItems(e.get(t, key))
	if len(items) != 1 || strings.Join(items[0].Notify.Delivered, ",") != "good" {
		t.Fatalf("after a 500: %+v", items)
	}
	_ = e.m.Dispatch(ctx)
	if n := len(good.deliveries()); n != 1 {
		t.Errorf("the good receiver got %d", n)
	}
	bad.answer(http.StatusOK)
	if err := e.m.Dispatch(ctx); err != nil {
		t.Fatal(err)
	}
	if n, m := len(good.deliveries()), len(bad.deliveries()); n != 1 || m != 3 {
		t.Errorf("good %d, bad %d deliveries", n, m)
	}
	if items := notifyItems(e.get(t, key)); len(items) != 0 {
		t.Errorf("delivered to both, the item stayed: %+v", items)
	}
}

func TestNotifyRefusesRedirects(t *testing.T) {
	target := newReceiver(t)
	moved := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, http.StatusFound) }))
	t.Cleanup(moved.Close)
	e, _ := notifyEnv(t, notifier("moved", moved.URL, nil, v1alpha1.NotifyQuestion))
	key := e.addTask(t, "x", v1alpha1.AssigneeAgent)
	e.question(t, key)
	if err := e.m.Dispatch(ctx); err == nil {
		t.Error("a redirect counted as delivered")
	}
	if len(target.deliveries()) != 0 || len(notifyItems(e.get(t, key))) != 1 {
		t.Error("the redirect was followed")
	}
}

func TestNotifyGivesUpAfterADay(t *testing.T) {
	bad := newReceiver(t)
	bad.answer(http.StatusBadGateway)
	e, recorded := notifyEnv(t, notifier("dead", bad.URL, nil, v1alpha1.NotifyQuestion))
	key := e.addTask(t, "x", v1alpha1.AssigneeAgent)
	e.question(t, key)
	_ = e.m.Dispatch(ctx)
	e.clock.add(NotifyTTL + time.Minute)
	if err := e.m.Dispatch(ctx); err != nil {
		t.Fatal(err)
	}
	if items := notifyItems(e.get(t, key)); len(items) != 0 {
		t.Errorf("a day later the item stayed: %+v", items)
	}
	if n := len(bad.deliveries()); n != 1 {
		t.Errorf("%d deliveries, want the one before the day ran out", n)
	}
	last := (*recorded)[len(*recorded)-1]
	if !strings.HasPrefix(last, "warn team-a/x NotificationDropped") || !strings.Contains(last, "dead") {
		t.Errorf("recorded %v", *recorded)
	}
}

func TestNotifyCancelled(t *testing.T) {
	rec := newReceiver(t)
	e, _ := notifyEnv(t, notifier("ends", rec.URL, nil, v1alpha1.NotifyQuestion, v1alpha1.NotifyCancelled))
	key := e.addTask(t, "x", v1alpha1.AssigneeAgent)
	e.question(t, key)
	if _, err := e.m.Cancel(ctx, key); err != nil {
		t.Fatal(err)
	}
	items := notifyItems(e.get(t, key))
	if len(items) != 2 || items[0].Notify.Event != v1alpha1.NotifyQuestion || items[1].Notify.Event != v1alpha1.NotifyCancelled {
		t.Fatalf("a cancel kept and added %+v", items)
	}
	if err := e.m.Dispatch(ctx); err != nil {
		t.Fatal(err)
	}
	var events []string
	for _, d := range rec.deliveries() {
		var p Payload
		_ = json.Unmarshal(d.body, &p)
		events = append(events, string(p.Event))
	}
	if strings.Join(events, ",") != "Question,Cancelled" {
		t.Errorf("events %v", events)
	}
}
