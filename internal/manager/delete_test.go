package manager

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	"github.com/j75689/cadre/internal/storage"
)

// deleteEnv is a test env whose Team deletes finished tasks after an hour,
// with a manager recording Events to rec.
func deleteEnv(t *testing.T) (*env, *events.FakeRecorder) {
	t.Helper()
	e := newEnv(t, 1)
	team := e.team(t)
	team.Spec.DeleteFinishedAfter = &metav1.Duration{Duration: time.Hour}
	if err := e.store.UpdateTeamSpec(ctx, team); err != nil {
		t.Fatal(err)
	}
	rec := events.NewFakeRecorder(100)
	e.m = e.deleter(rec, nil)
	return e, rec
}

// deleter returns another Task Manager on e's store with rec. A delete
// of it calls before, if not nil, with the task as marked, just before it
// deletes the task's streams: after the mark and the snapshot.
func (e *env) deleter(rec events.EventRecorder, before func(context.Context, *v1alpha1.Task) error) *Manager {
	store := e.store
	if before != nil {
		store = beforeStreams{e.store, before}
	}
	return New(store, Config{Agents: testAgents, Events: rec}, WithClock(e.clock.now))
}

// beforeStreams is a store that calls before with a task, read again,
// just before it deletes the task's streams.
type beforeStreams struct {
	storage.Driver
	before func(context.Context, *v1alpha1.Task) error
}

func (b beforeStreams) DeleteStream(ctx context.Context, stream string) error {
	if namespace, name, _ := strings.Cut(stream, "/"); !strings.Contains(name, "/") {
		tk, err := b.GetTask(ctx, storage.Key{Namespace: namespace, Name: name})
		if err != nil {
			return err
		}
		if err := b.before(ctx, tk); err != nil {
			return err
		}
	}
	return b.Driver.DeleteStream(ctx, stream)
}

// finishedRun is the run of finished's run record.
const finishedRun = "01JB0000000000000000000001"

// finished adds a task, cancels it now, and gives it a thread entry and a
// run record.
func (e *env) finished(t *testing.T, name string) storage.Key {
	t.Helper()
	key := e.addTask(t, name, v1alpha1.AssigneeUser)
	if _, err := e.m.Cancel(ctx, key); err != nil {
		t.Fatal(err)
	}
	if err := e.store.Append(ctx, ThreadStream(key), storage.SeqKey(1), []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	if err := e.store.Append(ctx, RunsStream(key), finishedRun, []byte(`{"id":"`+finishedRun+`","mode":"runtime"}`)); err != nil {
		t.Fatal(err)
	}
	return key
}

// gone reports whether key's task is deleted, failing if it is gone but
// its streams are not.
func (e *env) gone(t *testing.T, key storage.Key) bool {
	t.Helper()
	_, err := e.store.GetTask(ctx, key)
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		t.Fatal(err)
	}
	streams := e.entries(t, ThreadStream(key)) + e.entries(t, RunsStream(key))
	if err == nil {
		return false
	}
	if streams != 0 {
		t.Fatalf("%s is gone, but %d log entries are not", key.Name, streams)
	}
	return true
}

