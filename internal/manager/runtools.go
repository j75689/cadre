package manager

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	"github.com/j75689/cadre/internal/storage"
	"github.com/j75689/cadre/internal/task"
)

// This file holds what a run may do through its MCP endpoint. Every
// operation is fenced: it acts only while the task still holds runID, and
// fails with task.ErrRunMismatch once the run has lost its task.

// activeRun reads the task fresh and checks it still holds runID.
func (m *Manager) activeRun(ctx context.Context, key storage.Key, runID string) (*v1alpha1.Task, error) {
	t, err := m.store.GetTask(ctx, key)
	if err != nil {
		return nil, err
	}
	if t.Status.Run == nil || t.Status.Run.ID != runID {
		return nil, task.ErrRunMismatch
	}
	return t, nil
}

// agentAuthor is the thread author for a run's agent.
func agentAuthor(t *v1alpha1.Task) Author {
	name := "agent"
	if t.Spec.Assignee != nil {
		name = t.Spec.Assignee.Name
	}
	return Author{Kind: AuthorAgent, Name: name}
}

// RunPost appends the agent's comment to the thread.
func (m *Manager) RunPost(ctx context.Context, key storage.Key, runID, body string) (*ThreadEntry, error) {
	t, err := m.activeRun(ctx, key, runID)
	if err != nil {
		return nil, err
	}
	return m.appendThread(ctx, key, agentAuthor(t), EntryComment, body, runID)
}

// RunAsk appends the agent's question and ends the run as
// Waiting{Question}. When it returns without error the run is over: the
// runner must stop, without finishing.
//
// There is one Question per ask: if the thread's latest entry is already
// this run's Question with the same text (a retry after ending the run
// failed), it is reused and only the run is ended.
func (m *Manager) RunAsk(ctx context.Context, key storage.Key, runID, question string) (*ThreadEntry, error) {
	t, err := m.activeRun(ctx, key, runID)
	if err != nil {
		return nil, err
	}
	entry, err := m.lastEntry(ctx, key)
	if err != nil {
		return nil, err
	}
	if entry == nil || entry.Kind != EntryQuestion || entry.RunID != runID || entry.Body != question {
		if entry, err = m.appendThread(ctx, key, agentAuthor(t), EntryQuestion, question, runID); err != nil {
			return nil, err
		}
	}
	if _, _, err := m.Finish(ctx, key, FinishRequest{
		RunID: runID, Outcome: task.OutcomeWaiting, Reason: v1alpha1.WaitingQuestion, ThreadSeq: entry.Seq,
	}); err != nil {
		return entry, err
	}
	return entry, nil
}

// lastEntry returns the thread's latest entry, or nil for an empty thread.
func (m *Manager) lastEntry(ctx context.Context, key storage.Key) (*ThreadEntry, error) {
	stream := ThreadStream(key)
	latest, err := m.latestSeq(ctx, stream, 0)
	if err != nil || latest == 0 {
		return nil, err
	}
	var after string
	if latest > 1 {
		after = storage.SeqKey(latest - 1)
	}
	entries, err := readStream[ThreadEntry](ctx, m.store, stream, after, 1)
	if err != nil || len(entries) == 0 {
		return nil, err
	}
	return &entries[0], nil
}

// SubtaskCreated is a subtask a run asked for.
type SubtaskCreated struct {
	Name   string `json:"name"`
	Number int64  `json:"number,omitempty"` // 0 when it is not numbered yet
}

// RunCreateSubtask records a subtask in the task's outbox (the Record
// transition, fenced on the run), then applies the outbox and numbers the
// subtask at once so the agent learns its number. Applying the outbox
// applies every item pending on the task, not only this one, which is what
// the dispatcher would do next anyway. That eager part is best effort: if
// it fails the subtask appears with the next dispatch, and Number is 0.
func (m *Manager) RunCreateSubtask(ctx context.Context, key storage.Key, runID string, spec v1alpha1.TaskSpec) (*SubtaskCreated, error) {
	if err := requireRunID(runID); err != nil {
		return nil, err
	}
	spec.ParentRef = nil // the dispatcher sets the parent
	item := v1alpha1.OutboxItem{ID: m.newID(), Kind: v1alpha1.OutboxCreateSubtask, Spec: &spec}
	if _, err := m.transition(ctx, key, task.Record{RunID: runID, Outbox: []v1alpha1.OutboxItem{item}}); err != nil {
		return nil, err
	}
	created := &SubtaskCreated{Name: task.SubtaskName(item)}
	if err := m.dispatchTask(ctx, key); err != nil {
		return created, nil
	}
	if sub, err := m.numberTask(ctx, storage.Key{Namespace: key.Namespace, Name: created.Name}); err == nil {
		created.Number = sub.Status.Number
	} else if sub, err := m.store.GetTask(ctx, storage.Key{Namespace: key.Namespace, Name: created.Name}); err == nil {
		created.Number = sub.Status.Number // numbered by someone else first
	}
	return created, nil
}

// File limits in M1.
const (
	MaxFileSize = 1 << 20 // bytes per write
	MaxFilePath = 255     // bytes
)

// FileInfo describes a task file's latest version.
type FileInfo struct {
	Path    string `json:"path"`
	Version int64  `json:"version"`
	Size    int    `json:"size"`
}

