package manager

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
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
		Spec:       v1alpha1.NotifierSpec{On: on, Webhook: &v1alpha1.Webhook{URL: url, SecretRef: secret, Headers: map[string]string{"X-Team": "a"}}},
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
	if err := e.m.DeliverNotifications(ctx); err != nil {
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
	_ = e.m.DeliverNotifications(ctx)
	items := notifyItems(e.get(t, key))
	if len(items) != 1 || strings.Join(items[0].Notify.Delivered, ",") != "good" {
		t.Fatalf("after a 500: %+v", items)
	}
	_ = e.m.DeliverNotifications(ctx)
	if n := len(good.deliveries()); n != 1 {
		t.Errorf("the good receiver got %d", n)
	}
	bad.answer(http.StatusOK)
	if err := e.m.DeliverNotifications(ctx); err != nil {
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
	if err := e.m.DeliverNotifications(ctx); err == nil {
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
	_ = e.m.DeliverNotifications(ctx)
	e.clock.add(NotifyTTL + time.Minute)
	if err := e.m.DeliverNotifications(ctx); err != nil {
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
	if err := e.m.DeliverNotifications(ctx); err != nil {
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

// A run handing its work to a human (OutcomeReview, phase Review) tells
// Review, as a wait for a review does.
func TestNotifyReview(t *testing.T) {
	rec := newReceiver(t)
	e, _ := notifyEnv(t, notifier("reviews", rec.URL, nil, v1alpha1.NotifyReview))
	key := e.addTask(t, "x", v1alpha1.AssigneeAgent)
	c := e.claim(t)
	if _, _, err := e.m.Finish(ctx, key, FinishRequest{RunID: c.RunID, Outcome: task.OutcomeReview}); err != nil {
		t.Fatal(err)
	}
	items := notifyItems(e.get(t, key))
	if len(items) != 1 || items[0].Notify.Event != v1alpha1.NotifyReview {
		t.Fatalf("a finish for review wrote %+v", items)
	}
}

// waitOn puts a task in Waiting on an entry not in the thread yet, its
// notification written with it, as a thread_ask's Finish does.
func (e *env) waitOn(t *testing.T, key storage.Key, item string) {
	t.Helper()
	tk := e.get(t, key)
	before := tk.Status.DeepCopy()
	tk.Status.Phase = v1alpha1.PhaseWaiting
	tk.Status.Waiting = &v1alpha1.Waiting{Reason: v1alpha1.WaitingQuestion, Since: metav1.NewTime(e.clock.now()), ItemID: item}
	tk.Status.Outbox = nil
	tk.Status.Run = nil
	e.m.addNotify(ctx, &v1alpha1.Task{ObjectMeta: tk.ObjectMeta, Status: *before}, &tk.Status)
	if err := e.store.UpdateTaskStatus(ctx, tk); err != nil {
		t.Fatal(err)
	}
}

func payloads(t *testing.T, r *receiver) []Payload {
	t.Helper()
	var out []Payload
	for _, d := range r.deliveries() {
		var p Payload
		if err := json.Unmarshal(d.body, &p); err != nil {
			t.Fatal(err)
		}
		out = append(out, p)
	}
	return out
}

// The entry a Question waits on, placed after the wait began, is in the
// first delivery; a later question's is never in an earlier one's.
func TestNotifyThreadSeq(t *testing.T) {
	rec := newReceiver(t)
	e, _ := notifyEnv(t, notifier("asks", rec.URL, nil, v1alpha1.NotifyQuestion))
	key := e.addTask(t, "x", v1alpha1.AssigneeAgent)
	const item = "01JB0000000000000000000009"
	e.waitOn(t, key, item)
	if n := notifyItems(e.get(t, key))[0].Notify; n.ThreadSeq != 0 || n.WaitingItem != item {
		t.Fatalf("notification %+v", n)
	}
	if _, err := e.m.transition(ctx, key, task.WaitingAt{ItemID: item, Seq: 5}); err != nil {
		t.Fatal(err)
	}
	if err := e.m.DeliverNotifications(ctx); err != nil {
		t.Fatal(err)
	}
	if p := payloads(t, rec); len(p) != 1 || p[0].ThreadSeq != 5 {
		t.Fatalf("payloads %+v", p)
	}

	// An old notification, retried while the task waits on another entry.
	rec2 := newReceiver(t)
	rec2.answer(http.StatusInternalServerError)
	e2, _ := notifyEnv(t, notifier("asks", rec2.URL, nil, v1alpha1.NotifyQuestion))
	key = e2.addTask(t, "y", v1alpha1.AssigneeAgent)
	e2.waitOn(t, key, "01JB0000000000000000000001")
	tk := e2.get(t, key)
	tk.Status.Waiting.ItemID, tk.Status.Waiting.ThreadSeq = "01JB0000000000000000000002", 9
	if err := e2.store.UpdateTaskStatus(ctx, tk); err != nil {
		t.Fatal(err)
	}
	_ = e2.m.DeliverNotifications(ctx)
	if p := payloads(t, rec2); len(p) != 1 || p[0].ThreadSeq != 0 {
		t.Errorf("an old question labelled with a later one's entry: %+v", p)
	}
}

// A notification goes to the Notifiers listening when it was written, not
// to one created later.
func TestNotifyGoesToThoseListeningThen(t *testing.T) {
	early, late := newReceiver(t), newReceiver(t)
	early.answer(http.StatusInternalServerError)
	e, _ := notifyEnv(t, notifier("early", early.URL, nil, v1alpha1.NotifyQuestion))
	key := e.addTask(t, "x", v1alpha1.AssigneeAgent)
	e.question(t, key)
	nf := e.m.cfg.Notifiers.(StaticNotifiers)
	nf.Items = append(nf.Items, notifier("late", late.URL, nil, v1alpha1.NotifyQuestion))
	e.m.cfg.Notifiers = nf
	_ = e.m.DeliverNotifications(ctx)
	if n := len(late.deliveries()); n != 0 {
		t.Errorf("a Notifier made after the question got %d", n)
	}
	// Gone, it counts as having taken it.
	nf.Items = nf.Items[1:]
	e.m.cfg.Notifiers = nf
	if err := e.m.DeliverNotifications(ctx); err != nil {
		t.Fatal(err)
	}
	if items := notifyItems(e.get(t, key)); len(items) != 0 {
		t.Errorf("the item outlived its only Notifier: %+v", items)
	}
}

// Two servers on one store: one sends, the other waits its turn.
func TestNotifyOneServerAtATime(t *testing.T) {
	rec := newReceiver(t)
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		rec.Config.Handler.ServeHTTP(w, r)
	}))
	t.Cleanup(slow.Close)
	e, _ := notifyEnv(t, notifier("hook", slow.URL, nil, v1alpha1.NotifyQuestion))
	key := e.addTask(t, "x", v1alpha1.AssigneeAgent)
	e.question(t, key)
	other := New(e.store, e.m.cfg, WithClock(e.clock.now))
	var wg sync.WaitGroup
	for _, m := range []*Manager{e.m, other} {
		wg.Go(func() { _ = m.DeliverNotifications(ctx) })
	}
	wg.Wait()
	if n := len(rec.deliveries()); n != 1 {
		t.Errorf("%d deliveries from two servers, want 1", n)
	}
}

// failingNotifiers cannot list.
type failingNotifiers struct{ StaticNotifiers }

func (failingNotifiers) ListNotifiers(context.Context, string) ([]v1alpha1.Notifier, error) {
	return nil, errors.New("the Notifier CRD is not installed")
}

// Notifiers that cannot be listed cost the notification, never the
// transition.
func TestNotifyNeverBlocksATransition(t *testing.T) {
	e := newEnv(t, 1)
	e.m = New(e.store, Config{Agents: testAgents, Notifiers: failingNotifiers{}}, WithClock(e.clock.now))
	key := e.addTask(t, "x", v1alpha1.AssigneeAgent)
	e.question(t, key)
	if tk := e.get(t, key); tk.Status.Phase != v1alpha1.PhaseWaiting || len(notifyItems(tk)) != 0 {
		t.Errorf("phase %s, items %+v", tk.Status.Phase, notifyItems(tk))
	}
	if _, err := e.m.Cancel(ctx, key); err != nil {
		t.Errorf("cancel: %v", err)
	}
}

// A Notifier that is not Ready is not tried, nor is that recorded.
func TestNotifySkipsANotifierNotReady(t *testing.T) {
	rec := newReceiver(t)
	nf := notifier("hook", rec.URL, nil, v1alpha1.NotifyQuestion)
	nf.Generation = 2 // Ready was decided for the spec before
	e, recorded := notifyEnv(t, nf)
	key := e.addTask(t, "x", v1alpha1.AssigneeAgent)
	e.question(t, key)
	_ = e.m.DeliverNotifications(ctx)
	if len(rec.deliveries()) != 0 || len(*recorded) != 0 {
		t.Errorf("deliveries %d, recorded %v", len(rec.deliveries()), *recorded)
	}
}

func TestRedact(t *testing.T) {
	if got := redact("https://user:token@hooks.example.test/x?key=abc#f"); got != "https://hooks.example.test/x" {
		t.Errorf("redact: %q", got)
	}
}

// A server whose read is stale sends nothing another one already sent.
func TestNotifyAfterAnotherServer(t *testing.T) {
	a, b := newReceiver(t), newReceiver(t)
	e, _ := notifyEnv(t, notifier("a", a.URL, nil, v1alpha1.NotifyQuestion), notifier("b", b.URL, nil, v1alpha1.NotifyQuestion))
	key := e.addTask(t, "x", v1alpha1.AssigneeAgent)
	e.question(t, key)
	stale := notifyItems(e.get(t, key))[0]
	nfs, _ := e.m.cfg.Notifiers.ListNotifiers(ctx, ns)
	byName := map[string]*v1alpha1.Notifier{}
	for i := range nfs {
		byName[nfs[i].Name] = &nfs[i]
	}
	// Another server sent it to a.
	if _, err := e.m.transition(ctx, key, task.NotifyDelivered{ID: stale.ID, To: []string{"a"}}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.m.notifyItem(ctx, key, stale, byName); err != nil {
		t.Fatal(err)
	}
	if na, nb := len(a.deliveries()), len(b.deliveries()); na != 0 || nb != 1 {
		t.Errorf("a %d, b %d deliveries; want what was left, b only", na, nb)
	}
	// Then finished it.
	if _, err := e.m.transition(ctx, key, task.AckOutbox{IDs: []string{stale.ID}}); err != nil {
		t.Fatal(err)
	}
	if done, _, err := e.m.notifyItem(ctx, key, stale, byName); err != nil || done {
		t.Errorf("a gone item: done %v, %v", done, err)
	}
	if na, nb := len(a.deliveries()), len(b.deliveries()); na != 0 || nb != 1 {
		t.Errorf("a gone item was sent again: a %d, b %d", na, nb)
	}
}

func TestNotifyTurnCoversTheDeliveries(t *testing.T) {
	for n, want := range map[int]time.Duration{1: 20 * time.Second, 4: 20 * time.Second, 5: 30 * time.Second, 13: 50 * time.Second} {
		if got := notifyTurn(n); got != want {
			t.Errorf("notifyTurn(%d) = %s, want %s", n, got, want)
		}
	}
}

// failAcks fails a status write that takes items out of the outbox.
type failAcks struct{ storage.Driver }

func (f failAcks) UpdateTaskStatus(ctx context.Context, t *v1alpha1.Task) error {
	cur, err := f.GetTask(ctx, storage.Key{Namespace: t.Namespace, Name: t.Name})
	if err == nil && len(t.Status.Outbox) < len(cur.Status.Outbox) {
		return errors.New("the ack is lost")
	}
	return f.Driver.UpdateTaskStatus(ctx, t)
}

// Who took it is written as it is taken, so a Notifier that failed is sent
// to again without resending to one that took it.
func TestNotifyRecordsWhoTookIt(t *testing.T) {
	ok, bad := newReceiver(t), newReceiver(t)
	bad.answer(http.StatusBadGateway)
	e, _ := notifyEnv(t, notifier("a", ok.URL, nil, v1alpha1.NotifyQuestion), notifier("b", bad.URL, nil, v1alpha1.NotifyQuestion))
	key := e.addTask(t, "x", v1alpha1.AssigneeAgent)
	e.question(t, key)
	_ = e.m.DeliverNotifications(ctx)
	_ = e.m.DeliverNotifications(ctx)
	if na, nb := len(ok.deliveries()), len(bad.deliveries()); na != 1 || nb != 2 {
		t.Errorf("a %d, b %d deliveries; want a once and b on every pass", na, nb)
	}
	if items := notifyItems(e.get(t, key)); len(items) != 1 || strings.Join(items[0].Notify.Delivered, ",") != "a" {
		t.Errorf("items %+v", items)
	}
}

// A notification every Notifier takes on the first try costs two writes:
// the claim, and the record of who took it, which removes it. No ack.
func TestNotifyTakenByAllIsTwoWrites(t *testing.T) {
	a, b := newReceiver(t), newReceiver(t)
	e, _ := notifyEnv(t, notifier("a", a.URL, nil, v1alpha1.NotifyQuestion), notifier("b", b.URL, nil, v1alpha1.NotifyQuestion))
	key := e.addTask(t, "x", v1alpha1.AssigneeAgent)
	e.question(t, key)
	var writes int
	e.m.store = countWrites{e.store, &writes}
	if err := e.m.DeliverNotifications(ctx); err != nil {
		t.Fatal(err)
	}
	if writes != 2 || len(a.deliveries()) != 1 || len(b.deliveries()) != 1 {
		t.Errorf("%d writes, %d and %d deliveries; want 2 writes, one delivery each", writes, len(a.deliveries()), len(b.deliveries()))
	}
	if items := notifyItems(e.get(t, key)); len(items) != 0 {
		t.Errorf("a notification all took stayed: %+v", items)
	}
	// Nothing left: another pass writes nothing.
	if err := e.m.DeliverNotifications(ctx); err != nil || writes != 2 {
		t.Errorf("%d writes after another pass, %v", writes, err)
	}
}

// A Notifier deleted since the notification was written counts as having
// taken it: the record of the others removes it.
func TestNotifyGoneNotifierCountsAsTaken(t *testing.T) {
	a, b := newReceiver(t), newReceiver(t)
	e, _ := notifyEnv(t, notifier("a", a.URL, nil, v1alpha1.NotifyQuestion), notifier("b", b.URL, nil, v1alpha1.NotifyQuestion))
	key := e.addTask(t, "x", v1alpha1.AssigneeAgent)
	e.question(t, key)
	nf := e.m.cfg.Notifiers.(StaticNotifiers)
	nf.Items = nf.Items[:1] // b is deleted
	e.m.cfg.Notifiers = nf
	var writes int
	e.m.store = countWrites{e.store, &writes}
	if err := e.m.DeliverNotifications(ctx); err != nil {
		t.Fatal(err)
	}
	if writes != 2 || len(a.deliveries()) != 1 || len(b.deliveries()) != 0 {
		t.Errorf("%d writes, a %d, b %d deliveries", writes, len(a.deliveries()), len(b.deliveries()))
	}
	if items := notifyItems(e.get(t, key)); len(items) != 0 {
		t.Errorf("items %+v", items)
	}
}

// unseenNotifiers is a cache that has seen only the first listed of its
// Notifiers: the others exist, and the fresh read finds them.
type unseenNotifiers struct {
	StaticNotifiers
	listed int
}

func (u unseenNotifiers) ListNotifiers(ctx context.Context, ns string) ([]v1alpha1.Notifier, error) {
	all, err := u.StaticNotifiers.ListNotifiers(ctx, ns)
	return all[:u.listed], err
}

// A Notifier missing from this server's cache, but not from the API
// server, has not taken a notification: it is neither acked nor removed,
// and is sent to once the cache has it.
func TestNotifyUnseenNotifierIsNotGone(t *testing.T) {
	a, b := newReceiver(t), newReceiver(t)
	e, _ := notifyEnv(t, notifier("a", a.URL, nil, v1alpha1.NotifyQuestion), notifier("b", b.URL, nil, v1alpha1.NotifyQuestion))
	key := e.addTask(t, "x", v1alpha1.AssigneeAgent)
	e.question(t, key)
	nf := e.m.cfg.Notifiers.(StaticNotifiers)
	for listed, want := range []struct{ a, b, items int }{ // deliveries so far
		{0, 0, 1}, // neither seen: not acked
		{1, 0, 1}, // a seen: sent to a, not removed
		{1, 1, 0}, // both: sent to b, and removed
	} {
		e.m.cfg.Notifiers = unseenNotifiers{nf, listed}
		if err := e.m.DeliverNotifications(ctx); err != nil {
			t.Fatal(err)
		}
		items := notifyItems(e.get(t, key))
		if len(a.deliveries()) != want.a || len(b.deliveries()) != want.b || len(items) != want.items {
			t.Errorf("%d seen: a %d, b %d deliveries, items %+v", listed, len(a.deliveries()), len(b.deliveries()), items)
		}
	}
}

// A server with no Notifiers has nobody to tell: it acks what it finds.
func TestNotifyWithoutNotifiers(t *testing.T) {
	rec := newReceiver(t)
	e, _ := notifyEnv(t, notifier("hook", rec.URL, nil, v1alpha1.NotifyQuestion))
	key := e.addTask(t, "x", v1alpha1.AssigneeAgent)
	e.question(t, key)
	e.m.cfg.Notifiers = nil
	if err := e.m.DeliverNotifications(ctx); err != nil {
		t.Fatal(err)
	}
	if items := notifyItems(e.get(t, key)); len(items) != 0 || len(rec.deliveries()) != 0 {
		t.Errorf("items %+v, deliveries %d", items, len(rec.deliveries()))
	}
}

// countWrites counts task status writes.
type countWrites struct {
	storage.Driver
	n *int
}

func (c countWrites) UpdateTaskStatus(ctx context.Context, t *v1alpha1.Task) error {
	*c.n++
	return c.Driver.UpdateTaskStatus(ctx, t)
}

// A notification nobody Ready can take costs no write, pass after pass.
func TestNotifyNotReadyCostsNoWrite(t *testing.T) {
	rec := newReceiver(t)
	nf := notifier("hook", rec.URL, nil, v1alpha1.NotifyQuestion)
	nf.Generation = 2
	e, _ := notifyEnv(t, nf)
	key := e.addTask(t, "x", v1alpha1.AssigneeAgent)
	e.question(t, key)
	var writes int
	e.m.store = countWrites{e.store, &writes}
	for range 3 {
		_ = e.m.DeliverNotifications(ctx)
	}
	if writes != 0 {
		t.Errorf("%d task writes for a Notifier not Ready", writes)
	}
}

// Finished by another server since it was read: the claim finds the item
// gone (task.ErrNotifyGone), so nothing is sent and nothing is left to ack.
// The turn that finds an item still there with nothing pending is
// TestNotifyTakenItemLeftByAnOlderServer.
func TestNotifyFinishedBeforeTheClaim(t *testing.T) {
	rec := newReceiver(t)
	e, _ := notifyEnv(t, notifier("hook", rec.URL, nil, v1alpha1.NotifyQuestion))
	key := e.addTask(t, "x", v1alpha1.AssigneeAgent)
	e.question(t, key)
	stale := notifyItems(e.get(t, key))[0]
	if _, err := e.m.transition(ctx, key, task.NotifyDelivered{ID: stale.ID, To: []string{"hook"}}); err != nil {
		t.Fatal(err)
	}
	if items := notifyItems(e.get(t, key)); len(items) != 0 {
		t.Fatalf("the record of the last Notifier left %+v", items)
	}
	nfs, _ := e.m.cfg.Notifiers.ListNotifiers(ctx, ns)
	ack, _, err := e.m.notifyItem(ctx, key, stale, map[string]*v1alpha1.Notifier{"hook": &nfs[0]})
	if err != nil || ack || len(rec.deliveries()) != 0 {
		t.Fatalf("ack %v, %v, %d deliveries", ack, err, len(rec.deliveries()))
	}
}

// An item recorded as taken by all but still there, as a server before
// NotifyDelivered removed items left it, is removed by the turn that finds
// nothing pending: the turn ends with it.
func TestNotifyTakenItemLeftByAnOlderServer(t *testing.T) {
	rec := newReceiver(t)
	e, _ := notifyEnv(t, notifier("hook", rec.URL, nil, v1alpha1.NotifyQuestion))
	key := e.addTask(t, "x", v1alpha1.AssigneeAgent)
	e.question(t, key)
	stale := notifyItems(e.get(t, key))[0]
	tk := e.get(t, key)
	for i := range tk.Status.Outbox {
		if tk.Status.Outbox[i].ID == stale.ID {
			tk.Status.Outbox[i].Notify.Delivered = []string{"hook"}
		}
	}
	if err := e.store.UpdateTaskStatus(ctx, tk); err != nil {
		t.Fatal(err)
	}
	nfs, _ := e.m.cfg.Notifiers.ListNotifiers(ctx, ns)
	ack, _, err := e.m.notifyItem(ctx, key, stale, map[string]*v1alpha1.Notifier{"hook": &nfs[0]})
	if err != nil || ack || len(rec.deliveries()) != 0 {
		t.Fatalf("ack %v, %v, %d deliveries", ack, err, len(rec.deliveries()))
	}
	if items := notifyItems(e.get(t, key)); len(items) != 0 {
		t.Errorf("items %+v", items)
	}
}

// A dropped notification is told of only once the drop is written; the
// turn ends either way.
func TestNotifyDropIsToldAfterTheAck(t *testing.T) {
	bad := newReceiver(t)
	bad.answer(http.StatusBadGateway)
	e, recorded := notifyEnv(t, notifier("dead", bad.URL, nil, v1alpha1.NotifyQuestion))
	key := e.addTask(t, "x", v1alpha1.AssigneeAgent)
	e.question(t, key)
	e.clock.add(NotifyTTL + time.Minute)
	e.m.store = failAcks{e.store}
	_ = e.m.DeliverNotifications(ctx)
	for _, r := range *recorded {
		if strings.HasPrefix(r, "warn") {
			t.Errorf("told of a drop that did not happen: %s", r)
		}
	}
	if n := notifyItems(e.get(t, key))[0].Notify; n.SendingBy != "" {
		t.Errorf("the turn outlived a failed drop: %+v", n)
	}
	e.m.store = e.store
	if err := e.m.DeliverNotifications(ctx); err != nil {
		t.Fatal(err)
	}
	if last := (*recorded)[len(*recorded)-1]; !strings.HasPrefix(last, "warn team-a/x NotificationDropped") {
		t.Errorf("recorded %v", *recorded)
	}
}

// A server without Notifiers leaves alone a notification another server is
// sending.
func TestNotifyWithoutNotifiersHonoursATurn(t *testing.T) {
	rec := newReceiver(t)
	e, _ := notifyEnv(t, notifier("hook", rec.URL, nil, v1alpha1.NotifyQuestion))
	key := e.addTask(t, "x", v1alpha1.AssigneeAgent)
	e.question(t, key)
	id := notifyItems(e.get(t, key))[0].ID
	if _, err := e.m.transition(ctx, key, task.NotifyClaim{ID: id, By: "another server", Until: e.clock.now().Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	e.m.cfg.Notifiers = nil
	if err := e.m.DeliverNotifications(ctx); err != nil {
		t.Fatal(err)
	}
	if items := notifyItems(e.get(t, key)); len(items) != 1 {
		t.Errorf("acked under another server's turn: %+v", items)
	}
}

// fakeTools records the calls it is asked to make, and through which entry
// point: only notifications may come in through CallForNotification.
type fakeTools struct {
	mu      sync.Mutex
	calls   []ToolCall
	fail    bool
	notMade string
	viaTool int // calls through CallTool, the approved-call path
}

func (f *fakeTools) CallTool(context.Context, string, ToolCall) ToolResult {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.viaTool++
	return ToolResult{Body: "ok"}
}

func (f *fakeTools) CallForNotification(_ context.Context, _ string, call ToolCall) ToolResult {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
	if f.notMade != "" {
		return ToolResult{Body: "The call was not made.", IsError: true, NotMade: f.notMade}
	}
	return ToolResult{Body: "ok", IsError: f.fail}
}

func (f *fakeTools) CallTimeout() time.Duration { return time.Second }

// The notification goes under the argument notification, whatever the
// static arguments say; a tool answering with an error has not taken it.
func TestNotifyThroughATool(t *testing.T) {
	nf := notifier("chat", "", nil, v1alpha1.NotifyQuestion)
	nf.Spec.Webhook = nil
	nf.Spec.ToolServer = &v1alpha1.NotifierToolServer{Name: "chat", Tool: "post", Arguments: map[string]string{"channel": "#oncall", "notification": "spoofed"}}
	e, recorded := notifyEnv(t, nf)
	tools := &fakeTools{fail: true}
	e.m.cfg.Tools = tools
	key := e.addTask(t, "x", v1alpha1.AssigneeAgent)
	e.question(t, key)
	_ = e.m.DeliverNotifications(ctx)
	if len(notifyItems(e.get(t, key))) != 1 {
		t.Fatal("an error from the tool counted as delivered")
	}
	if last := (*recorded)[len(*recorded)-1]; last != "delivered chat: tool post on ToolServer chat answered with an error" {
		t.Errorf("recorded %q", last)
	}
	tools.fail = false
	if err := e.m.DeliverNotifications(ctx); err != nil {
		t.Fatal(err)
	}
	call := tools.calls[len(tools.calls)-1]
	var args struct {
		Notification Payload `json:"notification"`
		Channel      string  `json:"channel"`
	}
	if err := json.Unmarshal(call.Arguments, &args); err != nil {
		t.Fatalf("arguments %s: %v", call.Arguments, err)
	}
	if call.ToolServer != "chat" || call.Tool != "post" || args.Channel != "#oncall" || args.Notification.Event != v1alpha1.NotifyQuestion || args.Notification.Task.Name != "x" {
		t.Errorf("call %+v, arguments %s", call, call.Arguments)
	}
	if len(notifyItems(e.get(t, key))) != 0 {
		t.Error("delivered through the tool, the item stayed")
	}
	if tools.viaTool != 0 {
		t.Errorf("%d notification calls through the approved-call path", tools.viaTool)
	}
}

// A call never made says why, not that the tool erred.
func TestNotifyToolNotMade(t *testing.T) {
	nf := notifier("chat", "", nil, v1alpha1.NotifyQuestion)
	nf.Spec.Webhook, nf.Spec.ToolServer = nil, &v1alpha1.NotifierToolServer{Name: "chat", Tool: "post"}
	e, recorded := notifyEnv(t, nf)
	e.m.cfg.Tools = &fakeTools{notMade: NotMadeDenied}
	key := e.addTask(t, "x", v1alpha1.AssigneeAgent)
	e.question(t, key)
	_ = e.m.DeliverNotifications(ctx)
	if last := (*recorded)[len(*recorded)-1]; last != "delivered chat: tool post on ToolServer chat was not called: "+NotMadeDenied {
		t.Errorf("recorded %q", last)
	}
}

// A server that cannot call tools leaves a tool's notifications alone: no
// write, no attempt, no blame on the Notifier.
func TestNotifyWithoutTools(t *testing.T) {
	nf := notifier("chat", "", nil, v1alpha1.NotifyQuestion)
	nf.Spec.Webhook, nf.Spec.ToolServer = nil, &v1alpha1.NotifierToolServer{Name: "chat", Tool: "post"}
	e, recorded := notifyEnv(t, nf)
	key := e.addTask(t, "x", v1alpha1.AssigneeAgent)
	e.question(t, key)
	var writes int
	e.m.store = countWrites{e.store, &writes}
	_ = e.m.DeliverNotifications(ctx)
	if writes != 0 || len(*recorded) != 0 {
		t.Errorf("%d writes, recorded %v", writes, *recorded)
	}
}

// A notification dropped without ever being tried says so.
func TestNotifyDropUntried(t *testing.T) {
	nf := notifier("chat", "", nil, v1alpha1.NotifyQuestion)
	nf.Spec.Webhook, nf.Spec.ToolServer = nil, &v1alpha1.NotifierToolServer{Name: "chat", Tool: "post"}
	e, recorded := notifyEnv(t, nf)
	key := e.addTask(t, "x", v1alpha1.AssigneeAgent)
	e.question(t, key)
	e.clock.add(NotifyTTL + time.Minute)
	if err := e.m.DeliverNotifications(ctx); err != nil {
		t.Fatal(err)
	}
	if len(notifyItems(e.get(t, key))) != 0 {
		t.Fatal("not dropped")
	}
	if last := (*recorded)[len(*recorded)-1]; !strings.Contains(last, "without being tried: chat") {
		t.Errorf("recorded %q", last)
	}
}
