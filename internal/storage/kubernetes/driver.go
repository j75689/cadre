// Package kubernetes is the default storage driver. Task and Team control
// state are CRDs written through the status subresource with resourceVersion
// preconditions; the log is one object per entry in S3-compatible storage; the
// index is an informer cache of Tasks, indexed in memory.
//
// +kubebuilder:rbac:groups=cadre.dev,resources=tasks;teams,verbs=get;list;watch;create
// +kubebuilder:rbac:groups=cadre.dev,resources=tasks/status;teams/status,verbs=get;update
package kubernetes

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	toolscache "k8s.io/client-go/tools/cache"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	"github.com/j75689/cadre/internal/storage"
)

// Config configures a Driver.
type Config struct {
	REST *rest.Config
	S3   S3Config
	// Cache, when set, is the informer cache the index is served from,
	// shared with others (the controller manager's): the driver adds its
	// informers to it but neither starts nor stops it. Without it the
	// driver runs a cache of its own.
	Cache cache.Cache
}

// Driver implements storage.Driver on the Kubernetes API and object storage.
type Driver struct {
	*objectLog

	// client reads and writes the API server directly. Conditional writes
	// must start from a fresh read; a cached read would only add conflicts.
	client client.Client
	// index is the informer cache that serves ListTasks.
	index cache.Cache
	stop  context.CancelFunc // nil when the cache is shared
	done  chan error

	closeOnce sync.Once
	closeErr  error

	mu   sync.RWMutex
	subs []func(*v1alpha1.Task)

	// handler is the Task informer's handler, removed on Close when the
	// cache outlives the driver.
	tasks   cache.Informer
	handler toolscache.ResourceEventHandlerRegistration
}

var (
	_ storage.Driver   = (*Driver)(nil)
	_ storage.Notifier = (*Driver)(nil)
)

// NotifyTasks implements storage.Notifier from the Task informer.
func (d *Driver) NotifyTasks(fn func(*v1alpha1.Task)) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.subs = append(d.subs, fn)
}

func (d *Driver) notify(obj any) {
	t, ok := obj.(*v1alpha1.Task)
	if !ok {
		return
	}
	d.mu.RLock()
	subs := d.subs
	d.mu.RUnlock()
	for _, fn := range subs {
		fn(t.DeepCopy())
	}
}

// Open connects to the cluster and object storage, and returns once the
// index has been rebuilt from a full list of Tasks. It refuses to start on an
// object store that does not enforce If-None-Match (see probe).
func Open(ctx context.Context, cfg Config) (*Driver, error) {
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		return nil, err
	}
	c, err := client.New(cfg.REST, client.Options{Scheme: scheme})
	if err != nil {
		return nil, fmt.Errorf("kubernetes client: %w", err)
	}
	log, err := newObjectLog(ctx, cfg.S3)
	if err != nil {
		return nil, err
	}
	if err := log.probe(ctx); err != nil {
		return nil, err
	}
	d := &Driver{objectLog: log, client: c, index: cfg.Cache}
	if d.index == nil {
		if d.index, err = cache.New(cfg.REST, cache.Options{Scheme: scheme}); err != nil {
			return nil, fmt.Errorf("index cache: %w", err)
		}
	}
	var synced []toolscache.InformerSynced
	for _, obj := range []client.Object{&v1alpha1.Task{}, &v1alpha1.Team{}} {
		inf, err := d.index.GetInformer(ctx, obj)
		if err != nil {
			return nil, fmt.Errorf("informer for %T: %w", obj, err)
		}
		synced = append(synced, inf.HasSynced)
		if _, ok := obj.(*v1alpha1.Task); ok {
			if err := indexTasks(ctx, d.index); err != nil {
				return nil, err
			}
			d.tasks = inf
			if d.handler, err = inf.AddEventHandler(toolscache.ResourceEventHandlerFuncs{
				AddFunc:    func(o any) { d.notify(o) },
				UpdateFunc: func(_, o any) { d.notify(o) },
			}); err != nil {
				return nil, fmt.Errorf("watch tasks: %w", err)
			}
		}
	}
	if cfg.Cache == nil {
		runCtx, stop := context.WithCancel(context.Background())
		d.stop, d.done = stop, make(chan error, 1)
		go func() { d.done <- d.index.Start(runCtx) }()
	}
	// The driver's own informers only: a shared cache may hold others
	// that sync later, or never.
	if !toolscache.WaitForCacheSync(ctx.Done(), synced...) {
		_ = d.Close()
		return nil, errors.New("index did not sync")
	}
	return d, nil
}

