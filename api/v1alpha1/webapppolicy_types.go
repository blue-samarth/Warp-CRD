package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type EnforcementMode string

const (
	EnforcementEnforce EnforcementMode = "Enforce"
	EnforcementWarn    EnforcementMode = "Warn"
)

type ResourcePolicy struct {
	MinRequests     corev1.ResourceList `json:"minRequests,omitempty"`
	MaxLimits       corev1.ResourceList `json:"maxLimits,omitempty"`
	RequireRequests bool                `json:"requireRequests,omitempty"`
	RequireLimits   bool                `json:"requireLimits,omitempty"`
}

type WebAppPolicySpec struct {
	NamespaceSelector *metav1.LabelSelector `json:"namespaceSelector,omitempty"`
	Resources         *ResourcePolicy       `json:"resources,omitempty"`
	// +kubebuilder:validation:Minimum=1
	MaxReplicas *int32 `json:"maxReplicas,omitempty"`
	// +kubebuilder:validation:Enum=Enforce;Warn
	// +kubebuilder:default=Enforce
	Enforcement EnforcementMode `json:"enforcement,omitempty"`
}

type WebAppPolicyStatus struct {
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=wap
// +kubebuilder:printcolumn:name="Enforcement",type=string,JSONPath=`.spec.enforcement`
// +kubebuilder:printcolumn:name="Max Replicas",type=integer,JSONPath=`.spec.maxReplicas`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type WebAppPolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              WebAppPolicySpec   `json:"spec,omitempty"`
	Status            WebAppPolicyStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type WebAppPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []WebAppPolicy `json:"items"`
}