// File is one version of a task file.
type File struct {
	FileInfo
	Content string `json:"content"`
}

// A task's files: each path has its own stream of versions,
// <ns>/<task>/files/<base64url(path)>, keyed by version; the path is one
// segment, so no file stream sits under another stream's entry. An index
// stream, <ns>/<task>/files-index, records every write's path, version and
// size, so files can be listed without reading their contents. A write
// appends the version first and the index entry second; reads go to the
// version streams, so a write whose index entry was lost is still read, and
// the next write of that path indexes it again.

func filesIndexStream(key storage.Key) string {
	return key.Namespace + "/" + key.Name + "/files-index"
}

func fileStream(key storage.Key, path string) string {
	return key.Namespace + "/" + key.Name + "/files/" + base64.RawURLEncoding.EncodeToString([]byte(path))
}

func validFile(path string, content *string) error {
	switch {
	case path == "" || len(path) > MaxFilePath || !utf8.ValidString(path):
		return fmt.Errorf("path must be 1 to %d bytes of UTF-8: %w", MaxFilePath, ErrInvalid)
	case strings.ContainsRune(path, 0):
		return fmt.Errorf("path contains NUL: %w", ErrInvalid)
	case content != nil && len(*content) > MaxFileSize:
		return fmt.Errorf("content is %d bytes, the limit is %d: %w", len(*content), MaxFileSize, ErrInvalid)
	case content != nil && !utf8.ValidString(*content):
		return fmt.Errorf("content must be UTF-8 text: %w", ErrInvalid)
	}
	return nil
}

// RunWriteFile writes a new version of a task file.
func (m *Manager) RunWriteFile(ctx context.Context, key storage.Key, runID, path, content string) (*FileInfo, error) {
	if err := validFile(path, &content); err != nil {
		return nil, err
	}
	if _, err := m.activeRun(ctx, key, runID); err != nil {
		return nil, err
	}
	stream := fileStream(key, path)
	latest, err := m.latestVersion(ctx, stream)
	if err != nil {
		return nil, err
	}
	info := &FileInfo{Path: path, Version: latest + 1, Size: len(content)}
	for attempt := 0; ; attempt++ {
		if attempt == maxSeqRaces {
			return nil, fmt.Errorf("file %q: %w", path, ErrBusy)
		}
		err := m.store.Append(ctx, stream, storage.SeqKey(info.Version), []byte(content))
		if err == nil {
			break
		}
		if !errors.Is(err, storage.ErrAlreadyExists) {
			return nil, err
		}
		info.Version++ // another write took this version
	}
	data, err := json.Marshal(info)
	if err != nil {
		return nil, err
	}
	if err := m.appendSeq(ctx, filesIndexStream(key), data); err != nil {
		return nil, err
	}
	return info, nil
}

// latestVersion returns the highest version in a file stream, 0 if none,
// from its keys alone.
func (m *Manager) latestVersion(ctx context.Context, stream string) (int64, error) {
	return m.latestSeq(ctx, stream, 0)
}

// appendSeq appends data under the next free sequence number.
func (m *Manager) appendSeq(ctx context.Context, stream string, data []byte) error {
	seq, err := m.latestSeq(ctx, stream, 0)
	if err != nil {
		return err
	}
	for range maxSeqRaces {
		seq++
		err := m.store.Append(ctx, stream, storage.SeqKey(seq), data)
		if !errors.Is(err, storage.ErrAlreadyExists) {
			return err
		}
	}
	return fmt.Errorf("%s: %w", stream, ErrBusy)
}

// RunReadFile reads the latest version of a task file.
func (m *Manager) RunReadFile(ctx context.Context, key storage.Key, runID, path string) (*File, error) {
	if err := validFile(path, nil); err != nil {
		return nil, err
	}
	if _, err := m.activeRun(ctx, key, runID); err != nil {
		return nil, err
	}
	stream := fileStream(key, path)
	latest, err := m.latestVersion(ctx, stream)
	if err != nil {
		return nil, err
	}
	if latest == 0 {
		return nil, fmt.Errorf("file %q: %w", path, storage.ErrNotFound)
	}
	entries, err := m.store.Read(ctx, stream, storage.SeqKey(latest-1), 1)
	if err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("file %q: %w", path, storage.ErrNotFound)
	}
	return &File{FileInfo: FileInfo{Path: path, Version: latest, Size: len(entries[0].Data)}, Content: string(entries[0].Data)}, nil
}

// RunListFiles lists the task's files with their latest version and size.
func (m *Manager) RunListFiles(ctx context.Context, key storage.Key, runID string) ([]FileInfo, error) {
	if _, err := m.activeRun(ctx, key, runID); err != nil {
		return nil, err
	}
	writes, err := readStream[FileInfo](ctx, m.store, filesIndexStream(key), "", 0)
	if err != nil {
		return nil, err
	}
	latest := map[string]FileInfo{}
	var order []string
	for _, w := range writes {
		if _, seen := latest[w.Path]; !seen {
			order = append(order, w.Path)
		}
		if w.Version >= latest[w.Path].Version {
			latest[w.Path] = w
		}
	}
	out := make([]FileInfo, 0, len(order))
	for _, p := range order {
		out = append(out, latest[p])
	}
	return out, nil
}
