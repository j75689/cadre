package manager

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	"github.com/j75689/cadre/internal/storage"
	"github.com/j75689/cadre/internal/task"
)

// This file holds what humans do on the board: create, read and cancel
// tasks, write to threads, answer a waiting agent, and set up a Team.

// CreateTask creates a task and numbers it right away, so the caller can show
// its number. Without a name the task is named after a fresh lowercase ULID.
// If numbering fails the task still exists and NumberNew numbers it later.
func (m *Manager) CreateTask(ctx context.Context, namespace, name string, spec v1alpha1.TaskSpec) (*v1alpha1.Task, error) {
	if err := task.ValidateSpec(&spec); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	if _, err := m.store.GetTeam(ctx, namespace); err != nil {
		return nil, fmt.Errorf("namespace %s has no Team: %w", namespace, err)
	}
	if name == "" {
		name = strings.ToLower(m.newID())
	}
	t := &v1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}, Spec: spec}
	if err := m.store.CreateTask(ctx, t); err != nil {
		return nil, err
	}
	key := storage.Key{Namespace: namespace, Name: name}
	if numbered, err := m.numberTask(ctx, key); err == nil {
		return numbered, nil
	}
	return m.store.GetTask(ctx, key)
}

// GetTask reads a task fresh.
func (m *Manager) GetTask(ctx context.Context, key storage.Key) (*v1alpha1.Task, error) {
	return m.store.GetTask(ctx, key)
}

// ListTasks lists tasks from the index, in board order.
func (m *Manager) ListTasks(ctx context.Context, q storage.Query) ([]v1alpha1.Task, error) {
	return m.store.ListTasks(ctx, q)
}

// AuthorKind says who wrote a thread entry.
type AuthorKind string

// Author kinds.
const (
	AuthorUser   AuthorKind = "User"
	AuthorAgent  AuthorKind = "Agent"
	AuthorSystem AuthorKind = "System"
)

// Author is who wrote a thread entry.
type Author struct {
	Kind AuthorKind `json:"kind"`
	Name string     `json:"name"`
}

// EntryKind is what a thread entry is.
type EntryKind string

// Entry kinds.
const (
	EntryComment  EntryKind = "Comment"
	EntryQuestion EntryKind = "Question"
	EntryAnswer   EntryKind = "Answer"
)

// ThreadEntry is one entry of a task's thread.
type ThreadEntry struct {
	Seq    int64     `json:"seq"`
	Author Author    `json:"author"`
	Kind   EntryKind `json:"kind"`
	Body   string    `json:"body"`
	At     time.Time `json:"at"`
}

// ThreadStream is the log stream holding a task's thread.
func ThreadStream(key storage.Key) string { return key.Namespace + "/" + key.Name + "/thread" }

// maxSeqRaces bounds how often an append may find its sequence number taken.
const maxSeqRaces = 20

// AppendThread appends an entry to the task's thread. The entry claims the
// next sequence number with a create-only append, which is the commit; the
// Task's thread revision is then advanced as a hint for watchers (see
// task.ThreadAdvanced). A revision left behind by a crashed appender is
// skipped by reading the log past it.
func (m *Manager) AppendThread(ctx context.Context, key storage.Key, author Author, kind EntryKind, body string) (*ThreadEntry, error) {
	if author.Name == "" || strings.TrimSpace(body) == "" {
		return nil, fmt.Errorf("author and body are required: %w", ErrInvalid)
	}
	if author.Kind == "" {
		author.Kind = AuthorUser
	}
	t, err := m.store.GetTask(ctx, key)
	if err != nil {
		return nil, err
	}
	stream := ThreadStream(key)
	var rev int64
	if t.Status.Thread != nil {
		rev = t.Status.Thread.Revision
	}
	entry := &ThreadEntry{Seq: rev + 1, Author: author, Kind: kind, Body: body, At: m.now().UTC().Truncate(time.Second)}
	for range maxSeqRaces {
		data, err := json.Marshal(entry)
		if err != nil {
			return nil, err
		}
		err = m.store.Append(ctx, stream, storage.SeqKey(entry.Seq), data)
		if err == nil {
			// The entry is committed. The revision is a hint: if advancing
			// it fails, the next appender advances it past this entry.
			_, _ = m.transition(ctx, key, task.ThreadAdvanced{Seq: entry.Seq})
			return entry, nil
		}
		if !errors.Is(err, storage.ErrAlreadyExists) {
			return nil, err
		}
		last, err := m.lastSeq(ctx, stream, entry.Seq)
		if err != nil {
			return nil, err
		}
		entry.Seq = last + 1
	}
	return nil, fmt.Errorf("thread %s: %w", key, ErrBusy)
}

// lastSeq returns the highest sequence number in the thread, given that
// taken is known to exist.
func (m *Manager) lastSeq(ctx context.Context, stream string, taken int64) (int64, error) {
	last := taken
	entries, err := m.store.Read(ctx, stream, storage.SeqKey(taken), 0)
	if err != nil {
		return 0, err
	}
	for _, e := range entries {
		seq, err := strconv.ParseInt(e.Key, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("thread entry key %q: %w", e.Key, err)
		}
		last = max(last, seq)
	}
	return last, nil
}

