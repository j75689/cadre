// Package conformance is the test suite every storage driver must pass. It
// checks the guarantees the core relies on, listed in docs/storage.md.
package conformance

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	"github.com/j75689/cadre/internal/storage"
	"github.com/j75689/cadre/internal/task"
)

// Env is one isolated store for one test.
type Env struct {
	// Open starts a new driver process on the store. Two drivers opened from
	// one Env are two servers sharing one store; closing one and opening
	// another is a restart.
	Open func() storage.Driver
	// Namespace is the namespace the test may use; it is empty.
	Namespace string
}

// Factory returns a fresh Env for each test.
type Factory func(t *testing.T) Env

// Run runs the whole suite.
func Run(t *testing.T, f Factory) {
	tests := []struct {
		name string
		fn   func(t *testing.T, s *suite)
	}{
		{"ConcurrentConditionalWrite", testConcurrentConditionalWrite},
		{"StaleVersionConflicts", testStaleVersionConflicts},
		{"CreateIsCreateIfNotExists", testCreateIsCreateIfNotExists},
		{"CreateIgnoresStatus", testCreateIgnoresStatus},
		{"NotFound", testNotFound},
		{"StatusWritesLeaveSpecAlone", testStatusWritesLeaveSpecAlone},
		{"TeamSpecWritesLeaveStatusAlone", testTeamSpecWritesLeaveStatusAlone},
		{"CloseIsIdempotent", testCloseIsIdempotent},
		{"ClaimExactlyOnce", testClaimExactlyOnce},
		{"RedeliveredStepAdvancesOnce", testRedeliveredStepAdvancesOnce},
		{"OutboxAppliedOnceAfterDispatcherDies", testOutboxAppliedOnce},
		{"NotifyItemRoundTrips", testNotifyItemRoundTrips},
		{"TeamSemaphoreAndCounter", testTeamSemaphoreAndCounter},
		{"IndexRebuildsAfterRestart", testIndexRebuilds},
		{"IndexQuery", testIndexQuery},
		{"IndexFilters", testIndexFilters},
		{"IndexRejectsInvalidQueries", testIndexRejectsInvalidQueries},
		{"IndexListsTeams", testIndexListsTeams},
		{"LogAppend", testLogAppend},
		{"LogNestedStreams", testLogNestedStreams},
		{"LogKeys", testLogKeys},
		{"LogAlreadyExistsMeansReadable", testLogAlreadyExistsMeansReadable},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env := f(t)
			s := &suite{t: t, env: env, ns: env.Namespace, ctx: context.Background()}
			tc.fn(t, s)
		})
	}
}

type suite struct {
	t   *testing.T
	env Env
	ns  string
	ctx context.Context
}

// open starts a driver and closes it when the test ends. Tests may close it
// earlier; Close is idempotent.
func (s *suite) open() storage.Driver {
	d := s.env.Open()
	s.t.Cleanup(func() {
		if err := d.Close(); err != nil {
			s.t.Errorf("close: %v", err)
		}
	})
	return d
}

// team reads the Team, failing the test if it cannot.
func (s *suite) team(d storage.Driver) *v1alpha1.Team {
	s.t.Helper()
	team, err := d.GetTeam(s.ctx, s.ns)
	if err != nil {
		s.t.Fatalf("get team: %v", err)
	}
	return team
}

// writeTeam reads the Team, applies change and writes it, retrying conflicts.
func (s *suite) writeTeam(d storage.Driver, change func(*v1alpha1.Team) (v1alpha1.TeamStatus, error)) error {
	for attempt := 0; attempt < maxConflicts; attempt++ {
		cur, err := d.GetTeam(s.ctx, s.ns)
		if err != nil {
			return err
		}
		next, err := change(cur)
		if err != nil {
			return err
		}
		cur.Status = next
		if err := d.UpdateTeamStatus(s.ctx, cur); !errors.Is(err, storage.ErrConflict) {
			return err
		}
	}
	return fmt.Errorf("team %s: %w", s.ns, errTooManyConflicts)
}

