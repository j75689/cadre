package task

import (
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Kubernetes serialises metav1.Time with second precision, so every time the
// machine writes is truncated to the second; otherwise a status read back from
// the API server would differ from the one written.
func metaTime(t time.Time) metav1.Time { return metav1.NewTime(t.Truncate(time.Second)) }

// leaseUntil rounds up, so truncation never shortens a lease.
func leaseUntil(now time.Time, lease time.Duration) metav1.Time {
	until := now.Add(lease)
	if r := until.Truncate(time.Second); r.Before(until) {
		until = r.Add(time.Second)
	}
	return metav1.NewTime(until)
}
