package api

import (
	"context"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/j75689/cadre/internal/manager"
	"github.com/j75689/cadre/internal/storage"
)

// Run tail defaults: a run's events are log appends, which no Notifier
// reports, so they are polled.
const (
	// MaxRunTails is how many streams of one replica watch a task's runs
	// at once, with a run or not; one more gets hello.runs false, and its
	// page polls the run.
	MaxRunTails    = 200
	defaultRunTail = 500 * time.Millisecond
)

// runTails are the Tasks whose run events a hub's streams watch (?task=),
// under the hub's mu.
type runTails struct {
	log   storage.Log // nil: no run hints
	max   int
	every time.Duration
	tasks map[storage.Key]*runTail
	subs  int // streams in tasks

	live atomic.Int64 // tailers running, for tests
}

// runTail is one Task's watchers, and the tailer of its current run, which
// they share.
type runTail struct {
	subs  map[*watcher]struct{}
	run   string             // the run tailed; "" for none
	known bool               // run is the Task's as it last read or changed
	stop  context.CancelFunc // ends the tailer; nil without a run
}

// TailRuns gives h's streams with ?task= a run hint when an event is
// appended to the task's current run, read from l every 500 ms. It follows
// the run through h's Notifier, so a hub without one has no run hints.
// Call it before serving.
func (h *Hub) TailRuns(l storage.Log) {
	if !h.tasks {
		return
	}
	// The Notifier may publish already.
	h.mu.Lock()
	defer h.mu.Unlock()
	h.runs = runTails{log: l, max: MaxRunTails, every: defaultRunTail, tasks: map[storage.Key]*runTail{}}
}

// tailRun adds w to key's watchers, unless the cap is reached. Its run is
// set by the first of followRun or a publish, so a change between the read
// and the publish is not lost.
func (h *Hub) tailRun(key storage.Key, w *watcher) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.runs.log == nil || h.runs.subs >= h.runs.max {
		return false
	}
	t := h.runs.tasks[key]
	if t == nil {
		t = &runTail{subs: map[*watcher]struct{}{}}
		h.runs.tasks[key] = t
	}
	t.subs[w] = struct{}{}
	h.runs.subs++
	return true
}

// followRun says which run, "" for none, the task had when a stream that
// tailRun added read it. A task without a run keeps its watchers, and
// publishTask tails the run it gets.
func (h *Hub) followRun(key storage.Key, run string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if t := h.runs.tasks[key]; !t.known {
		h.switchRun(key, t, run)
	}
}

// untailRun removes w from key's watchers, if it is one.
func (h *Hub) untailRun(key storage.Key, w *watcher) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if t := h.runs.tasks[key]; t != nil {
		if _, ok := t.subs[w]; ok {
			h.dropRun(key, t, w)
		}
	}
}

// dropRun removes w from t, and t's tailer with its last watcher. Call it
// with h.mu held.
func (h *Hub) dropRun(key storage.Key, t *runTail, w *watcher) {
	delete(t.subs, w)
	h.runs.subs--
	if len(t.subs) == 0 {
		h.switchRun(key, t, "")
		delete(h.runs.tasks, key)
	}
}

// switchRun has t tail run, stopping the tailer of the run before. Call it
// with h.mu held: it starts and stops goroutines, and does no I/O.
func (h *Hub) switchRun(key storage.Key, t *runTail, run string) {
	t.known = true
	if run == t.run {
		return
	}
	if t.stop != nil {
		t.stop()
		t.stop = nil
	}
	t.run = run
	if run != "" {
		ctx, stop := context.WithCancel(context.Background())
		t.stop = stop
		h.runs.live.Add(1)
		go h.tail(ctx, key, run)
	}
}

// tail lists run's events every interval until ctx ends, and hints key's
// watchers with the latest seq when there are new ones. The first listing
// hints too, so an event appended while a stream opened is not missed.
func (h *Hub) tail(ctx context.Context, key storage.Key, run string) {
	defer h.runs.live.Add(-1)
	tick := time.NewTicker(h.runs.every)
	defer tick.Stop()
	stream := manager.EventsStream(key, run)
	after := ""
	for {
		// Without h.mu: a slow store must hold up no publisher.
		keys, err := h.runs.log.Keys(ctx, stream, after, 0)
		if err == nil && len(keys) > 0 {
			after = keys[len(keys)-1].Key
			if seq, err := strconv.ParseInt(after, 10, 64); err == nil {
				h.publishRun(key, run, seq)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// publishRun hints key's watchers that run's events reached seq, unless
// the task has moved on from run.
func (h *Hub) publishRun(key storage.Key, run string, seq int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	t := h.runs.tasks[key]
	if t == nil || t.run != run {
		return
	}
	ev := watchEvent{kind: "run", run: RunHint{Task: key.Name, Run: run, Seq: seq}}
	for w := range t.subs {
		h.offer(w, ev)
	}
}
