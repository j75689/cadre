package manager

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"time"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	"github.com/j75689/cadre/internal/storage"
	"github.com/j75689/cadre/internal/task"
)

// A run's thread entries and file versions are written in two phases, so a
// run that lost its task cannot land one:
//
//  1. The content is written as a blob, a stream of its own under
//     <ns>/<task>/blobs/, named <runId>.<itemId>, where nothing reads it.
//  2. A pointer to it is committed as an outbox item (AppendThread,
//     WriteFile) in a run-fenced conditional write on the Task: the commit
//     point.
//
// The dispatcher then writes the thread entry or the file version, once:
// the entry or version carries the item's id, and applying the item again
// finds it after the position the item recorded at commit and writes
// nothing. A blob whose item was never committed is garbage; collecting it
// is left for later.
//
// A file's version stream holds a pointer per version (fileVersion); the
// content is in the blob. Its index stream, <ns>/<task>/files-index,
// records every write; every 64 writes the dispatcher snapshots the latest
// version of every path into <ns>/<task>/files-snapshots, keyed by the index
// position it covers, so listing reads the newest snapshot and what came
// after it.

// fileVersion is one version of a file, in its version stream.
type fileVersion struct {
	ItemID string `json:"itemId"`
	Blob   string `json:"blob"`
	Size   int    `json:"size"`
}

// snapshotEvery is how many index entries pass between snapshots.
const snapshotEvery = 64

const blobKey = "blob"

func blobName(runID, itemID string) string { return runID + "." + itemID }

func blobStream(key storage.Key, name string) string {
	return key.Namespace + "/" + key.Name + "/blobs/" + name
}

func filesIndexStream(key storage.Key) string {
	return key.Namespace + "/" + key.Name + "/files-index"
}

func filesSnapshotStream(key storage.Key) string {
	return key.Namespace + "/" + key.Name + "/files-snapshots"
}

func fileStream(key storage.Key, path string) string {
	return key.Namespace + "/" + key.Name + "/files/" + base64.RawURLEncoding.EncodeToString([]byte(path))
}

// writeBlob writes content once; a retry of the same blob is a no-op.
func (m *Manager) writeBlob(ctx context.Context, key storage.Key, name string, content []byte) error {
	err := m.store.Append(ctx, blobStream(key, name), blobKey, content)
	if errors.Is(err, storage.ErrAlreadyExists) {
		return nil
	}
	return err
}

func (m *Manager) readBlob(ctx context.Context, key storage.Key, name string) ([]byte, error) {
	entries, err := m.store.Read(ctx, blobStream(key, name), "", 1)
	if err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("blob %s: %w", name, storage.ErrNotFound)
	}
	return entries[0].Data, nil
}

// threadItem writes body as a blob and returns the item committing it as
// a thread entry of kind by the run's agent.
func (m *Manager) threadItem(ctx context.Context, key storage.Key, t *v1alpha1.Task, runID string, kind EntryKind, body string) (v1alpha1.OutboxItem, error) {
	if err := validEntry(agentAuthor(t), kind, body); err != nil {
		return v1alpha1.OutboxItem{}, err
	}
	item := v1alpha1.OutboxItem{ID: m.newID(), Kind: v1alpha1.OutboxAppendThread}
	blob := blobName(runID, item.ID)
	if err := m.writeBlob(ctx, key, blob, []byte(body)); err != nil {
		return v1alpha1.OutboxItem{}, err
	}
	var after int64
	if t.Status.Thread != nil {
		after = t.Status.Thread.Revision
	}
	item.Thread = &v1alpha1.ThreadAppend{Blob: blob, Kind: string(kind), Author: agentAuthor(t).Name, RunID: runID, After: after}
	return item, nil
}

// appliedEntry applies the task's outbox at once and returns the entry the
// item wrote; one with Seq 0 when it is not written yet, which the
// dispatcher does a moment later.
func (m *Manager) appliedEntry(ctx context.Context, key storage.Key, item v1alpha1.OutboxItem) *ThreadEntry {
	pending := &ThreadEntry{Kind: EntryKind(item.Thread.Kind), Body: "", RunID: item.Thread.RunID, ItemID: item.ID}
	if err := m.dispatchTask(ctx, key); err != nil {
		return pending
	}
	if e, err := m.findEntry(ctx, key, item.Thread.After, item.ID); err == nil && e != nil {
		return e
	}
	return pending
}

// findEntry returns the thread entry item itemID wrote after after, or nil.
func (m *Manager) findEntry(ctx context.Context, key storage.Key, after int64, itemID string) (*ThreadEntry, error) {
	entries, err := m.ReadThread(ctx, key, after, 0)
	if err != nil {
		return nil, err
	}
	for i := range entries {
		if entries[i].ItemID == itemID {
			return &entries[i], nil
		}
	}
	return nil, nil
}

