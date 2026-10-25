package v1alpha1_test

import (
	"context"
	"strings"
	"testing"
	"time"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
)

// The DNS-label rule on Runner and AgentAccess names refuses a create
// only: an object made before the rule, with a dot in its name, can still
// be updated, its status included, so its reconciler can say why it does
// nothing. Such an object is made here by lifting the rule for a moment.
func TestNameRuleRatchets(t *testing.T) {
	c := startAPIServer(t)
	ctx := context.Background()
	for crd, obj := range map[string]func(name string) client.Object{
		"runners.cadre.dev": func(name string) client.Object {
			return &v1alpha1.Runner{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}, Spec: v1alpha1.RunnerSpec{Labels: map[string]string{"pool": "a"}}}
		},
		"agentaccesses.cadre.dev": func(name string) client.Object {
			return &v1alpha1.AgentAccess{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}, Spec: v1alpha1.AgentAccessSpec{Role: "member"}}
		},
	} {
		// setRule takes the name rule off the CRD's root, or puts it back.
		var rule apiextensionsv1.ValidationRule
		setRule := func(on bool) {
			t.Helper()
			var def apiextensionsv1.CustomResourceDefinition
			if err := c.Get(ctx, client.ObjectKey{Name: crd}, &def); err != nil {
				t.Fatal(err)
			}
			schema := def.Spec.Versions[0].Schema.OpenAPIV3Schema
			var kept apiextensionsv1.ValidationRules
			for _, r := range schema.XValidations {
				if strings.Contains(r.Rule, "oldSelf.hasValue()") {
					rule = r
				} else {
					kept = append(kept, r)
				}
			}
			if on {
				kept = append(kept, rule)
			}
			schema.XValidations = kept
			if err := c.Update(ctx, &def); err != nil {
				t.Fatal(err)
			}
		}
		// eventually waits for a create of the dotted object name to be
		// accepted, or refused as Invalid, as the CRD now says.
		eventually := func(name string, accepted bool) client.Object {
			t.Helper()
			for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(100 * time.Millisecond) {
				o := obj(name)
				err := c.Create(ctx, o)
				if accepted && err == nil || !accepted && apierrors.IsInvalid(err) {
					return o
				}
				if err == nil {
					_ = c.Delete(ctx, o)
				}
				if time.Now().After(deadline) {
					t.Fatalf("%s: a dotted name still %v", crd, err)
				}
			}
		}
		setRule(false)
		old := eventually("old.a", true)
		setRule(true)
		eventually("new.a", false)
		// The object from before the rule takes a spec and a status write.
		switch o := old.(type) {
		case *v1alpha1.Runner:
			o.Spec.Labels["pool"] = "b"
		case *v1alpha1.AgentAccess:
			o.Spec.Role = "viewer"
		}
		if err := c.Update(ctx, old); err != nil {
			t.Fatalf("%s: updating an object from before the rule: %v", crd, err)
		}
		if err := c.Status().Update(ctx, old); err != nil {
			t.Fatalf("%s: writing its status: %v", crd, err)
		}
	}
}
