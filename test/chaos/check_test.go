package chaos_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	"github.com/j75689/cadre/internal/fakerunner"
	"github.com/j75689/cadre/internal/manager"
	"github.com/j75689/cadre/internal/storage"
	"github.com/j75689/cadre/internal/storage/kubernetes"
)

// window is the time a runner believed it held a task: from the claim
// response until its lease ran out or it sent its finish, whichever came
// first. Two runners must never share a task inside their windows.
type window struct {
	run        string
	from, till time.Time
}

// check verifies every invariant once the system has quiesced. A fresh
// driver reads the state, so the index under test is the rebuilt one.
func check(t *testing.T, admin ctrlclient.Client, cfg kubernetes.Config, rec *recorder) {
	ctx := context.Background()
	drv, err := kubernetes.Open(ctx, cfg)
	if err != nil {
		t.Fatalf("open a fresh driver: %v", err)
	}
	defer func() { _ = drv.Close() }()
	m := manager.New(drv, managerConfig)

	rec.mu.Lock()
	defer rec.mu.Unlock()

	// Index rebuild: the fresh driver's index lists exactly what the API
	// server holds, at the same versions.
	tasks := map[string]v1alpha1.Task{} // by "ns/name", from the API server
	for ns := range teams {
		var list v1alpha1.TaskList
		if err := admin.List(ctx, &list, ctrlclient.InNamespace(ns)); err != nil {
			t.Fatal(err)
		}
		indexed, err := drv.ListTasks(ctx, storage.Query{Namespace: ns})
		if err != nil {
			t.Fatal(err)
		}
		want := map[string]string{}
		for _, tk := range list.Items {
			tasks[ns+"/"+tk.Name] = tk
			want[tk.Name] = tk.ResourceVersion
		}
		got := map[string]string{}
		for _, tk := range indexed {
			got[tk.Name] = tk.ResourceVersion
		}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("index rebuild: %s lists %d tasks, the API server %d, or versions differ", ns, len(got), len(want))
		}
	}

	// Numbers are unique per namespace, and every task has one.
	numbers := map[string]string{}
	for key, tk := range tasks {
		ns := strings.SplitN(key, "/", 2)[0]
		n := fmt.Sprintf("%s#%d", ns, tk.Status.Number)
		if tk.Status.Number < 1 {
			t.Errorf("%s has no number", key)
		} else if other, dup := numbers[n]; dup {
			t.Errorf("%s and %s share number %s", key, other, n)
		}
		numbers[n] = key
	}

	// Run history per task, read through the Task Manager.
	history := map[string]v1alpha1.RunRecord{} // by run id
	historyTask := map[string]string{}
	for key, tk := range tasks {
		ns, name, _ := strings.Cut(key, "/")
		runs, err := m.Runs(ctx, storage.Key{Namespace: ns, Name: name})
		if err != nil {
			t.Fatalf("runs of %s: %v", key, err)
		}
		var count int64
		if tk.Status.Runs != nil {
			count = tk.Status.Runs.Count
		}
		if int64(len(runs)) != count {
			t.Errorf("%s: %d runs claimed, %d in its history; every run must end exactly once", key, count, len(runs))
		}
		for _, r := range runs {
			if _, dup := history[r.ID]; dup {
				t.Errorf("run %s ended twice", r.ID)
			}
			history[r.ID] = r
			historyTask[r.ID] = key
		}
	}

	windows := map[string][]window{} // by task
	stalledRuns, lateRejections := 0, 0
	for runID, obs := range rec.obs {
		claim := obs[0]
		if claim.Kind != fakerunner.Claimed {
			t.Errorf("run %s: first observation is %s, not Claimed", runID, claim.Kind)
			continue
		}
		key := claim.Task.Namespace + "/" + claim.Task.Name
		w := window{run: runID, from: claim.At, till: claim.LeaseUntil}
		rejected, finished, lateRejected := false, false, false
		var outcome v1alpha1.RunOutcome
		var subtasks []string
		for _, o := range obs[1:] {
			if o.RunID != runID || o.Task != claim.Task {
				t.Errorf("run %s: an observation names another run or task: %+v", runID, o)
			}
			switch o.Kind {
			case fakerunner.Renewed:
				if rejected {
					t.Errorf("run %s: a heartbeat succeeded after a run_mismatch", runID)
				}
				w.till = later(w.till, o.LeaseUntil)
			case fakerunner.FinishSent:
				w.till = earlier(w.till, o.At)
				outcome, subtasks = o.Outcome, o.Subtasks
			case fakerunner.Finished:
				if rejected {
					t.Errorf("run %s: a finish succeeded after a run_mismatch", runID)
				}
				finished = true
			case fakerunner.Rejected:
				rejected = true
				lateRejected = lateRejected || o.Late
			}
		}
		windows[key] = append(windows[key], w)

		// Every run a runner saw ends exactly once, for the right reason.
		end, ok := history[runID]
		switch {
		case !ok:
			t.Errorf("run %s on %s: claimed but missing from the run history", runID, key)
		case historyTask[runID] != key:
			t.Errorf("run %s: claimed on %s, recorded on %s", runID, key, historyTask[runID])
		case finished && (end.Reason != v1alpha1.RunFinished || end.Outcome != outcome):
			t.Errorf("run %s on %s: finish %s succeeded, history says %s %s", runID, key, outcome, end.Reason, end.Outcome)
		case !finished && end.Reason == v1alpha1.RunCancelled && !rec.cancelled[key]:
			t.Errorf("run %s on %s ended Cancelled, but nobody cancelled the task", runID, key)
		case !finished && end.Reason == v1alpha1.RunFinished && rejected:
			t.Errorf("run %s on %s: its finish was rejected, history says Finished", runID, key)
		case lateRejected && end.Reason == v1alpha1.RunFinished:
			t.Errorf("run %s on %s: stalled and rejected on waking, yet history says Finished", runID, key)
		}
		if lateRejected {
			lateRejections++
		}
		if hasLate(obs) {
			stalledRuns++
		}

		// A finish that landed (whether or not its response arrived) commits
		// the subtasks' intent to the outbox, not the subtasks themselves,
		// and cancelling a task drops intents not yet applied. So, judged on
		// the final state: every subtask exists with this task as its
		// parent, or the task ended Cancelled and those that exist have
		// this task as their parent. Nothing else.
		if ok && end.Reason == v1alpha1.RunFinished {
			ns := claim.Task.Namespace
			parent := tasks[key]
			for _, id := range subtasks {
				sub, ok := tasks[ns+"/"+strings.ToLower(id)]
				switch {
				case !ok && parent.Status.Phase != v1alpha1.PhaseCancelled:
					t.Errorf("run %s: subtask %s of %s was never created, and the task is %s, not Cancelled (cancel sent by a human: %v, outbox %d)",
						runID, id, key, parent.Status.Phase, rec.cancelled[key], len(parent.Status.Outbox))
				case ok && (sub.Spec.ParentRef == nil || sub.Spec.ParentRef.Name != claim.Task.Name):
					t.Errorf("run %s: subtask %s has parent %+v, want %s", runID, id, sub.Spec.ParentRef, claim.Task.Name)
				}
			}
		}
	}

	// The stall fault must have done its job in a full run: woken runs
	// were turned away by fencing.
	t.Logf("%d stalled runs woke up, %d were rejected by fencing", stalledRuns, lateRejections)
	if !testing.Short() && lateRejections == 0 {
		t.Error("no stalled run was rejected on waking; fencing after a reclaim went untested")
	}
	for _, e := range rec.runnerErr {
		t.Errorf("a runner gave up: %s", e)
	}

	// No task ever ran twice: the belief windows of its runs never overlap.
	overlaps := 0
	for key, ws := range windows {
		slices.SortFunc(ws, func(a, b window) int { return a.from.Compare(b.from) })
		for i := 1; i < len(ws); i++ {
			if ws[i].from.Before(ws[i-1].till) {
				overlaps++
				t.Errorf("%s ran twice: run %s held it until %s, run %s claimed it at %s",
					key, ws[i-1].run, ws[i-1].till.Format(time.RFC3339Nano), ws[i].run, ws[i].from.Format(time.RFC3339Nano))
			}
		}
	}

	// The semaphore was never exceeded, and every count settled at zero.
	for _, o := range rec.overshoot {
		t.Errorf("semaphore exceeded: %s", o)
	}

	t.Logf("checked %d tasks, %d runs in history, %d runs seen by runners, peak Running %v, %d overlaps",
		len(tasks), len(history), len(rec.obs), rec.maxSeen, overlaps)
	if len(rec.obs) == 0 || len(history) == 0 {
		t.Error("no runs happened; the chaos test exercised nothing")
	}
}

func later(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

func earlier(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}

func hasLate(obs []fakerunner.Observation) bool {
	for _, o := range obs {
		if o.Late {
			return true
		}
	}
	return false
}