func (e *env) entries(t *testing.T, stream string) int {
	t.Helper()
	keys, err := e.store.Keys(ctx, stream, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	return len(keys)
}

func (e *env) deleteFinished(t *testing.T) {
	t.Helper()
	if err := e.m.DeleteFinished(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestDeleteFinished(t *testing.T) {
	e, rec := deleteEnv(t)
	a := e.finished(t, "a")
	e.clock.add(time.Minute)
	a2 := e.finished(t, "a2") // a's streams are a prefix of a2's
	e.clock.add(58 * time.Minute)
	e.deleteFinished(t)
	if e.gone(t, a) {
		t.Fatal("a is deleted 59 minutes after it finished, want an hour")
	}

	e.clock.add(time.Minute)
	e.deleteFinished(t)
	if !e.gone(t, a) {
		t.Fatal("a is not deleted an hour after it finished")
	}
	if e.gone(t, a2) || e.entries(t, ThreadStream(a2)) != 1 || e.entries(t, RunsStream(a2)) != 1 {
		t.Fatal("a2 or its logs went with a")
	}
	if len(rec.Events) != 1 {
		t.Fatalf("%d Events, want 1", len(rec.Events))
	}
	if ev := <-rec.Events; !strings.HasPrefix(ev, "Normal TaskDeleted Task #1 a finished 2026-10-24T09:00:00Z (Cancelled), deleted 1h later") {
		t.Fatalf("Event %q", ev)
	}
}

// A Team without deleteFinishedAfter keeps its finished tasks, unstamped.
func TestDeleteFinishedUnset(t *testing.T) {
	e := newEnv(t, 1)
	key := e.finished(t, "a")
	e.legacy(t, key)
	e.clock.add(1000 * time.Hour)
	e.deleteFinished(t)
	if e.gone(t, key) || e.get(t, key).Status.FinishedAt != nil {
		t.Fatal("a task of a Team without deleteFinishedAfter is deleted or stamped")
	}
}

// legacy has key's task finished before finishedAt was set.
func (e *env) legacy(t *testing.T, key storage.Key) {
	t.Helper()
	tk := e.get(t, key)
	tk.Status.FinishedAt = nil
	if err := e.store.UpdateTaskStatus(ctx, tk); err != nil {
		t.Fatal(err)
	}
}

// A task finished before finishedAt was set goes one full
// deleteFinishedAfter after the first tick.
func TestDeleteFinishedStampsLegacy(t *testing.T) {
	e, rec := deleteEnv(t)
	key := e.finished(t, "a")
	e.legacy(t, key)
	e.clock.add(1000 * time.Hour)
	stamped := e.clock.now()
	e.deleteFinished(t)
	if e.gone(t, key) {
		t.Fatal("a legacy task is deleted at the first tick")
	}
	if at := e.get(t, key).Status.FinishedAt; at == nil || !at.Time.Equal(stamped) {
		t.Fatalf("finishedAt %v, want %v", at, stamped)
	}
	e.clock.add(time.Hour)
	e.deleteFinished(t)
	if !e.gone(t, key) || len(rec.Events) != 1 {
		t.Fatalf("gone %v with %d Events, want gone with 1", e.gone(t, key), len(rec.Events))
	}
}

// A parent stays while it has a subtask, and goes at the tick after.
func TestDeleteFinishedParentWaits(t *testing.T) {
	e, _ := deleteEnv(t)
	parent := e.finished(t, "p")
	e.clock.add(time.Minute)
	sub := &v1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: ns},
		Spec:       v1alpha1.TaskSpec{Title: "s", ParentRef: &v1alpha1.TaskReference{Name: "p"}},
	}
	if err := e.store.CreateTask(ctx, sub); err != nil {
		t.Fatal(err)
	}
	if err := e.m.NumberNew(ctx); err != nil {
		t.Fatal(err)
	}
	subKey := storage.Key{Namespace: ns, Name: "s"}
	if _, err := e.m.Cancel(ctx, subKey); err != nil {
		t.Fatal(err)
	}
	e.clock.add(time.Hour)
	e.deleteFinished(t)
	if e.gone(t, parent) || !e.gone(t, subKey) {
		t.Fatal("want the subtask gone and its parent kept")
	}
	e.deleteFinished(t)
	if !e.gone(t, parent) {
		t.Fatal("the parent is kept after its subtask went")
	}
}

// A tick deletes 20 tasks of a Team, the oldest first; a parent kept
// for its subtask is not one of them.
func TestDeleteFinishedBatch(t *testing.T) {
	e, rec := deleteEnv(t)
	parent := e.finished(t, "p")
	e.clock.add(time.Minute)
	if err := e.store.CreateTask(ctx, &v1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: ns},
		Spec:       v1alpha1.TaskSpec{Title: "s", ParentRef: &v1alpha1.TaskReference{Name: "p"}},
	}); err != nil {
		t.Fatal(err)
	}
	var keys []storage.Key
	for i := range 25 {
		keys = append(keys, e.addTask(t, fmt.Sprintf("t%02d", i), v1alpha1.AssigneeUser))
	}
	// Finished last to first, so the oldest are numbered last.
	for i := len(keys) - 1; i >= 0; i-- {
		if _, err := e.m.Cancel(ctx, keys[i]); err != nil {
			t.Fatal(err)
		}
		e.clock.add(time.Minute)
	}
	e.clock.add(time.Hour)
	e.deleteFinished(t)
	if e.gone(t, parent) {
		t.Fatal("the parent of a task not finished is deleted")
	}
	for i, key := range keys {
		if gone := e.gone(t, key); gone != (i >= 5) {
			t.Errorf("%s gone: %v, want %v", key.Name, gone, i >= 5)
		}
	}
	if len(rec.Events) != 20 {
		t.Fatalf("%d Events, want 20", len(rec.Events))
	}
}

