package manager

import (
	"context"
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

// CheckRun fails with task.ErrRunMismatch unless a fresh read shows the
// task still holding runID.
func (m *Manager) CheckRun(ctx context.Context, key storage.Key, runID string) error {
	_, err := m.activeRun(ctx, key, runID)
	return err
}

// agentAuthor is the thread author for a run's agent.
func agentAuthor(t *v1alpha1.Task) Author {
	name := "agent"
	if t.Spec.Assignee != nil {
		name = t.Spec.Assignee.Name
	}
	return Author{Kind: AuthorAgent, Name: name}
}

// RunPost posts the agent's comment to the thread. The comment is fenced:
// its body is written as a blob first, where nothing reads it, then
// committed with an outbox item in a run-fenced write, which the
// dispatcher turns into the thread entry. A run that lost its task cannot
// land one. The entry is applied at once when it can be; the returned
// entry's Seq is 0 when it is still pending, as the dispatcher writes it a
// moment later.
func (m *Manager) RunPost(ctx context.Context, key storage.Key, runID, body string) (*ThreadEntry, error) {
	t, err := m.activeRun(ctx, key, runID)
	if err != nil {
		return nil, err
	}
	item, err := m.threadItem(ctx, key, t, runID, EntryComment, body)
	if err != nil {
		return nil, err
	}
	if _, err := m.transition(ctx, key, task.Record{RunID: runID, Outbox: []v1alpha1.OutboxItem{item}}); err != nil {
		return nil, err
	}
	return m.appliedEntry(ctx, key, item), nil
}

// RunAsk posts the agent's question and ends the run as Waiting{Question},
// in one fenced write: the Finish carries the Question's outbox item, and
// the wait names it until the dispatcher has written it. When it returns
// without error the run is over: the runner must stop, without finishing.
// A retry after the Finish landed fails as the run is gone, and the one
// Question is the first attempt's.
func (m *Manager) RunAsk(ctx context.Context, key storage.Key, runID, question string) (*ThreadEntry, error) {
	t, err := m.activeRun(ctx, key, runID)
	if err != nil {
		return nil, err
	}
	item, err := m.threadItem(ctx, key, t, runID, EntryQuestion, question)
	if err != nil {
		return nil, err
	}
	if _, _, err := m.Finish(ctx, key, FinishRequest{
		RunID: runID, Outcome: task.OutcomeWaiting, Reason: v1alpha1.WaitingQuestion,
		WaitingItem: item.ID, Outbox: []v1alpha1.OutboxItem{item},
	}); err != nil {
		return nil, err
	}
	return m.appliedEntry(ctx, key, item), nil
}

// MaxThreadPage is the most thread entries a run reads in one call.
const MaxThreadPage = 200

// RunReadThread returns up to limit thread entries after sequence number
// after, oldest first; limit 0 or over MaxThreadPage means MaxThreadPage.
func (m *Manager) RunReadThread(ctx context.Context, key storage.Key, runID string, after int64, limit int) ([]ThreadEntry, error) {
	if after < 0 || limit < 0 {
		return nil, fmt.Errorf("after and limit must not be negative: %w", ErrInvalid)
	}
	if _, err := m.activeRun(ctx, key, runID); err != nil {
		return nil, err
	}
	if limit == 0 || limit > MaxThreadPage {
		limit = MaxThreadPage
	}
	var from string
	if after > 0 {
		from = storage.SeqKey(after)
	}
	return readStream[ThreadEntry](ctx, m.store, ThreadStream(key), from, limit)
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
	subKey := storage.Key{Namespace: key.Namespace, Name: created.Name}
	// This call dispatches and numbers at once: the hints its writes send
	// are not followed meanwhile.
	defer m.busy(key)()
	defer m.busy(subKey)()
	if err := m.dispatchTask(ctx, key); err != nil {
		return created, nil
	}
	if sub, err := m.numberTask(ctx, subKey); err == nil {
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

// RunWriteFile writes a new version of a task file, fenced as RunPost is:
// the content as a blob, then a run-fenced outbox item the dispatcher
// turns into the version. The returned Version is 0 when it is still
// pending.
func (m *Manager) RunWriteFile(ctx context.Context, key storage.Key, runID, path, content string) (*FileInfo, error) {
	if err := validFile(path, &content); err != nil {
		return nil, err
	}
	if _, err := m.activeRun(ctx, key, runID); err != nil {
		return nil, err
	}
	after, err := m.latestFileVersion(ctx, key, path)
	if err != nil {
		return nil, err
	}
	item := v1alpha1.OutboxItem{ID: m.newID(), Kind: v1alpha1.OutboxWriteFile}
	blob := blobName(runID, item.ID)
	if err := m.writeBlob(ctx, key, blob, []byte(content)); err != nil {
		return nil, err
	}
	item.File = &v1alpha1.FileWrite{Blob: blob, Path: path, Size: int64(len(content)), After: after}
	if _, err := m.transition(ctx, key, task.Record{RunID: runID, Outbox: []v1alpha1.OutboxItem{item}}); err != nil {
		return nil, err
	}
	info := &FileInfo{Path: path, Size: len(content)}
	m.applyNow(ctx, key)
	if v, ok, err := m.findVersion(ctx, key, path, after, item.ID); err == nil && ok {
		info.Version = v.Version
	}
	return info, nil
}

// latestVersion returns the highest version in a file stream, 0 if none,
// from its keys alone.
func (m *Manager) latestVersion(ctx context.Context, stream string) (int64, error) {
	return m.latestSeq(ctx, stream, 0)
}

// appendSeq appends data under the next free sequence number after from,
// a number known to be taken (or 0), and returns it.
func (m *Manager) appendSeq(ctx context.Context, stream string, from int64, data []byte) (int64, error) {
	seq, err := m.latestSeq(ctx, stream, from)
	if err != nil {
		return 0, err
	}
	for range maxSeqRaces {
		seq++
		err := m.store.Append(ctx, stream, storage.SeqKey(seq), data)
		if !errors.Is(err, storage.ErrAlreadyExists) {
			return seq, err
		}
	}
	return 0, fmt.Errorf("%s: %w", stream, ErrBusy)
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
	versions, err := readStream[fileVersion](ctx, m.store, stream, storage.SeqKey(latest-1), 1)
	if err != nil {
		return nil, err
	}
	if len(versions) == 0 {
		return nil, fmt.Errorf("file %q: %w", path, storage.ErrNotFound)
	}
	content, err := m.readBlob(ctx, key, versions[0].Blob)
	if err != nil {
		return nil, err
	}
	return &File{FileInfo: FileInfo{Path: path, Version: latest, Size: len(content)}, Content: string(content)}, nil
}

// RunListFiles lists the task's files with their latest version and size,
// from the newest snapshot of the files index and the index entries after
// it.
func (m *Manager) RunListFiles(ctx context.Context, key storage.Key, runID string) ([]FileInfo, error) {
	if _, err := m.activeRun(ctx, key, runID); err != nil {
		return nil, err
	}
	return m.listFiles(ctx, key, 0)
}