// Applying an AppendThread or WriteFile item writes exactly one entry, even
// with several dispatchers applying it at once (every replica hears the
// same watch event): the item first reserves where it writes, its Seq, with
// a compare-and-set on the Task (ReserveItem), and every applier then
// appends at that one key, create-only. If another write took the key
// first, the appliers reserve the next free one the same way: one wins and
// the others read its choice.

// pendingItem reads the task fresh and returns item id in its outbox, or
// nil when it is gone: applied and acknowledged already.
func (m *Manager) pendingItem(ctx context.Context, key storage.Key, id string) (*v1alpha1.Task, *v1alpha1.OutboxItem, error) {
	t, err := m.store.GetTask(ctx, key)
	if err != nil {
		return nil, nil, err
	}
	for i := range t.Status.Outbox {
		if t.Status.Outbox[i].ID == id {
			return t, &t.Status.Outbox[i], nil
		}
	}
	return t, nil, nil
}

// entryAt reads the entry at seq of a stream keyed by SeqKey.
func (m *Manager) entryAt(ctx context.Context, stream string, seq int64) ([]byte, bool, error) {
	entries, err := m.store.Read(ctx, stream, storage.SeqKey(seq-1), 1)
	if err != nil || len(entries) == 0 || entries[0].Key != storage.SeqKey(seq) {
		return nil, false, err
	}
	return entries[0].Data, true, nil
}

// placeItem writes an item's entry into stream at the item's reserved
// position, reserving one first, and returns the position. encode builds
// the entry for a position; mine says whether an entry found at the
// position is this item's. from is a position known to exist in the
// stream, where the search for a free one starts.
func (m *Manager) placeItem(ctx context.Context, key storage.Key, id, stream string, from int64, thread bool,
	encode func(seq int64) ([]byte, error), mine func([]byte) bool) (*v1alpha1.Task, int64, error) {
	for range maxSeqRaces {
		t, cur, err := m.pendingItem(ctx, key, id)
		if err != nil || cur == nil {
			return t, 0, err
		}
		if cur.Seq > 0 {
			data, err := encode(cur.Seq)
			if err != nil {
				return nil, 0, err
			}
			err = m.store.Append(ctx, stream, storage.SeqKey(cur.Seq), data)
			if err == nil {
				return t, cur.Seq, nil
			}
			if !errors.Is(err, storage.ErrAlreadyExists) {
				return nil, 0, err
			}
			there, ok, err := m.entryAt(ctx, stream, cur.Seq)
			if err != nil {
				return nil, 0, err
			}
			if ok && mine(there) {
				return t, cur.Seq, nil // another dispatcher wrote it
			}
		}
		// Unreserved, or the reserved key went to another write: reserve
		// the next free one.
		latest, err := m.latestSeq(ctx, stream, from)
		if err != nil {
			return nil, 0, err
		}
		if thread && t.Status.Thread != nil {
			latest = max(latest, t.Status.Thread.Revision)
		}
		if _, err := m.transition(ctx, key, task.ReserveItem{ID: id, From: cur.Seq, To: max(latest, cur.Seq) + 1, Thread: thread}); err != nil {
			return nil, 0, err
		}
	}
	return nil, 0, fmt.Errorf("outbox item %s: %w", id, ErrBusy)
}

// applyThread writes an AppendThread item's entry once, and points the
// task's wait at it if the wait is on this item.
func (m *Manager) applyThread(ctx context.Context, parent *v1alpha1.Task, it v1alpha1.OutboxItem) error {
	key := storage.Key{Namespace: parent.Namespace, Name: parent.Name}
	th := it.Thread
	body, err := m.readBlob(ctx, key, th.Blob)
	if err != nil {
		return err
	}
	var from int64
	if parent.Status.Thread != nil {
		from = parent.Status.Thread.Revision
	}
	at := m.now().UTC().Truncate(time.Second)
	t, seq, err := m.placeItem(ctx, key, it.ID, ThreadStream(key), from, true,
		func(seq int64) ([]byte, error) {
			return json.Marshal(ThreadEntry{
				Seq: seq, Author: Author{Kind: AuthorAgent, Name: th.Author}, Kind: EntryKind(th.Kind),
				Body: string(body), At: at, RunID: th.RunID, ItemID: it.ID,
			})
		},
		func(data []byte) bool {
			var e ThreadEntry
			return json.Unmarshal(data, &e) == nil && e.ItemID == it.ID
		})
	if err != nil || seq == 0 {
		return err
	}
	// The revision is a hint for watchers, as for any append.
	_, _ = m.transition(ctx, key, task.ThreadAdvanced{Seq: seq})
	if w := t.Status.Waiting; w != nil && w.ItemID == it.ID && w.ThreadSeq == 0 {
		if _, err := m.transition(ctx, key, task.WaitingAt{ItemID: it.ID, Seq: seq}); err != nil {
			return err
		}
	}
	return nil
}

