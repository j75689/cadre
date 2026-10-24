package controller_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	cadreclient "github.com/j75689/cadre/internal/client"
	"github.com/j75689/cadre/internal/controller"
	"github.com/j75689/cadre/internal/fakerunner"
	"github.com/j75689/cadre/internal/manager"
	"github.com/j75689/cadre/internal/runner"
	"github.com/j75689/cadre/internal/storage"
	"github.com/j75689/cadre/internal/storage/kubernetes"
	"github.com/j75689/cadre/internal/testenv"
)

// jobEnv is cadre-server in one process, on envtest: the controllers, the
// Task Manager and its loops, the runner protocol. Runners are not
// authenticated; envtest has no kubelet, so kubelet stands in for it. The
// controllers run on a manager of their own, so restart can stop them and
// start fresh ones, as a cadre-server restart does.
type jobEnv struct {
	*env
	m       *manager.Manager
	url     string
	cfg     *rest.Config
	lease   time.Duration
	stopped func() // stops the running controllers
}

const jobsNS = "team-jobs"

func startJobs(t *testing.T) *jobEnv {
	t.Helper()
	cfg := testenv.APIServer(t)
	serving, err := controller.NewManager(cfg)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- serving.Start(ctx) }()
	store, err := kubernetes.Open(ctx, kubernetes.Config{REST: cfg, S3: testenv.S3(t), Cache: serving.GetCache()})
	if err != nil {
		t.Fatal(err)
	}
	lease := 2 * time.Second
	m := manager.New(store, manager.Config{Agents: controller.Agents(serving), Lease: lease, Heartbeat: 500 * time.Millisecond})
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	runner.NewServer(m, log, srv.URL).Register(mux)
	loops := make(chan struct{})
	go func() {
		defer close(loops)
		m.Run(ctx, manager.LoopConfig{Reclaim: 200 * time.Millisecond, Dispatch: 200 * time.Millisecond}, log)
	}()
	c, err := client.New(cfg, client.Options{Scheme: serving.GetScheme()})
	if err != nil {
		t.Fatal(err)
	}
	e := &jobEnv{env: &env{t: t, c: c}, m: m, url: srv.URL, cfg: cfg, lease: lease}
	e.restart()
	t.Cleanup(func() {
		e.stopped()
		cancel()
		<-loops
		_ = store.Close()
		if err := <-done; err != nil {
			t.Errorf("manager: %v", err)
		}
	})
	e.create(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: jobsNS}},
		&v1alpha1.Team{ObjectMeta: meta2(jobsNS, v1alpha1.TeamName), Spec: v1alpha1.TeamSpec{MaxConcurrentRuns: 3}},
		&v1alpha1.ModelProvider{ObjectMeta: meta2(jobsNS, "claude"), Spec: v1alpha1.ModelProviderSpec{
			Type: v1alpha1.ProviderBedrock, Model: "anthropic.claude", WorkloadIdentity: true, Region: "us-east-1",
		}},
		&corev1.ServiceAccount{ObjectMeta: meta2(jobsNS, "job-runner")},
		&v1alpha1.Runner{ObjectMeta: meta2(jobsNS, "jobs"), Spec: v1alpha1.RunnerSpec{
			ServiceAccount: &v1alpha1.ServiceAccountRef{Name: "job-runner"}, Labels: map[string]string{"pool": "jobs", "zone": "a"},
		}},
	)
	return e
}

// restart stops the controllers, if they run, and starts fresh ones, which
// remember nothing.
func (e *jobEnv) restart() {
	e.t.Helper()
	if e.stopped != nil {
		e.stopped()
	}
	mgr, err := controller.NewManager(e.cfg)
	if err != nil {
		e.t.Fatal(err)
	}
	if err := controller.Setup(mgr, controller.Options{Resync: 200 * time.Millisecond, ServerURL: e.url, Lease: e.lease}, controller.SkipNameValidation()); err != nil {
		e.t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- mgr.Start(ctx) }()
	e.stopped = func() {
		cancel()
		if err := <-done; err != nil {
			e.t.Errorf("controllers: %v", err)
		}
	}
}

