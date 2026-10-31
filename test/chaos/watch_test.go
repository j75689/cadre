package chaos_test

import (
	"bufio"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	"github.com/j75689/cadre/internal/api"
	"github.com/j75689/cadre/internal/client"
	"github.com/j75689/cadre/internal/httpjson"
	"github.com/j75689/cadre/internal/manager"
	"github.com/j75689/cadre/internal/storage"
	"github.com/j75689/cadre/internal/storage/kubernetes"
)

// Browsers watch the boards as the web UI does (web/src/live.ts): each
// keeps a watch stream open, reads again on a hint, a resync or a hello,
// reconnects after the stream's retry when it ends or drops, and polls
// while it is refused. Once the system quiesces every browser must show
// what the store holds, and no stream may outlive its instance's kill.

const (
	boardWatchers = 2 // per namespace, besides the one that follows a task
	// pollEvery is how often a refused browser reads, and refusedRetry when
	// it opens the stream again: a page's are 5 s and 60 s, shortened so the
	// chaos covers many.
	pollEvery    = time.Second
	refusedRetry = 3 * time.Second
	// readRetry is how soon a failed read is tried again.
	readRetry = 500 * time.Millisecond
	// convergeBound is how long after quiescence a browser may take to show
	// the store: a hint waits 250 ms for the coalescing and a read, a
	// dropped stream 3 s for the retry and a read on its hello; the rest is
	// slack for a loaded machine.
	convergeBound = 10 * time.Second
	// endBound is how long after its instance's kill began a stream may stay
	// open. The design allows a peer that vanished silently the ping plus the
	// write deadline; here a drain ends the streams at hub.Close and a cut
	// closes the connections, so they end at once.
	endBound = 5 * time.Second
	// specMarker is the description of every watched task, and replyBody
	// the human's answers: no hint may carry either.
	specMarker = "watched-spec-text"
	replyBody  = "carry on"
)

// watches are the browsers, and when each instance's kill began.
type watches struct {
	browsers []*browser
	cancel   context.CancelFunc
	wg       sync.WaitGroup

	mu    sync.Mutex
	kills map[string]time.Time // by instance id
}

// browser is one page with its watch stream: a board, or with task the
// page of that task and its latest run.
type browser struct {
	name, ns, task string
	url            string
	hc             *http.Client
	cl             *client.Client
	dirty          chan struct{}

	mu      sync.Mutex
	phases  map[string]v1alpha1.Phase // the board as last read
	run     runView                   // the task as last read
	streams []*stream
	refused int
	events  map[string]int // by kind
	unsafe  []string       // events that are more than a hint
}

// runView is what a task's page shows of it here.
type runView struct {
	phase  v1alpha1.Phase
	run    string // its latest run
	events int    // that run's events
}

type stream struct {
	instance      string
	opened, ended time.Time
	end           string // the end event's reason; "" if the connection dropped
}

func newWatches() *watches { return &watches{kills: map[string]time.Time{}} }

// killed records that the kill of instance id begins.
func (w *watches) killed(id string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.kills[id] = time.Now()
}

// start creates a watched task in every namespace and opens the browsers.
func (w *watches) start(t *testing.T, cl *client.Client, base string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	w.cancel = cancel
	for ns := range teams {
		// The create may fail after it landed: a named one is retried.
		var err error
		for range 5 {
			_, err = cl.CreateTask(ctx, ns, "watched", v1alpha1.TaskSpec{
				Title: "watched", Description: specMarker,
				Assignee: &v1alpha1.Assignee{Kind: v1alpha1.AssigneeAgent, Name: "coder"},
			})
			if client.IsCode(err, httpjson.CodeAlreadyExists) {
				err = nil
			}
			if err == nil {
				break
			}
		}
		if err != nil {
			t.Fatalf("create the watched task of %s: %v", ns, err)
		}
		for i := range boardWatchers + 1 {
			b := &browser{name: fmt.Sprintf("%s board %d", ns, i), ns: ns, dirty: make(chan struct{}, 1), events: map[string]int{}}
			if i == boardWatchers {
				b.name, b.task = ns+" task page", "watched"
			}
			b.url = base + api.Base + "/namespaces/" + ns + "/watch"
			if b.task != "" {
				b.url += "?task=" + url.QueryEscape(b.task)
			}
			// Its own connections, as a browser has.
			b.hc = &http.Client{Transport: &http.Transport{}}
			b.cl = client.New(base, b.hc)
			w.browsers = append(w.browsers, b)
		}
	}
	for _, b := range w.browsers {
		w.wg.Add(2)
		go func() { defer w.wg.Done(); b.watch(ctx) }()
		go func() { defer w.wg.Done(); b.refresh(ctx) }()
	}
}