// findVersion returns the version of path item itemID wrote after after.
func (m *Manager) findVersion(ctx context.Context, key storage.Key, path string, after int64, itemID string) (FileInfo, bool, error) {
	var from string
	if after > 0 {
		from = storage.SeqKey(after)
	}
	entries, err := m.store.Read(ctx, fileStream(key, path), from, 0)
	if err != nil {
		return FileInfo{}, false, err
	}
	for _, e := range entries {
		var v fileVersion
		if err := json.Unmarshal(e.Data, &v); err != nil {
			return FileInfo{}, false, fmt.Errorf("file %q version %s: %w", path, e.Key, err)
		}
		if v.ItemID == itemID {
			n, err := strconv.ParseInt(e.Key, 10, 64)
			if err != nil {
				return FileInfo{}, false, err
			}
			return FileInfo{Path: path, Version: n, Size: v.Size}, true, nil
		}
	}
	return FileInfo{}, false, nil
}

// applyFile writes a WriteFile item's version once, and records it in the
// files index.
func (m *Manager) applyFile(ctx context.Context, parent *v1alpha1.Task, it v1alpha1.OutboxItem) error {
	key := storage.Key{Namespace: parent.Namespace, Name: parent.Name}
	fw := it.File
	_, v, err := m.placeItem(ctx, key, it.ID, fileStream(key, fw.Path), fw.After, false,
		func(int64) ([]byte, error) {
			return json.Marshal(fileVersion{ItemID: it.ID, Blob: fw.Blob, Size: int(fw.Size)})
		},
		func(data []byte) bool {
			var fv fileVersion
			return json.Unmarshal(data, &fv) == nil && fv.ItemID == it.ID
		})
	if err != nil || v == 0 {
		return err
	}
	// The index is written after the version; a lost index entry is
	// written again by this item's next apply, and a duplicate one is
	// harmless, as listing keeps each path's highest version.
	data, err := json.Marshal(FileInfo{Path: fw.Path, Version: v, Size: int(fw.Size)})
	if err != nil {
		return err
	}
	seq, err := m.appendSeq(ctx, filesIndexStream(key), data)
	if err != nil {
		return err
	}
	return m.snapshotFiles(ctx, key, seq)
}

// snapshotFiles writes a snapshot of the files index up to seq when the
// last one is snapshotEvery entries behind. Snapshots are create-only, so
// two dispatchers writing one at once leave one.
func (m *Manager) snapshotFiles(ctx context.Context, key storage.Key, seq int64) error {
	last, err := m.latestSeq(ctx, filesSnapshotStream(key), 0)
	if err != nil || seq-last < snapshotEvery {
		return err
	}
	files, err := m.listFiles(ctx, key, seq)
	if err != nil {
		return err
	}
	data, err := json.Marshal(files)
	if err != nil {
		return err
	}
	err = m.store.Append(ctx, filesSnapshotStream(key), storage.SeqKey(seq), data)
	if errors.Is(err, storage.ErrAlreadyExists) {
		return nil
	}
	return err
}

// listFiles is every path's latest version, in the order paths were first
// written, from the newest snapshot and the index entries after it, up to
// index position upTo (0 for all).
func (m *Manager) listFiles(ctx context.Context, key storage.Key, upTo int64) ([]FileInfo, error) {
	var files []FileInfo
	snap, err := m.latestSeq(ctx, filesSnapshotStream(key), 0)
	if err != nil {
		return nil, err
	}
	if upTo > 0 && snap > upTo {
		snap = 0 // never in practice: snapshots only cover what was written
	}
	if snap > 0 {
		entries, err := m.store.Read(ctx, filesSnapshotStream(key), storage.SeqKey(snap-1), 1)
		if err != nil {
			return nil, err
		}
		if len(entries) == 1 {
			if err := json.Unmarshal(entries[0].Data, &files); err != nil {
				return nil, fmt.Errorf("files snapshot %d: %w", snap, err)
			}
		} else {
			snap = 0
		}
	}
	var from string
	if snap > 0 {
		from = storage.SeqKey(snap)
	}
	entries, err := m.store.Read(ctx, filesIndexStream(key), from, 0)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if upTo > 0 && e.Key > storage.SeqKey(upTo) {
			break
		}
		var w FileInfo
		if err := json.Unmarshal(e.Data, &w); err != nil {
			return nil, fmt.Errorf("files index %s: %w", e.Key, err)
		}
		if i := slices.IndexFunc(files, func(f FileInfo) bool { return f.Path == w.Path }); i < 0 {
			files = append(files, w)
		} else if w.Version >= files[i].Version {
			files[i] = w
		}
	}
	if files == nil {
		files = []FileInfo{}
	}
	return files, nil
}
