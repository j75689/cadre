// Package kubernetes is the default storage driver. Task and Team control
// state are CRDs written through the status subresource with resourceVersion
// preconditions; the log is one object per entry in S3-compatible storage; the
// index is an informer cache of Tasks, indexed in memory.
package kubernetes

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	"github.com/j75689/cadre/internal/storage"
)

// Config configures a Driver.
type Config struct {
	REST *rest.Config
	S3   S3Config
}

// Driver implements storage.Driver on the Kubernetes API and object storage.
type Driver struct {
	*objectLog

	// client reads and writes the API server directly. Conditional writes
	// must start from a fresh read; a cached read would only add conflicts.
	client client.Client
	// index is the informer cache that serves ListTasks.
	index cache.Cache
	stop  context.CancelFunc
	done  chan error

	closeOnce sync.Once
	closeErr  error
}

var _ storage.Driver = (*Driver)(nil)

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
	index, err := cache.New(cfg.REST, cache.Options{Scheme: scheme})
	if err != nil {
		return nil, fmt.Errorf("index cache: %w", err)
	}
	for _, obj := range []client.Object{&v1alpha1.Task{}, &v1alpha1.Team{}} {
		if _, err := index.GetInformer(ctx, obj); err != nil {
			return nil, fmt.Errorf("informer for %T: %w", obj, err)
		}
	}

	runCtx, stop := context.WithCancel(context.Background())
	d := &Driver{objectLog: log, client: c, index: index, stop: stop, done: make(chan error, 1)}
	go func() { d.done <- index.Start(runCtx) }()
	if !index.WaitForCacheSync(ctx) {
		_ = d.Close()
		return nil, errors.New("index did not sync")
	}
	return d, nil
}

// Close stops the informer. It is idempotent.
func (d *Driver) Close() error {
	d.closeOnce.Do(func() {
		d.stop()
		d.closeErr = <-d.done
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

// ListTeams implements storage.Index from the informer cache.
func (d *Driver) ListTeams(ctx context.Context) ([]v1alpha1.Team, error) {
	list := &v1alpha1.TeamList{}
	if err := d.index.List(ctx, list); err != nil {
		return nil, fmt.Errorf("list teams: %w", err)
	}
	slices.SortFunc(list.Items, func(a, b v1alpha1.Team) int { return strings.Compare(a.Namespace, b.Namespace) })
	return list.Items, nil
}

// ListTasks implements storage.Index from the informer cache.
func (d *Driver) ListTasks(ctx context.Context, q storage.Query) ([]v1alpha1.Task, error) {
	if err := q.Validate(); err != nil {
		return nil, err
	}
	list := &v1alpha1.TaskList{}
	if err := d.index.List(ctx, list, client.InNamespace(q.Namespace)); err != nil {
		return nil, fmt.Errorf("list tasks: %w", err)
	}
	matched := make([]*v1alpha1.Task, 0, len(list.Items))
	for i := range list.Items {
		if q.Matches(&list.Items[i]) {
			matched = append(matched, &list.Items[i])
		}
	}
	slices.SortFunc(matched, storage.CompareTasks)
	page := storage.Page(q, matched)
	out := make([]v1alpha1.Task, len(page))
	for i, t := range page {
		out[i] = *t
	}
	return out, nil
}

func keyOf(t *v1alpha1.Task) storage.Key { return storage.Key{Namespace: t.Namespace, Name: t.Name} }
