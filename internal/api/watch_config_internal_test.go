package api

import (
	"bufio"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	toolscache "k8s.io/client-go/tools/cache"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	"github.com/j75689/cadre/internal/manager"
	"github.com/j75689/cadre/internal/storage/memory"
)

// A change is a hint; what the informer had already, listed again or
// resynced, is none.
func TestConfigHandler(t *testing.T) {
	h := NewHub(nil)
	w, _ := h.subscribe("team-a", 2)
	other, _ := h.subscribe("team-b", 2)
	on := h.configHandler("Agent")
	agent := func(ns, rv string) *v1alpha1.Agent {
		return &v1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "writer", ResourceVersion: rv}}
	}
	hints := func() int {
		n := 0
		for len(w.events) > 0 {
			if ev := <-w.events; ev.kind != "config" || ev.data != (ConfigHint{Kind: "Agent", Name: "writer"}) {
				t.Fatalf("event %+v, want an Agent writer hint", ev)
			}
			n++
		}
		return n
	}

	for _, c := range []struct {
		what  string
		event func()
		want  int
	}{
		{"listed at start", func() { on.OnAdd(agent("team-a", "1"), true) }, 0},
		{"resynced", func() { on.OnUpdate(agent("team-a", "1"), agent("team-a", "1")) }, 0},
		{"applied", func() { on.OnAdd(agent("team-a", "2"), false) }, 1},
		{"changed", func() { on.OnUpdate(agent("team-a", "2"), agent("team-a", "3")) }, 1},
		{"deleted", func() { on.OnDelete(agent("team-a", "3")) }, 1},
		{"deleted while the informer was away", func() {
			on.OnDelete(toolscache.DeletedFinalStateUnknown{Key: "team-a/writer", Obj: agent("team-a", "3")})
		}, 1},
		{"changed in another namespace", func() { on.OnUpdate(agent("team-b", "4"), agent("team-b", "5")) }, 0},
	} {
		c.event()
		if got := hints(); got != c.want {
			t.Errorf("%s: %d hints, want %d", c.what, got, c.want)
		}
	}
	if len(other.events) != 1 {
		t.Errorf("team-b's stream has %d hints, want 1", len(other.events))
	}
}

// A stream that does not read loses its config hints for one resync, and
// the informer does not wait for it.
func TestConfigOverflow(t *testing.T) {
	h := NewHub(nil)
	w, _ := h.subscribe("team-a", 1)
	on := h.configHandler("Agent")
	for i := range 2 * watchBuffer {
		on.OnAdd(&v1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "a" + strconv.Itoa(i)}}, false)
	}
	resyncs, hints := 0, 0
	for len(w.events) > 0 {
		if ev := <-w.events; ev.kind == "resync" {
			resyncs++
		} else {
			hints++
		}
	}
	if resyncs != 1 || hints != watchBuffer-1 {
		t.Fatalf("%d resyncs and %d hints, want 1 and %d", resyncs, hints, watchBuffer-1)
	}
}

// Publishing a config hint to a stream that reads while its buffer
// overflows never blocks the informer.
func TestConfigOverflowWhileReading(t *testing.T) {
	for round := range 200 {
		h := NewHub(nil)
		w, _ := h.subscribe("team-a", 1)
		on := h.configHandler("Agent")
		stop := make(chan struct{})
		go func() {
			for {
				select {
				case <-w.events:
				case <-stop:
					return
				}
			}
		}()
		done := make(chan struct{})
		go func() {
			defer close(done)
			for i := range 20000 {
				o := &v1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "a" + strconv.Itoa(i%300), ResourceVersion: strconv.Itoa(i)}}
				on.OnUpdate(&v1alpha1.Agent{}, o)
			}
		}()
		select {
		case <-done:
			close(stop)
		case <-time.After(5 * time.Second):
			t.Fatalf("round %d: the config handler is stuck holding h.mu", round)
		}
	}
}

// A burst of changes to one resource is one hint, kept apart from a task
// and a resource of another kind with the same name.
func TestConfigCoalesce(t *testing.T) {
	h := NewHub(nil)
	mux := http.NewServeMux()
	m := manager.New(memory.NewBackend().Open(), manager.Config{})
	NewServer(m, slog.New(slog.NewTextHandler(io.Discard, nil)), WithWatch(h, WatchConfig{Coalesce: 200 * time.Millisecond})).Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	res, err := http.Get(srv.URL + Base + "/namespaces/team-a/watch")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = res.Body.Close() })
	lines := make(chan string, 64)
	go func() {
		sc := bufio.NewScanner(res.Body)
		for sc.Scan() {
			if line, ok := strings.CutPrefix(sc.Text(), "data: "); ok {
				lines <- line
			}
		}
		close(lines)
	}()
	if hello := <-lines; !strings.Contains(hello, `"config":false`) {
		t.Fatalf("hello %s, want config:false without ConfigEvents", hello)
	}

	agents, team := h.configHandler("Agent"), h.configHandler("Team")
	for i := range 10 {
		on := agents
		if i == 5 {
			on = team
		}
		on.OnUpdate(&v1alpha1.Agent{}, &v1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "x", ResourceVersion: strconv.Itoa(i + 1)}})
	}
	h.publishTask(&v1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "x"}})
	// Past the flush, and long enough for any second one.
	var got []string
	for timeout := time.After(3 * 200 * time.Millisecond); timeout != nil; {
		select {
		case line := <-lines:
			got = append(got, line)
		case <-timeout:
			timeout = nil
		}
	}
	want := []string{`{"kind":"Agent","name":"x"}`, `{"kind":"Team","name":"x"}`, `{"name":"x","number":0,"phase":""}`}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("hints %q, want %q", got, want)
	}
}
