package manager

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	"github.com/j75689/cadre/internal/storage"
	"github.com/j75689/cadre/internal/task"
)

// committed records a run's comment in the outbox without applying it, as
// a tool whose eager apply was lost would leave it.
func (e *env) committed(t *testing.T, key storage.Key, runID, body string) v1alpha1.OutboxItem {
	t.Helper()
	tk := e.get(t, key)
	item, err := e.m.threadItem(ctx, key, tk, runID, EntryComment, body)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.m.transition(ctx, key, task.Record{RunID: runID, Outbox: []v1alpha1.OutboxItem{item}}); err != nil {
		t.Fatal(err)
	}
	return item
}

// beforeThread runs hook once, just before the first append of item's
// entry to a thread.
type beforeThread struct {
	storage.Driver
	item string
	once sync.Once
	hook func()
}

func (b *beforeThread) Append(c context.Context, stream, key string, data []byte) error {
	if strings.HasSuffix(stream, "/thread") && strings.Contains(string(data), `"itemId":"`+b.item+`"`) {
		b.once.Do(b.hook)
	}
	return b.Driver.Append(c, stream, key, data)
}

func (e *env) entriesOf(t *testing.T, key storage.Key, itemID string) (mine int, all []ThreadEntry) {
	t.Helper()
	all, err := e.m.ReadThread(ctx, key, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, en := range all {
		if en.ItemID == itemID {
			mine++
		}
	}
	return mine, all
}

// Two dispatchers apply one item while a human comments in between: the
// item's entry is written once, at its reserved position, and the human's
// comment lands after it, not on it.
func TestTwoAppliersAndAHumanWriteOneEntry(t *testing.T) {
	e := newEnv(t, 1)
	key := e.addTask(t, "t", v1alpha1.AssigneeAgent)
	c := e.claim(t)
	item := e.committed(t, key, c.RunID, "from the run")

	other := e.manager(e.store)
	first := e.manager(&beforeThread{Driver: e.store, item: item.ID, hook: func() {
		if _, err := e.m.AppendThread(ctx, key, alice, EntryComment, "from a human"); err != nil {
			t.Error(err)
		}
		if err := other.dispatchTask(ctx, key); err != nil {
			t.Error(err)
		}
	}})
	if err := first.dispatchTask(ctx, key); err != nil {
		t.Fatal(err)
	}
	mine, all := e.entriesOf(t, key, item.ID)
	if mine != 1 || len(all) != 2 {
		t.Fatalf("%d entries of the item, thread %+v; want the item once and the human's comment", mine, all)
	}
	// The reservation moved the revision past it, so the human's comment
	// did not land on the reserved key: the item keeps its place.
	if all[0].ItemID != item.ID || all[1].Body != "from a human" {
		t.Fatalf("thread %+v; want the item at its reserved first place, the human's comment after it", all)
	}
	if out := e.get(t, key).Status.Outbox; len(out) != 0 {
		t.Fatalf("outbox %+v, want it applied", out)
	}
}

// An entry that took the item's reserved key first (a write that read the
// revision before the reservation moved it) pushes the item to the next
// free key; nothing is overwritten and the item is written once.
func TestAReservedKeyTakenByAnotherWrite(t *testing.T) {
	e := newEnv(t, 1)
	key := e.addTask(t, "t", v1alpha1.AssigneeAgent)
	c := e.claim(t)
	item := e.committed(t, key, c.RunID, "from the run")
	squatter := e.manager(&beforeThread{Driver: e.store, item: item.ID, hook: func() {
		tk := e.get(t, key)
		seq := tk.Status.Outbox[0].Seq
		data, _ := json.Marshal(ThreadEntry{Seq: seq, Author: alice, Kind: EntryComment, Body: "got there first"})
		if err := e.store.Append(ctx, ThreadStream(key), storage.SeqKey(seq), data); err != nil {
			t.Error(err)
		}
	}})
	if err := squatter.dispatchTask(ctx, key); err != nil {
		t.Fatal(err)
	}
	mine, all := e.entriesOf(t, key, item.ID)
	if mine != 1 || len(all) != 2 || all[0].Body != "got there first" || all[1].ItemID != item.ID {
		t.Fatalf("thread %+v; want the other write first and the item once after it", all)
	}
}

// Many dispatchers at once, as every replica hears the same watch event:
// each committed comment and file version is written exactly once.
func TestConcurrentDispatchersWriteEachItemOnce(t *testing.T) {
	e := newEnv(t, 1)
	key := e.addTask(t, "t", v1alpha1.AssigneeAgent)
	c := e.claim(t)
	var items []v1alpha1.OutboxItem
	for i := range 8 {
		items = append(items, e.committed(t, key, c.RunID, fmt.Sprintf("comment %d", i)))
	}
	var files []v1alpha1.OutboxItem
	for i := range 8 {
		fi := v1alpha1.OutboxItem{ID: e.m.newID(), Kind: v1alpha1.OutboxWriteFile}
		blob := blobName(c.RunID, fi.ID)
		if err := e.m.writeBlob(ctx, key, blob, []byte(fmt.Sprintf("v%d", i))); err != nil {
			t.Fatal(err)
		}
		fi.File = &v1alpha1.FileWrite{Blob: blob, Path: "notes.md", Size: 2}
		if _, err := e.m.transition(ctx, key, task.Record{RunID: c.RunID, Outbox: []v1alpha1.OutboxItem{fi}}); err != nil {
			t.Fatal(err)
		}
		files = append(files, fi)
	}
	var wg sync.WaitGroup
	for range 6 {
		m := e.manager(e.store)
		wg.Go(func() {
			for range 5 {
				_ = m.dispatchTask(ctx, key)
			}
		})
	}
	wg.Wait()
	if err := e.m.dispatchTask(ctx, key); err != nil {
		t.Fatal(err)
	}
	for _, it := range items {
		if mine, all := e.entriesOf(t, key, it.ID); mine != 1 {
			t.Fatalf("item %s written %d times: %+v", it.ID, mine, all)
		}
	}
	versions, err := e.store.Read(ctx, fileStream(key, "notes.md"), "", 0)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]int{}
	for _, v := range versions {
		var fv fileVersion
		if err := json.Unmarshal(v.Data, &fv); err != nil {
			t.Fatal(err)
		}
		seen[fv.ItemID]++
	}
	for _, fi := range files {
		if seen[fi.ID] != 1 {
			t.Fatalf("file item %s written %d times", fi.ID, seen[fi.ID])
		}
	}
	if len(versions) != len(files) {
		t.Fatalf("%d versions for %d writes", len(versions), len(files))
	}
}

