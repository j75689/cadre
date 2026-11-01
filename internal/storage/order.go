package storage

import (
	"cmp"
	"slices"
	"strconv"
	"strings"
	"unicode"

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
		(q.Parent == "" || t.Spec.ParentRef != nil && t.Spec.ParentRef.Name == q.Parent) &&
		(q.Number == 0 || t.Status.Number == q.Number) &&
		(q.Assignee == "" || t.Spec.Assignee != nil && t.Spec.Assignee.Name == q.Assignee) &&
		!slices.ContainsFunc(q.Labels, func(l string) bool { return !slices.Contains(t.Spec.Labels, l) }) &&
		q.matchesText(t)
}

// matchesText is Query.Text's half of Matches, the web board's search: a
// case-insensitive substring of the fields a card shows, never the
// description or the thread.
func (q Query) matchesText(t *v1alpha1.Task) bool {
	text := fold(strings.TrimSpace(q.Text))
	if text == "" {
		return true
	}
	fields := append([]string{t.Spec.Title, t.Name}, t.Spec.Labels...)
	if t.Spec.Assignee != nil {
		fields = append(fields, t.Spec.Assignee.Name)
	}
	if t.Status.Number > 0 {
		fields = append(fields, "#"+strconv.FormatInt(t.Status.Number, 10))
	}
	return slices.ContainsFunc(fields, func(f string) bool { return strings.Contains(fold(f), text) })
}

// fold maps each rune of s to one rune of its case: the smallest of the simple
// case folding orbit of its lower case, so σ, ς and Σ are one rune, and runes
// that lower to the same rune stay one (İ and i).
func fold(s string) string {
	return strings.Map(func(r rune) rune {
		r = unicode.ToLower(r)
		m := r
		for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
			m = min(m, f)
		}
		return m
	}, s)
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
