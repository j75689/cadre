// Package chaos_test runs two cadre-server instances against one API server
// and object store, with fake runners, while servers and runners are killed
// and restarted and storage writes are delayed or fail ambiguously. It then
// checks the invariants of docs/storage.md: no task ever runs twice, every
// run ends exactly once and for the right reason, outbox effects land
// exactly once, the Team semaphore holds, and the index rebuilds completely.
//
// CADRE_CHAOS_SEED replays a seed; CADRE_CHAOS_DURATION sets how long the
// chaos lasts (60s by default, 20s with -short).
package chaos_test

import (
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	"github.com/j75689/cadre/internal/api"
	"github.com/j75689/cadre/internal/client"
	"github.com/j75689/cadre/internal/fakerunner"
	"github.com/j75689/cadre/internal/runner"
	"github.com/j75689/cadre/internal/storage/kubernetes"
	"github.com/j75689/cadre/internal/testenv"
)

// teams are the namespaces under test and their concurrency limits.
var teams = map[string]int32{"chaos-a": 2, "chaos-b": 3}

const runners = 6

// recorder keeps everything the runners and the human saw.
type recorder struct {
	mu        sync.Mutex
	obs       map[string][]fakerunner.Observation // by run id, in order
	cancelled map[string]bool                     // "ns/name" of tasks a human sent a cancel for
	overshoot []string                            // the first sample of each semaphore violation episode
	inExcess  map[string]bool                     // whether the last sample of a namespace was over its limit
	maxSeen   map[string]int                      // highest Running count seen per namespace
	stalled   map[string]bool                     // run ids planned to stall
	runnerErr []string                            // errors a runner gave up on while the chaos ran
}

func newRecorder() *recorder {
	return &recorder{
		obs: map[string][]fakerunner.Observation{}, cancelled: map[string]bool{},
		inExcess: map[string]bool{}, maxSeen: map[string]int{}, stalled: map[string]bool{},
	}
}

func (r *recorder) observe(o fakerunner.Observation) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.obs[o.RunID] = append(r.obs[o.RunID], o)
}

