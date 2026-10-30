package api

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
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