// jobAgent is a runner agent whose work runs in Jobs.
func jobAgent(name string, maxPending int32) *v1alpha1.Agent {
	return &v1alpha1.Agent{ObjectMeta: meta2(jobsNS, name), Spec: v1alpha1.AgentSpec{
		ModelRef: "claude",
		Runtime: v1alpha1.AgentRuntime{Type: v1alpha1.RuntimeRunner, Selector: map[string]string{"pool": "jobs"}, Job: &v1alpha1.AgentJob{
			RunnerRef: "jobs", MaxPending: maxPending,
			Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
				ServiceAccountName: "picked-by-the-template",
				Containers:         []corev1.Container{{Name: "runner", Image: "example.test/runner:v1"}},
			}},
		}},
	}}
}

func (e *jobEnv) task(t *testing.T, title, agent string) {
	t.Helper()
	if _, err := e.m.CreateTask(context.Background(), jobsNS, "", v1alpha1.TaskSpec{Title: title, Assignee: &v1alpha1.Assignee{Kind: v1alpha1.AssigneeAgent, Name: agent}}); err != nil {
		t.Fatal(err)
	}
}

func (e *jobEnv) jobs(t *testing.T, agent string) []batchv1.Job {
	t.Helper()
	var list batchv1.JobList
	if err := e.c.List(context.Background(), &list, client.InNamespace(jobsNS), client.MatchingLabels{controller.LabelAgent: agent}); err != nil {
		t.Fatal(err)
	}
	return list.Items
}

