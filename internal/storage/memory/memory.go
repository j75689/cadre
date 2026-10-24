// Package memory is an in-memory storage driver. It backs unit tests and is
// the reference the conformance suite is first written against; it is not for
// production use.
package memory

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	"github.com/j75689/cadre/internal/storage"
)

// Backend is the shared state several Drivers see, like one cluster seen by
// several server processes.
type Backend struct {
	mu      sync.Mutex
	version int64
	tasks   map[storage.Key]*v1alpha1.Task
	teams   map[string]*v1alpha1.Team
	logs    map[string]map[string][]byte
	drivers map[*Driver]struct{}
}

// NewBackend returns an empty backend.
func NewBackend() *Backend {
	return &Backend{
		tasks:   map[storage.Key]*v1alpha1.Task{},
		teams:   map[string]*v1alpha1.Team{},
		logs:    map[string]map[string][]byte{},
		drivers: map[*Driver]struct{}{},
	}
}

// EditSpec changes a task's spec in place, as a human's kubectl edit would;
// the storage interface has no spec writes, so tests use this.
func (b *Backend) EditSpec(key storage.Key, edit func(*v1alpha1.TaskSpec)) {
	b.mu.Lock()
	defer b.mu.Unlock()
	t, ok := b.tasks[key]
	if !ok {
		return
	}
	t = t.DeepCopy()
	edit(&t.Spec)
	t.ResourceVersion = b.nextVersion()
	b.tasks[key] = t
	b.notify(t)
}

// Open starts a Driver on the backend. Its index is rebuilt from a full list,
// as a restarted process would rebuild it, then kept current by watch events.
func (b *Backend) Open() *Driver {
	d := &Driver{b: b, index: map[storage.Key]*v1alpha1.Task{}, events: make(chan *v1alpha1.Task, 4096), stopped: make(chan struct{})}
	go d.deliver()
	b.mu.Lock()
	defer b.mu.Unlock()
	for k, t := range b.tasks {
		d.index[k] = t.DeepCopy()
	}
	b.drivers[d] = struct{}{}
	return d
}

// Driver is one process's view of a Backend.
type Driver struct {
	b *Backend

	mu    sync.RWMutex
	index map[storage.Key]*v1alpha1.Task
	subs  []func(*v1alpha1.Task)

	// events carries changed Tasks to deliver, outside the backend's lock
	// and in order; one that does not fit is dropped, as a hint may be.
	events    chan *v1alpha1.Task
	stopped   chan struct{}
	closeOnce sync.Once
}

func (d *Driver) deliver() {
	for {
		select {
		case <-d.stopped:
			return
		case t := <-d.events:
			d.mu.RLock()
			subs := d.subs
			d.mu.RUnlock()
			for _, fn := range subs {
				fn(t.DeepCopy())
			}
		}
	}
}

var _ storage.Notifier = (*Driver)(nil)

// NotifyTasks implements storage.Notifier from the driver's watch.
func (d *Driver) NotifyTasks(fn func(*v1alpha1.Task)) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.subs = append(d.subs, fn)
}

var _ storage.Driver = (*Driver)(nil)

// Close stops the driver's watch.
func (d *Driver) Close() error {
	d.b.mu.Lock()
	delete(d.b.drivers, d)
	d.b.mu.Unlock()
	d.closeOnce.Do(func() { close(d.stopped) })
	return nil
}

// nextVersion must be called with b.mu held.
func (b *Backend) nextVersion() string {
	b.version++
	return strconv.FormatInt(b.version, 10)
}

// notify delivers a watch event to every open driver. It must be called with
// b.mu held.
func (b *Backend) notify(t *v1alpha1.Task) {
	for d := range b.drivers {
		d.mu.Lock()
		d.index[storage.Key{Namespace: t.Namespace, Name: t.Name}] = t.DeepCopy()
		d.mu.Unlock()
		select {
		case d.events <- t.DeepCopy():
		default:
		}
	}
}

func notFound(kind string, key fmt.Stringer) error {
	return fmt.Errorf("%s %s: %w", kind, key, storage.ErrNotFound)
}

// GetTask implements storage.Objects.
func (d *Driver) GetTask(_ context.Context, key storage.Key) (*v1alpha1.Task, error) {
	d.b.mu.Lock()
	defer d.b.mu.Unlock()
	t, ok := d.b.tasks[key]
	if !ok {
		return nil, notFound("task", key)
	}
	return t.DeepCopy(), nil
}

// CreateTask implements storage.Objects.
func (d *Driver) CreateTask(_ context.Context, t *v1alpha1.Task) error {
	key := storage.Key{Namespace: t.Namespace, Name: t.Name}
	d.b.mu.Lock()
	defer d.b.mu.Unlock()
	if _, ok := d.b.tasks[key]; ok {
		return fmt.Errorf("task %s: %w", key, storage.ErrAlreadyExists)
	}
	stored := t.DeepCopy()
	stored.Status = v1alpha1.TaskStatus{}
	stored.ResourceVersion = d.b.nextVersion()
	d.b.tasks[key] = stored
	d.b.notify(stored)
	t.ResourceVersion = stored.ResourceVersion
	t.Status = v1alpha1.TaskStatus{}
	return nil
}

