package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestValidateTimings(t *testing.T) {
	for _, tc := range []struct {
		lease, heartbeat time.Duration
		ok               bool
	}{
		{30 * time.Second, 10 * time.Second, true},
		{2 * time.Second, time.Second, true},
		{30 * time.Second, 500 * time.Millisecond, false},
		{1500 * time.Millisecond, time.Second, false},
		{10 * time.Second, 60 * time.Second, false},
		{10 * time.Second, 10 * time.Second, false},
		{0, 0, false},
	} {
		if err := validateTimings(tc.lease, tc.heartbeat); (err == nil) != tc.ok {
			t.Errorf("lease %s heartbeat %s: %v, want ok=%v", tc.lease, tc.heartbeat, err, tc.ok)
		}
	}
}

func TestBoundRequests(t *testing.T) {
	var deadline time.Time
	h := boundRequests(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		deadline, _ = r.Context().Deadline()
	}), time.Minute)
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil))
	if until := time.Until(deadline); until <= 0 || until > time.Minute {
		t.Fatalf("request deadline in %s, want within a minute", until)
	}
}
