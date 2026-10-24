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
	"slices"
	"strings"
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
// Waiting, for its reason, or ending. "" for anything else.
func notifyEvent(before, after *v1alpha1.TaskStatus) v1alpha1.NotifyEvent {
	switch after.Phase {
	case v1alpha1.PhaseWaiting:
		w := after.Waiting
		if w == nil || (before.Phase == v1alpha1.PhaseWaiting && before.Waiting != nil && before.Waiting.Since.Equal(&w.Since) && before.Waiting.Reason == w.Reason) {
			return ""
		}
		return v1alpha1.NotifyEvent(w.Reason)
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
// namespace listens for. No Notifier listening, no item: the write is the
// same as without Notifiers.
func (m *Manager) addNotify(ctx context.Context, t *v1alpha1.Task, next *v1alpha1.TaskStatus) error {
	ev := notifyEvent(&t.Status, next)
	if ev == "" || m.cfg.Notifiers == nil {
		return nil
	}
	notifiers, err := m.cfg.Notifiers.ListNotifiers(ctx, t.Namespace)
	if err != nil {
		return err
	}
	if !slices.ContainsFunc(notifiers, func(n v1alpha1.Notifier) bool { return n.Listens(ev) }) {
		return nil
	}
	n := &v1alpha1.Notification{Event: ev, At: metav1.NewTime(m.now())}
	if next.Waiting != nil {
		n.ThreadSeq = next.Waiting.ThreadSeq
	}
	next.Outbox = append(next.Outbox, v1alpha1.OutboxItem{ID: m.newID(), Kind: v1alpha1.OutboxNotify, Notify: n})
	return nil
}

// NotifyTTL bounds how long a notification is retried: past it, the item
// is dropped with a warning naming the Notifiers that never took it, so a
// dead webhook does not pin a task's outbox.
const NotifyTTL = 24 * time.Hour

// notifyTimeout bounds one delivery.
const notifyTimeout = 10 * time.Second

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

// undelivered is a Notify item some Notifiers took and others did not yet.
type undelivered struct {
	took []string
	err  error
}

func (u *undelivered) Error() string { return u.err.Error() }
func (u *undelivered) Unwrap() error { return u.err }

// applyNotify delivers a Notify item to every Notifier listening for its
// event that has not taken it. It succeeds once all have, or once the item
// is older than NotifyTTL; otherwise it returns an *undelivered naming the
// ones that took it this time.
func (m *Manager) applyNotify(ctx context.Context, t *v1alpha1.Task, it v1alpha1.OutboxItem) error {
	n := it.Notify
	all, err := m.cfg.Notifiers.ListNotifiers(ctx, t.Namespace)
	if err != nil {
		return err
	}
	var pending []v1alpha1.Notifier
	for _, nf := range all {
		if nf.Listens(n.Event) && !slices.Contains(n.Delivered, nf.Name) {
			pending = append(pending, nf)
		}
	}
	if len(pending) == 0 {
		return nil
	}
	// Notify.At is the item's ULID time, on the manager's clock.
	if m.now().Sub(n.At.Time) > NotifyTTL {
		names := make([]string, len(pending))
		for i := range pending {
			names[i] = pending[i].Name
		}
		m.cfg.Notifiers.Warn(ctx, t, "NotificationDropped", fmt.Sprintf("the %s notification %s was not taken within %s by: %s", n.Event, it.ID, NotifyTTL, strings.Join(names, ", ")))
		return nil
	}
	body, err := json.Marshal(m.payload(t, it))
	if err != nil {
		return err
	}
	var took []string
	var errs []error
	for i := range pending {
		nf := &pending[i]
		err := m.deliver(ctx, nf, it.ID, body)
		m.cfg.Notifiers.Delivered(ctx, nf, m.now(), err)
		if err != nil {
			errs = append(errs, fmt.Errorf("notifier %s: %w", nf.Name, err))
			continue
		}
		took = append(took, nf.Name)
	}
	if len(errs) == 0 {
		return nil
	}
	return &undelivered{took: took, err: errors.Join(errs...)}
}

func (m *Manager) payload(t *v1alpha1.Task, it v1alpha1.OutboxItem) Payload {
	n := it.Notify
	seq := n.ThreadSeq
	// A Question's entry may reach the thread after the wait began.
	if w := t.Status.Waiting; seq == 0 && w != nil && t.Status.Phase == v1alpha1.PhaseWaiting && v1alpha1.NotifyEvent(w.Reason) == n.Event {
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

// deliver POSTs body to nf's webhook: signed if it has a secret, never
// following a redirect, within notifyTimeout. Only a 2xx is a delivery.
func (m *Manager) deliver(ctx context.Context, nf *v1alpha1.Notifier, id string, body []byte) error {
	if !nf.Status.ReadyFor(nf.Generation) {
		return errors.New("the Notifier is not Ready")
	}
	ctx, cancel := context.WithTimeout(ctx, notifyTimeout)
	defer cancel()
	wh := nf.Spec.Webhook
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, wh.URL, bytes.NewReader(body))
	if err != nil {
		return err
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
		return err
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 64<<10))
		_ = res.Body.Close()
	}()
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return fmt.Errorf("%s answered %s", wh.URL, res.Status)
	}
	return nil
}

// notifyClient follows no redirect: a webhook that moved says so with an
// error, rather than the notification going wherever it points.
var notifyClient = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

// markDelivered records the Notifiers that took Notify items.
func (m *Manager) markDelivered(ctx context.Context, key storage.Key, took map[string][]string) error {
	var errs []error
	for id, names := range took {
		if _, err := m.transition(ctx, key, task.NotifyDelivered{ID: id, To: names}); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
