package api

import (
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	"github.com/j75689/cadre/internal/manager"
	"github.com/j75689/cadre/internal/storage/memory"
)

func TestHubOverflow(t *testing.T) {
	h := NewHub(nil)
	w, _ := h.subscribe("team-a", 1)
	task := func(title string) *v1alpha1.Task {
		return &v1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "x"}, Spec: v1alpha1.TaskSpec{Title: title}}
	}
	// A stream that does not read loses its hints for one resync.
	for i := range 2 * watchBuffer {
		h.publishTask(task(string(rune('a' + i%26))))
	}
	resyncs, tasks := 0, 0
	for len(w.events) > 0 {
		if ev := <-w.events; ev.kind == "resync" {
			resyncs++
		} else {
			tasks++
		}
	}
	if resyncs != 1 || tasks != watchBuffer-1 {
		t.Fatalf("%d resyncs and %d hints, want 1 and %d", resyncs, tasks, watchBuffer-1)
	}
	// An unchanged task is no hint.
	h.publishTask(task(string(rune('a' + (2*watchBuffer-1)%26))))
	if len(w.events) != 0 {
		t.Fatalf("%d hints for an unchanged task", len(w.events))
	}
}

// A stream that reads while its buffer overflows must not leave the
// publisher waiting on an empty channel with h.mu held.
func TestHubOverflowWhileReading(t *testing.T) {
	for round := range 200 {
		h := NewHub(nil)
		w, _ := h.subscribe("team-a", 1)
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
				h.publishTask(&v1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "x" + strconv.Itoa(i%300)}, Spec: v1alpha1.TaskSpec{Title: strconv.Itoa(i)}})
			}
		}()
		select {
		case <-done:
			close(stop)
		case <-time.After(5 * time.Second):
			t.Fatalf("round %d: publishTask is stuck holding h.mu", round)
		}
	}
}

// An absent field is not skipped, so two boards that differ do not hash the
// same.
func TestBoardDigestAbsentFields(t *testing.T) {
	assigned := &v1alpha1.Task{Spec: v1alpha1.TaskSpec{Assignee: &v1alpha1.Assignee{Kind: v1alpha1.AssigneeAgent, Name: "x"}}}
	labelled := &v1alpha1.Task{Spec: v1alpha1.TaskSpec{Labels: []string{"Agent/x"}}, Status: v1alpha1.TaskStatus{Waiting: &v1alpha1.Waiting{}}}
	if boardDigest(assigned) == boardDigest(labelled) {
		t.Fatal("an assignee and a label of the same text hash the same")
	}
}

// A stream whose client reads nothing is dropped at the write deadline, and
// its slot freed.
func TestWatchWriteTimeout(t *testing.T) {
	// Runs after srv.Close (cleanups are LIFO), which waits for the handler that reads it.
	saved := watchWriteTimeout
	t.Cleanup(func() { watchWriteTimeout = saved })
	watchWriteTimeout = 100 * time.Millisecond
	h := NewHub(nil)
	mux := http.NewServeMux()
	m := manager.New(memory.NewBackend().Open(), manager.Config{})
	NewServer(m, slog.New(slog.NewTextHandler(io.Discard, nil)), WithWatch(h, WatchConfig{MaxStreams: 1, Coalesce: time.Millisecond})).Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	_ = conn.(*net.TCPConn).SetReadBuffer(4096)
	if _, err := io.WriteString(conn, "GET "+Base+"/namespaces/team-a/watch HTTP/1.1\r\nHost: cadre\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	streams := func() int {
		h.mu.Lock()
		defer h.mu.Unlock()
		return h.streams
	}
	for start := time.Now(); streams() == 0; time.Sleep(time.Millisecond) {
		if time.Since(start) > time.Second {
			t.Fatal("the stream did not open")
		}
	}
	// Hints until the buffers between fill, and a write waits past its
	// deadline.
	name := strings.Repeat("x", 1000)
	for i, start := 0, time.Now(); streams() != 0; i++ {
		if time.Since(start) > 5*time.Second {
			t.Fatal("a stream that reads nothing is still open after 5s")
		}
		h.publishTask(&v1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: name + strconv.Itoa(i)}})
		if i%100 == 0 {
			time.Sleep(time.Millisecond)
		}
	}
	res, err := http.Get(srv.URL + Base + "/namespaces/team-a/watch")
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("a stream after the stuck one: %d, want 200", res.StatusCode)
	}
}
