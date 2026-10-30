package api_test

import (
	"bufio"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	"github.com/j75689/cadre/internal/api"
	"github.com/j75689/cadre/internal/authz"
	"github.com/j75689/cadre/internal/httpjson"
	"github.com/j75689/cadre/internal/manager"
	"github.com/j75689/cadre/internal/storage"
	"github.com/j75689/cadre/internal/storage/memory"
)

// fastWatch has the stream's intervals short enough for tests.
var fastWatch = api.WatchConfig{Ping: 100 * time.Millisecond, Lifetime: time.Minute, Coalesce: 50 * time.Millisecond}

// sseEvent is one event of a stream; a ping is kind "ping", and a retry
// line kind "retry".
type sseEvent struct{ kind, data string }

// watch opens path's stream with token, and returns its status and events,
// closed when the stream ends.
func watch(t *testing.T, url, token, path string) (int, <-chan sseEvent) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	res, events := watchWith(ctx, t, http.DefaultClient, url, token, path)
	return res.StatusCode, events
}

// watchWith is watch through c until ctx ends, with the whole response.
func watchWith(ctx context.Context, t *testing.T, c *http.Client, url, token, path string) (*http.Response, <-chan sseEvent) {
	t.Helper()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url+api.Base+path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	events := make(chan sseEvent, 1024)
	go func() {
		defer close(events)
		defer func() { _ = res.Body.Close() }()
		if res.StatusCode != http.StatusOK {
			return
		}
		var ev sseEvent
		sc := bufio.NewScanner(res.Body)
		for sc.Scan() {
			switch line := sc.Text(); {
			case line == ": ping":
				events <- sseEvent{kind: "ping"}
			case strings.HasPrefix(line, "retry: "):
				events <- sseEvent{kind: "retry", data: strings.TrimPrefix(line, "retry: ")}
			case strings.HasPrefix(line, "event: "):
				ev.kind = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				ev.data = strings.TrimPrefix(line, "data: ")
			case line == "" && ev.kind != "":
				events <- ev
				ev = sseEvent{}
			}
		}
	}()
	return res, events
}

// next is the stream's next event other than a ping or retry, within a
// second.
func next(t *testing.T, events <-chan sseEvent) sseEvent {
	t.Helper()
	timeout := time.After(time.Second)
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				t.Fatal("the stream ended")
			}
			if ev.kind != "ping" && ev.kind != "retry" {
				return ev
			}
		case <-timeout:
			t.Fatal("no event in a second")
		}
	}
}

// quiet fails on any event other than a ping for d.
func quiet(t *testing.T, events <-chan sseEvent, d time.Duration) {
	t.Helper()
	timeout := time.After(d)
	for {
		select {
		case ev, ok := <-events:
			if ok && ev.kind != "ping" {
				t.Fatalf("event %s %s, want none", ev.kind, ev.data)
			}
		case <-timeout:
			return
		}
	}
}

