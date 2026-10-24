package chaos_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	"github.com/j75689/cadre/internal/client"
	"github.com/j75689/cadre/internal/controller"
	"github.com/j75689/cadre/internal/fakerunner"
	"github.com/j75689/cadre/internal/runner"
)

// The Job backend under chaos: a third of the tasks go to jobber, a runner
// agent whose work cadre-server starts Jobs for. envtest has no kubelet, so
// kubelet stands in: it starts a pod per Job, leaves some Pending a while,
// kills some mid-run and freezes others, while servers restart (each with
// fresh controllers, which elect a leader).
const (
	jobAgentName = "jobber"
	jobMaxPend   = 2
	// jobResync is the controllers' resync in the chaos test.
	jobResync = time.Second
	// leaderNS holds the controllers' leader election Lease.
	leaderNS = "cadre-system"
	// leaderLease is the controllers' leader election Lease.
	leaderLease = 2 * time.Second
	// jobGone bounds how long a Job outlives its run: a lease for its pod
	// to exit and a resync for the controller to look, then, when the
	// leader died meanwhile, a leader Lease for another to take over and a
	// resync more, and slack for a loaded machine.
	jobGone = 3*time.Second + 2*jobResync + leaderLease + 3*time.Second
)

// jobObjects are what a Team namespace needs for jobber.
func jobObjects(ns string) []ctrlclient.Object {
	return []ctrlclient.Object{
		&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "job-runner", Namespace: ns}},
		&v1alpha1.Runner{ObjectMeta: metav1.ObjectMeta{Name: "jobs", Namespace: ns}, Spec: v1alpha1.RunnerSpec{
			ServiceAccount: &v1alpha1.ServiceAccountRef{Name: "job-runner"}, Labels: map[string]string{"pool": "jobs"},
		}},
		&v1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: jobAgentName, Namespace: ns}, Spec: v1alpha1.AgentSpec{
			ModelRef: "claude",
			Runtime: v1alpha1.AgentRuntime{Type: v1alpha1.RuntimeRunner, Selector: map[string]string{"pool": "jobs"}, Job: &v1alpha1.AgentJob{
				RunnerRef: "jobs", MaxPending: jobMaxPend,
				Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "runner", Image: "example.test/runner:v1"}}}},
			}},
		}},
	}
}

// jobRecorder keeps what kubelet and the monitor saw.
type jobRecorder struct {
	mu      sync.Mutex
	runs    map[string]string    // Job name → the run its pod claimed
	ended   map[string]time.Time // Job name → when its run was first seen over
	started int
	killed  int
	pending int
	frozen  int
	broken  int
	errs    []string
	// over is the first sample of each episode with more active Jobs than
	// maxPending plus the agent's runs.
	over     []string
	reported map[string]bool // Jobs reported outliving their run
	outlived time.Duration   // the longest a Job outlived its run
	overNow  map[string]bool
	peakJobs int
}

func newJobRecorder() *jobRecorder {
	return &jobRecorder{runs: map[string]string{}, ended: map[string]time.Time{}, overNow: map[string]bool{}, reported: map[string]bool{}}
}

func (r *jobRecorder) errorf(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.errs = append(r.errs, fmt.Sprintf(format, args...))
}

// kubelet runs a pod for every Job it sees, until ctx ends.
func kubelet(ctx context.Context, admin ctrlclient.Client, rnd *lockedRand, rec *recorder, jrec *jobRecorder) {
	seen := map[string]bool{}
	var pods sync.WaitGroup
	defer pods.Wait()
	for ctx.Err() == nil {
		var jobs batchv1.JobList
		if err := admin.List(ctx, &jobs, ctrlclient.HasLabels{controller.LabelAgent}); err == nil {
			for _, j := range jobs.Items {
				if seen[j.Namespace+"/"+j.Name] {
					continue
				}
				seen[j.Namespace+"/"+j.Name] = true
				pods.Add(1)
				go func() { defer pods.Done(); pod(ctx, admin, rnd, rec, jrec, j) }()
			}
		}
		sleep(ctx, 100*time.Millisecond)
	}
}

