package fakerunner

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	"github.com/j75689/cadre/internal/client"
	"github.com/j75689/cadre/internal/httpjson"
	"github.com/j75689/cadre/internal/runner"
)

// stub is a /runner/v1 server whose endpoints the test scripts.
type stub struct {
	claims    atomic.Int32
	claimWait time.Duration
	heartbeat http.HandlerFunc
	finish    http.HandlerFunc
}

func (s *stub) start(t *testing.T) *client.Client {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+runner.PathClaim, func(w http.ResponseWriter, _ *http.Request) {
		if s.claims.Add(1) > 1 {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		time.Sleep(s.claimWait)
		httpjson.Write(w, http.StatusOK, runner.ClaimResponse{
			Task:  runner.ClaimedTask{Namespace: "team-a", Name: "t", Number: 1, Title: "t"},
			RunID: "01JB0000000000000000000001", Mode: v1alpha1.RunModeRuntime,
			LeaseUntil: time.Now().Add(2 * time.Second), LeaseSeconds: 2, HeartbeatSeconds: 1,
		})
	})
	mux.HandleFunc("POST "+runner.PathEvents, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusAccepted) })
	mux.HandleFunc("POST "+runner.PathHeartbeat, func(w http.ResponseWriter, r *http.Request) { s.heartbeat(w, r) })
	mux.HandleFunc("POST "+runner.PathFinish, func(w http.ResponseWriter, r *http.Request) { s.finish(w, r) })
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return client.New(srv.URL, nil)
}

func notFound(w http.ResponseWriter, _ *http.Request) {
	httpjson.WriteError(w, http.StatusNotFound, httpjson.CodeNotFound, "not found")
}

type recorder struct {
	mu  sync.Mutex
	obs []Observation
}

func (r *recorder) observe(o Observation) { r.mu.Lock(); defer r.mu.Unlock(); r.obs = append(r.obs, o) }

func (r *recorder) last() Observation { r.mu.Lock(); defer r.mu.Unlock(); return r.obs[len(r.obs)-1] }

// The claim's response takes a second and every heartbeat hangs. The lease
// is counted from before the claim request, and the hung heartbeat is cut at
// the lease, so the runner gives up two seconds after it asked, not later.
func TestRunnerStopsWhenItsLeaseRunsOut(t *testing.T) {
	s := &stub{
		claimWait: time.Second,
		heartbeat: func(_ http.ResponseWriter, r *http.Request) {
			select {
			case <-r.Context().Done():
			case <-time.After(10 * time.Second):
			}
		},
	}
	c := s.start(t)
	rec := &recorder{}
	start := time.Now()
	results, err := Run(context.Background(), c, Options{Runner: "r", Once: true, Observe: rec.observe, Plan: Plan{Work: time.Minute}})
	took := time.Since(start)
	if err != nil || len(results) != 1 || !results[0].Lost || rec.last().Kind != Expired {
		t.Fatalf("results %+v, last %+v, err %v; want the run to expire", results, rec.last(), err)
	}
	if took > 2500*time.Millisecond {
		t.Fatalf("stopped %s after asking for the claim; the 2s lease ran out earlier", took)
	}
}

// not_found means the task is gone: the run stops like on run_mismatch, and
// the runner carries on.
func TestNotFoundLosesTheTask(t *testing.T) {
	for name, s := range map[string]*stub{
		"on heartbeat": {heartbeat: notFound},
		"on finish":    {heartbeat: func(w http.ResponseWriter, _ *http.Request) { httpjson.Write(w, 200, runner.HeartbeatResponse{}) }, finish: notFound},
	} {
		work := 1500 * time.Millisecond
		if name == "on finish" {
			work = 10 * time.Millisecond
		}
		rec := &recorder{}
		results, err := Run(context.Background(), s.start(t), Options{Runner: "r", Once: true, Observe: rec.observe, Plan: Plan{Work: work}})
		if err != nil || len(results) != 1 || !results[0].Lost || rec.last().Kind != Rejected {
			t.Errorf("%s: results %+v, last %+v, err %v; want the run rejected", name, results, rec.last(), err)
		}
	}
}

func TestBadPlansAreRefusedBeforeClaiming(t *testing.T) {
	for _, p := range []Plan{
		{Outcome: "Paused"},
		{Outcome: v1alpha1.RunOutcomeWaiting, Reason: "Lunch"},
		{Subtasks: -1},
	} {
		s := &stub{}
		c := s.start(t)
		if _, err := Run(context.Background(), c, Options{Runner: "r", Once: true, Plan: p}); err == nil {
			t.Errorf("plan %+v accepted", p)
		}
		if n := s.claims.Load(); n != 0 {
			t.Errorf("plan %+v: %d claims made before refusing it", p, n)
		}
	}
}

// A stalled run stops renewing, then wakes and sends a heartbeat and a
// finish without checking its lease; both are marked late.
func TestStalledRunSendsLateRequests(t *testing.T) {
	var beats atomic.Int32
	s := &stub{
		heartbeat: func(w http.ResponseWriter, _ *http.Request) {
			if beats.Add(1) > 1 {
				httpjson.WriteError(w, http.StatusConflict, httpjson.CodeRunMismatch, "gone")
				return
			}
			httpjson.Write(w, http.StatusOK, runner.HeartbeatResponse{LeaseUntil: time.Now().Add(2 * time.Second), LeaseSeconds: 2})
		},
		finish: func(w http.ResponseWriter, _ *http.Request) {
			httpjson.WriteError(w, http.StatusConflict, httpjson.CodeRunMismatch, "gone")
		},
	}
	rec := &recorder{}
	plan := Plan{Work: time.Minute, Stall: 3 * time.Second, StallAfter: 1}
	results, err := Run(context.Background(), s.start(t), Options{Runner: "r", Once: true, Observe: rec.observe, Plan: plan})
	if err != nil || len(results) != 1 || !results[0].Lost {
		t.Fatalf("results %+v, %v", results, err)
	}
	var late []ObservationKind
	for _, o := range rec.obs {
		if o.Late {
			late = append(late, o.Kind)
		}
	}
	if want := []ObservationKind{Rejected, FinishSent, Rejected}; fmt.Sprint(late) != fmt.Sprint(want) {
		t.Fatalf("late observations %v, want %v", late, want)
	}
	if n := beats.Load(); n != 2 {
		t.Fatalf("%d heartbeats, want one before the stall and one late", n)
	}
}