// A task with outbox items not applied stays until they are.
func TestDeleteFinishedOutboxWaits(t *testing.T) {
	e, rec := deleteEnv(t)
	key := e.finished(t, "a")
	tk := e.get(t, key)
	tk.Status.Outbox = append(tk.Status.Outbox, v1alpha1.OutboxItem{ID: "item-1", Kind: v1alpha1.OutboxAppendThread})
	if err := e.store.UpdateTaskStatus(ctx, tk); err != nil {
		t.Fatal(err)
	}
	e.clock.add(time.Hour)
	e.deleteFinished(t)
	if e.gone(t, key) || e.get(t, key).Status.DeletingSince != nil || len(rec.Events) != 0 {
		t.Fatal("a task with an outbox is marked or deleted")
	}
	tk = e.get(t, key)
	tk.Status.Outbox = nil
	if err := e.store.UpdateTaskStatus(ctx, tk); err != nil {
		t.Fatal(err)
	}
	e.deleteFinished(t)
	if !e.gone(t, key) {
		t.Fatal("the task is kept once its outbox is applied")
	}
}

// Tasks waiting on their outbox use none of the batch: the tick deletes
// the 20 due tasks after them.
func TestDeleteFinishedOutboxSkipped(t *testing.T) {
	e, rec := deleteEnv(t)
	var keys []storage.Key
	for i := range deleteBatch + 1 {
		keys = append(keys, e.finished(t, fmt.Sprintf("t%02d", i)))
		e.clock.add(time.Minute)
	}
	tk := e.get(t, keys[0])
	tk.Status.Outbox = append(tk.Status.Outbox, v1alpha1.OutboxItem{ID: "item-1", Kind: v1alpha1.OutboxAppendThread})
	if err := e.store.UpdateTaskStatus(ctx, tk); err != nil {
		t.Fatal(err)
	}
	e.clock.add(time.Hour)
	e.deleteFinished(t)
	for i, key := range keys {
		if gone := e.gone(t, key); gone != (i > 0) {
			t.Errorf("%s gone: %v, want %v", key.Name, gone, i > 0)
		}
	}
	if len(rec.Events) != deleteBatch {
		t.Fatalf("%d Events, want %d", len(rec.Events), deleteBatch)
	}
}

// A task marked deleting by a delete that stopped half way is deleted at
// the next tick, however young.
func TestDeleteFinishedResumes(t *testing.T) {
	e, rec := deleteEnv(t)
	key := e.finished(t, "a")
	failing := e.deleter(rec, func(context.Context, *v1alpha1.Task) error { return errors.New("stopped") })
	if err := failing.deleteTask(ctx, key, time.Hour, false); err == nil {
		t.Fatal("a delete whose streams fail to delete succeeds")
	}
	if e.gone(t, key) || e.get(t, key).Status.DeletingSince == nil {
		t.Fatal("want the task marked and kept")
	}
	e.deleteFinished(t)
	if !e.gone(t, key) || len(rec.Events) != 1 {
		t.Fatalf("want the marked task deleted with 1 Event, got %d", len(rec.Events))
	}
}

// Of two replicas deleting one task, the one whose delete lands records
// the Event.
func TestDeleteFinishedTwoManagers(t *testing.T) {
	e, rec := deleteEnv(t)
	key := e.finished(t, "a")
	e.clock.add(time.Hour)
	other := e.deleter(rec, nil)
	// The other replica deletes it after this one's mark, before its delete.
	slow := e.deleter(rec, func(ctx context.Context, _ *v1alpha1.Task) error {
		return other.DeleteFinished(ctx)
	})
	if err := slow.DeleteFinished(ctx); err != nil {
		t.Fatal(err)
	}
	if !e.gone(t, key) || len(rec.Events) != 1 {
		t.Fatalf("want the task deleted with 1 Event, got %d", len(rec.Events))
	}
}

// A task made again under the name while a delete is half way is kept:
// the delete of the marked version conflicts, and the re-read finds the
// new task unmarked.
func TestDeleteFinishedKeepsRecreated(t *testing.T) {
	e, rec := deleteEnv(t)
	key := e.finished(t, "a")
	e.clock.add(time.Hour)
	e.m = e.deleter(rec, func(ctx context.Context, marked *v1alpha1.Task) error {
		if err := e.store.DeleteTask(ctx, marked); err != nil {
			return err
		}
		again := &v1alpha1.Task{
			ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: ns},
			Spec:       v1alpha1.TaskSpec{Title: "again"},
		}
		if err := e.store.CreateTask(ctx, again); err != nil {
			return err
		}
		// Finished too, so a delete of it would record an Event.
		again.Status = marked.Status
		again.Status.DeletingSince = nil
		return e.store.UpdateTaskStatus(ctx, again)
	})
	e.deleteFinished(t)
	if tk := e.get(t, key); tk.Spec.Title != "again" || len(rec.Events) != 0 {
		t.Fatalf("task %q with %d Events, want the new task kept with none", tk.Spec.Title, len(rec.Events))
	}
}

