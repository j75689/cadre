package manager

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	"github.com/j75689/cadre/internal/storage"
	"github.com/j75689/cadre/internal/task"
)

// Notifiers gives the Task Manager the Notifiers of a namespace and what
// delivering to them needs. Like Agents, they are CRDs, not storage-driver
// data.
type Notifiers interface {
	// ListNotifiers lists a namespace's Notifiers, possibly from a cache.
	ListNotifiers(ctx context.Context, namespace string) ([]v1alpha1.Notifier, error)
	// NotifierGone reports, from a fresh read, that a Notifier does not
	// exist: one missing from a cached list may be one the cache has not
	// seen yet.
	NotifierGone(ctx context.Context, namespace, name string) (bool, error)
	// SecretValue reads one Secret key, fresh, for a signing secret.
	SecretValue(ctx context.Context, namespace string, ref v1alpha1.SecretKeyRef) (string, error)
	// Delivered records a delivery on the Notifier's status, err nil for
	// one that succeeded. It is best effort: nothing depends on it.
	Delivered(ctx context.Context, n *v1alpha1.Notifier, at time.Time, err error)
	// Warn records a warning about a task where people look for one (an
	// Event on the Task, in a cluster).
	Warn(ctx context.Context, t *v1alpha1.Task, reason, message string)
}

// StaticNotifiers serves fixed Notifiers and Secrets, for tests and for
// drivers without a cluster, and keeps what is recorded.
type StaticNotifiers struct {
	Items   []v1alpha1.Notifier
	Secrets map[string]string // by "namespace/name/key"
	// Record, if set, receives deliveries and warnings.
	Record func(what string)
}

// ListNotifiers implements Notifiers.
func (s StaticNotifiers) ListNotifiers(_ context.Context, ns string) ([]v1alpha1.Notifier, error) {
	var out []v1alpha1.Notifier
	for _, n := range s.Items {
		if n.Namespace == ns {
			out = append(out, *n.DeepCopy())
		}
	}
	return out, nil
}

// NotifierGone implements Notifiers.
func (s StaticNotifiers) NotifierGone(_ context.Context, ns, name string) (bool, error) {
	return !slices.ContainsFunc(s.Items, func(n v1alpha1.Notifier) bool { return n.Namespace == ns && n.Name == name }), nil
}

// SecretValue implements Notifiers.
func (s StaticNotifiers) SecretValue(_ context.Context, ns string, ref v1alpha1.SecretKeyRef) (string, error) {
	if v, ok := s.Secrets[ns+"/"+ref.Name+"/"+ref.Key]; ok {
		return v, nil
	}
	return "", fmt.Errorf("secret %s/%s key %s: %w", ns, ref.Name, ref.Key, storage.ErrNotFound)
}

// Delivered implements Notifiers.
func (s StaticNotifiers) Delivered(_ context.Context, n *v1alpha1.Notifier, _ time.Time, err error) {
	if s.Record != nil {
		s.Record(fmt.Sprintf("delivered %s: %v", n.Name, err))
	}
}

// Warn implements Notifiers.
func (s StaticNotifiers) Warn(_ context.Context, t *v1alpha1.Task, reason, message string) {
	if s.Record != nil {
		s.Record(fmt.Sprintf("warn %s/%s %s: %s", t.Namespace, t.Name, reason, message))
	}
}

// notifyEvent is what a transition from before to after tells: entering
// Waiting, for its reason, entering Review (how a run hands its work to a
// human), or ending. "" for anything else.
func notifyEvent(before, after *v1alpha1.TaskStatus) v1alpha1.NotifyEvent {
	switch after.Phase {
	case v1alpha1.PhaseWaiting:
		w := after.Waiting
		if w == nil || (before.Phase == v1alpha1.PhaseWaiting && before.Waiting != nil && before.Waiting.Since.Equal(&w.Since) && before.Waiting.Reason == w.Reason) {
			return ""
		}
		return v1alpha1.NotifyEvent(w.Reason)
	case v1alpha1.PhaseReview:
		if before.Phase == after.Phase {
			return ""
		}
		return v1alpha1.NotifyReview
	case v1alpha1.PhaseDone, v1alpha1.PhaseFailed, v1alpha1.PhaseCancelled:
		if before.Phase == after.Phase {
			return ""
		}
		return v1alpha1.NotifyEvent(after.Phase)
	}
	return ""
}

