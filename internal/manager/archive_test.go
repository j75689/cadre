package manager

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	"github.com/j75689/cadre/internal/storage"
)

// archiveEnv is deleteEnv for a Team that archives finished tasks after an
// hour instead.
func archiveEnv(t *testing.T) (*env, *events.FakeRecorder) {
	t.Helper()
	e, rec := deleteEnv(t)
	e.setTeam(t, func(s *v1alpha1.TeamSpec) {
		s.ArchiveAfter = &metav1.Duration{Duration: time.Hour}
		s.DeleteFinishedAfter = nil
	})
	return e, rec
}

func (e *env) setTeam(t *testing.T, change func(*v1alpha1.TeamSpec)) {
	t.Helper()
	team := e.team(t)
	change(&team.Spec)
	if err := e.store.UpdateTeamSpec(ctx, team); err != nil {
		t.Fatal(err)
	}
}

// The runs of a rich task.
const (
	stepRun    = "01JB0000000000000000000002"
	runtimeRun = "01JB0000000000000000000003"
)

// Committed steps of stepRun, as a provider sends them: compact, with
// characters an HTML-escaping encoder would rewrite.
var stepEntries = []string{
	`{"step":0,"kind":"start","request":{"system":"a <b> & c"}}`,
	`{"step":1,"kind":"llm_turn","text":"x < y"}`,
}

// rich is finished task "a" with a step run whose transcript has a failed
// attempt, a runtime run, events of both, and a file.
func (e *env) rich(t *testing.T) storage.Key {
	t.Helper()
	key := e.finished(t, "a")
	appends := []struct {
		stream, key, data string
	}{
		{ThreadStream(key), storage.SeqKey(2), `{"seq":2,"author":{"kind":"user","name":"alice"},"kind":"comment","body":"hi","at":"2026-10-24T09:00:00Z"}`},
		{RunsStream(key), stepRun, `{"id":"` + stepRun + `","mode":"step","reason":"Completed"}`},
		{RunsStream(key), runtimeRun, `{"id":"` + runtimeRun + `","mode":"runtime","reason":"Completed"}`},
		{EventsStream(key, stepRun), storage.SeqKey(1), `{"runId":"` + stepRun + `","seq":1,"inputTokens":5}`},
		{EventsStream(key, runtimeRun), storage.SeqKey(1), `{"runId":"` + runtimeRun + `","seq":1,"progress":"working"}`},
		{TranscriptStream(key, stepRun), storage.SeqKey(0), stepEntries[0]},
		{TranscriptStream(key, stepRun), attemptKey(1, 1), `{"step":1,"attempt":1,"error":"timeout"}`},
		{TranscriptStream(key, stepRun), storage.SeqKey(1), stepEntries[1]},
		{filesIndexStream(key), storage.SeqKey(1), `{"path":"notes.md","version":1,"size":5}`},
		{fileStream(key, "notes.md"), storage.SeqKey(1), `{"itemId":"i1","blob":"b1","size":5}`},
		{blobStream(key, "b1"), blobKey, `hello`},
	}
	for _, a := range appends {
		if err := e.store.Append(ctx, a.stream, a.key, []byte(a.data)); err != nil {
			t.Fatal(err)
		}
	}
	return key
}

// snapshots reads ns's archive index and every body it names.
func (e *env) snapshots(t *testing.T) ([]storage.Entry, map[string][]storage.Entry) {
	t.Helper()
	index, err := e.store.Read(ctx, ArchiveStream(ns), "", 0)
	if err != nil {
		t.Fatal(err)
	}
	bodies := map[string][]storage.Entry{}
	for _, en := range index {
		_, uid, ok := parseArchiveKey(en.Key)
		if !ok {
			continue
		}
		if bodies[uid], err = e.store.Read(ctx, SnapshotStream(ns, uid), "", 0); err != nil {
			t.Fatal(err)
		}
	}
	return index, bodies
}

// drain empties rec.
func drain(rec *events.FakeRecorder) []string {
	var out []string
	for len(rec.Events) > 0 {
		out = append(out, <-rec.Events)
	}
	return out
}