// A delete that conflicts with a write to the still marked task reads it
// again and deletes it in the same tick.
func TestDeleteFinishedRetriesConflict(t *testing.T) {
	e, rec := deleteEnv(t)
	key := e.finished(t, "a")
	e.clock.add(time.Hour)
	e.m = e.deleter(rec, func(ctx context.Context, marked *v1alpha1.Task) error {
		return e.store.UpdateTaskStatus(ctx, marked.DeepCopy())
	})
	e.deleteFinished(t)
	if !e.gone(t, key) || len(rec.Events) != 1 {
		t.Fatalf("want the task deleted with 1 Event, got %d", len(rec.Events))
	}
}

// failStreams is a store whose stream deletes fail.
type failStreams struct{ storage.Driver }

func (failStreams) DeleteStream(context.Context, string) error { return errors.New("streams down") }

// A task whose logs fail to delete is kept marked, with its logs, until a
// tick deletes both.
func TestDeleteFinishedStreamFails(t *testing.T) {
	e, rec := deleteEnv(t)
	key := e.finished(t, "a")
	e.clock.add(time.Hour)
	failing := New(failStreams{e.store}, Config{Agents: testAgents, Events: rec}, WithClock(e.clock.now))
	if err := failing.DeleteFinished(ctx); err == nil {
		t.Fatal("a delete whose logs fail to delete succeeds")
	}
	if e.gone(t, key) || e.get(t, key).Status.DeletingSince == nil || len(rec.Events) != 0 {
		t.Fatal("want the task marked and kept, with no Event")
	}
	if n := e.entries(t, ThreadStream(key)) + e.entries(t, RunsStream(key)); n != 2 {
		t.Fatalf("%d log entries, want 2", n)
	}
	e.deleteFinished(t)
	if !e.gone(t, key) || len(rec.Events) != 1 {
		t.Fatalf("want the task deleted with 1 Event, got %d", len(rec.Events))
	}
}

// Run deletes finished tasks on its own ticker.
func TestRunDeletesFinished(t *testing.T) {
	e, _ := deleteEnv(t)
	key := e.finished(t, "a")
	e.clock.add(time.Hour)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go e.m.Run(ctx, LoopConfig{DeleteFinished: 10 * time.Millisecond}, nil)
	for start := time.Now(); !e.gone(t, key); time.Sleep(5 * time.Millisecond) {
		if time.Since(start) > time.Second {
			t.Fatal("Run has not deleted the task in a second")
		}
	}
}

// finalizers is a store whose Tasks in held have a finalizer, as on
// kubernetes: a delete that lands sets their DeletionTimestamp and bumps
// their version, and they stay until it clears.
type finalizers struct {
	storage.Driver
	held    map[string]bool
	deleted map[string]bool
}

func (f *finalizers) mark(t *v1alpha1.Task) {
	if f.deleted[t.Name] {
		now := metav1.Now()
		t.DeletionTimestamp = &now
	}
}

func (f *finalizers) GetTask(ctx context.Context, key storage.Key) (*v1alpha1.Task, error) {
	t, err := f.Driver.GetTask(ctx, key)
	if err == nil {
		f.mark(t)
	}
	return t, err
}

func (f *finalizers) ListTasks(ctx context.Context, q storage.Query) ([]v1alpha1.Task, error) {
	tasks, err := f.Driver.ListTasks(ctx, q)
	for i := range tasks {
		f.mark(&tasks[i])
	}
	return tasks, err
}

func (f *finalizers) DeleteTask(ctx context.Context, t *v1alpha1.Task) error {
	if !f.held[t.Name] {
		return f.Driver.DeleteTask(ctx, t)
	}
	cur, err := f.Driver.GetTask(ctx, storage.Key{Namespace: t.Namespace, Name: t.Name})
	if err != nil {
		return err
	}
	if cur.ResourceVersion != t.ResourceVersion {
		return storage.ErrConflict
	}
	if !f.deleted[t.Name] {
		f.deleted[t.Name] = true
		return f.UpdateTaskStatus(ctx, cur)
	}
	return nil
}