func TestWatchHints(t *testing.T) {
	store := memory.NewBackend().Open()
	e := startRolesWith(t, store, nil, &authz.Policy{}, api.WithWatch(api.NewHub(store), fastWatch))
	viewer := e.as("eve@example.test", "readers")
	member := e.as("dana@example.test", "backend")

	open, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	res, events := watchWith(open, t, http.DefaultClient, e.url, viewer, "/namespaces/team-a/watch?task=x")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("watch: %d", res.StatusCode)
	}
	for k, v := range map[string]string{"Content-Type": "text/event-stream", "Cache-Control": "no-cache", "X-Accel-Buffering": "no"} {
		if got := res.Header.Get(k); got != v {
			t.Errorf("%s: %q, want %q", k, got, v)
		}
	}
	if ev := <-events; ev.kind != "retry" || ev.data != "3000" {
		t.Fatalf("first line %+v, want retry: 3000", ev)
	}
	if ev := next(t, events); ev.kind != "hello" || ev.data != `{"tasks":true,"config":false,"runs":false,"lifetimeSeconds":60}` {
		t.Fatalf("first event %+v, want hello", ev)
	}

	// A phase change is one hint, however many writes it takes.
	if code, out := e.do(member, "POST", "/namespaces/team-a/tasks/x/cancel", "{}"); code != http.StatusOK {
		t.Fatalf("cancel: %d %s", code, out)
	}
	if ev := next(t, events); ev.kind != "task" || !strings.Contains(ev.data, `"name":"x"`) || !strings.Contains(ev.data, `"phase":"Cancelled"`) {
		t.Fatalf("event %+v, want a task hint", ev)
	}
	quiet(t, events, 200*time.Millisecond)

	// A lease renewed is not on the board.
	key := storage.Key{Namespace: "team-a", Name: "y"}
	if err := store.CreateTask(ctx, &v1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Namespace: key.Namespace, Name: key.Name}, Spec: v1alpha1.TaskSpec{Title: "y"}}); err != nil {
		t.Fatal(err)
	}
	tk, _ := store.GetTask(ctx, key)
	tk.Status.Phase = v1alpha1.PhaseRunning
	tk.Status.Run = &v1alpha1.Run{ID: "01J00000000000000000000000", LeaseUntil: metav1.NewTime(time.Now())}
	if err := store.UpdateTaskStatus(ctx, tk); err != nil {
		t.Fatal(err)
	}
	// y's creation may be a hint of its own, if the flush came between.
	for ev := next(t, events); !strings.Contains(ev.data, `"phase":"Running"`); ev = next(t, events) {
		if ev.kind != "task" || !strings.Contains(ev.data, `"name":"y"`) {
			t.Fatalf("event %+v, want y's hints", ev)
		}
	}
	tk.Status.Run.LeaseUntil = metav1.NewTime(time.Now().Add(time.Minute))
	if err := store.UpdateTaskStatus(ctx, tk); err != nil {
		t.Fatal(err)
	}
	// Nor is another team's board.
	tb, _ := store.GetTask(ctx, storage.Key{Namespace: "team-b", Name: "x"})
	tb.Status.Phase = v1alpha1.PhaseCancelled
	if err := store.UpdateTaskStatus(ctx, tb); err != nil {
		t.Fatal(err)
	}
	quiet(t, events, 300*time.Millisecond)
}

func TestWatchWithoutNotifier(t *testing.T) {
	e := startRolesWith(t, memory.NewBackend().Open(), nil, &authz.Policy{}, api.WithWatch(api.NewHub(nil), fastWatch))
	_, events := watch(t, e.url, e.as("eve@example.test", "readers"), "/namespaces/team-a/watch")
	if ev := next(t, events); ev.kind != "hello" || !strings.Contains(ev.data, `"tasks":false`) {
		t.Fatalf("first event %+v, want hello without tasks", ev)
	}
}

func TestWatchAccess(t *testing.T) {
	store := memory.NewBackend().Open()
	hub := api.NewHub(store)
	c := fastWatch
	c.MaxStreams = 1
	e := startRolesWith(t, store, nil, &authz.Policy{}, api.WithWatch(hub, c))
	viewer := e.as("eve@example.test", "readers")

	if code, _ := watch(t, e.url, "", "/namespaces/team-a/watch"); code != http.StatusUnauthorized {
		t.Errorf("without a login: %d, want 401", code)
	}
	if code, _ := watch(t, e.url, viewer, "/namespaces/team-b/watch"); code != http.StatusForbidden {
		t.Errorf("without a role: %d, want 403", code)
	}
	_, events := watch(t, e.url, viewer, "/namespaces/team-a/watch")
	next(t, events)
	if code, _ := watch(t, e.url, viewer, "/namespaces/team-a/watch"); code != http.StatusServiceUnavailable {
		t.Errorf("past the cap: %d, want 503", code)
	}

	// A role taken away ends the stream at the next ping.
	team, _ := store.GetTeam(ctx, "team-a")
	team.Spec.Roles = team.Spec.Roles[1:]
	if err := store.UpdateTeamSpec(ctx, team); err != nil {
		t.Fatal(err)
	}
	if ev := next(t, events); ev.kind != "end" || ev.data != `{"reason":"no_role"}` {
		t.Fatalf("event %+v, want end no_role", ev)
	}
	waitClosed(t, events)
	if code, _ := watch(t, e.url, viewer, "/namespaces/team-a/watch"); code != http.StatusForbidden {
		t.Errorf("reconnect without the role: %d, want 403", code)
	}
}