// An archived task leaves one body with all of its logs and one index
// entry, then is deleted, with one TaskArchived Event.
func TestArchiveAfterSnapshotsThenDeletes(t *testing.T) {
	e, rec := archiveEnv(t)
	key := e.rich(t)
	uid := string(e.get(t, key).UID)
	e.clock.add(time.Hour)
	archived := e.clock.now()
	e.deleteFinished(t)
	if !e.gone(t, key) {
		t.Fatal("a is not archived an hour after it finished")
	}
	for _, stream := range []string{TranscriptStream(key, stepRun), EventsStream(key, stepRun), fileStream(key, "notes.md")} {
		if n := e.entries(t, stream); n != 0 {
			t.Errorf("%s has %d entries after the delete", stream, n)
		}
	}

	index, bodies := e.snapshots(t)
	if len(index) != 1 || index[0].Key != archiveKey(archived, uid) {
		t.Fatalf("index %+v, want one entry %s", index, archiveKey(archived, uid))
	}
	var sum SnapshotSummary
	if err := json.Unmarshal(index[0].Data, &sum); err != nil {
		t.Fatal(err)
	}
	if sum.Format != 1 || sum.UID != uid || sum.Namespace != ns || sum.Name != "a" || sum.Number != 1 ||
		sum.Phase != v1alpha1.PhaseCancelled || !sum.ArchivedAt.Time.Equal(archived) || sum.ArchiveAfter.Duration != time.Hour ||
		sum.FinishedAt == nil || !sum.FinishedAt.Time.Equal(archived.Add(-time.Hour)) {
		t.Fatalf("summary %+v", sum)
	}
	if len(bodies[uid]) != 1 || bodies[uid][0].Key != "snapshot" {
		t.Fatalf("body %+v, want one entry snapshot", bodies[uid])
	}
	body := bodies[uid][0].Data
	var snap Snapshot
	if err := json.Unmarshal(body, &snap); err != nil {
		t.Fatal(err)
	}
	if snap.UID != uid || snap.Format != 1 || snap.Task.Name != "a" || snap.Task.Status.DeletingSince == nil {
		t.Errorf("snapshot of %s, format %d, deletingSince %v", snap.Task.Name, snap.Format, snap.Task.Status.DeletingSince)
	}
	if len(snap.Thread) != 2 || snap.Thread[1].Body != "hi" {
		t.Errorf("thread %+v", snap.Thread)
	}
	if len(snap.Runs) != 3 || snap.Runs[0].ID != finishedRun || snap.Runs[1].ID != stepRun || snap.Runs[2].ID != runtimeRun {
		t.Errorf("runs %+v", snap.Runs)
	}
	if ev := snap.Events[stepRun]; len(ev) != 1 || ev[0].InputTokens != 5 {
		t.Errorf("events of the step run %+v", ev)
	}
	if ev := snap.Events[runtimeRun]; len(ev) != 1 || ev[0].Progress != "working" {
		t.Errorf("events of the runtime run %+v", ev)
	}
	if ev, ok := snap.Events[finishedRun]; !ok || ev == nil {
		t.Errorf("events of a run without any: %v, want []", ev)
	}
	if len(snap.Transcripts) != 1 {
		t.Errorf("transcripts of %d runs, want the step run's alone", len(snap.Transcripts))
	}
	if tr := snap.Transcripts[stepRun]; len(tr) != len(stepEntries) {
		t.Errorf("transcript %s, want the %d committed steps", tr, len(stepEntries))
	} else {
		for i, step := range tr {
			if string(step) != stepEntries[i] {
				t.Errorf("step %d %s, want %s as stored", i, step, stepEntries[i])
			}
		}
	}
	if len(snap.Files) != 1 || snap.Files[0].Path != "notes.md" || snap.Files[0].Version != 1 || snap.Files[0].Content != "hello" {
		t.Errorf("files %+v", snap.Files)
	}
	if !strings.Contains(string(body), stepEntries[0]) {
		t.Errorf("the body has not the provider's JSON as it came: %s", body)
	}

	evs := drain(rec)
	if len(evs) != 1 || evs[0] != "Normal TaskArchived Task #1 a finished 2026-10-24T09:00:00Z (Cancelled), archived 1h later by Team.spec.archiveAfter; its snapshot is kept for 2160h" {
		t.Fatalf("Events %q", evs)
	}
}

// A task with nothing but its object has a body of empty lists, not nulls.
func TestArchiveEmptyTask(t *testing.T) {
	e, _ := archiveEnv(t)
	key := e.addTask(t, "a", v1alpha1.AssigneeUser)
	if _, err := e.m.Cancel(ctx, key); err != nil {
		t.Fatal(err)
	}
	e.clock.add(time.Hour)
	e.deleteFinished(t)
	_, bodies := e.snapshots(t)
	if len(bodies) != 1 {
		t.Fatalf("%d snapshots, want 1", len(bodies))
	}
	for _, en := range bodies {
		for _, field := range []string{`"thread":[]`, `"runs":[]`, `"events":{}`, `"transcripts":{}`, `"files":[]`} {
			if !strings.Contains(string(en[0].Data), field) {
				t.Errorf("body without %s: %s", field, en[0].Data)
			}
		}
	}
}

