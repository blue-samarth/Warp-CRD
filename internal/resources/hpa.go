package resources

import (
	"errors"

	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/blue-samarth/Warp-CRD/api/v1alpha1"
)

const DefaultTargetCPU int32 = 70

var ErrNoMaxReplicas = errors.New("resources: autoscaling is enabled without maxReplicas")

func WantsHPA(app *v1alpha1.WebApp) bool {
	return app.Spec.Autoscaling != nil && app.Spec.Autoscaling.Enabled
}

func HPA(app *v1alpha1.WebApp) (*autoscalingv2.HorizontalPodAutoscaler, error) {
	a := app.Spec.Autoscaling
	if a.MaxReplicas == nil {
		return nil, ErrNoMaxReplicas
	}
	minReplicas := new(int32(1))
	if a.MinReplicas != nil {
		minReplicas = a.MinReplicas
	}
	return &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{
			Name:            app.Name,
			Namespace:       app.Namespace,
			Labels:          Labels(app),
			OwnerReferences: OwnerRefs(app),
		},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
				APIVersion: "apps/v1",
				Kind:       "Deployment",
				Name:       app.Name,
			},
			MinReplicas: minReplicas,
			MaxReplicas: *a.MaxReplicas,
			Metrics:     hpaMetrics(a),
			Behavior:    a.Behavior,
		},
	}, nil
}

func TargetedResources(a *v1alpha1.AutoscalingSpec) []corev1.ResourceName {
	var out []corev1.ResourceName
	if a.TargetCPUUtilizationPercentage != nil {
		out = append(out, corev1.ResourceCPU)
	}
	if a.TargetMemoryUtilizationPercentage != nil {
		out = append(out, corev1.ResourceMemory)
	}
	if len(out) == 0 {
		out = append(out, corev1.ResourceCPU)
	}
	return out
}

func hpaMetrics(a *v1alpha1.AutoscalingSpec) []autoscalingv2.MetricSpec {
	targets := map[corev1.ResourceName]*int32{
		corev1.ResourceCPU:    a.TargetCPUUtilizationPercentage,
		corev1.ResourceMemory: a.TargetMemoryUtilizationPercentage,
	}
	var out []autoscalingv2.MetricSpec
	for _, name := range TargetedResources(a) {
		target := targets[name]
		if target == nil {
			target = new(DefaultTargetCPU)
		}
		out = append(out, autoscalingv2.MetricSpec{
			Type: autoscalingv2.ResourceMetricSourceType,
			Resource: &autoscalingv2.ResourceMetricSource{
				Name: name,
				Target: autoscalingv2.MetricTarget{
					Type:               autoscalingv2.UtilizationMetricType,
					AverageUtilization: target,
				},
			},
		})
	}
	return out
}
