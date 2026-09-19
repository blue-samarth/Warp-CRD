// +kubebuilder:object:generate=true
// +groupName=webapps.example.com
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var (
	GroupVersion  = schema.GroupVersion{Group: "webapps.example.com", Version: "v1alpha1"}
	SchemeBuilder = runtime.NewSchemeBuilder(addKnownTypes)
	AddToScheme   = SchemeBuilder.AddToScheme
)

func addKnownTypes(s *runtime.Scheme) error {
	s.AddKnownTypes(GroupVersion,
		&WebApp{}, &WebAppList{},
		&WebAppPolicy{}, &WebAppPolicyList{},
	)
	metav1.AddToGroupVersion(s, GroupVersion)
	return nil
}
