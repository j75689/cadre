package storage

import (
	"slices"
	"testing"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
)

func TestCompareTasksUnknownPrioritySortsLast(t *testing.T) {
	mk := func(name string, p v1alpha1.Priority) *v1alpha1.Task {
		tk := &v1alpha1.Task{Spec: v1alpha1.TaskSpec{Priority: p}}
		tk.Name = name
		return tk
	}
	ts := []*v1alpha1.Task{mk("bogus", "whenever"), mk("low", v1alpha1.PriorityLow), mk("urgent", v1alpha1.PriorityUrgent), mk("unset", "")}
	slices.SortFunc(ts, CompareTasks)
	var got []string
	for _, tk := range ts {
		got = append(got, tk.Name)
	}
	if want := []string{"urgent", "unset", "low", "bogus"}; !slices.Equal(got, want) {
		t.Fatalf("order %v, want %v", got, want)
	}
}

func TestPage(t *testing.T) {
	ts := []int{1, 2, 3}
	for _, tc := range []struct {
		q    Query
		want []int
	}{
		{Query{}, []int{1, 2, 3}},
		{Query{Offset: 1, Limit: 1}, []int{2}},
		{Query{Offset: 3}, nil},
		{Query{Limit: 10}, []int{1, 2, 3}},
	} {
		if got := Page(tc.q, ts); !slices.Equal(got, tc.want) {
			t.Errorf("Page(%+v) = %v, want %v", tc.q, got, tc.want)
		}
	}
}