// A Team with only deleteFinishedAfter deletes as before: no snapshot.
func TestDeleteFinishedAfterUnchanged(t *testing.T) {
	e, rec := deleteEnv(t)
	key := e.rich(t)
	e.clock.add(time.Hour)
	e.deleteFinished(t)
	if !e.gone(t, key) {
		t.Fatal("a is not deleted")
	}
	if n := e.entries(t, ArchiveStream(ns)); n != 0 {
		t.Fatalf("%d archive entries, want none", n)
	}
	if evs := drain(rec); len(evs) != 1 || !strings.HasPrefix(evs[0], "Normal TaskDeleted ") {
		t.Fatalf("Events %q, want one TaskDeleted", evs)
	}
}

// With both set, archiveAfter decides when, and the task is archived.
func TestArchiveAfterWinsOverDeleteFinishedAfter(t *testing.T) {
	e, rec := archiveEnv(t)
	e.setTeam(t, func(s *v1alpha1.TeamSpec) {
		s.ArchiveAfter = &metav1.Duration{Duration: 3 * time.Hour}
		s.DeleteFinishedAfter = &metav1.Duration{Duration: time.Hour}
	})
	key := e.finished(t, "a")
	e.clock.add(2 * time.Hour)
	e.deleteFinished(t)
	if e.gone(t, key) {
		t.Fatal("a is deleted past deleteFinishedAfter, before archiveAfter")
	}
	e.clock.add(time.Hour)
	e.deleteFinished(t)
	if !e.gone(t, key) || e.entries(t, ArchiveStream(ns)) != 1 {
		t.Fatal("a is not archived past archiveAfter")
	}
	if evs := drain(rec); len(evs) != 1 || !strings.HasPrefix(evs[0], "Normal TaskArchived ") || !strings.Contains(evs[0], "archived 3h later") {
		t.Fatalf("Events %q, want one TaskArchived", evs)
	}
}

// failAppends is a store whose appends to the streams fail picks fail.
type failAppends struct {
	storage.Driver
	fail func(stream string) bool
}

func (f *failAppends) Append(ctx context.Context, stream, key string, data []byte) error {
	if f.fail != nil && f.fail(stream) {
		return errors.New("store down")
	}
	return f.Driver.Append(ctx, stream, key, data)
}

// A snapshot that fails to write stops the delete before anything is
// deleted, whichever of its writes fails; once the store takes them, the
// next tick archives the task, with one snapshot.
func TestArchiveSnapshotFails(t *testing.T) {
	for name, fail := range map[string]func(string) bool{
		"body":  func(stream string) bool { return strings.Contains(stream, "/_archive/") },
		"index": func(stream string) bool { return strings.HasSuffix(stream, "/_archive") },
	} {
		e, rec := archiveEnv(t)
		key := e.rich(t)
		e.clock.add(time.Hour)
		store := &failAppends{Driver: e.store, fail: fail}
		e.m = New(store, Config{Agents: testAgents, Events: rec}, WithClock(e.clock.now))
		err := e.m.DeleteFinished(ctx)
		if err == nil || !strings.Contains(err.Error(), "snapshot: store down") {
			t.Fatalf("%s: tick error %v, want a snapshot error", name, err)
		}
		if e.gone(t, key) || e.get(t, key).Status.DeletingSince == nil || e.entries(t, TranscriptStream(key, stepRun)) != 3 || len(rec.Events) != 0 {
			t.Fatalf("%s: want the task marked and kept with its logs, no Event", name)
		}
		if name == "body" && e.entries(t, ArchiveStream(ns)) != 0 {
			t.Fatal("an index entry is written before its body")
		}
		store.fail = nil
		e.deleteFinished(t)
		index, bodies := e.snapshots(t)
		if !e.gone(t, key) || len(index) != 1 || len(bodies) != 1 || len(rec.Events) != 1 {
			t.Fatalf("%s: gone %v, %d index entries, %d bodies, %d Events: want gone, 1, 1, 1", name, e.gone(t, key), len(index), len(bodies), len(rec.Events))
		}
	}
}

