package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	"github.com/j75689/cadre/internal/authz"
	"github.com/j75689/cadre/internal/httpjson"
	"github.com/j75689/cadre/internal/storage"
)

// Watch stream defaults: see WatchConfig.
const (
	DefaultWatchMaxStreams = 2000
	defaultWatchPing       = 20 * time.Second
	defaultWatchLifetime   = 300 * time.Second
	defaultWatchCoalesce   = 250 * time.Millisecond

	// watchBuffer is how many events wait for one stream before it is told
	// to resync instead.
	watchBuffer = 256
	// watchWriteTimeout bounds each write to a stream, in place of the
	// server's WriteTimeout, which would end it.
	watchWriteTimeout = 30 * time.Second
	// watchRetryMillis is how long a browser waits to reconnect.
	watchRetryMillis = 3000
)

// Reasons an end event gives.
const (
	EndLifetime = "lifetime"
	EndShutdown = "shutdown"
	EndNoRole   = "no_role"
)

// WatchConfig bounds the watch streams of one replica. A zero field takes
// its default.
type WatchConfig struct {
	MaxStreams int           // streams open at once; one more is answered 503 (2000)
	Ping       time.Duration // between pings, each of which checks the role again (20s)
	Lifetime   time.Duration // of a stream, ±20 %, before it ends and the browser reconnects (300s)
	Coalesce   time.Duration // a task's changes within it are one hint (250ms)
}

type watchServer struct {
	hub *Hub
	WatchConfig
}

// WithWatch serves GET .../namespaces/{ns}/watch, the stream of hints that
// a namespace's board changed, from h.
func WithWatch(h *Hub, c WatchConfig) Option {
	def := func(d *time.Duration, v time.Duration) {
		if *d <= 0 {
			*d = v
		}
	}
	if c.MaxStreams <= 0 {
		c.MaxStreams = DefaultWatchMaxStreams
	}
	def(&c.Ping, defaultWatchPing)
	def(&c.Lifetime, defaultWatchLifetime)
	def(&c.Coalesce, defaultWatchCoalesce)
	return func(s *Server) { s.watch = &watchServer{hub: h, WatchConfig: c} }
}

// IsWatch reports whether r opens a watch stream, which outlives the
// request bound and sets its own.
func IsWatch(r *http.Request) bool {
	rest, ok := strings.CutPrefix(r.URL.Path, Base+"/namespaces/")
	ns, tail, _ := strings.Cut(rest, "/")
	get := r.Method == http.MethodGet || r.Method == http.MethodHead // GET serves both
	return get && ok && ns != "" && tail == "watch"
}

// Hub hands each watch stream the hints for its namespace. There is one per
// server, fed by the store's Notifier. Hints are lossy: a stream that falls
// behind is told to resync, and a browser that reconnects reads its board
// again.
type Hub struct {
	tasks bool // a Notifier feeds it

	mu      sync.Mutex
	subs    map[string]map[*watcher]struct{} // by namespace
	streams int
	// digests are the board-visible fields of every Task seen, so a change
	// the board does not show (a lease renewed) is no hint.
	digests map[storage.Key]uint64

	done      chan struct{}
	closeOnce sync.Once
}

type watcher struct{ events chan watchEvent }

type watchEvent struct {
	kind string // "task" or "resync"
	task TaskHint
}

// NewHub returns a hub fed by n; with a nil n it has no task hints, and its
// streams say so.
func NewHub(n storage.Notifier) *Hub {
	h := &Hub{tasks: n != nil, subs: map[string]map[*watcher]struct{}{}, digests: map[storage.Key]uint64{}, done: make(chan struct{})}
	if n != nil {
		n.NotifyTasks(h.publishTask)
	}
	return h
}

// Close ends every stream, now and to come. Call it as the server shuts
// down: its shutdown waits for handlers, and a stream's would not end.
func (h *Hub) Close() { h.closeOnce.Do(func() { close(h.done) }) }

// publishTask hints t's namespace's streams that t changed, if it changed
// in a way the board shows. It never blocks: a stream whose buffer is full
// loses what it holds and gets one resync instead.
func (h *Hub) publishTask(t *v1alpha1.Task) {
	key := storage.Key{Namespace: t.Namespace, Name: t.Name}
	d := boardDigest(t)
	h.mu.Lock()
	defer h.mu.Unlock()
	if old, ok := h.digests[key]; ok && old == d {
		return
	}
	h.digests[key] = d
	ev := watchEvent{kind: "task", task: TaskHint{Name: t.Name, Number: t.Status.Number, Phase: t.Status.Phase}}
	if t.Spec.ParentRef != nil {
		ev.task.Parent = t.Spec.ParentRef.Name
	}
	for w := range h.subs[t.Namespace] {
		select {
		case w.events <- ev:
		default:
			// The stream reads too, so a receive could wait on an
			// emptied channel: drain without blocking.
		drain:
			for {
				select {
				case <-w.events:
				default:
					break drain
				}
			}
			// Only publishers send, under h.mu: there is room now.
			w.events <- watchEvent{kind: "resync"}
		}
	}
}