// pod is one Job's pod: Pending for a while now and then, then its runner,
// configured from the env cadre-server gave the pod alone; killed mid-run
// now and then (the Job fails), frozen now and then; and the Job completes
// a moment after the runner exits.
func pod(ctx context.Context, admin ctrlclient.Client, rnd *lockedRand, rec *recorder, jrec *jobRecorder, j batchv1.Job) {
	key := ctrlclient.ObjectKeyFromObject(&j)
	setStatus := func(f func(*batchv1.JobStatus)) bool {
		err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			var cur batchv1.Job
			if err := admin.Get(context.Background(), key, &cur); err != nil {
				return err
			}
			if cur.DeletionTimestamp != nil {
				return apierrors.NewNotFound(batchv1.Resource("jobs"), j.Name)
			}
			f(&cur.Status)
			return admin.Status().Update(context.Background(), &cur)
		})
		return err == nil
	}
	gone := func() bool {
		var cur batchv1.Job
		err := admin.Get(context.Background(), key, &cur)
		return apierrors.IsNotFound(err) || (err == nil && cur.DeletionTimestamp != nil)
	}
	started := metav1.Now()
	zero, one := int32(0), int32(1)
	if !setStatus(func(s *batchv1.JobStatus) { *s = batchv1.JobStatus{StartTime: &started, Active: 1, Ready: &zero} }) {
		return
	}
	// Waiting for a node or the image: longer than the stuck rule's two
	// leases at times, which must not count against the pod.
	if rnd.Float64() < 0.2 {
		jrec.mu.Lock()
		jrec.pending++
		jrec.mu.Unlock()
		sleep(ctx, rnd.between(time.Second, 3*managerConfig.Lease))
	}
	if gone() {
		// Nothing deletes a Job whose pod never ran: it is not stuck.
		if ctx.Err() == nil {
			jrec.errorf("Job %s/%s was deleted while its pod was Pending", j.Namespace, j.Name)
		}
		return
	}
	if !setStatus(func(s *batchv1.JobStatus) { s.Ready = &one }) {
		return
	}
	jrec.mu.Lock()
	jrec.started++
	jrec.mu.Unlock()
	if rnd.Float64() < 0.05 {
		// A broken runner: the pod runs but never claims, until the
		// controller deletes its Job, knowing it from no memory.
		jrec.mu.Lock()
		jrec.broken++
		jrec.mu.Unlock()
		for ctx.Err() == nil && !gone() {
			sleep(ctx, 200*time.Millisecond)
		}
		return
	}

	env := map[string]string{}
	for _, v := range j.Spec.Template.Spec.Containers[0].Env {
		env[v.Name] = v.Value
	}
	plan := fakerunner.Plan{Work: rnd.between(100*time.Millisecond, 1500*time.Millisecond), Outcome: v1alpha1.RunOutcomeDone}
	switch r := rnd.Float64(); {
	case r < 0.3:
		// A run longer than the stuck rule's two leases, which must not
		// cost the pod its Job while the run is its own.
		plan.Work = rnd.between(2*managerConfig.Lease+time.Second, 3*managerConfig.Lease+time.Second)
	case r < 0.4:
		// A pod that freezes for good: its run is reclaimed, and only the
		// controller deleting its Job ends it.
		plan.Work, plan.Stall, plan.StallAfter = time.Hour, time.Hour, rnd.IntN(2)
		jrec.mu.Lock()
		jrec.frozen++
		jrec.mu.Unlock()
	}
	life, kill := context.WithCancel(ctx)
	defer kill()
	// Now and then the pod is killed mid-run, once it holds a run.
	killed := rnd.Float64() < 0.15
	killAfter := rnd.between(50*time.Millisecond, plan.Work)
	var claim fakerunner.Observation
	var finished time.Time // when the run's finish succeeded
	var mu sync.Mutex
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = fakerunner.Run(life, client.New(env[runner.EnvServer], nil), fakerunner.Options{
			Runner: env[runner.EnvRunner], Namespaces: []string{env[runner.EnvNamespace]}, Agent: env[runner.EnvAgent],
			Labels: map[string]string{"pool": "jobs"}, Plan: plan, Once: true, Poll: 50 * time.Millisecond,
			Observe: func(o fakerunner.Observation) {
				if o.Kind == fakerunner.Claimed {
					mu.Lock()
					claim = o
					mu.Unlock()
					if killed {
						time.AfterFunc(killAfter, kill)
					}
					jrec.mu.Lock()
					jrec.runs[j.Name] = o.RunID
					jrec.mu.Unlock()
				}
				if o.Kind == fakerunner.Finished {
					mu.Lock()
					finished = o.At
					mu.Unlock()
				}
				rec.observe(o)
			},
		})
	}()
	// The Job deleted under a running pod: its pod gets a moment, as a
	// deleted pod does before it is killed. Its run may then finish only if
	// the run is still its own, which deleting the Job must not allow.
	var goneAt time.Time
	for running := true; running; {
		select {
		case <-done:
			running = false
		case <-time.After(200 * time.Millisecond):
			if goneAt.IsZero() && gone() {
				goneAt = time.Now()
				time.AfterFunc(time.Second, kill)
			}
		}
	}
	if !goneAt.IsZero() {
		mu.Lock()
		c, f := claim, finished
		mu.Unlock()
		if !f.IsZero() && f.After(goneAt) {
			jrec.errorf("Job %s was deleted at %s while its pod still had run %s, which finished as the task's at %s",
				j.Name, goneAt.Format(time.RFC3339Nano), c.RunID, f.Format(time.RFC3339Nano))
		}
		return
	}
	if killed && life.Err() != nil {
		jrec.mu.Lock()
		jrec.killed++
		jrec.mu.Unlock()
		now := metav1.Now()
		setStatus(func(s *batchv1.JobStatus) {
			*s = batchv1.JobStatus{StartTime: &started, Failed: 1, Ready: &zero, Conditions: []batchv1.JobCondition{
				{Type: batchv1.JobFailureTarget, Status: corev1.ConditionTrue, LastTransitionTime: now, Reason: "BackoffLimitExceeded"},
				{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, LastTransitionTime: now, Reason: "BackoffLimitExceeded"},
			}}
		})
		return
	}
	if ctx.Err() != nil {
		return
	}
	sleep(ctx, rnd.between(0, time.Second)) // the pod exiting
	now := metav1.Now()
	setStatus(func(s *batchv1.JobStatus) {
		*s = batchv1.JobStatus{StartTime: &started, CompletionTime: &now, Succeeded: 1, Ready: &zero, Conditions: []batchv1.JobCondition{
			{Type: batchv1.JobSuccessCriteriaMet, Status: corev1.ConditionTrue, LastTransitionTime: now},
			{Type: batchv1.JobComplete, Status: corev1.ConditionTrue, LastTransitionTime: now},
		}}
	})
}

