package v1alpha1

import (
	"context"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	apiv1alpha1 "github.com/blue-samarth/Warp-CRD/api/v1alpha1"
	"github.com/blue-samarth/Warp-CRD/internal/policy"
)

// +kubebuilder:webhook:path=/validate-webapps-example-com-v1alpha1-webapppolicy,mutating=false,failurePolicy=fail,sideEffects=None,groups=webapps.example.com,resources=webapppolicies,verbs=create;update,versions=v1alpha1,name=vwebapppolicy.kb.io,admissionReviewVersions=v1

func SetupWebAppPolicyWebhook(mgr ctrl.Manager) error {
	return builder.WebhookManagedBy(mgr, &apiv1alpha1.WebAppPolicy{}).
		WithValidator(WebAppPolicyValidator{}).
		Complete()
}

type WebAppPolicyValidator struct{}

func (v WebAppPolicyValidator) ValidateCreate(_ context.Context, p *apiv1alpha1.WebAppPolicy) (admission.Warnings, error) {
	return nil, v.validate(p)
}

func (v WebAppPolicyValidator) ValidateUpdate(_ context.Context, _, p *apiv1alpha1.WebAppPolicy) (admission.Warnings, error) {
	return nil, v.validate(p)
}

func (WebAppPolicyValidator) ValidateDelete(_ context.Context, _ *apiv1alpha1.WebAppPolicy) (admission.Warnings, error) {
	return nil, nil
}

func (WebAppPolicyValidator) validate(p *apiv1alpha1.WebAppPolicy) error {
	errs := policy.Validate(p)
	if len(errs) == 0 {
		return nil
	}
	return apierrors.NewInvalid(
		apiv1alpha1.GroupVersion.WithKind("WebAppPolicy").GroupKind(), p.Name, errs)
}