// boardDigest hashes what a board shows of t.
func boardDigest(t *v1alpha1.Task) uint64 {
	f := fnv.New64a()
	field := func(s string) {
		_, _ = f.Write([]byte(s))
		_, _ = f.Write([]byte{0})
	}
	field(string(t.Status.Phase))
	field(string(t.Spec.Priority))
	field(t.Spec.Title)
	// An absent field hashes as empty, so the next one keeps its place.
	var assignee, waiting, parent, revision, run string
	if a := t.Spec.Assignee; a != nil {
		assignee = string(a.Kind) + "/" + a.Name
	}
	field(assignee)
	field(strings.Join(t.Spec.Labels, "\x01"))
	if w := t.Status.Waiting; w != nil {
		waiting = string(w.Reason)
	}
	field(waiting)
	field(strconv.FormatInt(t.Status.Number, 10))
	if p := t.Spec.ParentRef; p != nil {
		parent = p.Name
	}
	field(parent)
	if th := t.Status.Thread; th != nil {
		revision = strconv.FormatInt(th.Revision, 10)
	}
	field(revision)
	if r := t.Status.Run; r != nil {
		run = r.ID
	}
	field(run)
	return f.Sum64()
}

// subscribe adds a stream on ns, unless limit are open already.
func (h *Hub) subscribe(ns string, limit int) (*watcher, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.streams >= limit {
		return nil, false
	}
	h.streams++
	w := &watcher{events: make(chan watchEvent, watchBuffer)}
	if h.subs[ns] == nil {
		h.subs[ns] = map[*watcher]struct{}{}
	}
	h.subs[ns][w] = struct{}{}
	return w, true
}

func (h *Hub) unsubscribe(ns string, w *watcher) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.streams--
	delete(h.subs[ns], w)
	if len(h.subs[ns]) == 0 {
		delete(h.subs, ns)
	}
}

// watchStream serves one stream: hello, then hints as they come, a ping
// that checks the caller's role again, and end before it closes. ?task= is
// for run tailing, which is not served yet.
func (s *Server) watchStream(w http.ResponseWriter, r *http.Request) {
	c := s.watch
	ns := r.PathValue("ns")
	sub, ok := c.hub.subscribe(ns, c.MaxStreams)
	if !ok {
		httpjson.WriteError(w, http.StatusServiceUnavailable, httpjson.CodeUnavailable,
			fmt.Sprintf("this replica serves %d watch streams already: retry later; the board polls meanwhile", c.MaxStreams))
		return
	}
	defer c.hub.unsubscribe(ns, sub)

	rc := http.NewResponseController(w)
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	write := func(format string, args ...any) bool {
		_ = rc.SetWriteDeadline(time.Now().Add(watchWriteTimeout))
		if _, err := fmt.Fprintf(w, format, args...); err != nil {
			return false
		}
		return rc.Flush() == nil
	}
	send := func(kind string, data any) bool {
		b, _ := json.Marshal(data)
		return write("event: %s\ndata: %s\n\n", kind, b)
	}

	hello := WatchHello{Tasks: c.hub.tasks, LifetimeSeconds: int(c.Lifetime / time.Second)}
	if !write("retry: %d\n", watchRetryMillis) || !send("hello", hello) {
		return
	}
	lifetime := time.NewTimer(time.Duration(float64(c.Lifetime) * (0.8 + 0.4*rand.Float64())))
	defer lifetime.Stop()
	ping := time.NewTicker(c.Ping)
	defer ping.Stop()
	// A task's hints wait for the flush, so a burst of changes to it is one.
	var (
		order   []string
		pending = map[string]TaskHint{}
		flush   <-chan time.Time
	)
	for {
		select {
		case <-r.Context().Done():
			return
		case <-c.hub.done:
			send("end", WatchEnd{Reason: EndShutdown})
			return
		case <-lifetime.C:
			send("end", WatchEnd{Reason: EndLifetime})
			return
		case <-ping.C:
			var refused *Refused
			if err := Check(r.Context(), s.m, s.policy, ns, authz.Read); errors.As(err, &refused) {
				send("end", WatchEnd{Reason: EndNoRole})
				return
			}
			if !write(": ping\n\n") {
				return
			}
		case ev := <-sub.events:
			if ev.kind == "resync" {
				order, pending, flush = nil, map[string]TaskHint{}, nil
				if !send("resync", struct{}{}) {
					return
				}
				continue
			}
			if _, ok := pending[ev.task.Name]; !ok {
				order = append(order, ev.task.Name)
			}
			pending[ev.task.Name] = ev.task
			if flush == nil {
				flush = time.After(c.Coalesce)
			}
		case <-flush:
			for _, name := range order {
				if !send("task", pending[name]) {
					return
				}
			}
			order, pending, flush = nil, map[string]TaskHint{}, nil
		}
	}
}