// heldEnv is deleteEnv on a store where a finalizer holds task "held",
// with the stream deletes counted by task name.
func heldEnv(t *testing.T) (*env, *events.FakeRecorder, map[string]int) {
	t.Helper()
	e, rec := deleteEnv(t)
	e.store = &finalizers{Driver: e.store, held: map[string]bool{"held": true}, deleted: map[string]bool{}}
	streams := map[string]int{}
	e.m = e.deleter(rec, count(streams))
	return e, rec, streams
}

// count counts the tasks it is called with in n, by name.
func count(n map[string]int) func(context.Context, *v1alpha1.Task) error {
	return func(_ context.Context, tk *v1alpha1.Task) error {
		n[tk.Name]++
		return nil
	}
}

// deletedEvents counts rec's TaskDeleted Events by task name.
func deletedEvents(rec *events.FakeRecorder) map[string]int {
	n := map[string]int{}
	for len(rec.Events) > 0 {
		ev := <-rec.Events
		for _, name := range []string{"held", "free"} {
			if strings.Contains(ev, " "+name+" finished ") {
				n[name]++
			}
		}
	}
	return n
}

// A task held by a finalizer is deleted once, at one stream delete and one
// Event, and left alone at the ticks of either replica after; a task
// without one goes as before.
func TestDeleteFinishedHeldByFinalizer(t *testing.T) {
	e, rec, streams := heldEnv(t)
	held := e.finished(t, "held")
	free := e.finished(t, "free")
	e.clock.add(time.Hour)
	other := e.deleter(rec, count(streams))
	for range 3 {
		for _, m := range []*Manager{e.m, other} {
			if err := m.DeleteFinished(ctx); err != nil {
				t.Fatal(err)
			}
		}
		e.clock.add(time.Minute)
	}
	if tk := e.get(t, held); tk.DeletionTimestamp == nil {
		t.Fatal("want the held task kept, terminating")
	}
	if !e.gone(t, free) {
		t.Fatal("the task without a finalizer is kept")
	}
	if n := deletedEvents(rec); n["held"] != 1 || n["free"] != 1 {
		t.Fatalf("TaskDeleted Events %v, want 1 each", n)
	}
	if streams["held"] != 1 || streams["free"] != 1 {
		t.Fatalf("stream deletes %v, want 1 each", streams)
	}
}

// Of two replicas deleting a task held by a finalizer, only the one whose
// delete lands records the Event: the other's delete conflicts and reads
// the task terminating, or reads it so before its stream delete.
func TestDeleteFinishedHeldTwoManagers(t *testing.T) {
	e, rec, _ := heldEnv(t)
	key := e.finished(t, "held")
	e.clock.add(time.Hour)
	other := e.deleter(rec, nil)
	slow := e.deleter(rec, func(ctx context.Context, _ *v1alpha1.Task) error {
		return other.DeleteFinished(ctx)
	})
	if err := slow.DeleteFinished(ctx); err != nil {
		t.Fatal(err)
	}
	if err := slow.deleteTask(ctx, key, time.Hour, false); err != nil {
		t.Fatal(err)
	}
	if n := deletedEvents(rec); n["held"] != 1 {
		t.Fatalf("%d TaskDeleted Events, want 1", n["held"])
	}
}

// A task held by a finalizer since a delete from elsewhere (kubectl, say)
// uses none of the batch and takes no write: the tick deletes the 20 due
// tasks after it.
func TestDeleteFinishedHeldSkipped(t *testing.T) {
	e, rec, streams := heldEnv(t)
	held := e.finished(t, "held")
	e.store.(*finalizers).deleted["held"] = true
	var keys []storage.Key
	for i := range deleteBatch {
		e.clock.add(time.Minute)
		keys = append(keys, e.finished(t, fmt.Sprintf("t%02d", i)))
	}
	e.clock.add(time.Hour)
	e.deleteFinished(t)
	for _, key := range keys {
		if !e.gone(t, key) {
			t.Errorf("%s is kept", key.Name)
		}
	}
	if e.get(t, held).Status.DeletingSince != nil || streams["held"] != 0 || len(rec.Events) != deleteBatch {
		t.Fatalf("held task marked %v, %d stream deletes, %d Events: want unmarked, 0, %d",
			e.get(t, held).Status.DeletingSince != nil, streams["held"], len(rec.Events), deleteBatch)
	}
}
