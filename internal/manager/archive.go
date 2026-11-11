package manager

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	"github.com/j75689/cadre/internal/storage"
)

// SnapshotFormat is the version of the Snapshot JSON written.
const SnapshotFormat = 1

// MaxSnapshotBytes caps a task's logs, as their stored sizes add up, for a
// snapshot: a task over it is deleted without one.
const MaxSnapshotBytes = 50 << 20

// DefaultArchiveRetention is how long a snapshot is kept when the Team
// sets no spec.archiveRetention.
const DefaultArchiveRetention = 90 * 24 * time.Hour

// expireBatch bounds the snapshots one tick removes in one Team.
const expireBatch = 100

// snapshotKey is the key of a snapshot body's one entry.
const snapshotKey = "snapshot"

// SnapshotSummary describes an archived task: the entry of the Team's
// archive index, made from the Task object alone, so every replica writes
// the same one.
type SnapshotSummary struct {
	Format       int                `json:"format"`
	UID          string             `json:"uid"`
	Namespace    string             `json:"namespace"`
	Name         string             `json:"name"`
	Number       int64              `json:"number"`
	Title        string             `json:"title"`
	Phase        v1alpha1.Phase     `json:"phase"`
	Priority     string             `json:"priority,omitempty"`
	Assignee     *v1alpha1.Assignee `json:"assignee,omitempty"`
	Labels       []string           `json:"labels,omitempty"`
	FinishedAt   *metav1.Time       `json:"finishedAt,omitempty"`
	ArchivedAt   metav1.Time        `json:"archivedAt"`
	ArchiveAfter metav1.Duration    `json:"archiveAfter"`
}

// Snapshot is a read-only copy of an archived task and its logs.
type Snapshot struct {
	SnapshotSummary
	// Task is as read when marked deleting: metadata, spec and status.
	Task v1alpha1.Task `json:"task"`
	// Thread is every entry, by sequence number.
	Thread []ThreadEntry `json:"thread"`
	// Runs is the run history, by id.
	Runs []v1alpha1.RunRecord `json:"runs"`
	// Events are each run's progress events, by run id, in order.
	Events map[string][]Event `json:"events"`
	// Transcripts are each step-mode run's committed steps as stored, by
	// run id, in step order.
	Transcripts map[string][]json.RawMessage `json:"transcripts"`
	// Files are the latest version of each path, with its content.
	Files []File `json:"files"`
}

// ArchiveStream is the log stream indexing a Team's snapshots, oldest
// first.
func ArchiveStream(ns string) string { return ns + "/_archive" }

// SnapshotStream is the log stream holding the snapshot of the task uid.
func SnapshotStream(ns, uid string) string { return ns + "/_archive/" + uid }

// archiveKey is a snapshot's index key, which sorts by archivedAt.
func archiveKey(archivedAt time.Time, uid string) string {
	return fmt.Sprintf("%020d-%s", archivedAt.UnixNano(), uid)
}

// parseArchiveKey reads archiveKey's time and uid back.
func parseArchiveKey(key string) (time.Time, string, bool) {
	nanos, uid, ok := strings.Cut(key, "-")
	if !ok || len(nanos) != 20 || uid == "" {
		return time.Time{}, "", false
	}
	n, err := strconv.ParseInt(nanos, 10, 64)
	if err != nil {
		return time.Time{}, "", false
	}
	return time.Unix(0, n).UTC(), uid, true
}

// archivedAt is when t was archived: when it was marked deleting, to the
// second, so every replica, and a re-read from a store that keeps only
// seconds, makes the same key.
func archivedAt(t *v1alpha1.Task) time.Time {
	return t.Status.DeletingSince.UTC().Truncate(time.Second)
}

// overCap measures the logs of key for a snapshot, and says why it gets
// none if they are over MaxSnapshotBytes.
func (m *Manager) overCap(ctx context.Context, key storage.Key) (string, error) {
	size, err := m.measureLogs(ctx, key)
	if err != nil || size <= MaxSnapshotBytes {
		return "", err
	}
	return fmt.Sprintf("its logs are %d MiB, over the %d MiB cap", (size+1<<20-1)>>20, MaxSnapshotBytes>>20), nil
}