func (w *watches) stop() {
	w.cancel()
	w.wg.Wait()
}

// watch keeps the stream open: after it ends or drops the browser opens it
// again once the retry is due, and while it is refused it polls.
func (b *browser) watch(ctx context.Context) {
	for ctx.Err() == nil {
		retry, ok := b.open(ctx)
		if ok {
			sleep(ctx, retry)
			continue
		}
		b.mu.Lock()
		b.refused++
		b.mu.Unlock()
		for due := time.Now().Add(refusedRetry); ctx.Err() == nil && time.Now().Before(due); {
			b.reread()
			sleep(ctx, pollEvery)
		}
	}
}

// open reads one stream until it closes, and returns its retry; false if
// it was refused.
func (b *browser) open(ctx context.Context) (time.Duration, bool) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.url, nil)
	if err != nil {
		return 0, false
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := b.hc.Do(req)
	if err != nil {
		return 0, false
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return 0, false
	}
	s := &stream{instance: resp.Header.Get(instanceHeader), opened: time.Now()}
	b.mu.Lock()
	b.streams = append(b.streams, s)
	b.mu.Unlock()
	retry := refusedRetry // until the stream says
	var kind string
	var data []string
	r := bufio.NewReader(resp.Body)
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			b.mu.Lock()
			s.ended = time.Now()
			b.mu.Unlock()
			return retry, true
		}
		line = strings.TrimSuffix(line, "\n")
		if line == "" {
			if kind != "" {
				b.dispatch(s, kind, strings.Join(data, "\n"))
			}
			kind, data = "", nil
			continue
		}
		field, value, _ := strings.Cut(line, ":")
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "": // a ping
		case "event":
			kind = value
		case "data":
			data = append(data, value)
		case "retry":
			if ms, err := strconv.Atoi(value); err == nil {
				retry = time.Duration(ms) * time.Millisecond
			}
		}
	}
}

// dispatch handles one event: the board reads again on a hint for it.
func (b *browser) dispatch(s *stream, kind, data string) {
	problem := hintProblem(kind, data)
	b.mu.Lock()
	b.events[kind]++
	if problem != "" {
		b.unsafe = append(b.unsafe, problem)
	}
	b.mu.Unlock()
	switch kind {
	case "task":
		var h api.TaskHint
		if json.Unmarshal([]byte(data), &h) == nil && b.task != "" && h.Name != b.task {
			return // another task's: the task page does not show it
		}
	case "end":
		var e api.WatchEnd
		_ = json.Unmarshal([]byte(data), &e)
		b.mu.Lock()
		s.end = e.Reason
		b.mu.Unlock()
		return // the server closes the stream next
	case "config":
		return // no page here shows configuration
	}
	b.reread()
}

// hintProblem says what is wrong with an event, if it is more than its
// hint: a field a hint does not have, or text of a spec or a thread.
func hintProblem(kind, data string) string {
	var v any
	switch kind {
	case "hello":
		v = &api.WatchHello{}
	case "task":
		v = &api.TaskHint{}
	case "config":
		v = &api.ConfigHint{}
	case "run":
		v = &api.RunHint{}
	case "resync":
		v = &struct{}{}
	case "end":
		v = &api.WatchEnd{}
	default:
		return "unknown event " + kind + ": " + data
	}
	dec := json.NewDecoder(strings.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Sprintf("%s %s: %v", kind, data, err)
	}
	if strings.Contains(data, specMarker) || strings.Contains(data, replyBody) {
		return fmt.Sprintf("%s %s carries a spec or thread text", kind, data)
	}
	return ""
}

// reread asks for a read, coalesced with one that waits already.
func (b *browser) reread() {
	select {
	case b.dirty <- struct{}{}:
	default:
	}
}

// refresh reads whenever asked, again and again while the read fails.
func (b *browser) refresh(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-b.dirty:
		}
		for ctx.Err() == nil && b.read(ctx) != nil {
			sleep(ctx, readRetry)
		}
	}
}

// read reads what the page shows through the routes the UI reads.
func (b *browser) read(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if b.task != "" {
		t, err := b.cl.GetTask(ctx, b.ns, b.task)
		if err != nil {
			return err
		}
		v := runView{phase: t.Status.Phase}
		if t.Status.Runs != nil {
			v.run = t.Status.Runs.LastID
		}
		if v.run != "" {
			ev, err := b.cl.Events(ctx, b.ns, b.task, v.run)
			if err != nil {
				return err
			}
			v.events = len(ev.Items)
		}
		b.mu.Lock()
		b.run = v
		b.mu.Unlock()
		return nil
	}
	phases := map[string]v1alpha1.Phase{}
	for offset := 0; ; {
		page, err := b.cl.ListTasks(ctx, b.ns, client.TaskQuery{Offset: offset, Limit: api.MaxLimit})
		if err != nil {
			return err
		}
		for _, t := range page.Items {
			phases[t.Name] = t.Status.Phase
		}
		if len(page.Items) < page.Limit {
			break
		}
		offset += len(page.Items)
	}
	b.mu.Lock()
	b.phases = phases
	b.mu.Unlock()
	return nil
}