// addNotify adds a Notify item to next, the status a transition of t is
// about to write, when the transition tells something a Notifier of t's
// namespace listens for, naming those Notifiers: the item goes to them and
// no others. No Notifier listening, no item: the write is the same as
// without Notifiers. Notifiers that cannot be listed cost the notification,
// never the transition.
func (m *Manager) addNotify(ctx context.Context, t *v1alpha1.Task, next *v1alpha1.TaskStatus) {
	ev := notifyEvent(&t.Status, next)
	if ev == "" || m.cfg.Notifiers == nil {
		return
	}
	notifiers, err := m.cfg.Notifiers.ListNotifiers(ctx, t.Namespace)
	if err != nil {
		m.log.Warn("cannot list Notifiers: the notification is not sent", "task", t.Namespace+"/"+t.Name, "event", ev, "err", err)
		return
	}
	var to []string
	for _, n := range notifiers {
		if n.Listens(ev) {
			to = append(to, n.Name)
		}
	}
	if len(to) == 0 {
		return
	}
	slices.Sort(to)
	n := &v1alpha1.Notification{Event: ev, At: metav1.NewTime(m.now()), To: to}
	if w := next.Waiting; next.Phase == v1alpha1.PhaseWaiting && w != nil {
		n.ThreadSeq, n.WaitingItem = w.ThreadSeq, w.ItemID
	}
	next.Outbox = append(next.Outbox, v1alpha1.OutboxItem{ID: m.newID(), Kind: v1alpha1.OutboxNotify, Notify: n})
}

// NotifyTTL bounds how long a notification is retried: past it, the item
// is dropped with a warning naming the Notifiers that never took it, so a
// dead webhook does not pin a task's outbox.
const NotifyTTL = 24 * time.Hour

// notifyTimeout bounds one delivery.
const notifyTimeout = 10 * time.Second

// notifyTurn is how long a server has a notification for n Notifiers to
// itself: as many rounds of deliveries as they take, plus one.
func notifyTurn(n int) time.Duration {
	return time.Duration((n+notifyDelivers-1)/notifyDelivers+1) * notifyTimeout
}

// How many tasks' notifications are sent at once, and how many deliveries
// of one notification.
const (
	notifyTasks    = 8
	notifyDelivers = 4
)

// Payload is the JSON a webhook receives.
type Payload struct {
	ID        string               `json:"id"`
	Event     v1alpha1.NotifyEvent `json:"event"`
	At        time.Time            `json:"at"`
	ThreadSeq int64                `json:"threadSeq,omitempty"`
	Task      PayloadTask          `json:"task"`
	URL       string               `json:"url,omitempty"`
}

// PayloadTask is the task a notification is about.
type PayloadTask struct {
	Namespace string             `json:"namespace"`
	Name      string             `json:"name"`
	Number    int64              `json:"number,omitempty"`
	Title     string             `json:"title"`
	Phase     v1alpha1.Phase     `json:"phase"`
	Assignee  *v1alpha1.Assignee `json:"assignee,omitempty"`
}

// DeliverNotifications sends every pending notification. It runs on its own
// ticker, apart from the dispatcher and the reclaim, so a webhook that does
// not answer delays nothing else: tasks are worked a few at a time, and a
// notification's deliveries at once.
func (m *Manager) DeliverNotifications(ctx context.Context) error {
	return m.forEachTeam(ctx, func(ns string) error {
		tasks, err := m.store.ListTasks(ctx, storage.Query{Namespace: ns})
		if err != nil {
			return err
		}
		sem := make(chan struct{}, notifyTasks)
		var mu sync.Mutex
		var wg sync.WaitGroup
		var errs []error
		for _, t := range tasks {
			if !slices.ContainsFunc(t.Status.Outbox, func(it v1alpha1.OutboxItem) bool { return it.Kind == v1alpha1.OutboxNotify }) {
				continue
			}
			key := storage.Key{Namespace: ns, Name: t.Name}
			sem <- struct{}{}
			wg.Go(func() {
				defer func() { <-sem }()
				if err := m.notifyTask(ctx, key); err != nil {
					mu.Lock()
					errs = append(errs, err)
					mu.Unlock()
				}
			})
		}
		wg.Wait()
		return errors.Join(errs...)
	})
}