func TestChaos(t *testing.T) {
	seed := uint64(time.Now().UnixNano())
	if v := os.Getenv("CADRE_CHAOS_SEED"); v != "" {
		var err error
		if seed, err = strconv.ParseUint(v, 10, 64); err != nil {
			t.Fatalf("CADRE_CHAOS_SEED: %v", err)
		}
	}
	duration := 60 * time.Second
	if testing.Short() {
		duration = 20 * time.Second
	}
	if v := os.Getenv("CADRE_CHAOS_DURATION"); v != "" {
		var err error
		if duration, err = time.ParseDuration(v); err != nil {
			t.Fatalf("CADRE_CHAOS_DURATION: %v", err)
		}
	}
	t.Logf("seed %d (replay with CADRE_CHAOS_SEED=%d), duration %s", seed, seed, duration)
	rnd := &lockedRand{r: rand.New(rand.NewPCG(seed, seed))}

	restCfg := testenv.APIServer(t)
	cfg := kubernetes.Config{REST: restCfg, S3: testenv.S3(t)}
	admin := adminClient(t, restCfg)
	ctx := context.Background()
	for ns, maxRuns := range teams {
		if err := admin.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil {
			t.Fatal(err)
		}
		if err := admin.Create(ctx, &v1alpha1.Team{ObjectMeta: metav1.ObjectMeta{Name: v1alpha1.TeamName, Namespace: ns}, Spec: v1alpha1.TeamSpec{MaxConcurrentRuns: maxRuns}}); err != nil {
			t.Fatal(err)
		}
		// The agent every task is assigned to, on a model that needs no
		// Secret, so the controllers find it Ready.
		for _, obj := range []ctrlclient.Object{
			&v1alpha1.ModelProvider{ObjectMeta: metav1.ObjectMeta{Name: "claude", Namespace: ns}, Spec: v1alpha1.ModelProviderSpec{
				Type: v1alpha1.ProviderBedrock, Model: "anthropic.claude", WorkloadIdentity: true, Region: "us-east-1",
			}},
			&v1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "coder", Namespace: ns}, Spec: v1alpha1.AgentSpec{
				ModelRef: "claude", Runtime: v1alpha1.AgentRuntime{Type: v1alpha1.RuntimeBuiltin},
			}},
		} {
			if err := admin.Create(ctx, obj); err != nil {
				t.Fatal(err)
			}
		}
	}

	px := newProxy(rnd)
	defer px.Close()
	cl := client.New(px.URL, nil)
	var srvMu sync.Mutex
	servers := map[string]*server{}
	start := func(name string) {
		s, err := startServer(name, cfg, rnd)
		if err != nil {
			t.Errorf("start %s: %v", name, err)
			return
		}
		srvMu.Lock()
		servers[name] = s
		srvMu.Unlock()
		px.set(name, s.srv.URL)
	}
	kill := func(name string) {
		srvMu.Lock()
		s := servers[name]
		delete(servers, name)
		srvMu.Unlock()
		if s != nil {
			px.set(name, "")
			s.kill()
		}
	}
	start("server-1")
	start("server-2")
	defer func() {
		kill("server-1")
		kill("server-2")
	}()

	rec := newRecorder()
	chaos, stopChaos := context.WithTimeout(ctx, duration)
	defer stopChaos()
	var wg sync.WaitGroup
	spawn := func(fn func()) {
		wg.Add(1)
		go func() { defer wg.Done(); fn() }()
	}

	// Humans keep adding work, answer waiting tasks and cancel a few.
	created := 0
	spawn(func() {
		for chaos.Err() == nil {
			for ns := range teams {
				if _, err := cl.CreateTask(chaos, ns, "", v1alpha1.TaskSpec{
					Title:    fmt.Sprintf("task %d", created),
					Assignee: &v1alpha1.Assignee{Kind: v1alpha1.AssigneeAgent, Name: "coder"},
				}); err == nil {
					created++
				}
			}
			sleep(chaos, rnd.between(200*time.Millisecond, 600*time.Millisecond))
		}
	})
	spawn(func() { human(chaos, cl, rnd, rec) })

	// Runners come and go.
	for i := range runners {
		spawn(func() { runnerSlot(chaos, cl, rnd, rec, fmt.Sprintf("runner-%d", i)) })
	}

	// Servers die and come back; each restart rebuilds the index from a list.
	spawn(func() {
		for chaos.Err() == nil {
			sleep(chaos, rnd.between(2*time.Second, 5*time.Second))
			if chaos.Err() != nil {
				return // never kill a server once quiescence has begun
			}
			name := fmt.Sprintf("server-%d", 1+rnd.IntN(2))
			kill(name)
			sleep(context.Background(), rnd.between(300*time.Millisecond, 2*time.Second))
			start(name)
		}
	})

	// The semaphore is sampled straight from the API server, never the index.
	spawn(func() { sampleSemaphore(chaos, admin, rec) })

	wg.Wait()
	t.Logf("chaos over: %d tasks created, %d runs observed, %d proxied requests", created, len(rec.obs), px.count.Load())

	// Quiesce with both servers up: every lease expires and is reclaimed,
	// every outbox drains, every task is numbered, the Team counts settle.
	srvMu.Lock()
	for _, name := range []string{"server-1", "server-2"} {
		if servers[name] == nil {
			srvMu.Unlock()
			start(name)
			srvMu.Lock()
		}
	}
	srvMu.Unlock()
	if t.Failed() {
		t.FailNow()
	}
	quiesce(t, admin)

	check(t, admin, cfg, rec)
}

// runnerSlot keeps one runner alive, killing and replacing it at random
// moments, often in the middle of a run.
func runnerSlot(chaos context.Context, cl *client.Client, rnd *lockedRand, rec *recorder, name string) {
	for chaos.Err() == nil {
		life, kill := context.WithTimeout(chaos, rnd.between(4*time.Second, 20*time.Second))
		_, err := fakerunner.Run(life, cl, fakerunner.Options{
			Runner:  name,
			Labels:  map[string]string{v1alpha1.LabelRuntime: string(v1alpha1.RuntimeBuiltin)},
			Poll:    50 * time.Millisecond,
			Observe: rec.observe,
			Decide:  func(task runner.ClaimedTask) fakerunner.Plan { return plan(rnd, task) },
		})
		if err != nil && life.Err() == nil {
			rec.mu.Lock()
			rec.runnerErr = append(rec.runnerErr, fmt.Sprintf("%s: %v", name, err))
			rec.mu.Unlock()
		}
		kill()
		sleep(chaos, rnd.between(0, 500*time.Millisecond))
	}
}

// plan picks what a run does: mostly short work, any outcome, and now and
// then subtasks (only for top-level tasks, so the board stays bounded).
func plan(rnd *lockedRand, task runner.ClaimedTask) fakerunner.Plan {
	p := fakerunner.Plan{Work: rnd.between(50*time.Millisecond, 1500*time.Millisecond)}
	switch r := rnd.Float64(); {
	case r < 0.4:
		p.Outcome = v1alpha1.RunOutcomeDone
	case r < 0.55:
		p.Outcome = v1alpha1.RunOutcomeReview
	case r < 0.65:
		p.Outcome = v1alpha1.RunOutcomeFailed
	default:
		p.Outcome = v1alpha1.RunOutcomeWaiting
	}
	if task.ParentRef == nil && rnd.Float64() < 0.25 {
		p.Subtasks = 1 + rnd.IntN(2)
	}
	// Now and then a runner freezes: it stops renewing for well past the
	// lease plus the reclaim grace, then wakes and sends a heartbeat and a
	// finish anyway. Fencing must reject them once the task was reclaimed.
	if rnd.Float64() < 0.1 {
		p.Work = 30 * time.Second
		p.Stall = 2*managerConfig.Lease + 2*time.Second
		p.StallAfter = rnd.IntN(2)
	}
	return p
}