// ReadThread returns up to limit entries after sequence number after.
func (m *Manager) ReadThread(ctx context.Context, key storage.Key, after int64, limit int) ([]ThreadEntry, error) {
	if _, err := m.store.GetTask(ctx, key); err != nil {
		return nil, err
	}
	var from string
	if after > 0 {
		from = storage.SeqKey(after)
	}
	entries, err := m.store.Read(ctx, ThreadStream(key), from, limit)
	if err != nil {
		return nil, err
	}
	out := make([]ThreadEntry, 0, len(entries))
	for _, e := range entries {
		var te ThreadEntry
		if err := json.Unmarshal(e.Data, &te); err != nil {
			return nil, fmt.Errorf("thread entry %s: %w", e.Key, err)
		}
		out = append(out, te)
	}
	return out, nil
}

// Answer is a human's reply to a waiting task: an Answer entry in the thread,
// then the task goes back to Todo.
//
// It refuses before writing anything when a fresh read shows the task is not
// Waiting. If the phase changes between that read and the transition, the
// entry stands as an ordinary comment and the call still fails with
// ErrInvalidTransition: the log is append-only, so the entry cannot be taken
// back.
func (m *Manager) Answer(ctx context.Context, key storage.Key, author Author, body string) (*v1alpha1.Task, *ThreadEntry, error) {
	t, err := m.store.GetTask(ctx, key)
	if err != nil {
		return nil, nil, err
	}
	if t.Status.Phase != v1alpha1.PhaseWaiting {
		return nil, nil, fmt.Errorf("reply to a %s task: %w", t.Status.Phase, task.ErrInvalidTransition)
	}
	entry, err := m.AppendThread(ctx, key, author, EntryAnswer, body)
	if err != nil {
		return nil, nil, err
	}
	t, err = m.transition(ctx, key, task.Reply{})
	if err != nil {
		return nil, entry, err
	}
	return t, entry, nil
}

// CancelTask cancels a task and returns it.
func (m *Manager) CancelTask(ctx context.Context, key storage.Key) (*v1alpha1.Task, error) {
	return m.transition(ctx, key, task.Cancel{})
}

// Runs returns the task's run history, oldest first.
func (m *Manager) Runs(ctx context.Context, key storage.Key) ([]v1alpha1.RunRecord, error) {
	if _, err := m.store.GetTask(ctx, key); err != nil {
		return nil, err
	}
	entries, err := m.store.Read(ctx, RunsStream(key), "", 0)
	if err != nil {
		return nil, err
	}
	out := make([]v1alpha1.RunRecord, 0, len(entries))
	for _, e := range entries {
		var rec v1alpha1.RunRecord
		if err := json.Unmarshal(e.Data, &rec); err != nil {
			return nil, fmt.Errorf("run history entry %s: %w", e.Key, err)
		}
		out = append(out, rec)
	}
	return out, nil
}

// RunEvents returns the progress events of one run, in order.
func (m *Manager) RunEvents(ctx context.Context, key storage.Key, runID string) ([]Event, error) {
	if _, err := m.store.GetTask(ctx, key); err != nil {
		return nil, err
	}
	if _, err := ulid.ParseStrict(runID); err != nil {
		return nil, fmt.Errorf("run id %q: %w", runID, ErrInvalid)
	}
	entries, err := m.store.Read(ctx, EventsStream(key, runID), "", 0)
	if err != nil {
		return nil, err
	}
	out := make([]Event, 0, len(entries))
	for _, e := range entries {
		var ev Event
		if err := json.Unmarshal(e.Data, &ev); err != nil {
			return nil, fmt.Errorf("event %s: %w", e.Key, err)
		}
		out = append(out, ev)
	}
	return out, nil
}

// InitTeam opts a namespace in to Cadre. It fails with ErrAlreadyExists if
// the namespace already has a Team.
func (m *Manager) InitTeam(ctx context.Context, namespace string, spec v1alpha1.TeamSpec) (*v1alpha1.Team, error) {
	if spec.MaxConcurrentRuns < 0 {
		return nil, fmt.Errorf("maxConcurrentRuns %d: %w", spec.MaxConcurrentRuns, ErrInvalid)
	}
	if spec.MaxConcurrentRuns == 0 {
		spec.MaxConcurrentRuns = 1
	}
	team := &v1alpha1.Team{ObjectMeta: metav1.ObjectMeta{Name: v1alpha1.TeamName, Namespace: namespace}, Spec: spec}
	if err := m.store.CreateTeam(ctx, team); err != nil {
		return nil, err
	}
	return team, nil
}

// ListTeams lists every Team.
func (m *Manager) ListTeams(ctx context.Context) ([]v1alpha1.Team, error) {
	return m.store.ListTeams(ctx)
}