// notifyTask sends a task's pending notifications and acks the ones
// notifyItem says to.
// A server without Notifiers has nobody to tell: it acks them.
func (m *Manager) notifyTask(ctx context.Context, key storage.Key) error {
	t, err := m.store.GetTask(ctx, key)
	if errors.Is(err, storage.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if m.cfg.Notifiers == nil {
		var ids []string
		for _, it := range t.Status.Outbox {
			// Another server sending it now finishes it.
			if it.Kind == v1alpha1.OutboxNotify && !m.sendingElsewhere(it.Notify) {
				ids = append(ids, it.ID)
			}
		}
		if len(ids) == 0 {
			return nil
		}
		_, err := m.transition(ctx, key, task.AckOutbox{IDs: ids})
		return err
	}
	all, err := m.cfg.Notifiers.ListNotifiers(ctx, key.Namespace)
	if err != nil {
		return err
	}
	byName := map[string]*v1alpha1.Notifier{}
	for i := range all {
		byName[all[i].Name] = &all[i]
	}
	var ack []string
	var dropped []func()
	var errs []error
	for _, it := range t.Status.Outbox {
		if it.Kind != v1alpha1.OutboxNotify || it.Notify == nil {
			continue
		}
		toAck, drop, err := m.notifyItem(ctx, key, it, byName)
		if err != nil {
			errs = append(errs, fmt.Errorf("notification %s: %w", it.ID, err))
		}
		if toAck {
			ack = append(ack, it.ID)
		}
		if drop != nil {
			dropped = append(dropped, drop)
		}
	}
	if len(ack) > 0 {
		if _, err := m.transition(ctx, key, task.AckOutbox{IDs: ack}); err != nil {
			return errors.Join(append(errs, err)...)
		}
		// A dropped notification is told of once it is gone for good.
		for _, warn := range dropped {
			warn()
		}
	}
	return errors.Join(errs...)
}

// sendingElsewhere reports whether another server holds n's turn now.
func (m *Manager) sendingElsewhere(n *v1alpha1.Notification) bool {
	return n != nil && n.SendingBy != "" && n.SendingBy != m.id && n.SendingUntil != nil && n.SendingUntil.After(m.now())
}

// notifyItem sends one notification to those of its Notifiers that have
// not taken it; a Notifier that no longer exists counts as having taken
// it, once a fresh read says so (confirmGone). It reports whether the item
// is to be acked: it was found with every Notifier having taken it, or it
// outlived NotifyTTL, in which case drop says so once it is. An item every
// Notifier took is removed by the very write that records it
// (task.NotifyDelivered), and needs no ack.
func (m *Manager) notifyItem(ctx context.Context, key storage.Key, it v1alpha1.OutboxItem, byName map[string]*v1alpha1.Notifier) (ack bool, drop func(), err error) {
	// The read that found it decides whether there is anything to try:
	// nothing pending, or nobody pending Ready, costs no write. Notifiers
	// missing from the cached list are confirmed with a fresh read, once
	// each (confirm), before they count as gone; while one is not, and the
	// item has not expired, nothing is written either.
	confirmed := map[string]bool{}
	confirm := func(n *v1alpha1.Notification) (gone, unseen []string) {
		for _, name := range goneOf(n, byName) {
			if _, ok := confirmed[name]; !ok {
				confirmed[name] = m.confirmGone(ctx, key.Namespace, name)
			}
			if confirmed[name] {
				gone = append(gone, name)
			} else {
				unseen = append(unseen, name)
			}
		}
		return gone, unseen
	}
	expired := func(n *v1alpha1.Notification) bool { return m.now().Sub(n.At.Time) > NotifyTTL }
	pending := pendingOf(it.Notify, byName)
	switch {
	case len(pending) == 0 && len(goneOf(it.Notify, byName)) == 0:
		return true, nil, nil
	case len(pending) == 0:
		if _, unseen := confirm(it.Notify); len(unseen) > 0 && !expired(it.Notify) {
			return false, nil, nil
		}
	case !slices.ContainsFunc(pending, m.canTry) && !expired(it.Notify):
		return false, nil, nil
	}
	// One server at a time: the claim is a conditional write on the Task,
	// for as long as delivering to every pending Notifier can take. What is
	// pending, and how old the item is, are read again from the task the
	// claim returns: another server may have finished it, or part of it,
	// since the read that found it.
	t, err := m.transition(ctx, key, task.NotifyClaim{ID: it.ID, By: m.id, Until: m.now().Add(notifyTurn(len(it.Notify.To)))})
	switch {
	case errors.Is(err, task.ErrNotifySending), errors.Is(err, task.ErrNotifyGone):
		return false, nil, nil
	case err != nil:
		return false, nil, err
	}
	// The claim found the item, so it is in the task the claim wrote.
	it = t.Status.Outbox[slices.IndexFunc(t.Status.Outbox, func(o v1alpha1.OutboxItem) bool { return o.ID == it.ID })]
	n := it.Notify
	gone, unseen := confirm(n)
	endTurn := func() error {
		_, err := m.transition(ctx, key, task.NotifyDelivered{ID: it.ID, By: m.id, Gone: gone})
		return err
	}
	pending = pendingOf(n, byName)
	if len(pending) == 0 && (len(unseen) == 0 || !expired(n)) {
		// Taken by all since the read that found it, or gone, and ending
		// the turn removes it; or left only to Notifiers this server's
		// cache has not seen yet, which a later pass sends to.
		return false, nil, endTurn()
	}
	if expired(n) {
		var names []string
		for _, nf := range pending {
			names = append(names, nf.Name)
		}
		names = append(names, unseen...)
		msg := fmt.Sprintf("the %s notification %s was not taken within %s by: %s", n.Event, it.ID, NotifyTTL, strings.Join(names, ", "))
		// Said as it is when nothing was ever tried.
		if !slices.ContainsFunc(pending, m.canTry) {
			var why []string
			if len(pending) > 0 {
				why = append(why, fmt.Sprintf("%s were not Ready, or this server calls no ToolServers for them", strings.Join(names[:len(pending)], ", ")))
			}
			if len(unseen) > 0 {
				why = append(why, fmt.Sprintf("%s were not in this server's cache of Notifiers, nor confirmed deleted", strings.Join(unseen, ", ")))
			}
			msg = fmt.Sprintf("the %s notification %s was dropped after %s without being tried: %s", n.Event, it.ID, NotifyTTL, strings.Join(why, "; "))
		}
		if err := endTurn(); err != nil {
			return false, nil, err
		}
		return true, func() { m.cfg.Notifiers.Warn(ctx, t, "NotificationDropped", msg) }, nil
	}
	body, err := json.Marshal(m.payload(t, it))
	if err != nil {
		return false, nil, err
	}
	sem := make(chan struct{}, notifyDelivers)
	var mu sync.Mutex
	var wg sync.WaitGroup
	var took []string
	var errs []error
	for _, nf := range pending {
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem }()
			err := m.deliver(ctx, nf, it.ID, body)
			if !errors.Is(err, errNotReady) && !errors.Is(err, errNoCaller) {
				m.cfg.Notifiers.Delivered(ctx, nf, m.now(), err)
			}
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, fmt.Errorf("notifier %s: %w", nf.Name, err))
				return
			}
			took = append(took, nf.Name)
		})
	}
	wg.Wait()
	// Record who took it, and end this server's turn; once all have, the
	// same write removes the item. A write that fails records nothing, and
	// the next pass sends to them again.
	if _, err := m.transition(ctx, key, task.NotifyDelivered{ID: it.ID, To: took, By: m.id, Gone: gone}); err != nil {
		return false, nil, errors.Join(append(errs, err)...)
	}
	return false, nil, errors.Join(errs...)
}