// human answers waiting tasks and cancels a few tasks. It pages through
// every live task, so the board outgrowing one page changes nothing.
func human(chaos context.Context, cl *client.Client, rnd *lockedRand, rec *recorder) {
	live := []v1alpha1.Phase{v1alpha1.PhaseWaiting, v1alpha1.PhaseTodo, v1alpha1.PhaseRunning}
	for chaos.Err() == nil {
		for ns := range teams {
			var tasks []v1alpha1.Task
			for offset := 0; ; {
				page, err := cl.ListTasks(chaos, ns, live, offset, api.MaxLimit)
				if err != nil {
					break
				}
				tasks = append(tasks, page.Items...)
				if len(page.Items) < page.Limit {
					break
				}
				offset += len(page.Items)
			}
			for _, t := range tasks {
				switch {
				case t.Status.Phase == v1alpha1.PhaseWaiting:
					_, _ = cl.Reply(chaos, ns, t.Name, "human", "carry on")
				case isLive(t.Status.Phase) && rnd.Float64() < 0.01:
					// Recorded before sending: a cancel whose response is cut
					// by a server kill may still have landed.
					rec.mu.Lock()
					rec.cancelled[ns+"/"+t.Name] = true
					rec.mu.Unlock()
					_, _ = cl.CancelTask(chaos, ns, t.Name)
				}
			}
		}
		sleep(chaos, rnd.between(100*time.Millisecond, 300*time.Millisecond))
	}
}

func isLive(p v1alpha1.Phase) bool {
	return p == v1alpha1.PhaseTodo || p == v1alpha1.PhaseRunning || p == v1alpha1.PhaseWaiting
}

// sampleSemaphore lists Tasks from the API server and records any namespace
// with more Running tasks than its Team allows.
func sampleSemaphore(chaos context.Context, admin ctrlclient.Client, rec *recorder) {
	for chaos.Err() == nil {
		for ns, maxRuns := range teams {
			var list v1alpha1.TaskList
			if err := admin.List(chaos, &list, ctrlclient.InNamespace(ns)); err != nil {
				continue
			}
			running := 0
			for _, t := range list.Items {
				if t.Status.Phase == v1alpha1.PhaseRunning {
					running++
				}
			}
			rec.mu.Lock()
			rec.maxSeen[ns] = max(rec.maxSeen[ns], running)
			over := running > int(maxRuns)
			if over && !rec.inExcess[ns] {
				rec.overshoot = append(rec.overshoot, fmt.Sprintf("%s: %d Running, limit %d, from %s", ns, running, maxRuns, time.Now().Format(time.RFC3339Nano)))
			}
			rec.inExcess[ns] = over
			rec.mu.Unlock()
		}
		sleep(chaos, 20*time.Millisecond)
	}
}

// quiesce waits until nothing is Running, every outbox is empty, every task
// is numbered and every Team's running count is 0.
func quiesce(t *testing.T, admin ctrlclient.Client) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for {
		pending := settledProblems(admin)
		if len(pending) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("did not quiesce: %v", pending)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func settledProblems(admin ctrlclient.Client) []string {
	ctx := context.Background()
	var problems []string
	for ns := range teams {
		var list v1alpha1.TaskList
		if err := admin.List(ctx, &list, ctrlclient.InNamespace(ns)); err != nil {
			return []string{err.Error()}
		}
		for _, tk := range list.Items {
			switch {
			case tk.Status.Phase == v1alpha1.PhaseRunning:
				problems = append(problems, ns+"/"+tk.Name+" Running")
			case tk.Status.Phase == "":
				problems = append(problems, ns+"/"+tk.Name+" unnumbered")
			case len(tk.Status.Outbox) > 0:
				problems = append(problems, fmt.Sprintf("%s/%s outbox %d", ns, tk.Name, len(tk.Status.Outbox)))
			}
		}
		var team v1alpha1.Team
		if err := admin.Get(ctx, ctrlclient.ObjectKey{Namespace: ns, Name: v1alpha1.TeamName}, &team); err != nil {
			return []string{err.Error()}
		}
		if team.Status.Running != 0 {
			problems = append(problems, fmt.Sprintf("team %s running %d", ns, team.Status.Running))
		}
	}
	return problems
}

func adminClient(t *testing.T, restCfg *rest.Config) ctrlclient.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c, err := ctrlclient.New(restCfg, ctrlclient.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func sleep(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}