// Close stops the informer cache, when it is the driver's own. It is
// idempotent.
func (d *Driver) Close() error {
	d.closeOnce.Do(func() {
		if d.stop != nil {
			d.stop()
			d.closeErr = <-d.done
			return
		}
		if d.handler != nil {
			d.closeErr = d.tasks.RemoveEventHandler(d.handler)
		}
	})
	return d.closeErr
}

// mapError turns API errors into storage errors.
func mapError(err error, what string) error {
	switch {
	case err == nil:
		return nil
	case apierrors.IsNotFound(err):
		return fmt.Errorf("%s: %w", what, storage.ErrNotFound)
	case apierrors.IsConflict(err):
		return fmt.Errorf("%s: %w", what, storage.ErrConflict)
	case apierrors.IsAlreadyExists(err):
		return fmt.Errorf("%s: %w", what, storage.ErrAlreadyExists)
	case apierrors.IsInvalid(err), apierrors.IsBadRequest(err):
		return fmt.Errorf("%s: %w: %w", what, storage.ErrInvalid, err)
	default:
		return fmt.Errorf("%s: %w", what, err)
	}
}

// GetTask implements storage.Objects.
func (d *Driver) GetTask(ctx context.Context, key storage.Key) (*v1alpha1.Task, error) {
	t := &v1alpha1.Task{}
	err := d.client.Get(ctx, client.ObjectKey{Namespace: key.Namespace, Name: key.Name}, t)
	if err != nil {
		return nil, mapError(err, "get task "+key.String())
	}
	return t, nil
}

// CreateTask implements storage.Objects. The API server drops status on
// create, which is the contract.
func (d *Driver) CreateTask(ctx context.Context, t *v1alpha1.Task) error {
	return mapError(d.client.Create(ctx, t), "create task "+keyOf(t).String())
}

// UpdateTaskStatus implements storage.Objects. The status subresource
// ignores spec, and the object's resourceVersion is the precondition.
func (d *Driver) UpdateTaskStatus(ctx context.Context, t *v1alpha1.Task) error {
	if t.ResourceVersion == "" {
		return fmt.Errorf("update task %s: no resourceVersion: %w", keyOf(t), storage.ErrConflict)
	}
	return mapError(d.client.Status().Update(ctx, t), "update task "+keyOf(t).String())
}

// GetTeam implements storage.Objects.
func (d *Driver) GetTeam(ctx context.Context, namespace string) (*v1alpha1.Team, error) {
	team := &v1alpha1.Team{}
	err := d.client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: v1alpha1.TeamName}, team)
	if err != nil {
		return nil, mapError(err, "get team "+namespace)
	}
	return team, nil
}

// CreateTeam implements storage.Objects.
func (d *Driver) CreateTeam(ctx context.Context, team *v1alpha1.Team) error {
	return mapError(d.client.Create(ctx, team), "create team "+team.Namespace)
}

// UpdateTeamStatus implements storage.Objects.
func (d *Driver) UpdateTeamStatus(ctx context.Context, team *v1alpha1.Team) error {
	if team.ResourceVersion == "" {
		return fmt.Errorf("update team %s: no resourceVersion: %w", team.Namespace, storage.ErrConflict)
	}
	return mapError(d.client.Status().Update(ctx, team), "update team "+team.Namespace)
}

// UpdateTeamSpec implements storage.Objects: an update of the Team, whose
// status the API server leaves alone, as it is a subresource.
func (d *Driver) UpdateTeamSpec(ctx context.Context, team *v1alpha1.Team) error {
	if team.ResourceVersion == "" {
		return fmt.Errorf("update team %s: no resourceVersion: %w", team.Namespace, storage.ErrConflict)
	}
	return mapError(d.client.Update(ctx, team), "update team "+team.Namespace)
}

// ListTeams implements storage.Index from the informer cache.
func (d *Driver) ListTeams(ctx context.Context) ([]v1alpha1.Team, error) {
	list := &v1alpha1.TeamList{}
	if err := d.index.List(ctx, list); err != nil {
		return nil, fmt.Errorf("list teams: %w", err)
	}
	slices.SortFunc(list.Items, func(a, b v1alpha1.Team) int { return strings.Compare(a.Namespace, b.Namespace) })
	return list.Items, nil
}

