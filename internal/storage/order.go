package storage

import (
	"cmp"
	"slices"
	"strings"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
)

// rank orders priorities, urgent first. Unset counts as the default, medium;
// a value outside the enum sorts last rather than above urgent.
func rank(p v1alpha1.Priority) int {
	switch p {
	case v1alpha1.PriorityUrgent:
		return 0
	case v1alpha1.PriorityHigh:
		return 1
	case v1alpha1.PriorityMedium, "":
		return 2
	case v1alpha1.PriorityLow:
		return 3
	default:
		return 4
	}
}

// CompareTasks is the board order of the Index: priority (urgent first), then
// number, then name. Drivers that sort in memory use it.
func CompareTasks(a, b *v1alpha1.Task) int {
	return cmp.Or(
		cmp.Compare(rank(a.Spec.Priority), rank(b.Spec.Priority)),
		cmp.Compare(a.Status.Number, b.Status.Number),
		strings.Compare(a.Name, b.Name),
	)
}

// Matches reports whether t is selected by q.
func (q Query) Matches(t *v1alpha1.Task) bool {
	return t.Namespace == q.Namespace && (len(q.Phases) == 0 || slices.Contains(q.Phases, t.Status.Phase)) &&
		(q.Parent == "" || t.Spec.ParentRef != nil && t.Spec.ParentRef.Name == q.Parent)
}

// Page applies q's offset and limit to sorted results. q must be valid.
func Page[T any](q Query, ts []T) []T {
	if q.Offset >= len(ts) {
		return nil
	}
	ts = ts[q.Offset:]
	if q.Limit > 0 && q.Limit < len(ts) {
		ts = ts[:q.Limit]
	}
	return ts
}