// UpdateTaskStatus implements storage.Objects.
func (d *Driver) UpdateTaskStatus(_ context.Context, t *v1alpha1.Task) error {
	key := storage.Key{Namespace: t.Namespace, Name: t.Name}
	d.b.mu.Lock()
	defer d.b.mu.Unlock()
	cur, ok := d.b.tasks[key]
	if !ok {
		return notFound("task", key)
	}
	if cur.ResourceVersion != t.ResourceVersion {
		return fmt.Errorf("task %s: %w", key, storage.ErrConflict)
	}
	stored := cur.DeepCopy()
	t.Status.DeepCopyInto(&stored.Status)
	stored.ResourceVersion = d.b.nextVersion()
	d.b.tasks[key] = stored
	d.b.notify(stored)
	t.ResourceVersion = stored.ResourceVersion
	return nil
}

// GetTeam implements storage.Objects.
func (d *Driver) GetTeam(_ context.Context, namespace string) (*v1alpha1.Team, error) {
	d.b.mu.Lock()
	defer d.b.mu.Unlock()
	team, ok := d.b.teams[namespace]
	if !ok {
		return nil, notFound("team", storage.Key{Namespace: namespace, Name: v1alpha1.TeamName})
	}
	return team.DeepCopy(), nil
}

// CreateTeam implements storage.Objects.
func (d *Driver) CreateTeam(_ context.Context, team *v1alpha1.Team) error {
	if team.Name != v1alpha1.TeamName {
		return fmt.Errorf("team must be named %q, not %q", v1alpha1.TeamName, team.Name)
	}
	d.b.mu.Lock()
	defer d.b.mu.Unlock()
	if _, ok := d.b.teams[team.Namespace]; ok {
		return fmt.Errorf("team %s: %w", team.Namespace, storage.ErrAlreadyExists)
	}
	stored := team.DeepCopy()
	stored.Status = v1alpha1.TeamStatus{}
	stored.ResourceVersion = d.b.nextVersion()
	d.b.teams[team.Namespace] = stored
	team.ResourceVersion = stored.ResourceVersion
	team.Status = v1alpha1.TeamStatus{}
	return nil
}

// UpdateTeamStatus implements storage.Objects.
func (d *Driver) UpdateTeamStatus(_ context.Context, team *v1alpha1.Team) error {
	d.b.mu.Lock()
	defer d.b.mu.Unlock()
	cur, ok := d.b.teams[team.Namespace]
	if !ok {
		return notFound("team", storage.Key{Namespace: team.Namespace, Name: v1alpha1.TeamName})
	}
	if cur.ResourceVersion != team.ResourceVersion {
		return fmt.Errorf("team %s: %w", team.Namespace, storage.ErrConflict)
	}
	stored := cur.DeepCopy()
	stored.Status = team.Status
	stored.ResourceVersion = d.b.nextVersion()
	d.b.teams[team.Namespace] = stored
	team.ResourceVersion = stored.ResourceVersion
	return nil
}

// Append implements storage.Log.
func (d *Driver) Append(_ context.Context, stream, key string, data []byte) error {
	if err := errors.Join(storage.ValidateStream(stream), storage.ValidateKey(key)); err != nil {
		return err
	}
	d.b.mu.Lock()
	defer d.b.mu.Unlock()
	s := d.b.logs[stream]
	if s == nil {
		s = map[string][]byte{}
		d.b.logs[stream] = s
	}
	if _, ok := s[key]; ok {
		return fmt.Errorf("log %s entry %s: %w", stream, key, storage.ErrAlreadyExists)
	}
	s[key] = slices.Clone(data)
	return nil
}

// Read implements storage.Log.
func (d *Driver) Read(_ context.Context, stream, after string, limit int) ([]storage.Entry, error) {
	if err := storage.ValidateStream(stream); err != nil {
		return nil, err
	}
	d.b.mu.Lock()
	defer d.b.mu.Unlock()
	var out []storage.Entry
	for k, v := range d.b.logs[stream] {
		if k > after {
			out = append(out, storage.Entry{Key: k, Data: slices.Clone(v)})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// Keys implements storage.Log.
func (d *Driver) Keys(ctx context.Context, stream, after string, limit int) ([]storage.KeyInfo, error) {
	entries, err := d.Read(ctx, stream, after, limit)
	if err != nil {
		return nil, err
	}
	out := make([]storage.KeyInfo, len(entries))
	for i, e := range entries {
		out[i] = storage.KeyInfo{Key: e.Key, Size: int64(len(e.Data))}
	}
	return out, nil
}

// ListTeams implements storage.Index.
func (d *Driver) ListTeams(context.Context) ([]v1alpha1.Team, error) {
	d.b.mu.Lock()
	defer d.b.mu.Unlock()
	out := make([]v1alpha1.Team, 0, len(d.b.teams))
	for _, team := range d.b.teams {
		out = append(out, *team.DeepCopy())
	}
	slices.SortFunc(out, func(a, b v1alpha1.Team) int { return strings.Compare(a.Namespace, b.Namespace) })
	return out, nil
}

// ListTasks implements storage.Index.
func (d *Driver) ListTasks(_ context.Context, q storage.Query) ([]v1alpha1.Task, error) {
	if err := q.Validate(); err != nil {
		return nil, err
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	var matched []*v1alpha1.Task
	for _, t := range d.index {
		if q.Matches(t) {
			matched = append(matched, t)
		}
	}
	slices.SortFunc(matched, storage.CompareTasks)
	page := storage.Page(q, matched)
	out := make([]v1alpha1.Task, len(page))
	for i, t := range page {
		t.DeepCopyInto(&out[i])
	}
	return out, nil
}
