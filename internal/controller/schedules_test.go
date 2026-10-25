package controller_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	"github.com/j75689/cadre/internal/controller"
	"github.com/j75689/cadre/internal/storage/kubernetes"
	"github.com/j75689/cadre/internal/testenv"
)

// clock is the time the Schedule controllers see.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = t
}

// scheduleEnv runs Schedule controllers over one store, on a clock the test
// moves.
type scheduleEnv struct {
	*env
	cfg   *rest.Config
	store *kubernetes.Driver
	clock *clock
	stop  func()
}

func startSchedules(t *testing.T) *scheduleEnv {
	t.Helper()
	cfg := testenv.APIServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	store, err := kubernetes.Open(ctx, kubernetes.Config{REST: cfg, S3: testenv.S3(t)})
	if err != nil {
		t.Fatal(err)
	}
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	e := &scheduleEnv{env: &env{t: t, c: c}, cfg: cfg, store: store, clock: &clock{t: time.Now()}}
	t.Cleanup(func() {
		if e.stop != nil {
			e.stop()
		}
		cancel()
		_ = store.Close()
	})
	return e
}

// run stops the controllers, if they run, and starts n fresh ones, which
// remember nothing, as after a restart; with n > 1 they overlap as two
// leaders would. Each registers the Schedule controller once its manager
// has started, as cadre-server does.
func (e *scheduleEnv) run(n int) {
	e.t.Helper()
	if e.stop != nil {
		e.stop()
	}
	var stops []func()
	for range n {
		mgr, err := controller.NewManager(e.cfg)
		if err != nil {
			e.t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- mgr.Start(ctx) }()
		if err := controller.SetupSchedules(mgr, e.store, controller.Options{Resync: 100 * time.Millisecond, Now: e.clock.Now}, controller.SkipNameValidation()); err != nil {
			e.t.Fatal(err)
		}
		stops = append(stops, func() {
			cancel()
			if err := <-done; err != nil {
				e.t.Errorf("manager: %v", err)
			}
		})
	}
	e.stop = func() {
		for _, s := range stops {
			s()
		}
	}
}

// team makes a namespace with a Team.
func (e *scheduleEnv) team(ns string) {
	e.t.Helper()
	e.create(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}, &v1alpha1.Team{ObjectMeta: meta2(ns, v1alpha1.TeamName)})
}

// schedule creates a Schedule with spec's cron, zone, jitter and suspend,
// and a task to make.
func (e *scheduleEnv) schedule(ns, name string, spec v1alpha1.ScheduleSpec) *v1alpha1.Schedule {
	e.t.Helper()
	spec.Task = v1alpha1.TaskSpec{Title: "Today's thread", Assignee: &v1alpha1.Assignee{Kind: v1alpha1.AssigneeAgent, Name: "writer"}}
	s := &v1alpha1.Schedule{ObjectMeta: meta2(ns, name), Spec: spec}
	e.create(s)
	return s
}

// update changes a Schedule's spec.
func (e *scheduleEnv) update(s *v1alpha1.Schedule, change func(*v1alpha1.ScheduleSpec)) {
	e.t.Helper()
	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := e.c.Get(context.Background(), client.ObjectKeyFromObject(s), s); err != nil {
			return err
		}
		change(&s.Spec)
		return e.c.Update(context.Background(), s)
	}); err != nil {
		e.t.Fatal(err)
	}
}

// tasks lists the tasks a Schedule made, by name.
func (e *scheduleEnv) tasks(s *v1alpha1.Schedule) []string {
	e.t.Helper()
	var list v1alpha1.TaskList
	if err := e.c.List(context.Background(), &list, client.InNamespace(s.Namespace), client.MatchingLabels{v1alpha1.LabelSchedule: s.Name}); err != nil {
		e.t.Fatal(err)
	}
	var names []string
	for _, t := range list.Items {
		names = append(names, t.Name)
	}
	return names
}