// confirmGone reports whether a Notifier missing from the cached list is
// gone, by a fresh read: one the cache has not seen yet is not, and a later
// pass sends to it. A Notifier that cannot be read is not gone either; the
// error is logged, as it costs this pass no more than that Notifier.
func (m *Manager) confirmGone(ctx context.Context, ns, name string) bool {
	gone, err := m.cfg.Notifiers.NotifierGone(ctx, ns, name)
	if err != nil {
		m.log.Warn("cannot read a Notifier: it is not counted as deleted", "notifier", ns+"/"+name, "err", err)
		return false
	}
	return gone
}

// goneOf is the Notifiers of n missing from byName that have not taken it:
// gone, or not yet seen (confirmGone).
func goneOf(n *v1alpha1.Notification, byName map[string]*v1alpha1.Notifier) []string {
	var out []string
	for _, name := range n.To {
		if _, ok := byName[name]; !ok && !slices.Contains(n.Delivered, name) {
			out = append(out, name)
		}
	}
	return out
}

// pendingOf is the Notifiers of n that exist and have not taken it.
func pendingOf(n *v1alpha1.Notification, byName map[string]*v1alpha1.Notifier) []*v1alpha1.Notifier {
	var out []*v1alpha1.Notifier
	for _, name := range n.To {
		if nf, ok := byName[name]; ok && !slices.Contains(n.Delivered, name) {
			out = append(out, nf)
		}
	}
	return out
}

