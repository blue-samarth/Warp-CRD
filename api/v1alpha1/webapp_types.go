package v1alpha1

import (
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
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
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Name string `json:"name"`
	// +kubebuilder:validation:MinLength=1
	Image string `json:"image"`
	// +kubebuilder:validation:MaxItems=32
	// +listType=map
	// +listMapKey=containerPort
	// +listMapKey=protocol
	Ports           []ContainerPort             `json:"ports,omitempty"`
	Env             []corev1.EnvVar             `json:"env,omitempty"`
	EnvFrom         []corev1.EnvFromSource      `json:"envFrom,omitempty"`
	Resources       corev1.ResourceRequirements `json:"resources,omitempty"`
	Command         []string                    `json:"command,omitempty"`
	Args            []string                    `json:"args,omitempty"`
	LivenessProbe   *corev1.Probe               `json:"livenessProbe,omitempty"`
	ReadinessProbe  *corev1.Probe               `json:"readinessProbe,omitempty"`
	StartupProbe    *corev1.Probe               `json:"startupProbe,omitempty"`
	SecurityContext *ContainerSecurityContext   `json:"securityContext,omitempty"`
}

type ContainerSecurityContext struct {
	ReadOnlyRootFilesystem *bool `json:"readOnlyRootFilesystem,omitempty"`
	// +kubebuilder:validation:Minimum=1
	RunAsUser *int64 `json:"runAsUser,omitempty"`
	// +kubebuilder:validation:Minimum=1
	RunAsGroup *int64 `json:"runAsGroup,omitempty"`
}

type PodSecurityContext struct {
	// +kubebuilder:validation:Minimum=1
	RunAsUser *int64 `json:"runAsUser,omitempty"`
	// +kubebuilder:validation:Minimum=1
	RunAsGroup *int64 `json:"runAsGroup,omitempty"`
	// +kubebuilder:validation:Minimum=1
	FSGroup *int64 `json:"fsGroup,omitempty"`
}

type ContainerPort struct {
	// +kubebuilder:validation:MaxLength=15
	// +kubebuilder:validation:Pattern=`^[a-z0-9]+(-[a-z0-9]+)*$`
	// +kubebuilder:validation:XValidation:rule="self.matches('[a-z]')",message="port name must contain at least one letter (a-z)"
	Name string `json:"name,omitempty"`
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	ContainerPort int32 `json:"containerPort"`
	// +kubebuilder:validation:Enum=TCP;UDP
	// +kubebuilder:default=TCP
	Protocol corev1.Protocol `json:"protocol,omitempty"`
}

// +kubebuilder:validation:XValidation:rule="!has(self.enabled) || !self.enabled || has(self.maxReplicas)",message="spec.autoscaling.maxReplicas is required when autoscaling.enabled is true"
// +kubebuilder:validation:XValidation:rule="!has(self.maxReplicas) || !has(self.minReplicas) || self.maxReplicas >= self.minReplicas",message="spec.autoscaling.maxReplicas must be greater than or equal to minReplicas"
type AutoscalingSpec struct {
	Enabled bool `json:"enabled,omitempty"`
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=1
	MinReplicas *int32 `json:"minReplicas,omitempty"`
	// +kubebuilder:validation:Minimum=1
	MaxReplicas *int32 `json:"maxReplicas,omitempty"`
	// +kubebuilder:validation:Minimum=1
	TargetCPUUtilizationPercentage *int32 `json:"targetCPUUtilizationPercentage,omitempty"`
	// +kubebuilder:validation:Minimum=1
	TargetMemoryUtilizationPercentage *int32                                         `json:"targetMemoryUtilizationPercentage,omitempty"`
	Behavior                          *autoscalingv2.HorizontalPodAutoscalerBehavior `json:"behavior,omitempty"`
}

// +kubebuilder:validation:XValidation:rule="!has(self.rollingUpdate) || !has(self.type) || self.type == 'RollingUpdate'",message="spec.strategy.rollingUpdate may only be set when strategy.type is RollingUpdate"
type StrategySpec struct {
	// +kubebuilder:validation:Enum=RollingUpdate;Recreate
	// +kubebuilder:default=RollingUpdate
	Type          appsv1.DeploymentStrategyType   `json:"type,omitempty"`
	RollingUpdate *appsv1.RollingUpdateDeployment `json:"rollingUpdate,omitempty"`
}

// +kubebuilder:validation:XValidation:rule="!has(self.ingressClassName) || has(self.domain)",message="spec.ingressClassName is only meaningful with spec.domain"
// +kubebuilder:validation:XValidation:rule="!has(self.ingressPortName) || has(self.domain)",message="spec.ingressPortName is only meaningful with spec.domain"
type WebAppSpec struct {
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=16
	// +listType=map
	// +listMapKey=name
	Containers []Container `json:"containers"`
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	Domain           string  `json:"domain,omitempty"`
	IngressClassName *string `json:"ingressClassName,omitempty"`
	// +kubebuilder:validation:MaxLength=15
	IngressPortName string `json:"ingressPortName,omitempty"`
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
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	ServiceAccountName string              `json:"serviceAccountName,omitempty"`
	SecurityContext    *PodSecurityContext `json:"securityContext,omitempty"`
	PodLabels          map[string]string   `json:"podLabels,omitempty"`
	PodAnnotations     map[string]string   `json:"podAnnotations,omitempty"`
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
	Conditions []metav1.Condition `json:"conditions,omitempty"`
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