// expectTasks waits until a Schedule made exactly the named tasks, then
// checks it makes no more for a while.
func (e *scheduleEnv) expectTasks(s *v1alpha1.Schedule, names ...string) {
	e.t.Helper()
	want := strings.Join(names, ",")
	eventually(e.t, 10*time.Second, func() (bool, string) {
		got := strings.Join(e.tasks(s), ",")
		return got == want, "tasks " + got + ", want " + want
	})
	time.Sleep(time.Second) // ten resyncs
	if got := strings.Join(e.tasks(s), ","); got != want {
		e.t.Fatalf("tasks %s, want %s", got, want)
	}
}

// taskName is a tick's task's name.
func taskName(s *v1alpha1.Schedule, tick time.Time) string {
	return s.Name + "-" + tick.UTC().Format("200601021504")
}

// hourly is "0 * * * *": the n-th tick after t.
func hourly(t time.Time, n int) time.Time {
	return t.UTC().Truncate(time.Hour).Add(time.Duration(n) * time.Hour)
}

func TestSchedules(t *testing.T) {
	e := startSchedules(t)
	e.run(1)
	const ns = "team-schedules"
	e.team(ns)

	t.Run("fires once per tick", func(t *testing.T) {
		e.t = t
		s := e.schedule(ns, "hourly", v1alpha1.ScheduleSpec{Cron: "0 * * * *"})
		e.ready(s, metav1.ConditionTrue, "Scheduled")
		created := s.CreationTimestamp.Time
		t1 := hourly(created, 1)
		if s.Status.NextScheduleTime == nil || !s.Status.NextScheduleTime.Time.Equal(t1) {
			t.Fatalf("nextScheduleTime %v, want %v", s.Status.NextScheduleTime, t1)
		}
		e.expectTasks(s)

		e.clock.set(t1.Add(time.Second))
		e.expectTasks(s, taskName(s, t1))
		var task v1alpha1.Task
		if err := e.c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: taskName(s, t1)}, &task); err != nil {
			t.Fatal(err)
		}
		if task.Annotations[v1alpha1.AnnotationScheduledAt] != t1.Format(time.RFC3339) || task.Spec.Title != "Today's thread" || len(task.OwnerReferences) != 0 {
			t.Errorf("task %+v %+v", task.ObjectMeta, task.Spec)
		}
		eventually(t, 10*time.Second, func() (bool, string) {
			_ = e.c.Get(context.Background(), client.ObjectKeyFromObject(s), s)
			st := s.Status
			return st.LastTask == taskName(s, t1) && st.LastScheduleTime.Equal(&metav1.Time{Time: t1}) && st.NextScheduleTime.Equal(&metav1.Time{Time: hourly(created, 2)}),
				"status " + st.LastTask
		})

		// Restarted, it remembers what fired from the store and the status.
		e.run(1)
		e.expectTasks(s, taskName(s, t1))

		// Three ticks late: only the latest fires.
		t5 := hourly(created, 5)
		e.clock.set(t5.Add(time.Minute))
		e.expectTasks(s, taskName(s, t1), taskName(s, t5))
		eventually(t, 10*time.Second, func() (bool, string) {
			var list eventsv1.EventList
			if err := e.c.List(context.Background(), &list, client.InNamespace(ns)); err != nil {
				return false, err.Error()
			}
			for _, ev := range list.Items {
				if ev.Regarding.Name == s.Name && ev.Reason == "MissedTicks" {
					return strings.HasPrefix(ev.Note, "3 earlier ticks"), "note " + ev.Note
				}
			}
			return false, "no MissedTicks event"
		})
	})

	t.Run("suspended ticks never fire", func(t *testing.T) {
		e.t = t
		e.clock.set(time.Now())
		s := e.schedule(ns, "paused", v1alpha1.ScheduleSpec{Cron: "0 * * * *"})
		e.ready(s, metav1.ConditionTrue, "Scheduled")
		e.update(s, func(spec *v1alpha1.ScheduleSpec) { spec.Suspend = true })
		e.ready(s, metav1.ConditionTrue, "Suspended")
		created := s.CreationTimestamp.Time
		e.clock.set(hourly(created, 2).Add(time.Second))
		e.expectTasks(s)

		e.update(s, func(spec *v1alpha1.ScheduleSpec) { spec.Suspend = false })
		e.ready(s, metav1.ConditionTrue, "Scheduled")
		e.expectTasks(s)
		if t3 := hourly(created, 3); !s.Status.NextScheduleTime.Equal(&metav1.Time{Time: t3}) {
			t.Errorf("nextScheduleTime %v, want %v", s.Status.NextScheduleTime, t3)
		}
		e.clock.set(hourly(created, 3).Add(time.Second))
		e.expectTasks(s, taskName(s, hourly(created, 3)))
	})

	t.Run("jitter", func(t *testing.T) {
		e.t = t
		e.clock.set(time.Now())
		s := e.schedule(ns, "jittered", v1alpha1.ScheduleSpec{Cron: "0 * * * *", Jitter: &metav1.Duration{Duration: 30 * time.Minute}})
		e.ready(s, metav1.ConditionTrue, "Scheduled")
		created := s.CreationTimestamp.Time
		t1 := hourly(created, 1)
		next := s.Status.NextScheduleTime.Time
		if next.Before(t1) || !next.Before(t1.Add(30*time.Minute)) {
			t.Fatalf("nextScheduleTime %v, not within the jitter after %v", next, t1)
		}
		// Every leader computes the same fire time.
		e.run(1)
		time.Sleep(500 * time.Millisecond)
		if err := e.c.Get(context.Background(), client.ObjectKeyFromObject(s), s); err != nil || !s.Status.NextScheduleTime.Equal(&metav1.Time{Time: next}) {
			t.Fatalf("nextScheduleTime %v after a restart, was %v (%v)", s.Status.NextScheduleTime, next, err)
		}
		e.clock.set(next.Add(-time.Second))
		e.expectTasks(s)
		e.clock.set(next.Add(time.Second))
		e.expectTasks(s, taskName(s, t1))
	})

	t.Run("a late tick and the one it held back", func(t *testing.T) {
		e.t = t
		e.clock.set(time.Now())
		// A Schedule whose first tick's jitter outlasts the second tick, and
		// the second is due when the first fires, but not the third.
		const jitter = 5 * time.Minute
		var s *v1alpha1.Schedule
		var t1 time.Time
		var offset func(time.Time) time.Duration
		for i := 0; s == nil; i++ {
			if i == 50 {
				t.Fatal("no Schedule with fitting jitter in 50")
			}
			c := e.schedule(ns, fmt.Sprintf("late-%d", i), v1alpha1.ScheduleSpec{Cron: "* * * * *", Jitter: &metav1.Duration{Duration: jitter}})
			offset = func(tick time.Time) time.Duration { return controller.JitterOffset(string(c.UID), tick, jitter) }
			first := c.CreationTimestamp.Truncate(time.Minute).Add(time.Minute)
			o1, o2, o3 := offset(first), offset(first.Add(time.Minute)), offset(first.Add(2*time.Minute))
			if o1 > time.Minute+2*time.Second && o2+time.Minute < o1 && o3+2*time.Minute > o1+2*time.Second {
				s, t1 = c, first
				continue
			}
			if err := e.c.Delete(context.Background(), c); err != nil {
				t.Fatal(err)
			}
		}
		t2 := t1.Add(time.Minute)
		// The clock stops just after t1's fire time: nothing else happens.
		e.clock.set(t1.Add(offset(t1)).Add(time.Second))
		e.expectTasks(s, taskName(s, t1), taskName(s, t2))
	})

	t.Run("invalid", func(t *testing.T) {
		e.t = t
		e.clock.set(time.Now())
		s := e.schedule(ns, "bad-cron", v1alpha1.ScheduleSpec{Cron: "61 * * * *"})
		e.ready(s, metav1.ConditionFalse, "InvalidSchedule")
		e.update(s, func(spec *v1alpha1.ScheduleSpec) { spec.Cron = "1 * * * *" })
		e.ready(s, metav1.ConditionTrue, "Scheduled")

		z := e.schedule(ns, "bad-zone", v1alpha1.ScheduleSpec{Cron: "0 9 * * *", Timezone: "Mars/Olympus_Mons"})
		e.ready(z, metav1.ConditionFalse, "InvalidSchedule")
		e.update(z, func(spec *v1alpha1.ScheduleSpec) { spec.Timezone = "Asia/Taipei" })
		e.ready(z, metav1.ConditionTrue, "Scheduled")
		// 09:00 in Taipei is 01:00 UTC.
		if next := z.Status.NextScheduleTime.UTC(); next.Hour() != 1 || next.Minute() != 0 {
			t.Errorf("nextScheduleTime %v, want 01:00 UTC", next)
		}

		// A task the Task Manager would refuse is not created, and nothing
		// is due while it stands.
		v := e.schedule(ns, "blank-title", v1alpha1.ScheduleSpec{Cron: "0 * * * *"})
		e.ready(v, metav1.ConditionTrue, "Scheduled")
		e.update(v, func(spec *v1alpha1.ScheduleSpec) { spec.Task.Title = " " })
		// Refused as soon as it is seen, before any tick.
		e.ready(v, metav1.ConditionFalse, "InvalidSchedule")
		e.clock.set(hourly(v.CreationTimestamp.Time, 1).Add(time.Second))
		if v.Status.ActiveSince != nil || v.Status.NextScheduleTime != nil {
			t.Errorf("activeSince %v, nextScheduleTime %v while invalid", v.Status.ActiveSince, v.Status.NextScheduleTime)
		}
		e.expectTasks(v)
	})

	t.Run("no team", func(t *testing.T) {
		e.t = t
		e.create(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "no-team"}})
		s := e.schedule("no-team", "orphan", v1alpha1.ScheduleSpec{Cron: "@hourly"})
		e.ready(s, metav1.ConditionFalse, "NoTeam")
		// With a Team it schedules; its Team deleted, it says so again.
		team := &v1alpha1.Team{ObjectMeta: meta2("no-team", v1alpha1.TeamName)}
		e.create(team)
		e.ready(s, metav1.ConditionTrue, "Scheduled")
		if err := e.c.Delete(context.Background(), team); err != nil {
			t.Fatal(err)
		}
		e.ready(s, metav1.ConditionFalse, "NoTeam")
		e.create(&v1alpha1.Team{ObjectMeta: meta2("no-team", v1alpha1.TeamName)})
		e.ready(s, metav1.ConditionTrue, "Scheduled")
	})

	t.Run("a leader died before its status write", func(t *testing.T) {
		e.t = t
		e.clock.set(time.Now())
		s := e.schedule(ns, "crashed", v1alpha1.ScheduleSpec{Cron: "0 * * * *"})
		e.ready(s, metav1.ConditionTrue, "Scheduled")
		t1 := hourly(s.CreationTimestamp.Time, 1)
		// The tick's task is there, the status does not say so.
		if err := e.store.CreateTask(context.Background(), &v1alpha1.Task{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: taskName(s, t1), Labels: map[string]string{v1alpha1.LabelSchedule: s.Name}},
			Spec:       v1alpha1.TaskSpec{Title: "Today's thread"},
		}); err != nil {
			t.Fatal(err)
		}
		e.clock.set(t1.Add(time.Second))
		e.expectTasks(s, taskName(s, t1))
		eventually(t, 10*time.Second, func() (bool, string) {
			_ = e.c.Get(context.Background(), client.ObjectKeyFromObject(s), s)
			return s.Status.LastTask == taskName(s, t1), "lastTask " + s.Status.LastTask
		})
	})

	t.Run("two leaders", func(t *testing.T) {
		e.t = t
		e.clock.set(time.Now())
		s := e.schedule(ns, "contended", v1alpha1.ScheduleSpec{Cron: "0 * * * *"})
		e.ready(s, metav1.ConditionTrue, "Scheduled")
		e.run(2)
		created := s.CreationTimestamp.Time
		var want []string
		for n := 1; n <= 4; n++ {
			e.clock.set(hourly(created, n).Add(time.Second))
			want = append(want, taskName(s, hourly(created, n)))
			eventually(t, 10*time.Second, func() (bool, string) {
				got := e.tasks(s)
				return len(got) >= n, strings.Join(got, ",")
			})
		}
		e.expectTasks(s, want...)
	})
}