// Of two replicas archiving one task, both write the snapshot, which is
// there once, and the one whose delete lands records the Event.
func TestArchiveTwoManagers(t *testing.T) {
	e, rec := archiveEnv(t)
	key := e.rich(t)
	e.clock.add(time.Hour)
	other := e.deleter(rec, nil)
	// The other replica archives it after this one's snapshot, before its
	// stream delete.
	slow := e.deleter(rec, func(ctx context.Context, _ *v1alpha1.Task) error {
		return other.DeleteFinished(ctx)
	})
	if err := slow.DeleteFinished(ctx); err != nil {
		t.Fatal(err)
	}
	index, bodies := e.snapshots(t)
	if !e.gone(t, key) || len(index) != 1 || len(bodies) != 1 {
		t.Fatalf("gone %v with %d index entries and %d bodies, want gone with 1 each", e.gone(t, key), len(index), len(bodies))
	}
	if evs := drain(rec); len(evs) != 1 || !strings.HasPrefix(evs[0], "Normal TaskArchived ") {
		t.Fatalf("Events %q, want one TaskArchived", evs)
	}
}

// bigKeys is a store that reports the entries of streams ending in suffix
// as size bytes each.
type bigKeys struct {
	storage.Driver
	suffix string
	size   int64
}

func (b bigKeys) Keys(ctx context.Context, stream, after string, limit int) ([]storage.KeyInfo, error) {
	keys, err := b.Driver.Keys(ctx, stream, after, limit)
	if strings.HasSuffix(stream, b.suffix) {
		for i := range keys {
			keys[i].Size = b.size
		}
	}
	return keys, err
}

// A task whose logs measure over the cap, a transcript alone or a file
// alone, is deleted without a snapshot, with one SnapshotSkipped Event.
func TestArchiveOverCap(t *testing.T) {
	for name, over := range map[string]func(*env, storage.Key) storage.Driver{
		"transcript": func(e *env, _ storage.Key) storage.Driver {
			// Over the cap with its attempt entry alone.
			return bigKeys{e.store, "/transcript/" + stepRun, MaxSnapshotBytes/3 + 1}
		},
		"file": func(e *env, key storage.Key) storage.Driver {
			if err := e.store.Append(ctx, filesIndexStream(key), storage.SeqKey(2), []byte(fmt.Sprintf(`{"path":"big","version":1,"size":%d}`, MaxSnapshotBytes+1))); err != nil {
				t.Fatal(err)
			}
			return e.store
		},
	} {
		e, rec := archiveEnv(t)
		key := e.rich(t)
		e.clock.add(time.Hour)
		m := New(over(e, key), Config{Agents: testAgents, Events: rec}, WithClock(e.clock.now))
		if err := m.DeleteFinished(ctx); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !e.gone(t, key) {
			t.Fatalf("%s: the task over the cap is kept", name)
		}
		if n := e.entries(t, ArchiveStream(ns)); n != 0 {
			t.Fatalf("%s: %d archive entries, want none", name, n)
		}
		want := "Warning SnapshotSkipped Task #1 a finished 2026-10-24T09:00:00Z (Cancelled), deleted 1h later by Team.spec.archiveAfter without a snapshot: its logs are 51 MiB, over the 50 MiB cap"
		if evs := drain(rec); len(evs) != 1 || evs[0] != want {
			t.Fatalf("%s: Events %q, want %q", name, evs, want)
		}
	}
}

// The index key of a task is the same whether its deletingSince is read
// with nanoseconds or, as a store keeps it, to the second.
func TestArchiveKeyIsStable(t *testing.T) {
	at := time.Date(2026, 11, 9, 13, 0, 0, 123456789, time.FixedZone("x", 3600))
	tk := &v1alpha1.Task{Status: v1alpha1.TaskStatus{DeletingSince: &metav1.Time{Time: at}}}
	data, err := json.Marshal(tk)
	if err != nil {
		t.Fatal(err)
	}
	var stored v1alpha1.Task
	if err := json.Unmarshal(data, &stored); err != nil {
		t.Fatal(err)
	}
	if a, b := archiveKey(archivedAt(tk), "u-1"), archiveKey(archivedAt(&stored), "u-1"); a != b {
		t.Fatalf("keys %s and %s, want the same", a, b)
	}
	got, uid, ok := parseArchiveKey(archiveKey(archivedAt(tk), "u-1"))
	if !ok || uid != "u-1" || !got.Equal(at.Truncate(time.Second)) {
		t.Fatalf("parsed %v %q %v, want %v u-1", got, uid, ok, at.Truncate(time.Second))
	}
}

