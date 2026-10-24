package controller

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	"github.com/j75689/cadre/internal/storage"
	"github.com/j75689/cadre/internal/task"
)

// A Schedule's tasks are created through the store, as every task is: they
// are Kubernetes objects only under the Kubernetes driver.
//
// +kubebuilder:rbac:groups=cadre.dev,resources=schedules,verbs=get;list;watch
// +kubebuilder:rbac:groups=cadre.dev,resources=schedules/status,verbs=get;update
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch

// SetupSchedules registers the Schedule reconciler with mgr. It needs the
// store, which opens on mgr's cache once mgr has started, so it is called
// after the others, and may be called after mgr.Start: controller-runtime
// starts a controller added late, with leader election once elected.
func SetupSchedules(mgr ctrl.Manager, store storage.Objects, o Options, opts ...SetupOption) error {
	var sc setupConfig
	for _, opt := range opts {
		opt(&sc)
	}
	if o.Resync <= 0 {
		o.Resync = DefaultResync
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	r := &scheduleReconciler{
		base:   base{client: mgr.GetClient(), secrets: mgr.GetAPIReader(), resync: o.Resync, skipNames: sc.skipNameValidation},
		store:  store,
		now:    o.Now,
		events: mgr.GetEventRecorder("cadre-schedule"),
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.Schedule{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Named("schedule").WithOptions(r.options()).Complete(r)
}

type scheduleReconciler struct {
	base
	store  storage.Objects
	now    func() time.Time
	events events.EventRecorder
}

// Reconcile creates the task of the latest tick that is due and has not
// fired, and comes back when the next one is. A tick's task has a name made
// of the Schedule's and the tick's, so creating it twice, after a restart
// or by two leaders at once, fails with ErrAlreadyExists the second time:
// the store, not this reconciler, remembers what fired.
func (r *scheduleReconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	var s v1alpha1.Schedule
	if err := r.client.Get(ctx, req.NamespacedName, &s); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}
	again := reconcile.Result{RequeueAfter: r.resync}
	st := &s.Status
	// Ticks pass unfired while it is suspended or cannot run: it is active
	// again only from when it next can.
	inactive := func(ready bool, reason, msg string) (reconcile.Result, error) {
		changed := st.ActiveSince != nil || st.NextScheduleTime != nil
		st.ActiveSince, st.NextScheduleTime = nil, nil
		return again, r.writeStatus(ctx, &s, &st.ConfigStatus, ready, reason, msg, changed)
	}
	if ok, err := r.hasTeam(ctx, req.Namespace); err != nil {
		return reconcile.Result{}, err
	} else if !ok {
		return inactive(false, "NoTeam", "namespace "+req.Namespace+" has no Team")
	}
	sched, err := parseSchedule(s.Spec)
	switch {
	case err != nil:
		return inactive(false, "InvalidSchedule", err.Error())
	case s.Spec.Suspend:
		return inactive(true, "Suspended", "")
	}
	now := r.now()

	var changed bool
	if st.ActiveSince == nil {
		// Never reconciled: it is active from its creation. Otherwise it
		// was inactive until now.
		since := now
		if meta.FindStatusCondition(st.Conditions, v1alpha1.ConditionReady) == nil {
			since = s.CreationTimestamp.Time
		}
		st.ActiveSince, changed = stamp(since), true
	}
	after := st.ActiveSince.Time
	if st.LastScheduleTime != nil && st.LastScheduleTime.After(after) {
		after = st.LastScheduleTime.Time
	}
	jitter := time.Duration(0)
	if s.Spec.Jitter != nil {
		jitter = s.Spec.Jitter.Duration
	}
	fire := func(tick time.Time) time.Time { return tick.Add(jitterOffset(string(s.UID), tick, jitter)) }

	due, missed := latestDue(sched, after, now, fire)
	if !due.IsZero() {
		if err := task.ValidateSpec(&s.Spec.Task); err != nil {
			return inactive(false, "InvalidSchedule", "its task is invalid: "+err.Error())
		}
		name := fmt.Sprintf("%s-%s", s.Name, due.UTC().Format("200601021504"))
		created := &v1alpha1.Task{
			ObjectMeta: metav1.ObjectMeta{
				Namespace:   s.Namespace,
				Name:        name,
				Labels:      map[string]string{v1alpha1.LabelSchedule: s.Name},
				Annotations: map[string]string{v1alpha1.AnnotationScheduledAt: due.UTC().Format(time.RFC3339)},
			},
			Spec: *s.Spec.Task.DeepCopy(),
		}
		switch err := r.store.CreateTask(ctx, created); {
		case err == nil:
			if missed != "" {
				r.events.Eventf(&s, nil, corev1.EventTypeWarning, "MissedTicks", "Schedule",
					"%s earlier ticks were missed and skipped; only the latest, %s, fired", missed, due.UTC().Format(time.RFC3339))
			}
		case errors.Is(err, storage.ErrInvalid):
			return inactive(false, "InvalidSchedule", "its task is refused: "+err.Error())
		case !errors.Is(err, storage.ErrAlreadyExists):
			return reconcile.Result{}, err
		}
		st.LastScheduleTime, st.LastTask, changed = stamp(due), name, true
		after = due
	}

	// Every tick after after whose fire time has come fired, or was skipped.
	next := sched.Next(after)
	if next.IsZero() {
		// No tick in the next five years, as for 30 February.
		changed = setTime(&st.NextScheduleTime, nil) || changed
		return again, r.writeStatus(ctx, &s, &st.ConfigStatus, true, "Scheduled", "no tick is coming", changed)
	}
	at := fire(next)
	changed = setTime(&st.NextScheduleTime, stamp(at)) || changed
	// A tick held back by a late one is due already: come back at once. A
	// delay of zero or less would requeue nothing.
	return reconcile.Result{RequeueAfter: max(min(at.Sub(now), r.resync), overdue)}, r.writeStatus(ctx, &s, &st.ConfigStatus, true, "Scheduled", "", changed)
}

// overdue is how soon a Schedule whose next tick is due already is looked
// at again.
const overdue = 100 * time.Millisecond

// parser reads standard five-field cron expressions and descriptors.
var parser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)

// parseSchedule reads a Schedule's cron in its time zone. The parser would
// take a zone from a TZ= or CRON_TZ= prefix too, over timezone, and panics
// on one without an expression; the zone is timezone's alone, and Local,
// the serving pod's, is not one. Ticks under a minute apart are refused:
// two would share a task name.
func parseSchedule(spec v1alpha1.ScheduleSpec) (cron.Schedule, error) {
	loc := time.UTC
	switch {
	case spec.Timezone == "Local":
		return nil, errors.New(`timezone "Local": name an IANA zone`)
	case spec.Timezone != "":
		var err error
		if loc, err = time.LoadLocation(spec.Timezone); err != nil {
			return nil, fmt.Errorf("timezone %q: %w", spec.Timezone, err)
		}
	}
	if expr := strings.TrimSpace(spec.Cron); strings.HasPrefix(expr, "TZ=") || strings.HasPrefix(expr, "CRON_TZ=") {
		return nil, fmt.Errorf("cron %q: set the zone with timezone, not a prefix", spec.Cron)
	}
	sched, err := parser.Parse(spec.Cron)
	if err != nil {
		return nil, fmt.Errorf("cron %q: %w", spec.Cron, err)
	}
	if every, ok := sched.(cron.ConstantDelaySchedule); ok && every.Delay < time.Minute {
		return nil, fmt.Errorf("cron %q: ticks must be at least a minute apart", spec.Cron)
	}
	return inZone{sched, loc}, nil
}

// inZone reads a schedule's ticks in loc, whatever the zone of the time it
// is asked about.
type inZone struct {
	cron.Schedule
	loc *time.Location
}

func (z inZone) Next(t time.Time) time.Time { return z.Schedule.Next(t.In(z.loc)) }

// maxScan bounds the ticks latestDue counts: a minutely Schedule two months
// behind. Past it, the ticks up to the last day are skipped uncounted.
const maxScan = 100_000

// latestDue returns the latest tick after after whose fire time has come by
// now, and how many earlier ones also had and are skipped: a count, or
// "more than" one when there were too many to count. Ticks fire in order: one
// whose fire time has not come holds back the later ones, and one whose jitter
// outlasts the next tick was late, not missed, and fires before it.
func latestDue(sched cron.Schedule, after, now time.Time, fire func(time.Time) time.Time) (due time.Time, missed string) {
	var n, seen int
	var far bool
	for t := sched.Next(after); !t.IsZero() && !t.After(now); t = sched.Next(t) {
		if seen++; seen == maxScan && t.Before(now.Add(-24*time.Hour)) {
			far, t = true, sched.Next(now.Add(-24*time.Hour))
			if t.IsZero() || t.After(now) {
				break
			}
		}
		if fire(t).After(now) {
			break
		}
		if !due.IsZero() {
			if fire(due).After(t) {
				break
			}
			n++
		}
		due = t
	}
	switch {
	case far:
		return due, fmt.Sprintf("more than %d", n)
	case n > 0:
		return due, fmt.Sprint(n)
	}
	return due, ""
}

// jitterOffset is how long after tick a Schedule's task is due: a fixed
// amount below jitter, drawn from the Schedule's UID and the tick, so every
// leader computes the same.
func jitterOffset(uid string, tick time.Time, jitter time.Duration) time.Duration {
	if jitter <= 0 {
		return 0
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(uid + "/" + tick.UTC().Format(time.RFC3339)))
	return time.Duration(h.Sum64() % uint64(jitter))
}

// stamp is t as status stores it: to the second.
func stamp(t time.Time) *metav1.Time {
	m := metav1.NewTime(t.Truncate(time.Second))
	return &m
}

// setTime sets *dst to t and reports whether that changed it.
func setTime(dst **metav1.Time, t *metav1.Time) bool {
	if (*dst == nil) == (t == nil) && (t == nil || (*dst).Equal(t)) {
		return false
	}
	*dst = t
	return true
}