// snapshot writes the snapshot of t, a task marked deleting, archived
// archiveAfter after it finished: its body, then its index entry. An entry
// already there was written by another replica, or by an earlier try, and
// counts as written; a body there is not made again, from logs a delete
// since may have taken part of.
func (m *Manager) snapshot(ctx context.Context, t *v1alpha1.Task, archiveAfter time.Duration) error {
	sum := summarize(t, archiveAfter)
	written, err := m.store.Keys(ctx, SnapshotStream(t.Namespace, string(t.UID)), "", 1)
	if err != nil {
		return err
	}
	if len(written) == 0 {
		body, err := m.snapshotBody(ctx, t, sum)
		if err != nil {
			return err
		}
		if err := m.store.Append(ctx, SnapshotStream(t.Namespace, string(t.UID)), snapshotKey, body); err != nil && !errors.Is(err, storage.ErrAlreadyExists) {
			return err
		}
	}
	summary, err := encodeSnapshot(sum)
	if err != nil {
		return err
	}
	if err := m.store.Append(ctx, ArchiveStream(t.Namespace), archiveKey(archivedAt(t), string(t.UID)), summary); err != nil && !errors.Is(err, storage.ErrAlreadyExists) {
		return err
	}
	return nil
}

// snapshotBody reads the logs of t into its snapshot's body, without the
// checks that refuse a task being deleted.
func (m *Manager) snapshotBody(ctx context.Context, t *v1alpha1.Task, sum SnapshotSummary) ([]byte, error) {
	key := storage.Key{Namespace: t.Namespace, Name: t.Name}
	var err error
	snap := Snapshot{
		SnapshotSummary: sum,
		Task:            *t,
		Events:          map[string][]Event{},
		Transcripts:     map[string][]json.RawMessage{},
	}
	if snap.Thread, err = readStream[ThreadEntry](ctx, m.store, ThreadStream(key), "", 0); err != nil {
		return nil, err
	}
	if snap.Runs, err = readStream[v1alpha1.RunRecord](ctx, m.store, RunsStream(key), "", 0); err != nil {
		return nil, err
	}
	for _, run := range snap.Runs {
		if snap.Events[run.ID], err = readStream[Event](ctx, m.store, EventsStream(key, run.ID), "", 0); err != nil {
			return nil, err
		}
		if run.Mode == v1alpha1.RunModeStep {
			if snap.Transcripts[run.ID], err = m.transcript(ctx, key, run.ID); err != nil {
				return nil, err
			}
		}
	}
	if snap.Files, err = m.archiveFiles(ctx, key); err != nil {
		return nil, err
	}
	return encodeSnapshot(snap)
}

// summarize makes t's index entry.
func summarize(t *v1alpha1.Task, archiveAfter time.Duration) SnapshotSummary {
	return SnapshotSummary{
		Format:       SnapshotFormat,
		UID:          string(t.UID),
		Namespace:    t.Namespace,
		Name:         t.Name,
		Number:       t.Status.Number,
		Title:        t.Spec.Title,
		Phase:        t.Status.Phase,
		Priority:     string(t.Spec.Priority),
		Assignee:     t.Spec.Assignee,
		Labels:       t.Spec.Labels,
		FinishedAt:   t.Status.FinishedAt,
		ArchivedAt:   metav1.NewTime(archivedAt(t)),
		ArchiveAfter: metav1.Duration{Duration: archiveAfter},
	}
}

// measureLogs adds up the stored sizes of what a snapshot of key holds,
// from the logs' keys and the files index, reading no entry's content:
// the thread, the run history, and each run's events and transcript, its
// attempt entries too, then each file's latest version.
func (m *Manager) measureLogs(ctx context.Context, key storage.Key) (int64, error) {
	var size int64
	add := func(stream string) ([]storage.KeyInfo, error) {
		keys, err := m.store.Keys(ctx, stream, "", 0)
		for _, k := range keys {
			size += k.Size
		}
		return keys, err
	}
	if _, err := add(ThreadStream(key)); err != nil {
		return 0, err
	}
	runs, err := add(RunsStream(key))
	if err != nil {
		return 0, err
	}
	// The history's keys are the run ids; a runtime run has no transcript.
	for _, run := range runs {
		if _, err := add(EventsStream(key, run.Key)); err != nil {
			return 0, err
		}
		if _, err := add(TranscriptStream(key, run.Key)); err != nil {
			return 0, err
		}
	}
	files, err := m.listFiles(ctx, key, 0)
	if err != nil {
		return 0, err
	}
	for _, f := range files {
		size += int64(f.Size)
	}
	return size, nil
}

