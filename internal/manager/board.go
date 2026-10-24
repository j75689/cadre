package manager

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	"github.com/j75689/cadre/internal/storage"
	"github.com/j75689/cadre/internal/task"
)

// This file holds what humans do on the board: create, read and cancel
// tasks, write to threads, answer a waiting agent, and set up a Team.

// CreateTask creates a task and numbers it right away, so the caller can show
// its number. Without a name the task is named after a fresh lowercase ULID.
// The spec is checked as the API server would check it, so every driver
// stores the same thing. If numbering fails the task still exists and
// NumberNew numbers it later.
func (m *Manager) CreateTask(ctx context.Context, namespace, name string, spec v1alpha1.TaskSpec) (*v1alpha1.Task, error) {
	if err := task.ValidateSpec(&spec); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	if err := validNamespace(namespace); err != nil {
		return nil, err
	}
	if name != "" {
		if errs := validation.IsDNS1123Subdomain(name); len(errs) > 0 {
			return nil, fmt.Errorf("task name %q: %s: %w", name, strings.Join(errs, "; "), ErrInvalid)
		}
	}
	if spec.ParentRef != nil && name != "" && spec.ParentRef.Name == name {
		return nil, fmt.Errorf("a task cannot be its own parent: %w", ErrInvalid)
	}
	if _, err := m.store.GetTeam(ctx, namespace); err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return nil, fmt.Errorf("namespace %s has no Team: %w", namespace, err)
		}
		return nil, err
	}
	if name == "" {
		name = strings.ToLower(m.newID())
	}
	t := &v1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}, Spec: spec}
	key := storage.Key{Namespace: namespace, Name: name}
	// This call numbers the task itself: the hint its creation sends is
	// not followed meanwhile, so the task gets one number.
	defer m.busy(key)()
	if err := m.store.CreateTask(ctx, t); err != nil {
		return nil, err
	}
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
	// A forwarded tool call that must ask first, the human's decision on
	// it, and what the call returned.
	EntryToolCall   EntryKind = "ToolCall"
	EntryApproved   EntryKind = "Approved"
	EntryDenied     EntryKind = "Denied"
	EntryToolResult EntryKind = "ToolResult"
)

var (
	authorKinds = []AuthorKind{AuthorUser, AuthorAgent, AuthorSystem}
	entryKinds  = []EntryKind{EntryComment, EntryQuestion, EntryAnswer, EntryToolCall, EntryApproved, EntryDenied, EntryToolResult}
	// callKinds are the entry kinds that carry a ToolCall, and only they do.
	callKinds = []EntryKind{EntryToolCall, EntryApproved, EntryDenied, EntryToolResult}
)

// ThreadEntry is one entry of a task's thread.
type ThreadEntry struct {
	Seq    int64     `json:"seq"`
	Author Author    `json:"author"`
	Kind   EntryKind `json:"kind"`
	Body   string    `json:"body"`
	At     time.Time `json:"at"`
	// RunID is the run that wrote an agent's entry.
	RunID string `json:"runId,omitempty"`
	// Call is the tool call a ToolCall, Approved, Denied or ToolResult
	// entry is about.
	Call *ToolCall `json:"call,omitempty"`
	// ItemID is the outbox item that wrote a run's fenced entry.
	ItemID string `json:"itemId,omitempty"`
}

// ThreadStream is the log stream holding a task's thread.
func ThreadStream(key storage.Key) string { return key.Namespace + "/" + key.Name + "/thread" }

// maxSeqRaces bounds how often an append may find its sequence number taken.
const maxSeqRaces = 20

// AppendThread appends an entry to the task's thread. The entry claims the
// next sequence number with a create-only append, which is the commit; the
// Task's thread revision is then advanced as a hint for watchers (see
// task.ThreadAdvanced). A revision left behind by a crashed appender is
// skipped by reading the log past it. Author and entry kinds are required:
// nobody is recorded as a human by default.
func (m *Manager) AppendThread(ctx context.Context, key storage.Key, author Author, kind EntryKind, body string) (*ThreadEntry, error) {
	return m.appendThread(ctx, key, author, kind, body, "")
}

// appendThread is AppendThread, recording the run that wrote the entry.
func (m *Manager) appendThread(ctx context.Context, key storage.Key, author Author, kind EntryKind, body, runID string) (*ThreadEntry, error) {
	return m.appendEntry(ctx, key, &ThreadEntry{Author: author, Kind: kind, Body: body, RunID: runID})
}