// A closed stream frees its slot.
func TestWatchFreesSlot(t *testing.T) {
	c := fastWatch
	c.MaxStreams = 1
	store := memory.NewBackend().Open()
	e := startRolesWith(t, store, nil, &authz.Policy{}, api.WithWatch(api.NewHub(store), c))
	viewer := e.as("eve@example.test", "readers")

	open, cancel := context.WithCancel(ctx)
	_, events := watchWith(open, t, http.DefaultClient, e.url, viewer, "/namespaces/team-a/watch")
	next(t, events)
	cancel()
	deadline := time.Now().Add(time.Second)
	for {
		code, _ := watch(t, e.url, viewer, "/namespaces/team-a/watch")
		if code == http.StatusOK {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("a second after the stream closed: %d, want 200", code)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A HEAD is answered as a GET would be, but holds no slot.
func TestWatchHead(t *testing.T) {
	c := fastWatch
	c.MaxStreams = 1
	store := memory.NewBackend().Open()
	e := startRolesWith(t, store, nil, &authz.Policy{}, api.WithWatch(api.NewHub(store), c))
	viewer := e.as("eve@example.test", "readers")
	// head fails if the HEAD does not return at once. Its connection is its
	// own, kept open, as a health check's.
	heads := &http.Client{Timeout: time.Second, Transport: &http.Transport{}}
	t.Cleanup(heads.CloseIdleConnections)
	head := func(ns string) int {
		t.Helper()
		req, _ := http.NewRequest(http.MethodHead, e.url+api.Base+"/namespaces/"+ns+"/watch", nil)
		req.Header.Set("Authorization", "Bearer "+viewer)
		res, err := heads.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = res.Body.Close()
		return res.StatusCode
	}

	if code := head("team-b"); code != http.StatusForbidden {
		t.Errorf("HEAD without a role: %d, want 403", code)
	}
	if code := head("team-a"); code != http.StatusOK {
		t.Fatalf("HEAD: %d", code)
	}
	code, events := watch(t, e.url, viewer, "/namespaces/team-a/watch")
	if code != http.StatusOK {
		t.Fatalf("GET after a HEAD: %d, want 200", code)
	}
	next(t, events)
	if code := head("team-a"); code != http.StatusServiceUnavailable {
		t.Errorf("HEAD past the cap: %d, want 503", code)
	}
}

// A browser's session cookie opens a stream, as a token does.
func TestWatchSession(t *testing.T) {
	store := memory.NewBackend().Open()
	e := startRolesWith(t, store, nil, &authz.Policy{}, api.WithWatch(api.NewHub(store), fastWatch))
	e.as("eve@example.test", "readers")
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	browser := &http.Client{Jar: jar}
	res, err := browser.Get(e.url + "/auth/login?next=/")
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()

	open, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	res, events := watchWith(open, t, browser, e.url, "", "/namespaces/team-a/watch")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("watch with a session: %d, want 200", res.StatusCode)
	}
	if ev := next(t, events); ev.kind != "hello" {
		t.Fatalf("first event %+v, want hello", ev)
	}
}

// waitClosed waits for the stream to close.
func waitClosed(t *testing.T, events <-chan sseEvent) {
	t.Helper()
	timeout := time.After(time.Second)
	for {
		select {
		case _, ok := <-events:
			if !ok {
				return
			}
		case <-timeout:
			t.Fatal("the stream is still open")
		}
	}
}

// startPlain serves the API with --auth=none as cadre-server does: through
// LogRequests, with its read and write timeouts, here short. team-a and
// team-b have a Team.
func startPlain(t *testing.T, hub *api.Hub, c api.WatchConfig) string {
	t.Helper()
	store := memory.NewBackend().Open()
	for _, ns := range []string{"team-a", "team-b"} {
		if err := store.CreateTeam(ctx, &v1alpha1.Team{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: v1alpha1.TeamName}}); err != nil {
			t.Fatal(err)
		}
	}
	m := manager.New(store, manager.Config{})
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	mux := http.NewServeMux()
	api.NewServer(m, log, api.WithWatch(hub, c)).Register(mux)
	srv := httptest.NewUnstartedServer(httpjson.LogRequests(mux, log))
	srv.Config.ReadTimeout = 100 * time.Millisecond
	srv.Config.WriteTimeout = 100 * time.Millisecond
	srv.Start()
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestWatchLifetime(t *testing.T) {
	c := fastWatch
	c.Lifetime = 500 * time.Millisecond
	url := startPlain(t, api.NewHub(nil), c)
	start := time.Now()
	_, events := watch(t, url, "", "/namespaces/team-a/watch")
	// Flushed through LogRequests, past the server's timeouts.
	if ev := next(t, events); ev.kind != "hello" {
		t.Fatalf("first event %+v, want hello", ev)
	}
	if ev := next(t, events); ev.kind != "end" || ev.data != `{"reason":"lifetime"}` {
		t.Fatalf("event %+v, want end lifetime", ev)
	}
	if d := time.Since(start); d < 400*time.Millisecond || d > 700*time.Millisecond {
		t.Errorf("stream lived %s, want 500ms ±20%%", d)
	}
	waitClosed(t, events)
}

func TestWatchShutdown(t *testing.T) {
	hub := api.NewHub(nil)
	_, events := watch(t, startPlain(t, hub, fastWatch), "", "/namespaces/team-a/watch")
	next(t, events)
	hub.Close()
	if ev := next(t, events); ev.kind != "end" || ev.data != `{"reason":"shutdown"}` {
		t.Fatalf("event %+v, want end shutdown", ev)
	}
	waitClosed(t, events)
}

// A namespace without a Team is a 404 to whoever the guard lets through,
// before the cap: a refused stream takes no slot.
func TestWatchWithoutTeam(t *testing.T) {
	c := fastWatch
	c.MaxStreams = 1
	store := memory.NewBackend().Open()
	e := startRolesWith(t, store, nil, &authz.Policy{AdminUsers: []string{"root@example.test"}}, api.WithWatch(api.NewHub(store), c))
	root := e.as("root@example.test")

	// The one slot is taken.
	_, events := watch(t, e.url, root, "/namespaces/team-a/watch")
	next(t, events)
	for _, method := range []string{"GET", "HEAD"} {
		for _, path := range []string{"/namespaces/team-c/watch", "/namespaces/team-c/watch?task=x"} {
			code, out := e.do(root, method, path, "")
			if code != http.StatusNotFound {
				t.Errorf("%s %s as a bootstrap admin: %d, want 404", method, path, code)
			}
			if method == "GET" && !strings.Contains(out, `"not_found"`) {
				t.Errorf("%s %s: %s, want not_found", method, path, out)
			}
		}
	}
	if code, out := e.do(root, "GET", "/namespaces/Team-C/watch", ""); code != http.StatusBadRequest {
		t.Errorf("a namespace that is no label: %d %s, want 400", code, out)
	}
	if code, _ := e.do(e.as("eve@example.test", "readers"), "GET", "/namespaces/team-c/watch", ""); code != http.StatusForbidden {
		t.Errorf("as a viewer of another team: %d, want 403", code)
	}

	url := startPlain(t, api.NewHub(nil), fastWatch)
	if code, _ := watch(t, url, "", "/namespaces/team-c/watch"); code != http.StatusNotFound {
		t.Errorf("with --auth=none: %d, want 404", code)
	}
}

// A Team deleted ends its namespace's streams at the next ping, and the
// reconnect is a 404.
func TestWatchTeamDeleted(t *testing.T) {
	store := memory.NewBackend().Open()
	var mu sync.Mutex
	teams := map[string]*v1alpha1.Team{"team-a": {ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: v1alpha1.TeamName}}}
	e := startRolesWith(t, store, func(d storage.Driver) storage.Driver { return stale{Driver: d, mu: &mu, teams: teams} },
		&authz.Policy{AdminUsers: []string{"root@example.test"}}, api.WithWatch(api.NewHub(store), fastWatch))
	root := e.as("root@example.test")

	_, events := watch(t, e.url, root, "/namespaces/team-a/watch")
	if ev := next(t, events); ev.kind != "hello" {
		t.Fatalf("first event %+v, want hello", ev)
	}
	mu.Lock()
	delete(teams, "team-a")
	mu.Unlock()
	if ev := next(t, events); ev.kind != "end" || ev.data != `{"reason":"no_team"}` {
		t.Fatalf("event %+v, want end no_team", ev)
	}
	waitClosed(t, events)
	if code, _ := watch(t, e.url, root, "/namespaces/team-a/watch"); code != http.StatusNotFound {
		t.Errorf("reconnect without the Team: %d, want 404", code)
	}
}
