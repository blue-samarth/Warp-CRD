package resources

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/blue-samarth/Warp-CRD/api/v1alpha1"
)

func Service(app *v1alpha1.WebApp) *corev1.Service {
	t := app.Spec.ServiceType
	if t == "" {
		t = corev1.ServiceTypeClusterIP
	}
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:            app.Name,
			Namespace:       app.Namespace,
			Labels:          Labels(app),
			OwnerReferences: OwnerRefs(app),
		},
		Spec: corev1.ServiceSpec{
			Type:     t,
			Selector: SelectorLabels(app),
			Ports:    servicePorts(app),
		},
	}
}

func servicePorts(app *v1alpha1.WebApp) []corev1.ServicePort {
	ports := allPorts(app)
	out := make([]corev1.ServicePort, 0, len(ports))
	for _, p := range ports {
		out = append(out, corev1.ServicePort{
			Name:       portName(p),
			Port:       p.ContainerPort,
			TargetPort: intstr.FromInt32(p.ContainerPort),
			Protocol:   protocol(p),
		})
	}
	return out
}
