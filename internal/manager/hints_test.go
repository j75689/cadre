package manager

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	"github.com/j75689/cadre/internal/storage"
	"github.com/j75689/cadre/internal/task"
)

// With the scans an hour apart, a task applied as kubectl would is
// numbered, and a finished run's subtask created and numbered, at once:
// the store's notifications drive both.
func TestHintsNumberAndDispatchAtOnce(t *testing.T) {
	e := newEnv(t, 2)
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	e.m.FollowHints(runCtx, slog.New(slog.NewTextHandler(io.Discard, nil)))

	key := storage.Key{Namespace: ns, Name: "applied"}
	if err := e.store.CreateTask(ctx, &v1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: key.Name},
		Spec:       v1alpha1.TaskSpec{Title: "applied", Assignee: &v1alpha1.Assignee{Kind: v1alpha1.AssigneeAgent, Name: "coder"}},
	}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the applied task numbered", func() bool { return e.get(t, key).Status.Number > 0 })

	c := e.claim(t)
	if _, _, err := e.m.Finish(ctx, key, FinishRequest{RunID: c.RunID, Outcome: task.OutcomeDone, Subtasks: []Subtask{{Spec: subSpec("follow-up")}}}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the subtask created and numbered", func() bool {
		subs, err := e.m.ListTasks(ctx, storage.Query{Namespace: ns})
		if err != nil {
			return false
		}
		for _, s := range subs {
			if s.Spec.ParentRef != nil && s.Spec.ParentRef.Name == key.Name && s.Status.Number > 0 {
				return len(e.get(t, key).Status.Outbox) == 0
			}
		}
		return false
	})
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatalf("%s: not within 3s, with the scans an hour away", what)
}

// A task the Manager creates and numbers itself, and a subtask a run
// creates, take one number each: the hints their writes send are not
// followed meanwhile.
func TestOneNumberPerTask(t *testing.T) {
	e := newEnv(t, 5)
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	e.m.FollowHints(runCtx, slog.New(slog.NewTextHandler(io.Discard, nil)))
	for i := range 20 {
		if _, err := e.m.CreateTask(ctx, ns, fmt.Sprintf("t%d", i), v1alpha1.TaskSpec{Title: "t", Assignee: &v1alpha1.Assignee{Kind: v1alpha1.AssigneeAgent, Name: "coder"}}); err != nil {
			t.Fatal(err)
		}
	}
	c := e.claim(t)
	for i := range 5 {
		if _, err := e.m.RunCreateSubtask(ctx, storage.Key{Namespace: ns, Name: c.Task.Name}, c.RunID, subSpec(fmt.Sprintf("sub %d", i))); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(200 * time.Millisecond) // let any stray hint land
	team, err := e.store.GetTeam(ctx, ns)
	if err != nil {
		t.Fatal(err)
	}
	if got := team.Status.NextNumber; got != 26 {
		t.Fatalf("next number %d after 25 tasks, want 26: numbers were claimed twice", got)
	}
}
