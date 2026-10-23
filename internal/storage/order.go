package storage

import (
	"cmp"
	"slices"
	"strings"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
)

var priorityRank = map[v1alpha1.Priority]int{
	v1alpha1.PriorityUrgent: 0,
	v1alpha1.PriorityHigh:   1,
	v1alpha1.PriorityMedium: 2,
	"":                      2, // unset defaults to medium
	v1alpha1.PriorityLow:    3,
}

// CompareTasks is the board order of the Index: priority (urgent first), then
// number, then name. Drivers that sort in memory use it.
func CompareTasks(a, b *v1alpha1.Task) int {
	return cmp.Or(
		cmp.Compare(priorityRank[a.Spec.Priority], priorityRank[b.Spec.Priority]),
		cmp.Compare(a.Status.Number, b.Status.Number),
		strings.Compare(a.Name, b.Name),
	)
}

// Matches reports whether t is selected by q.
func (q Query) Matches(t *v1alpha1.Task) bool {
	return t.Namespace == q.Namespace && (len(q.Phases) == 0 || slices.Contains(q.Phases, t.Status.Phase))
}

// Page applies q's offset and limit to sorted results.
func (q Query) Page(ts []v1alpha1.Task) []v1alpha1.Task {
	if q.Offset >= len(ts) {
		return nil
	}
	ts = ts[q.Offset:]
	if q.Limit > 0 && q.Limit < len(ts) {
		ts = ts[:q.Limit]
	}
	return ts
}
