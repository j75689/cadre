// Package v1alpha1 contains the cadre.dev/v1alpha1 API types.
//
// +kubebuilder:object:generate=true
// +groupName=cadre.dev
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// GroupVersion is the group and version used to register these objects.
var GroupVersion = schema.GroupVersion{Group: "cadre.dev", Version: "v1alpha1"}

var (
	// SchemeBuilder registers the types of this group version with a scheme.
	SchemeBuilder = runtime.NewSchemeBuilder(addKnownTypes)

	// AddToScheme adds the types of this group version to a scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)

func addKnownTypes(scheme *runtime.Scheme) error {
	scheme.AddKnownTypes(GroupVersion,
		&Task{}, &TaskList{}, &Team{}, &TeamList{},
		&Agent{}, &AgentList{}, &ModelProvider{}, &ModelProviderList{}, &ToolServer{}, &ToolServerList{}, &Runner{}, &RunnerList{},
	)
	metav1.AddToGroupVersion(scheme, GroupVersion)
	return nil
}
