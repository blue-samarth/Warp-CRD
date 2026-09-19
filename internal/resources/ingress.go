package resources

import (
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/blue-samarth/Warp-CRD/api/v1alpha1"
)

func WantsIngress(app *v1alpha1.WebApp) bool { return app.Spec.Domain != "" }

func IngressURL(app *v1alpha1.WebApp) string {
	if !WantsIngress(app) {
		return ""
	}
	return "http://" + app.Spec.Domain
}

func Ingress(app *v1alpha1.WebApp) (*networkingv1.Ingress, error) {
	port, err := backendPort(app)
	if err != nil {
		return nil, err
	}
	return &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{
			Name:            app.Name,
			Namespace:       app.Namespace,
			Labels:          Labels(app),
			OwnerReferences: OwnerRefs(app),
		},
		Spec: networkingv1.IngressSpec{
			IngressClassName: app.Spec.IngressClassName,
			Rules: []networkingv1.IngressRule{{
				Host: app.Spec.Domain,
				IngressRuleValue: networkingv1.IngressRuleValue{
					HTTP: &networkingv1.HTTPIngressRuleValue{
						Paths: []networkingv1.HTTPIngressPath{{
							Path:     "/",
							PathType: new(networkingv1.PathTypePrefix),
							Backend: networkingv1.IngressBackend{
								Service: &networkingv1.IngressServiceBackend{
									Name: app.Name,
									Port: networkingv1.ServiceBackendPort{Name: port},
								},
							},
						}},
					},
				},
			}},
		},
	}, nil
}