// active reports whether j still runs: neither finished nor being deleted.
func active(j batchv1.Job) bool {
	if j.DeletionTimestamp != nil {
		return false
	}
	for _, c := range j.Status.Conditions {
		if (c.Type == batchv1.JobComplete || c.Type == batchv1.JobFailed) && c.Status == corev1.ConditionTrue {
			return false
		}
	}
	return true
}

// monitorJobs samples the Jobs and tasks of every namespace: a Job whose run
// is over is gone within jobGone, and no more Jobs run than maxPending plus
// the agent's runs plus the Jobs still exiting from a run.
func monitorJobs(ctx context.Context, admin ctrlclient.Client, jrec *jobRecorder) {
	for ctx.Err() == nil {
		for ns := range teams {
			var jobs batchv1.JobList
			var tasks v1alpha1.TaskList
			if admin.List(ctx, &jobs, ctrlclient.InNamespace(ns), ctrlclient.MatchingLabels{controller.LabelAgent: jobAgentName}) != nil ||
				admin.List(ctx, &tasks, ctrlclient.InNamespace(ns)) != nil {
				continue
			}
			runs := map[string]bool{} // run ids of jobber's Running tasks
			for _, t := range tasks.Items {
				if t.Status.Phase == v1alpha1.PhaseRunning && t.Status.Run != nil && t.Spec.Assignee != nil && t.Spec.Assignee.Name == jobAgentName {
					runs[t.Status.Run.ID] = true
				}
			}
			now := time.Now()
			jrec.mu.Lock()
			live, exiting := 0, 0
			for _, j := range jobs.Items {
				if !active(j) {
					continue
				}
				live++
				run, claimed := jrec.runs[j.Name]
				switch {
				case !claimed || runs[run]:
					delete(jrec.ended, j.Name) // its claim may have landed after the tasks were listed
				case jrec.ended[j.Name].IsZero():
					jrec.ended[j.Name] = now
					exiting++
				default:
					exiting++
					d := now.Sub(jrec.ended[j.Name])
					jrec.outlived = max(jrec.outlived, d)
					if d > jobGone && !jrec.reported[j.Name] {
						jrec.reported[j.Name] = true
						jrec.errs = append(jrec.errs, fmt.Sprintf("%s/%s outlived its run %s by %s", ns, j.Name, run, d.Round(time.Second)))
					}
				}
			}
			jrec.peakJobs = max(jrec.peakJobs, live)
			over := live > jobMaxPend+len(runs)+exiting
			if over && !jrec.overNow[ns] {
				jrec.over = append(jrec.over, fmt.Sprintf("%s: %d active Jobs, %d runs, %d exiting, at %s", ns, live, len(runs), exiting, now.Format(time.RFC3339Nano)))
			}
			jrec.overNow[ns] = over
			jrec.mu.Unlock()
		}
		sleep(ctx, 250*time.Millisecond)
	}
}

