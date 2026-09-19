package v1alpha1

import (
	"context"
	"fmt"
	"slices"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	apiv1alpha1 "github.com/blue-samarth/Warp-CRD/api/v1alpha1"
	"github.com/blue-samarth/Warp-CRD/internal/policy"
	"github.com/blue-samarth/Warp-CRD/internal/resources"
)

// +kubebuilder:webhook:path=/mutate-webapps-example-com-v1alpha1-webapp,mutating=true,failurePolicy=fail,sideEffects=None,groups=webapps.example.com,resources=webapps,verbs=create;update,versions=v1alpha1,name=mwebapp.kb.io,admissionReviewVersions=v1
// +kubebuilder:webhook:path=/validate-webapps-example-com-v1alpha1-webapp,mutating=false,failurePolicy=fail,sideEffects=None,groups=webapps.example.com,resources=webapps,verbs=create;update,versions=v1alpha1,name=vwebapp.kb.io,admissionReviewVersions=v1

// +kubebuilder:rbac:groups=webapps.example.com,resources=webapppolicies,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch

func SetupWebAppWebhook(mgr ctrl.Manager) error {
	return builder.WebhookManagedBy(mgr, &apiv1alpha1.WebApp{}).
		WithDefaulter(WebAppDefaulter{}).
		WithValidator(WebAppValidator{Client: mgr.GetClient()}).
		Complete()
}

type WebAppDefaulter struct{}

func (WebAppDefaulter) Default(_ context.Context, app *apiv1alpha1.WebApp) error {
	Default(app)
	return nil
}

type WebAppValidator struct {
	Client client.Client
}

func (v WebAppValidator) ValidateCreate(ctx context.Context, app *apiv1alpha1.WebApp) (admission.Warnings, error) {
	return v.validate(ctx, app)
}

func (v WebAppValidator) ValidateUpdate(ctx context.Context, _, app *apiv1alpha1.WebApp) (admission.Warnings, error) {
	return v.validate(ctx, app)
}

func (v WebAppValidator) validate(ctx context.Context, app *apiv1alpha1.WebApp) (admission.Warnings, error) {
	errs := ValidateSpec(app)
	warns := warnings(app)

	res, err := v.evaluatePolicies(ctx, app)
	if err != nil {
		return warns, err
	}
	errs = append(errs, res.Violations...)
	warns = append(warns, res.Warnings...)

	if len(res.Audited) > 0 {
		log.FromContext(ctx).Info("webapppolicy audit",
			"webapp", client.ObjectKeyFromObject(app).String(), "violations", res.Audited)
	}

	return warns, invalidOrNil(app, errs)
}

func (v WebAppValidator) evaluatePolicies(ctx context.Context, app *apiv1alpha1.WebApp) (policy.Result, error) {
	if v.Client == nil {
		return policy.Result{}, nil
	}
	var policies apiv1alpha1.WebAppPolicyList
	if err := v.Client.List(ctx, &policies); err != nil {
		return policy.Result{}, fmt.Errorf("list webapppolicies: %w", err)
	}
	if len(policies.Items) == 0 {
		return policy.Result{}, nil
	}
	var ns corev1.Namespace
	if err := v.Client.Get(ctx, types.NamespacedName{Name: app.Namespace}, &ns); err != nil {
		return policy.Result{}, fmt.Errorf("get namespace %q: %w", app.Namespace, err)
	}
	return policy.Evaluate(app, policies.Items, ns.Labels)
}

func (WebAppValidator) ValidateDelete(_ context.Context, _ *apiv1alpha1.WebApp) (admission.Warnings, error) {
	return nil, nil
}

func invalidOrNil(app *apiv1alpha1.WebApp, errs field.ErrorList) error {
	if len(errs) == 0 {
		return nil
	}
	return apierrors.NewInvalid(
		apiv1alpha1.GroupVersion.WithKind("WebApp").GroupKind(), app.Name, errs)
}

func warnings(app *apiv1alpha1.WebApp) admission.Warnings {
	var w admission.Warnings
	if app.Spec.Autoscaling != nil && app.Spec.Autoscaling.Enabled && app.Spec.Replicas != nil {
		w = append(w, "spec.replicas is ignored while autoscaling is enabled")
	}
	for _, c := range app.Spec.Containers {
		if strings.HasSuffix(c.Image, ":latest") || !strings.Contains(c.Image, ":") {
			w = append(w, fmt.Sprintf("container %q uses a mutable image tag; pin a version or digest", c.Name))
		}
	}
	return w
}

func Default(app *apiv1alpha1.WebApp) {
	if app.Spec.ServiceType == "" {
		app.Spec.ServiceType = corev1.ServiceTypeClusterIP
	}

	if app.Spec.Strategy == nil {
		app.Spec.Strategy = &apiv1alpha1.StrategySpec{}
	}
	if app.Spec.Strategy.Type == "" {
		app.Spec.Strategy.Type = appsv1.RollingUpdateDeploymentStrategyType
	}

	autoscaling := app.Spec.Autoscaling != nil && app.Spec.Autoscaling.Enabled
	if autoscaling {
		if app.Spec.Autoscaling.MinReplicas == nil {
			app.Spec.Autoscaling.MinReplicas = new(int32(1))
		}
		if app.Spec.Autoscaling.TargetCPUUtilizationPercentage == nil &&
			app.Spec.Autoscaling.TargetMemoryUtilizationPercentage == nil {
			app.Spec.Autoscaling.TargetCPUUtilizationPercentage = new(resources.DefaultTargetCPU)
		}
	} else if app.Spec.Replicas == nil {
		app.Spec.Replicas = new(int32(1))
	}

	for i := range app.Spec.Containers {
		for j := range app.Spec.Containers[i].Ports {
			if app.Spec.Containers[i].Ports[j].Protocol == "" {
				app.Spec.Containers[i].Ports[j].Protocol = corev1.ProtocolTCP
			}
		}
	}

	if app.Labels == nil {
		app.Labels = map[string]string{}
	}
	app.Labels[resources.ManagedByLabel] = resources.ManagedByValue
	if _, ok := app.Labels[resources.NameLabel]; !ok {
		app.Labels[resources.NameLabel] = app.Name
	}
}