// Listing reads the newest snapshot of the files index and what follows
// it; snapshots are written every 64 index entries.
func TestFilesIndexSnapshots(t *testing.T) {
	e := newEnv(t, 1)
	key := e.addTask(t, "t", v1alpha1.AssigneeAgent)
	c := e.claim(t)
	for i := range 2*snapshotEvery + 5 {
		path := fmt.Sprintf("f%d.txt", i%10)
		if info, err := e.m.RunWriteFile(ctx, key, c.RunID, path, fmt.Sprintf("version %d", i)); err != nil || info.Version == 0 {
			t.Fatalf("write %d: %+v, %v", i, info, err)
		}
	}
	snaps, err := e.store.Keys(ctx, filesSnapshotStream(key), "", 0)
	if err != nil || len(snaps) != 2 {
		t.Fatalf("snapshots %+v, %v; want two", snaps, err)
	}
	files, err := e.m.RunListFiles(ctx, key, c.RunID)
	if err != nil || len(files) != 10 {
		t.Fatalf("list: %+v, %v", files, err)
	}
	f, err := e.m.RunReadFile(ctx, key, c.RunID, "f2.txt")
	last := 0
	for i := range 2*snapshotEvery + 5 {
		if i%10 == 2 {
			last = i
		}
	}
	if err != nil || f.Content != fmt.Sprintf("version %d", last) {
		t.Fatalf("read: %+v, %v", f, err)
	}
	for _, fi := range files {
		if fi.Path == "f2.txt" && fi.Version != f.Version {
			t.Fatalf("listed version %d, read %d", fi.Version, f.Version)
		}
	}
}

// Until the Question a run asked is in the thread, a reply is refused: it
// would come before the Question.
func TestAReplyWaitsForTheQuestion(t *testing.T) {
	e := newEnv(t, 1)
	key := e.addTask(t, "t", v1alpha1.AssigneeAgent)
	c := e.claim(t)
	tk := e.get(t, key)
	item, err := e.m.threadItem(ctx, key, tk, c.RunID, EntryQuestion, "Staging or production?")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.m.Finish(ctx, key, FinishRequest{RunID: c.RunID, Outcome: task.OutcomeWaiting, Reason: v1alpha1.WaitingQuestion,
		WaitingItem: item.ID, Outbox: []v1alpha1.OutboxItem{item}}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.m.Answer(ctx, key, alice, "staging"); !errors.Is(err, ErrQuestionPending) {
		t.Fatalf("an answer before the question: %v, want ErrQuestionPending", err)
	}
	if err := e.m.Dispatch(ctx); err != nil {
		t.Fatal(err)
	}
	if w := e.get(t, key).Status.Waiting; w == nil || w.ThreadSeq != 1 {
		t.Fatalf("waiting %+v, want it on the Question at 1", w)
	}
	if _, entry, err := e.m.Answer(ctx, key, alice, "staging"); err != nil || entry.Seq != 2 {
		t.Fatalf("an answer after the question: %+v, %v", entry, err)
	}
}