// jobProblems is what keeps a namespace from having quiesced for jobber: a
// task of its still Todo, or a Job still active.
func jobProblems(admin ctrlclient.Client, ns string, tasks []v1alpha1.Task) []string {
	var problems []string
	for _, t := range tasks {
		if t.Status.Phase == v1alpha1.PhaseTodo && t.Spec.Assignee != nil && t.Spec.Assignee.Name == jobAgentName {
			problems = append(problems, ns+"/"+t.Name+" Todo for jobber")
		}
	}
	var jobs batchv1.JobList
	if err := admin.List(context.Background(), &jobs, ctrlclient.InNamespace(ns), ctrlclient.HasLabels{controller.LabelAgent}); err != nil {
		return []string{err.Error()}
	}
	for _, j := range jobs.Items {
		if active(j) {
			problems = append(problems, ns+"/"+j.Name+" active")
		}
	}
	return problems
}

// checkJobs verifies the Job backend's invariants once the system has
// quiesced: what the pods and the monitor saw held.
func checkJobs(t *testing.T, jrec *jobRecorder) {
	// The quiescence waited for every Job to finish (jobProblems), its pod
	// exiting after a claim that found no work at worst: none waits for
	// ever.
	jrec.mu.Lock()
	defer jrec.mu.Unlock()
	for _, e := range jrec.errs {
		t.Error(e)
	}
	for _, o := range jrec.over {
		t.Errorf("too many Jobs: %s", o)
	}
	t.Logf("jobs: %d pods started, %d left Pending a while, %d killed mid-run, %d frozen, %d broken, %d claimed a run, peak %d active Jobs in a namespace, a Job outlived its run by %s at most (bound %s)",
		jrec.started, jrec.pending, jrec.killed, jrec.frozen, jrec.broken, len(jrec.runs), jrec.peakJobs, jrec.outlived.Round(100*time.Millisecond), jobGone)
	if jrec.started == 0 || len(jrec.runs) == 0 {
		t.Error("no Job's pod ran; the chaos test exercised nothing of the Job backend")
	}
}
