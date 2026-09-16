package v1alpha1

import (
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const Finalizer = "webapps.example.com/finalizer"

const (
	ConditionProgressing = "Progressing"
	ConditionAvailable   = "Available"
	ConditionDegraded    = "Degraded"
	ConditionReady       = "Ready"
)

type Container struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	Name string `json:"name"`
	// +kubebuilder:validation:MinLength=1
	Image string `json:"image"`
	// +listType=map
	// +listMapKey=containerPort
	Ports     []ContainerPort             `json:"ports,omitempty"`
	Env       []corev1.EnvVar             `json:"env,omitempty"`
	EnvFrom   []corev1.EnvFromSource      `json:"envFrom,omitempty"`
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`
	Command   []string                    `json:"command,omitempty"`
	Args      []string                    `json:"args,omitempty"`
}

type ContainerPort struct {
	// +kubebuilder:validation:MaxLength=15
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Name string `json:"name,omitempty"`
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	ContainerPort int32 `json:"containerPort"`
	// +kubebuilder:validation:Enum=TCP;UDP
	// +kubebuilder:default=TCP
	Protocol corev1.Protocol `json:"protocol,omitempty"`
}

type AutoscalingSpec struct {
	Enabled bool `json:"enabled,omitempty"`
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=1
	MinReplicas *int32 `json:"minReplicas,omitempty"`
	// +kubebuilder:validation:Minimum=1
	MaxReplicas *int32 `json:"maxReplicas,omitempty"`
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	// +kubebuilder:default=70
	TargetCPUUtilizationPercentage *int32 `json:"targetCPUUtilizationPercentage,omitempty"`
}

type StrategySpec struct {
	// +kubebuilder:validation:Enum=RollingUpdate;Recreate
	// +kubebuilder:default=RollingUpdate
	Type          appsv1.DeploymentStrategyType   `json:"type,omitempty"`
	RollingUpdate *appsv1.RollingUpdateDeployment `json:"rollingUpdate,omitempty"`
}

type WebAppSpec struct {
	// +kubebuilder:validation:MinItems=1
	// +listType=map
	// +listMapKey=name
	Containers []Container `json:"containers"`
	// +kubebuilder:validation:MaxLength=253
	Domain           string  `json:"domain,omitempty"`
	IngressClassName *string `json:"ingressClassName,omitempty"`
	// +kubebuilder:validation:Enum=ClusterIP;NodePort;LoadBalancer
	// +kubebuilder:default=ClusterIP
	ServiceType corev1.ServiceType `json:"serviceType,omitempty"`
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:default=1
	Replicas         *int32                        `json:"replicas,omitempty"`
	Autoscaling      *AutoscalingSpec              `json:"autoscaling,omitempty"`
	Strategy         *StrategySpec                 `json:"strategy,omitempty"`
	ImagePullSecrets []corev1.LocalObjectReference `json:"imagePullSecrets,omitempty"`
	NodeSelector     map[string]string             `json:"nodeSelector,omitempty"`
	Tolerations      []corev1.Toleration           `json:"tolerations,omitempty"`
	Affinity         *corev1.Affinity              `json:"affinity,omitempty"`
}

type WebAppStatus struct {
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// +optional
	Replicas int32 `json:"replicas"`
	// +optional
	ReadyReplicas int32 `json:"readyReplicas"`
	// +optional
	AvailableReplicas int32 `json:"availableReplicas"`
	// +optional
	UpdatedReplicas int32  `json:"updatedReplicas"`
	Selector        string `json:"selector,omitempty"`
	IngressURL      string `json:"ingressURL,omitempty"`
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:subresource:scale:specpath=.spec.replicas,statuspath=.status.replicas,selectorpath=.status.selector
// +kubebuilder:resource:shortName=wa,categories=all
// +kubebuilder:storageversion
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Replicas",type=integer,JSONPath=`.status.replicas`
// +kubebuilder:printcolumn:name="Available",type=integer,JSONPath=`.status.availableReplicas`
// +kubebuilder:printcolumn:name="URL",type=string,JSONPath=`.status.ingressURL`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type WebApp struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              WebAppSpec   `json:"spec,omitempty"`
	Status            WebAppStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type WebAppList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []WebApp `json:"items"`
}