func (m *Manager) payload(t *v1alpha1.Task, it v1alpha1.OutboxItem) Payload {
	n := it.Notify
	seq := n.ThreadSeq
	// A Question's entry may reach the thread after the wait began: its
	// sequence is the wait's, as long as the task still waits on it.
	if w := t.Status.Waiting; seq == 0 && n.WaitingItem != "" && w != nil && t.Status.Phase == v1alpha1.PhaseWaiting && w.ItemID == n.WaitingItem {
		seq = w.ThreadSeq
	}
	p := Payload{
		ID: it.ID, Event: n.Event, At: n.At.UTC(), ThreadSeq: seq,
		Task: PayloadTask{Namespace: t.Namespace, Name: t.Name, Number: t.Status.Number, Title: t.Spec.Title, Phase: t.Status.Phase, Assignee: t.Spec.Assignee},
	}
	if m.cfg.ExternalURL != "" {
		p.URL = strings.TrimRight(m.cfg.ExternalURL, "/") + "/t/" + t.Namespace + "/tasks/" + t.Name
	}
	return p
}

// Deliveries not attempted: the Notifier is not Ready, or this server
// has no caller for its tool, which is not the Notifier's fault. Neither is
// recorded on the Notifier.
var (
	errNotReady = errors.New("the Notifier is not Ready")
	errNoCaller = errors.New("this server calls no ToolServers for notifications")
)

// untried says why nf is not tried from this server, nil if it can be.
func (m *Manager) untried(nf *v1alpha1.Notifier) error {
	if !nf.Status.ReadyFor(nf.Generation) {
		return errNotReady
	}
	if _, ok := m.cfg.Tools.(NotificationCaller); nf.Spec.ToolServer != nil && !ok {
		return errNoCaller
	}
	return nil
}

// canTry reports whether nf can be delivered to from this server.
func (m *Manager) canTry(nf *v1alpha1.Notifier) bool { return m.untried(nf) == nil }

// deliver POSTs body to nf's webhook: signed if it has a secret, never
// following a redirect, within notifyTimeout. Only a 2xx is a delivery.
// Errors name the webhook without any credentials in its URL.
func (m *Manager) deliver(ctx context.Context, nf *v1alpha1.Notifier, id string, body []byte) error {
	if err := m.untried(nf); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, notifyTimeout)
	defer cancel()
	if nf.Spec.ToolServer != nil {
		return m.notifyTool(ctx, nf, m.cfg.Tools.(NotificationCaller), body) // untried checked it
	}
	wh := nf.Spec.Webhook
	if wh == nil {
		return errors.New("the Notifier has no channel")
	}
	shown := redact(wh.URL)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, wh.URL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("webhook %s: not a URL", shown)
	}
	for k, v := range wh.Headers {
		req.Header.Set(k, v)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", id)
	req.Header.Set("User-Agent", "cadre-server/"+m.cfg.Version)
	if wh.SecretRef != nil {
		secret, err := m.cfg.Notifiers.SecretValue(ctx, nf.Namespace, *wh.SecretRef)
		if err != nil {
			return fmt.Errorf("the signing secret: %w", err)
		}
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write(body)
		req.Header.Set("X-Cadre-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	}
	res, err := notifyClient.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			return fmt.Errorf("%s %s: %w", ue.Op, shown, ue.Err)
		}
		return err
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 64<<10))
		_ = res.Body.Close()
	}()
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return fmt.Errorf("%s answered %s", shown, res.Status)
	}
	return nil
}

// notifyTool calls nf's tool through the forwarder's notification entry
// point, which calls only a ToolServer that takes notifications and a tool
// its permissions allow, with the notification under the argument
// notification and nf's static arguments beside it. A call not made, or
// one the tool answers with an error, is not a delivery; the error names
// the ToolServer, the tool and why a call was not made, never the
// arguments nor what the tool answered.
func (m *Manager) notifyTool(ctx context.Context, nf *v1alpha1.Notifier, caller NotificationCaller, body []byte) error {
	ts := nf.Spec.ToolServer
	args := map[string]any{}
	for k, v := range ts.Arguments {
		args[k] = v
	}
	args["notification"] = json.RawMessage(body)
	raw, err := json.Marshal(args)
	if err != nil {
		return err
	}
	res := caller.CallForNotification(ctx, nf.Namespace, ToolCall{ToolServer: ts.Name, Tool: ts.Tool, Arguments: raw})
	switch {
	case res.NotMade != "":
		return fmt.Errorf("tool %s on ToolServer %s was not called: %s", ts.Tool, ts.Name, res.NotMade)
	case res.IsError:
		return fmt.Errorf("tool %s on ToolServer %s answered with an error", ts.Tool, ts.Name)
	}
	return nil
}

// redact is a webhook URL fit to show: without user info or query.
func redact(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "the webhook"
	}
	u.User, u.RawQuery, u.Fragment = nil, "", ""
	return u.String()
}

// notifyClient follows no redirect: a webhook that moved says so with an
// error, rather than the notification going wherever it points.
var notifyClient = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