func (s *suite) key(name string) storage.Key { return storage.Key{Namespace: s.ns, Name: name} }

var testNow = time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)

// newTask creates an assigned task and moves it to Todo.
func (s *suite) newTask(d storage.Driver, name string, number int64, prio v1alpha1.Priority) {
	s.t.Helper()
	s.newSubtask(d, name, number, prio, "")
}

// newSubtask is newTask for a subtask of parent, or of none if empty.
func (s *suite) newSubtask(d storage.Driver, name string, number int64, prio v1alpha1.Priority, parent string) {
	s.t.Helper()
	t := &v1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: s.ns},
		Spec: v1alpha1.TaskSpec{
			Title: name, Priority: prio,
			Assignee: &v1alpha1.Assignee{Kind: v1alpha1.AssigneeAgent, Name: "coder"},
		},
	}
	if parent != "" {
		t.Spec.ParentRef = &v1alpha1.TaskReference{Name: parent}
	}
	if err := d.CreateTask(s.ctx, t); err != nil {
		s.t.Fatalf("create %s: %v", name, err)
	}
	if err := transition(s.ctx, d, s.key(name), task.Create{Number: number}); err != nil {
		s.t.Fatalf("number %s: %v", name, err)
	}
}

func (s *suite) get(d storage.Driver, name string) *v1alpha1.Task {
	s.t.Helper()
	t, err := d.GetTask(s.ctx, s.key(name))
	if err != nil {
		s.t.Fatalf("get %s: %v", name, err)
	}
	return t
}

// transition reads the task, applies ev and writes the result, retrying only
// when another writer got in between. It returns the state machine's error.
func transition(ctx context.Context, d storage.Driver, key storage.Key, ev task.Event) error {
	return transitionAfter(ctx, d, key, ev, nil)
}

// maxConflicts bounds conflict retries, so a driver whose versions never match
// fails the test with its last error instead of hanging it.
const maxConflicts = 100

var errTooManyConflicts = errors.New("still conflicting after many retries")

// transitionAfter is transition, but the first attempt waits for every party
// to have read the task before it writes, so all of them race from the same
// version instead of happening to run one after another.
func transitionAfter(ctx context.Context, d storage.Driver, key storage.Key, ev task.Event, readers *sync.WaitGroup) error {
	for attempt := 0; ; attempt++ {
		if attempt == maxConflicts {
			return fmt.Errorf("%s on %s: %w", task.KindOf(ev), key, errTooManyConflicts)
		}
		t, err := d.GetTask(ctx, key)
		if readers != nil {
			readers.Done()
			readers.Wait()
			readers = nil
		}
		if err != nil {
			return err
		}
		res, err := task.Apply(t, ev, testNow)
		if err != nil {
			return err
		}
		t.Status = res.Status
		err = d.UpdateTaskStatus(ctx, t)
		if !errors.Is(err, storage.ErrConflict) {
			return err
		}
	}
}

// readBarrier is a barrier for n parties calling transitionAfter.
func readBarrier(n int) *sync.WaitGroup {
	var wg sync.WaitGroup
	wg.Add(n)
	return &wg
}

// race runs fn n times concurrently, spread over the drivers, and returns how
// many calls returned nil.
func race(n int, drivers []storage.Driver, fn func(i int, d storage.Driver) error) (wins int, errs []error) {
	var mu sync.Mutex
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			err := fn(i, drivers[i%len(drivers)])
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				wins++
			} else {
				errs = append(errs, err)
			}
		}()
	}
	close(start)
	wg.Wait()
	return wins, errs
}

// eventually polls cond until it holds or the deadline passes; the index is
// eventually consistent.
func eventually(t *testing.T, what string, cond func() error) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		err := cond()
		if err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: %v", what, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func allErrorsAre(t *testing.T, errs []error, target error) {
	t.Helper()
	for _, err := range errs {
		if !errors.Is(err, target) {
			t.Errorf("got %v, want %v", err, target)
		}
	}
}

func runID(i int) string { return fmt.Sprintf("01JB%022d", i) }