func ValidateSpec(app *apiv1alpha1.WebApp) field.ErrorList {
	var errs field.ErrorList
	spec := field.NewPath("spec")

	if len(app.Spec.Containers) == 0 {
		errs = append(errs, field.Required(spec.Child("containers"), "at least one container is required"))
	}

	containerNames := map[string]bool{}
	portOwners := map[string]string{}
	for i, c := range app.Spec.Containers {
		p := spec.Child("containers").Index(i)

		switch {
		case c.Name == "":
			errs = append(errs, field.Required(p.Child("name"), "container name is required"))
		case containerNames[c.Name]:
			errs = append(errs, field.Duplicate(p.Child("name"), c.Name))
		default:
			for _, msg := range validation.IsDNS1123Label(c.Name) {
				errs = append(errs, field.Invalid(p.Child("name"), c.Name, msg))
			}
		}
		containerNames[c.Name] = true

		if c.Image == "" {
			errs = append(errs, field.Required(p.Child("image"), "container image is required"))
		}

		for j, port := range c.Ports {
			pp := p.Child("ports").Index(j)
			if port.ContainerPort < 1 || port.ContainerPort > 65535 {
				errs = append(errs, field.Invalid(pp.Child("containerPort"), port.ContainerPort,
					"must be between 1 and 65535"))
			}
			if port.Name == "" {
				continue
			}
			for _, msg := range validation.IsValidPortName(port.Name) {
				errs = append(errs, field.Invalid(pp.Child("name"), port.Name, msg))
			}
			if owner, dup := portOwners[port.Name]; dup {
				errs = append(errs, field.Duplicate(pp.Child("name"),
					fmt.Sprintf("%s (already declared by container %q)", port.Name, owner)))
			}
			portOwners[port.Name] = c.Name
		}

		mountPaths := map[string]bool{}
		for j, v := range c.ScratchVolumes {
			vp := p.Child("scratchVolumes").Index(j)
			if mountPaths[v.MountPath] {
				errs = append(errs, field.Duplicate(vp.Child("mountPath"), v.MountPath))
			}
			mountPaths[v.MountPath] = true

			if v.SizeLimit != nil && v.SizeLimit.Sign() <= 0 {
				errs = append(errs, field.Invalid(vp.Child("sizeLimit"), v.SizeLimit.String(),
					"must be greater than zero"))
			}
		}
	}

	if len(app.Spec.Containers) > 0 && totalPorts(app) == 0 {
		errs = append(errs, field.Required(spec.Child("containers").Index(0).Child("ports"),
			"at least one container port must be declared; the generated Service requires one"))
	}

	if a := app.Spec.Autoscaling; a != nil && a.Enabled {
		ap := spec.Child("autoscaling")
		switch {
		case a.MaxReplicas == nil:
			errs = append(errs, field.Required(ap.Child("maxReplicas"),
				"required when autoscaling.enabled is true"))
		default:
			minReplicas := int32(1)
			if a.MinReplicas != nil {
				minReplicas = *a.MinReplicas
			}
			if *a.MaxReplicas < minReplicas {
				errs = append(errs, field.Invalid(ap.Child("maxReplicas"), *a.MaxReplicas,
					fmt.Sprintf("must be greater than or equal to minReplicas (%d)", minReplicas)))
			}
		}
		errs = append(errs, missingUtilizationRequests(app, a)...)
	}

	if want := app.Spec.IngressPortName; want != "" && !slices.Contains(resources.PortNames(app), want) {
		errs = append(errs, field.Invalid(spec.Child("ingressPortName"), want,
			"no container declares a port with this name"))
	}

	if app.Spec.Domain != "" {
		for _, msg := range validation.IsDNS1123Subdomain(app.Spec.Domain) {
			errs = append(errs, field.Invalid(spec.Child("domain"), app.Spec.Domain, msg))
		}
	}

	if s := app.Spec.Strategy; s != nil &&
		s.Type == appsv1.RecreateDeploymentStrategyType && s.RollingUpdate != nil {
		errs = append(errs, field.Invalid(spec.Child("strategy", "rollingUpdate"), s.RollingUpdate,
			"may only be set when strategy.type is RollingUpdate"))
	}

	return errs
}

func missingUtilizationRequests(app *apiv1alpha1.WebApp, a *apiv1alpha1.AutoscalingSpec) field.ErrorList {
	var errs field.ErrorList
	for _, name := range resources.TargetedResources(a) {
		for i, c := range app.Spec.Containers {
			if _, ok := c.Resources.Requests[name]; ok {
				continue
			}
			errs = append(errs, field.Required(
				field.NewPath("spec", "containers").Index(i).Child("resources", "requests").Key(string(name)),
				fmt.Sprintf("required while autoscaling targets %s utilization, which is measured against requests", name)))
		}
	}
	return errs
}

func totalPorts(app *apiv1alpha1.WebApp) int {
	n := 0
	for _, c := range app.Spec.Containers {
		n += len(c.Ports)
	}
	return n
}