// archiveFiles reads the latest version of each of key's files, as
// RunReadFile does.
func (m *Manager) archiveFiles(ctx context.Context, key storage.Key) ([]File, error) {
	infos, err := m.listFiles(ctx, key, 0)
	if err != nil {
		return nil, err
	}
	files := make([]File, 0, len(infos))
	for _, info := range infos {
		stream := fileStream(key, info.Path)
		latest, err := m.latestVersion(ctx, stream)
		if err != nil {
			return nil, err
		}
		versions, err := readStream[fileVersion](ctx, m.store, stream, storage.SeqKey(latest-1), 1)
		if err != nil {
			return nil, err
		}
		if len(versions) == 0 {
			return nil, fmt.Errorf("file %q: %w", info.Path, storage.ErrNotFound)
		}
		content, err := m.readBlob(ctx, key, versions[0].Blob)
		if err != nil {
			return nil, err
		}
		files = append(files, File{FileInfo: FileInfo{Path: info.Path, Version: latest, Size: len(content)}, Content: string(content)})
	}
	return files, nil
}

// encodeSnapshot encodes v without escaping HTML, as marshalEntry does, so
// the provider's JSON in a transcript stays as it came.
func encodeSnapshot(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// expireSnapshots removes the snapshots of ns archived retention ago or
// longer, up to expireBatch, oldest first: the body, then the index entry,
// so a crash between the two leaves an entry the next tick removes. A key
// it cannot read is left alone.
func (m *Manager) expireSnapshots(ctx context.Context, ns string, retention time.Duration) error {
	keys, err := m.store.Keys(ctx, ArchiveStream(ns), "", expireBatch)
	if err != nil {
		return err
	}
	now := m.now()
	for _, k := range keys {
		at, uid, ok := parseArchiveKey(k.Key)
		if !ok {
			m.log.Warn("cannot read an archive index key: its snapshot is not expired", "stream", ArchiveStream(ns), "key", k.Key)
			continue
		}
		if now.Sub(at) < retention {
			return nil // the keys after it are younger
		}
		if err := m.store.DeleteStream(ctx, SnapshotStream(ns, uid)); err != nil {
			return err
		}
		if err := m.store.DeleteEntry(ctx, ArchiveStream(ns), k.Key); err != nil {
			return err
		}
	}
	return nil
}

// ListSnapshots answers a page of the snapshots of ns, newest first, and
// how many there are: the index's keys say which, then a read gets the
// page's entries, which are next to each other in the index. An entry
// expired between the two is left out, not replaced by its neighbour; one
// inserted inside the page between the two is not in it, and the read goes
// on past it to the page's newest key.
func (m *Manager) ListSnapshots(ctx context.Context, ns string, offset, limit int) ([]SnapshotSummary, int, error) {
	keys, err := m.store.Keys(ctx, ArchiveStream(ns), "", 0)
	if err != nil {
		return nil, 0, err
	}
	// The page, oldest first, is keys[lo:hi].
	hi := len(keys) - offset
	if hi <= 0 {
		return []SnapshotSummary{}, len(keys), nil
	}
	lo := max(hi-limit, 0)
	after := ""
	if lo > 0 {
		after = keys[lo-1].Key
	}
	data := make(map[string][]byte, hi-lo)
	for {
		entries, err := m.store.Read(ctx, ArchiveStream(ns), after, hi-lo)
		if err != nil {
			return nil, 0, err
		}
		for _, en := range entries {
			data[en.Key] = en.Data
		}
		if len(entries) < hi-lo || entries[len(entries)-1].Key >= keys[hi-1].Key {
			break
		}
		after = entries[len(entries)-1].Key
	}
	items := make([]SnapshotSummary, 0, hi-lo)
	for i := hi - 1; i >= lo; i-- {
		d, ok := data[keys[i].Key]
		if !ok {
			continue
		}
		var sum SnapshotSummary
		if err := json.Unmarshal(d, &sum); err != nil {
			return nil, 0, fmt.Errorf("archive index entry %s: %w", keys[i].Key, err)
		}
		items = append(items, sum)
	}
	return items, len(keys), nil
}

// GetSnapshot is the body of the snapshot of the task uid in ns, as
// stored.
func (m *Manager) GetSnapshot(ctx context.Context, ns, uid string) (json.RawMessage, error) {
	entries, err := m.store.Read(ctx, SnapshotStream(ns, uid), "", 1)
	if err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("snapshot %s: %w", uid, storage.ErrNotFound)
	}
	return entries[0].Data, nil
}