// checkWatches waits up to convergeBound for every browser to show what
// the store holds, then checks that every stream a kill should have ended
// did end in time, and that no event was more than a hint.
func checkWatches(t *testing.T, admin ctrlclient.Client, cfg kubernetes.Config, w *watches) {
	t.Helper()
	ctx := context.Background()
	began := time.Now()
	drv, err := kubernetes.Open(ctx, cfg)
	if err != nil {
		t.Fatalf("open a fresh driver: %v", err)
	}
	defer func() { _ = drv.Close() }()
	m := manager.New(drv, managerConfig)
	for {
		stale, err := w.stale(ctx, admin, m)
		if err != nil {
			t.Fatal(err)
		}
		if len(stale) == 0 {
			t.Logf("watch: every browser shows the store %s after quiescence", time.Since(began).Round(100*time.Millisecond))
			break
		}
		if time.Since(began) > convergeBound {
			for _, s := range stale {
				t.Errorf("watch: %s %s after quiescence", s, convergeBound)
			}
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	w.mu.Lock()
	kills := maps.Clone(w.kills)
	w.mu.Unlock()
	cut, opened, refused := 0, 0, 0
	ends := map[string]int{}
	events := map[string]int{}
	for _, b := range w.browsers {
		b.mu.Lock()
		for _, s := range b.streams {
			opened++
			if !s.ended.IsZero() {
				ends[cmp.Or(s.end, "dropped")]++
			}
			at, ok := kills[s.instance]
			if !ok {
				continue
			}
			cut++
			switch {
			case s.ended.IsZero():
				t.Errorf("watch: %s's stream on %s is still open, its kill began %s ago", b.name, s.instance, time.Since(at).Round(time.Millisecond))
			case s.ended.Sub(at) > endBound:
				t.Errorf("watch: %s's stream on %s ended %s after its kill began, over %s", b.name, s.instance, s.ended.Sub(at).Round(time.Millisecond), endBound)
			}
		}
		for _, u := range b.unsafe {
			t.Errorf("watch: %s got %s", b.name, u)
		}
		for k, n := range b.events {
			events[k] += n
		}
		refused += b.refused
		b.mu.Unlock()
	}
	t.Logf("watch: %d streams opened, %d on killed instances, ends %v, %d refused, events %v", opened, cut, ends, refused, events)
	if len(kills) > 0 && cut == 0 {
		t.Errorf("watch: %d instances were killed, but no stream was on one", len(kills))
	}
}

// stale describes every browser whose last read differs from the store.
func (w *watches) stale(ctx context.Context, admin ctrlclient.Client, m *manager.Manager) ([]string, error) {
	boards := map[string]map[string]v1alpha1.Phase{}
	runs := map[string]runView{}
	for ns := range teams {
		var list v1alpha1.TaskList
		if err := admin.List(ctx, &list, ctrlclient.InNamespace(ns)); err != nil {
			return nil, err
		}
		boards[ns] = map[string]v1alpha1.Phase{}
		for _, tk := range list.Items {
			boards[ns][tk.Name] = tk.Status.Phase
			if tk.Name != "watched" {
				continue
			}
			v := runView{phase: tk.Status.Phase}
			if tk.Status.Runs != nil {
				v.run = tk.Status.Runs.LastID
			}
			if v.run != "" {
				ev, err := m.RunEvents(ctx, storage.Key{Namespace: ns, Name: tk.Name}, v.run)
				if err != nil {
					return nil, err
				}
				v.events = len(ev)
			}
			runs[ns] = v
		}
	}
	var stale []string
	for _, b := range w.browsers {
		b.mu.Lock()
		if b.task != "" {
			if want := runs[b.ns]; b.run != want {
				stale = append(stale, fmt.Sprintf("%s shows %+v, the store %+v", b.name, b.run, want))
			}
		} else if want := boards[b.ns]; !maps.Equal(b.phases, want) {
			var diff []string
			for name, p := range want {
				if got, ok := b.phases[name]; !ok || got != p {
					diff = append(diff, fmt.Sprintf("%s %q (store %q)", name, got, p))
				}
			}
			stale = append(stale, fmt.Sprintf("%s shows %d tasks, the store %d; differ: %v", b.name, len(b.phases), len(want), first(diff, 5)))
		}
		b.mu.Unlock()
	}
	return stale, nil
}

func first(s []string, n int) []string { return s[:min(n, len(s))] }
