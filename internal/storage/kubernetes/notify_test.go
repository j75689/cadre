package kubernetes

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	toolscache "k8s.io/client-go/tools/cache"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
)

// A delete the watch missed arrives as a tombstone; it is notified like any
// other, with the DeletionTimestamp set.
func TestNotifyDeletedUnwrapsTombstones(t *testing.T) {
	var got []*v1alpha1.Task
	d := &Driver{}
	d.NotifyTasks(func(tk *v1alpha1.Task) { got = append(got, tk) })
	gone := &v1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "a"}}

	d.notifyDeleted(gone)
	d.notifyDeleted(toolscache.DeletedFinalStateUnknown{Key: "ns/a", Obj: gone})
	d.notifyDeleted(toolscache.DeletedFinalStateUnknown{Key: "ns/b", Obj: "not a task"})

	if len(got) != 2 {
		t.Fatalf("%d notifications, want 2", len(got))
	}
	for _, tk := range got {
		if tk.Name != "a" || tk.DeletionTimestamp == nil {
			t.Errorf("notified %s with DeletionTimestamp %v", tk.Name, tk.DeletionTimestamp)
		}
	}
	if gone.DeletionTimestamp != nil {
		t.Error("the informer's copy was changed")
	}
}
