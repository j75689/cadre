package controller

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
)

// A Ready condition decided for an older spec does not make a reference
// Ready.
func TestIsReadyNeedsTheCurrentGeneration(t *testing.T) {
	mp := &v1alpha1.ModelProvider{ObjectMeta: metav1.ObjectMeta{Generation: 2}}
	ready := func(condGen, statusGen int64) v1alpha1.ConfigStatus {
		return v1alpha1.ConfigStatus{ObservedGeneration: statusGen, Conditions: []metav1.Condition{
			{Type: v1alpha1.ConditionReady, Status: metav1.ConditionTrue, ObservedGeneration: condGen},
		}}
	}
	if !isReady(mp, ready(2, 2)) {
		t.Error("ready for the current generation: want Ready")
	}
	if isReady(mp, ready(1, 1)) || isReady(mp, ready(1, 2)) || isReady(mp, ready(2, 1)) {
		t.Error("ready for an older generation: want not Ready")
	}
	if isReady(mp, v1alpha1.ConfigStatus{ObservedGeneration: 2}) {
		t.Error("no condition: want not Ready")
	}
}