// eventually waits for cond, failing with what it last said.
func eventually(t *testing.T, within time.Duration, cond func() (bool, string)) {
	t.Helper()
	deadline := time.Now().Add(within)
	var last string
	for time.Now().Before(deadline) {
		var ok bool
		if ok, last = cond(); ok {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out: %s", last)
}

// kubelet runs each new Job's pod: a fake runner that knows only the env
// cadre-server gave the pod, works one run with plan, and exits; the Job
// then completes, as the Job controller would mark it.
type kubelet struct {
	e    *jobEnv
	plan fakerunner.Plan
	// pending leaves every pod Pending, as one waiting for a node or its
	// image: active, never ready, never running its runner.
	pending bool
	mu      sync.Mutex
	seen    map[string]bool
}

func (k *kubelet) run(ctx context.Context, t *testing.T, agent string) {
	for ctx.Err() == nil {
		for _, j := range k.e.jobs(t, agent) {
			k.mu.Lock()
			fresh := !k.seen[j.Name]
			k.seen[j.Name] = true
			k.mu.Unlock()
			if fresh {
				go k.pod(ctx, t, j)
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func (k *kubelet) pod(ctx context.Context, t *testing.T, j batchv1.Job) {
	started := metav1.Now()
	ready := int32(1)
	if k.pending {
		ready = 0
	}
	j.Status = batchv1.JobStatus{StartTime: &started, Active: 1, Ready: &ready}
	if err := k.e.c.Status().Update(context.Background(), &j); err != nil && ctx.Err() == nil {
		t.Errorf("start %s: %v", j.Name, err)
		return
	}
	if k.pending {
		return
	}
	env := map[string]string{}
	for _, v := range j.Spec.Template.Spec.Containers[0].Env {
		env[v.Name] = v.Value
	}
	_, err := fakerunner.Run(ctx, cadreclient.New(env["CADRE_SERVER"], nil), fakerunner.Options{
		Runner: env["CADRE_RUNNER"], Namespaces: []string{env["CADRE_NAMESPACE"]}, Agent: env["CADRE_AGENT"],
		Labels: map[string]string{"pool": "jobs"}, Plan: k.plan, Once: true, Poll: 50 * time.Millisecond,
	})
	if err != nil && ctx.Err() == nil {
		t.Errorf("pod of %s: %v", j.Name, err)
		return
	}
	// The controller may have annotated the Job meanwhile: complete the
	// latest, as the Job controller does.
	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := k.e.c.Get(context.Background(), client.ObjectKeyFromObject(&j), &j); err != nil {
			return err
		}
		now := metav1.Now()
		j.Status = batchv1.JobStatus{StartTime: &started, CompletionTime: &now, Succeeded: 1, Conditions: []batchv1.JobCondition{
			{Type: batchv1.JobSuccessCriteriaMet, Status: corev1.ConditionTrue, LastTransitionTime: now},
			{Type: batchv1.JobComplete, Status: corev1.ConditionTrue, LastTransitionTime: now},
		}}
		return k.e.c.Status().Update(context.Background(), &j)
	})
	if err != nil && !apierrors.IsNotFound(err) && ctx.Err() == nil {
		t.Errorf("complete %s: %v", j.Name, err)
	}
}

// A runner agent's work is done by Jobs the controller starts, each pod a
// runner that claims one run as its Job, and no more Jobs wait at once than
// maxPending allows.
func TestJobsDoARunnerAgentsWork(t *testing.T) {
	e := startJobs(t)
	agent := jobAgent("coder", 2)
	e.create(agent)
	e.ready(agent, metav1.ConditionTrue, "Resolved")
	for _, title := range []string{"one", "two", "three", "four"} {
		e.task(t, title, "coder")
	}

	// Before any pod runs: maxPending Jobs, built as cadre-server builds
	// them.
	eventually(t, 10*time.Second, func() (bool, string) {
		n := len(e.jobs(t, "coder"))
		return n == 2, fmt.Sprintf("%d Jobs", n)
	})
	time.Sleep(time.Second)
	jobs := e.jobs(t, "coder")
	if len(jobs) != 2 {
		t.Fatalf("%d Jobs with none of them claiming, want maxPending 2", len(jobs))
	}
	j := jobs[0]
	pod := j.Spec.Template.Spec
	c := pod.Containers[0]
	env := map[string]string{}
	for _, v := range c.Env {
		env[v.Name] = v.Value
	}
	vol := pod.Volumes[len(pod.Volumes)-1]
	switch {
	case pod.ServiceAccountName != "job-runner":
		t.Errorf("the pod runs as %q, want the Runner's ServiceAccount", pod.ServiceAccountName)
	case pod.RestartPolicy != corev1.RestartPolicyNever || *j.Spec.BackoffLimit != 0 || *j.Spec.ActiveDeadlineSeconds != 3600 || *j.Spec.TTLSecondsAfterFinished != 600:
		t.Errorf("job spec %+v", j.Spec)
	case env["CADRE_SERVER"] != e.url || env["CADRE_NAMESPACE"] != jobsNS || env["CADRE_RUNNER"] != j.Name || env["CADRE_AGENT"] != "coder":
		t.Errorf("env %v", env)
	case vol.Projected == nil || vol.Projected.Sources[0].ServiceAccountToken.Audience != controller.RunnerAudience:
		t.Errorf("token volume %+v", vol)
	case len(c.VolumeMounts) != 1 || c.VolumeMounts[0].MountPath != "/var/run/secrets/cadre":
		t.Errorf("mounts %+v", c.VolumeMounts)
	case j.Labels[controller.LabelRunner] != "jobs" || len(j.OwnerReferences) != 1 || j.OwnerReferences[0].Name != "coder":
		t.Errorf("labels %v, owners %+v", j.Labels, j.OwnerReferences)
	}

	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	k := &kubelet{e: e, plan: fakerunner.Plan{Work: 200 * time.Millisecond, Outcome: v1alpha1.RunOutcomeDone}, seen: map[string]bool{}}
	go k.run(ctx, t, "coder")

	eventually(t, 30*time.Second, func() (bool, string) {
		tasks, err := e.m.ListTasks(context.Background(), storage.Query{Namespace: jobsNS})
		if err != nil {
			return false, err.Error()
		}
		done := 0
		for _, tk := range tasks {
			if tk.Status.Phase == v1alpha1.PhaseDone {
				done++
			}
		}
		return done == 4, fmt.Sprintf("%d of 4 tasks Done", done)
	})
	jobNames := map[string]bool{}
	for _, j := range e.jobs(t, "coder") {
		jobNames[j.Name] = true
	}
	tasks, _ := e.m.ListTasks(context.Background(), storage.Query{Namespace: jobsNS})
	for _, tk := range tasks {
		runs, err := e.m.Runs(context.Background(), storage.Key{Namespace: tk.Namespace, Name: tk.Name})
		if err != nil || len(runs) != 1 || !jobNames[runs[0].RunnerName] {
			t.Errorf("%s: runs %+v, %v; want one, by one of the Jobs", tk.Name, runs, err)
		}
	}
}

// A pod that freezes mid-run has its run reclaimed and its Job deleted,
// even when cadre-server restarted in between and forgot the Job claimed.
func TestFrozenPodIsDeleted(t *testing.T) {
	e := startJobs(t)
	agent := jobAgent("coder", 1)
	e.create(agent)
	e.ready(agent, metav1.ConditionTrue, "Resolved")
	e.task(t, "stuck", "coder")

	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	k := &kubelet{e: e, plan: fakerunner.Plan{Work: time.Minute, Outcome: v1alpha1.RunOutcomeDone, Stall: time.Minute}, seen: map[string]bool{}}
	go k.run(ctx, t, "coder")
	var frozen string
	eventually(t, 10*time.Second, func() (bool, string) {
		tasks, err := e.m.ListTasks(context.Background(), storage.Query{Namespace: jobsNS})
		if err != nil || len(tasks) != 1 || tasks[0].Status.Run == nil {
			return false, "the task is not running"
		}
		frozen = tasks[0].Status.Run.RunnerName
		return frozen != "", ""
	})
	// cadre-server is down while the run is reclaimed, and comes back
	// knowing nothing of the Job's claim.
	e.stopped()
	e.stopped = func() {}
	eventually(t, 15*time.Second, func() (bool, string) {
		tasks, err := e.m.ListTasks(context.Background(), storage.Query{Namespace: jobsNS})
		if err != nil || len(tasks) != 1 {
			return false, "no task"
		}
		r := tasks[0].Status.Run
		return r == nil || r.RunnerName != frozen, "the frozen pod still holds the run"
	})
	e.restart()
	eventually(t, 30*time.Second, func() (bool, string) {
		for _, j := range e.jobs(t, "coder") {
			if j.Name == frozen && j.DeletionTimestamp == nil {
				return false, "the frozen Job is still there"
			}
		}
		return true, ""
	})
}

// A pod still Pending (waiting for a node, pulling its image) is not
// stuck, however long that takes: its clock starts when it runs.
func TestAPendingPodIsLeftToStart(t *testing.T) {
	e := startJobs(t)
	agent := jobAgent("slow", 1)
	e.create(agent)
	e.ready(agent, metav1.ConditionTrue, "Resolved")
	e.task(t, "waits", "slow")
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	k := &kubelet{e: e, pending: true, seen: map[string]bool{}}
	go k.run(ctx, t, "slow")
	var first string
	eventually(t, 10*time.Second, func() (bool, string) {
		jobs := e.jobs(t, "slow")
		if len(jobs) == 0 || jobs[0].Status.Active == 0 {
			return false, "no started Job"
		}
		first = jobs[0].Name
		return true, ""
	})
	time.Sleep(3 * e.lease) // past the stuck rule's two leases
	jobs := e.jobs(t, "slow")
	if len(jobs) != 1 || jobs[0].Name != first || jobs[0].DeletionTimestamp != nil {
		t.Fatalf("jobs %v; want the Pending one, kept", names(jobs))
	}
}

// Two agents sharing a Runner: each Job's pod claims its own agent's task
// only.
func TestAJobWorksItsOwnAgentsTasks(t *testing.T) {
	e := startJobs(t)
	for _, name := range []string{"left", "right"} {
		a := jobAgent(name, 1)
		e.create(a)
		e.ready(a, metav1.ConditionTrue, "Resolved")
	}
	// The right agent's task is the older: a claim that ignored the agent
	// would take it first.
	e.task(t, "for right", "right")
	e.task(t, "for left", "left")
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	for _, name := range []string{"left", "right"} {
		k := &kubelet{e: e, plan: fakerunner.Plan{Work: 100 * time.Millisecond, Outcome: v1alpha1.RunOutcomeDone}, seen: map[string]bool{}}
		go k.run(ctx, t, name)
	}
	byJob := map[string]string{}
	eventually(t, 30*time.Second, func() (bool, string) {
		for _, name := range []string{"left", "right"} {
			for _, j := range e.jobs(t, name) {
				byJob[j.Name] = name
			}
		}
		tasks, err := e.m.ListTasks(context.Background(), storage.Query{Namespace: jobsNS})
		if err != nil {
			return false, err.Error()
		}
		for _, tk := range tasks {
			if tk.Status.Phase != v1alpha1.PhaseDone {
				return false, tk.Name + " is " + string(tk.Status.Phase)
			}
		}
		return true, ""
	})
	tasks, _ := e.m.ListTasks(context.Background(), storage.Query{Namespace: jobsNS})
	for _, tk := range tasks {
		runs, err := e.m.Runs(context.Background(), storage.Key{Namespace: tk.Namespace, Name: tk.Name})
		if err != nil || len(runs) != 1 || byJob[runs[0].RunnerName] != tk.Spec.Assignee.Name {
			t.Errorf("%s (%s): runs %+v; want one, by a Job of its own agent", tk.Name, tk.Spec.Assignee.Name, runs)
		}
	}
}

func names(jobs []batchv1.Job) []string {
	out := make([]string, 0, len(jobs))
	for _, j := range jobs {
		out = append(out, j.Name)
	}
	return out
}

// An agent that loses its job has its Jobs deleted.
func TestJobsGoWithTheJobBlock(t *testing.T) {
	e := startJobs(t)
	agent := jobAgent("coder", 1)
	e.create(agent)
	e.ready(agent, metav1.ConditionTrue, "Resolved")
	e.task(t, "never run", "coder")
	eventually(t, 10*time.Second, func() (bool, string) {
		return len(e.jobs(t, "coder")) > 0, "no Job"
	})
	var a v1alpha1.Agent
	if err := e.c.Get(context.Background(), client.ObjectKeyFromObject(agent), &a); err != nil {
		t.Fatal(err)
	}
	a.Spec.Runtime.Job = nil
	if err := e.c.Update(context.Background(), &a); err != nil {
		t.Fatal(err)
	}
	eventually(t, 10*time.Second, func() (bool, string) {
		for _, j := range e.jobs(t, "coder") {
			if j.DeletionTimestamp == nil {
				return false, "a Job outlived its agent's job block"
			}
		}
		return true, ""
	})
}

// A job whose Runner cannot run its Jobs keeps the agent from being Ready.
func TestAJobsRunnerMustFit(t *testing.T) {
	e := startJobs(t)
	e.create(&v1alpha1.Runner{ObjectMeta: meta2(jobsNS, "outside"), Spec: v1alpha1.RunnerSpec{Labels: map[string]string{"pool": "jobs"}}})
	for _, tc := range []struct{ runner, reason string }{
		{"missing", "RunnerNotFound"},
		{"outside", "RunnerOutsideTheCluster"},
	} {
		a := jobAgent("a-"+tc.runner, 1)
		a.Spec.Runtime.Job.RunnerRef = tc.runner
		e.create(a)
		e.ready(a, metav1.ConditionFalse, tc.reason)
	}
	wrong := jobAgent("wrong-labels", 1)
	wrong.Spec.Runtime.Selector = map[string]string{"pool": "other"}
	e.create(wrong)
	e.ready(wrong, metav1.ConditionFalse, "RunnerLabelsMismatch")
}
