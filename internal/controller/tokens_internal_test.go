package controller

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
)

// A Secret that is the other kind's token names its owner and never says
// to delete it; any other Secret not the owner's does.
func TestNotOwned(t *testing.T) {
	controlled := func(apiVersion, kind, name string) *corev1.Secret {
		yes := true
		return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "x-access-token", OwnerReferences: []metav1.OwnerReference{
			{APIVersion: apiVersion, Kind: kind, Name: name, UID: "other", Controller: &yes},
		}}}
	}
	runner := tokenOwner{obj: &v1alpha1.Runner{ObjectMeta: metav1.ObjectMeta{Name: "x-access", UID: "me"}}, kind: "Runner"}
	access := tokenOwner{obj: &v1alpha1.AgentAccess{ObjectMeta: metav1.ObjectMeta{Name: "x", UID: "me"}}, kind: "AgentAccess"}
	cadre := v1alpha1.GroupVersion.String()
	for name, tc := range map[string]struct {
		s    *corev1.Secret
		o    tokenOwner
		want string
	}{
		"an AgentAccess's, seen by a Runner": {controlled(cadre, "AgentAccess", "x"), runner, "is AgentAccess x's token; it yields within a minute"},
		"a Runner's, seen by an AgentAccess": {controlled(cadre, "Runner", "x-access"), access, "is Runner x-access's token; give this AgentAccess another name"},
		"another group's Runner":             {controlled("example.test/v1", "Runner", "x-access"), access, "is not this AgentAccess's token; delete it"},
		"an older Runner's of the same name": {controlled(cadre, "Runner", "x-access"), runner, "is not this Runner's token; delete it"},
		"nobody's":                           {&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "x-access-token"}}, runner, "is not this Runner's token; delete it"},
	} {
		if got := notOwned(tc.s, tc.o); !strings.Contains(got, tc.want) {
			t.Errorf("%s: %q, want %q", name, got, tc.want)
		}
	}
}