// appendEntry appends entry under the next sequence number, setting its Seq
// and time.
func (m *Manager) appendEntry(ctx context.Context, key storage.Key, entry *ThreadEntry) (*ThreadEntry, error) {
	if err := validEntry(entry.Author, entry.Kind, entry.Body); err != nil {
		return nil, err
	}
	if slices.Contains(callKinds, entry.Kind) != (entry.Call != nil) {
		return nil, fmt.Errorf("a %s entry and its call: %w", entry.Kind, ErrInvalid)
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
	entry.Seq, entry.At = rev+1, m.now().UTC().Truncate(time.Second)
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

// validEntry checks an entry's author, kind and body.
func validEntry(author Author, kind EntryKind, body string) error {
	switch {
	case author.Name == "" || strings.TrimSpace(body) == "":
		return fmt.Errorf("author and body are required: %w", ErrInvalid)
	case !slices.Contains(authorKinds, author.Kind):
		return fmt.Errorf("author kind %q: %w", author.Kind, ErrInvalid)
	case !slices.Contains(entryKinds, kind):
		return fmt.Errorf("entry kind %q: %w", kind, ErrInvalid)
	}
	return nil
}

// lastSeq returns the highest sequence number in the thread, given that
// taken is known to exist. It lists keys only.
func (m *Manager) lastSeq(ctx context.Context, stream string, taken int64) (int64, error) {
	return m.latestSeq(ctx, stream, taken)
}

// latestSeq returns the highest sequence number in a stream keyed by
// SeqKey, starting from one known to exist (0 for none). It lists keys
// after it, never entry contents, and makes no assumption that the
// numbers are dense.
func (m *Manager) latestSeq(ctx context.Context, stream string, from int64) (int64, error) {
	after := ""
	if from > 0 {
		after = storage.SeqKey(from)
	}
	keys, err := m.store.Keys(ctx, stream, after, 0)
	if err != nil {
		return 0, err
	}
	latest := from
	for _, k := range keys {
		seq, err := strconv.ParseInt(k.Key, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("%s key %q: %w", stream, k.Key, err)
		}
		latest = max(latest, seq)
	}
	return latest, nil
}

// readStream reads a log stream after the key after, decoding each entry.
func readStream[T any](ctx context.Context, store storage.Driver, stream, after string, limit int) ([]T, error) {
	entries, err := store.Read(ctx, stream, after, limit)
	if err != nil {
		return nil, err
	}
	out := make([]T, 0, len(entries))
	for _, e := range entries {
		var v T
		if err := json.Unmarshal(e.Data, &v); err != nil {
			return nil, fmt.Errorf("%s entry %s: %w", stream, e.Key, err)
		}
		out = append(out, v)
	}
	return out, nil
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
	return readStream[ThreadEntry](ctx, m.store, ThreadStream(key), from, limit)
}

// Answer is a human's reply to a waiting task: an Answer entry in the
// thread, then the task goes back to Todo.
//
// It refuses before writing anything when a fresh read shows the task is not
// Waiting. If the phase changes between that read and the transition, the
// entry stands as an ordinary comment and the call still fails with
// ErrInvalidTransition: the log is append-only, so the entry cannot be taken
// back.
//
// There is at most one Answer per wait. If the latest entry written since
// the task started waiting is already an Answer (a retry after the Reply
// transition failed, say), it is reused and only the transition is applied.
func (m *Manager) Answer(ctx context.Context, key storage.Key, author Author, body string) (*v1alpha1.Task, *ThreadEntry, error) {
	t, err := m.store.GetTask(ctx, key)
	if err != nil {
		return nil, nil, err
	}
	w := t.Status.Waiting
	if t.Status.Phase != v1alpha1.PhaseWaiting || w == nil {
		return nil, nil, fmt.Errorf("reply to a %s task: %w", t.Status.Phase, task.ErrInvalidTransition)
	}
	if w.Reason == v1alpha1.WaitingToolApproval {
		return nil, nil, fmt.Errorf("the task waits for a tool approval; approve or deny it: %w", task.ErrInvalidTransition)
	}
	if w.ItemID != "" && w.ThreadSeq == 0 {
		// The Question is committed but not in the thread yet: an answer
		// now would come before it.
		return nil, nil, ErrEntryPending
	}
	since, err := m.ReadThread(ctx, key, w.ThreadSeq, 0)
	if err != nil {
		return nil, nil, err
	}
	since = slices.DeleteFunc(since, func(e ThreadEntry) bool { return e.At.Before(w.Since.Time) })
	var entry *ThreadEntry
	if n := len(since); n > 0 && since[n-1].Kind == EntryAnswer {
		entry = &since[n-1]
	} else if entry, err = m.AppendThread(ctx, key, author, EntryAnswer, body); err != nil {
		return nil, nil, err
	}
	t, err = m.transition(ctx, key, task.Reply{ThreadSeq: w.ThreadSeq})
	if err != nil {
		return nil, entry, err
	}
	return t, entry, nil
}

// Runs returns the task's run history, oldest first, including runs whose
// end the dispatcher has not yet moved out of the outbox.
func (m *Manager) Runs(ctx context.Context, key storage.Key) ([]v1alpha1.RunRecord, error) {
	t, err := m.store.GetTask(ctx, key)
	if err != nil {
		return nil, err
	}
	runs, err := readStream[v1alpha1.RunRecord](ctx, m.store, RunsStream(key), "", 0)
	if err != nil {
		return nil, err
	}
	for _, it := range t.Status.Outbox {
		if it.Kind == v1alpha1.OutboxRunEnded && it.Run != nil &&
			!slices.ContainsFunc(runs, func(r v1alpha1.RunRecord) bool { return r.ID == it.ID }) {
			runs = append(runs, *it.Run.DeepCopy())
		}
	}
	slices.SortFunc(runs, func(a, b v1alpha1.RunRecord) int { return strings.Compare(a.ID, b.ID) })
	return runs, nil
}

// RunEvents returns the progress events of one run, in order. The run must
// be the task's current run or one in its history.
func (m *Manager) RunEvents(ctx context.Context, key storage.Key, runID string) ([]Event, error) {
	runID = strings.ToUpper(runID)
	if _, err := ulid.ParseStrict(runID); err != nil {
		return nil, fmt.Errorf("run id %q: %w", runID, ErrInvalid)
	}
	t, err := m.store.GetTask(ctx, key)
	if err != nil {
		return nil, err
	}
	if t.Status.Run == nil || t.Status.Run.ID != runID {
		runs, err := m.Runs(ctx, key)
		if err != nil {
			return nil, err
		}
		if !slices.ContainsFunc(runs, func(r v1alpha1.RunRecord) bool { return r.ID == runID }) {
			return nil, fmt.Errorf("run %s of %s: %w", runID, key, storage.ErrNotFound)
		}
	}
	return readStream[Event](ctx, m.store, EventsStream(key, runID), "", 0)
}

// InitTeam opts a namespace in to Cadre. It fails with ErrAlreadyExists if
// the namespace already has a Team.
func (m *Manager) InitTeam(ctx context.Context, namespace string, spec v1alpha1.TeamSpec) (*v1alpha1.Team, error) {
	if err := validNamespace(namespace); err != nil {
		return nil, err
	}
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

// ListAgents lists the Agents of a Team's namespace, by name.
func (m *Manager) ListAgents(ctx context.Context, namespace string) ([]v1alpha1.Agent, error) {
	if err := validNamespace(namespace); err != nil {
		return nil, err
	}
	if _, err := m.store.GetTeam(ctx, namespace); err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return nil, fmt.Errorf("namespace %s has no Team: %w", namespace, err)
		}
		return nil, err
	}
	return m.agents.ListAgents(ctx, namespace)
}

// Team reads a namespace's Team from the board index, as fresh as the
// index is (seconds behind the store at worst while its watch is healthy):
// for deciding who may read, on every request, without a read of the store
// each time. LiveTeam is for decisions that must not lag.
func (m *Manager) Team(ctx context.Context, namespace string) (*v1alpha1.Team, error) {
	if idx, ok := m.store.(storage.TeamIndex); ok {
		return idx.IndexedTeam(ctx, namespace)
	}
	teams, err := m.store.ListTeams(ctx)
	if err != nil {
		return nil, err
	}
	for i := range teams {
		if teams[i].Namespace == namespace {
			return &teams[i], nil
		}
	}
	return nil, fmt.Errorf("team %s: %w", namespace, storage.ErrNotFound)
}

// LiveTeam reads a namespace's Team from the store itself.
func (m *Manager) LiveTeam(ctx context.Context, namespace string) (*v1alpha1.Team, error) {
	return m.store.GetTeam(ctx, namespace)
}

// ListTeams lists every Team.
func (m *Manager) ListTeams(ctx context.Context) ([]v1alpha1.Team, error) {
	return m.store.ListTeams(ctx)
}

// validNamespace rejects what Kubernetes would reject as a namespace name.
func validNamespace(ns string) error {
	if errs := validation.IsDNS1123Label(ns); len(errs) > 0 {
		return fmt.Errorf("namespace %q: %s: %w", ns, strings.Join(errs, "; "), ErrInvalid)
	}
	return nil
}