// archive archives a new finished task name an hour from now, and
// returns its uid and when it was archived.
func (e *env) archive(t *testing.T, name string) (types.UID, time.Time) {
	t.Helper()
	key := e.finished(t, name)
	uid := e.get(t, key).UID
	e.clock.add(time.Hour)
	e.deleteFinished(t)
	if !e.gone(t, key) {
		t.Fatalf("%s is not archived", name)
	}
	return uid, e.clock.now()
}

// Snapshots past the Team's retention go, oldest first, body and index
// entry, and the rest stay; a Team that no longer archives still removes
// its old ones; archiveRetention shortens it.
func TestExpireSnapshots(t *testing.T) {
	e, _ := archiveEnv(t)
	day := 24 * time.Hour
	at := func(when time.Time) { e.clock.add(when.Sub(e.clock.now())) }
	kept := func(want ...types.UID) {
		t.Helper()
		e.deleteFinished(t)
		index, bodies := e.snapshots(t)
		var got []types.UID
		for _, en := range index {
			_, uid, _ := parseArchiveKey(en.Key)
			got = append(got, types.UID(uid))
			if len(bodies[uid]) != 1 {
				t.Errorf("snapshot %s has %d body entries", uid, len(bodies[uid]))
			}
		}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("snapshots %v, want %v", got, want)
		}
	}
	a, aAt := e.archive(t, "a")
	e.clock.add(10 * day)
	b, bAt := e.archive(t, "b")
	e.clock.add(10 * day)
	c, cAt := e.archive(t, "c")

	at(aAt.Add(90*day - time.Second))
	kept(a, b, c)
	at(bAt.Add(90 * day))
	kept(c)
	for _, uid := range []types.UID{a, b} {
		if n := e.entries(t, SnapshotStream(ns, string(uid))); n != 0 {
			t.Errorf("body of %s has %d entries after it expired", uid, n)
		}
	}
	kept(c)

	e.setTeam(t, func(s *v1alpha1.TeamSpec) { s.ArchiveAfter = nil })
	at(cAt.Add(90 * day))
	kept()

	e.setTeam(t, func(s *v1alpha1.TeamSpec) {
		s.ArchiveAfter = &metav1.Duration{Duration: time.Hour}
		s.ArchiveRetention = &metav1.Duration{Duration: 2 * time.Hour}
	})
	d, dAt := e.archive(t, "d")
	at(dAt.Add(2*time.Hour - time.Second))
	kept(d)
	at(dAt.Add(2 * time.Hour))
	kept()
}

// failArchiveStreams is a store whose deletes of snapshot bodies fail.
type failArchiveStreams struct{ storage.Driver }

func (f failArchiveStreams) DeleteStream(ctx context.Context, stream string) error {
	if strings.Contains(stream, "/_archive/") {
		return errors.New("store down")
	}
	return f.Driver.DeleteStream(ctx, stream)
}

// An expiry that fails is reported, and the tick's deletes still happen.
func TestExpireSnapshotsNeverBlocksDeletes(t *testing.T) {
	e, rec := archiveEnv(t)
	e.archive(t, "old")
	e.clock.add(DefaultArchiveRetention - time.Hour)
	key := e.finished(t, "a")
	e.clock.add(time.Hour)
	failing := New(failArchiveStreams{e.store}, Config{Agents: testAgents, Events: rec}, WithClock(e.clock.now))
	err := failing.DeleteFinished(ctx)
	if err == nil || !strings.Contains(err.Error(), "expire snapshots: store down") {
		t.Fatalf("tick error %v, want the expiry's", err)
	}
	if !e.gone(t, key) {
		t.Fatal("a due task is kept when the expiry fails")
	}
	if n := e.entries(t, ArchiveStream(ns)); n != 2 {
		t.Fatalf("%d index entries, want the old one kept and a's", n)
	}
}

// An index entry whose key cannot be read is left, and the expired ones
// after it still go.
func TestExpireSnapshotsSkipsUnknownKeys(t *testing.T) {
	e, _ := archiveEnv(t)
	if err := e.store.Append(ctx, ArchiveStream(ns), "0-unknown", []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	e.archive(t, "a")
	e.clock.add(DefaultArchiveRetention)
	e.deleteFinished(t)
	index, _ := e.snapshots(t)
	if len(index) != 1 || index[0].Key != "0-unknown" {
		t.Fatalf("index %+v, want the unknown key alone", index)
	}
}