// IndexedTeam implements storage.TeamIndex from the informer cache.
func (d *Driver) IndexedTeam(ctx context.Context, namespace string) (*v1alpha1.Team, error) {
	team := &v1alpha1.Team{}
	if err := d.index.Get(ctx, client.ObjectKey{Namespace: namespace, Name: v1alpha1.TeamName}, team); err != nil {
		return nil, mapError(err, "indexed team "+namespace)
	}
	return team, nil
}

// Cached Tasks are indexed by spec.parentRef.name, so that a task's
// subtasks are listed without reading its whole namespace, by status.number,
// so that a task is found by its number in one read, and by status.phase, so
// that a board column reads its own tasks only.
const (
	indexParent = "spec.parentRef.name"
	indexNumber = "status.number"
	indexPhase  = "status.phase"
)

// indexTasks adds the Task indexes to a cache. A cache shared with an
// earlier driver has them already: the same indexes, kept.
func indexTasks(ctx context.Context, c cache.Cache) error {
	for _, idx := range []struct {
		field string
		of    func(*v1alpha1.Task) []string
	}{
		{indexParent, func(t *v1alpha1.Task) []string {
			if p := t.Spec.ParentRef; p != nil {
				return []string{p.Name}
			}
			return nil
		}},
		{indexNumber, func(t *v1alpha1.Task) []string {
			if t.Status.Number > 0 {
				return []string{strconv.FormatInt(t.Status.Number, 10)}
			}
			return nil
		}},
		{indexPhase, func(t *v1alpha1.Task) []string { return []string{string(t.Status.Phase)} }},
	} {
		err := c.IndexField(ctx, &v1alpha1.Task{}, idx.field, func(o client.Object) []string { return idx.of(o.(*v1alpha1.Task)) })
		if err != nil && !strings.Contains(err.Error(), "indexer conflict") {
			return fmt.Errorf("index tasks by %s: %w", idx.field, err)
		}
	}
	return nil
}

// ListTasks implements storage.Index from the informer cache.
func (d *Driver) ListTasks(ctx context.Context, q storage.Query) ([]v1alpha1.Task, error) {
	matched, err := d.matching(ctx, q)
	if err != nil {
		return nil, err
	}
	slices.SortFunc(matched, storage.CompareTasks)
	page := storage.Page(q, matched)
	out := make([]v1alpha1.Task, len(page))
	for i, t := range page {
		out[i] = *t
	}
	return out, nil
}

// CountTasks implements storage.Index from the informer cache.
func (d *Driver) CountTasks(ctx context.Context, q storage.Query) (int, error) {
	matched, err := d.matching(ctx, q)
	return len(matched), err
}

// matching returns the cached Tasks q selects, unordered.
func (d *Driver) matching(ctx context.Context, q storage.Query) ([]*v1alpha1.Task, error) {
	if err := q.Validate(); err != nil {
		return nil, err
	}
	opts := []client.ListOption{client.InNamespace(q.Namespace)}
	switch {
	case q.Number > 0:
		// The one task, from the number index; Matches checks the rest.
		opts = append(opts, client.MatchingFields{indexNumber: strconv.FormatInt(q.Number, 10)})
	case q.Parent != "":
		// Its subtasks only, from the parent index; Matches checks again.
		opts = append(opts, client.MatchingFields{indexParent: q.Parent})
	case len(q.Phases) == 1:
		// One board column, from the phase index; Matches checks the rest.
		opts = append(opts, client.MatchingFields{indexPhase: string(q.Phases[0])})
	}
	list := &v1alpha1.TaskList{}
	if err := d.index.List(ctx, list, opts...); err != nil {
		return nil, fmt.Errorf("list tasks: %w", err)
	}
	matched := make([]*v1alpha1.Task, 0, len(list.Items))
	for i := range list.Items {
		if q.Matches(&list.Items[i]) {
			matched = append(matched, &list.Items[i])
		}
	}
	return matched, nil
}

func keyOf(t *v1alpha1.Task) storage.Key { return storage.Key{Namespace: t.Namespace, Name: t.Name} }
