package policy

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/validation/field"

	"github.com/blue-samarth/Warp-CRD/api/v1alpha1"
)

type Result struct {
	Violations field.ErrorList
	Warnings   []string
	Audited    []string
}

func Matches(p *v1alpha1.WebAppPolicy, nsLabels map[string]string) (bool, error) {
	if p.Spec.NamespaceSelector == nil {
		return true, nil
	}
	sel, err := metav1.LabelSelectorAsSelector(p.Spec.NamespaceSelector)
	if err != nil {
		return false, fmt.Errorf("policy %q has an invalid namespaceSelector: %w", p.Name, err)
	}
	return sel.Matches(labels.Set(nsLabels)), nil
}

func Evaluate(app *v1alpha1.WebApp, policies []v1alpha1.WebAppPolicy, nsLabels map[string]string) (Result, error) {
	var res Result
	for i := range policies {
		p := &policies[i]
		ok, err := Matches(p, nsLabels)
		if err != nil {
			res.Warnings = append(res.Warnings,
				fmt.Sprintf("policy %q is not being enforced: %s", p.Name, err))
			continue
		}
		if !ok {
			continue
		}
		record(&res, p, violations(app, p))
	}
	return res, nil
}

func EvaluateScale(app *v1alpha1.WebApp, policies []v1alpha1.WebAppPolicy, nsLabels map[string]string) (Result, error) {
	var res Result
	for i := range policies {
		p := &policies[i]
		ok, err := Matches(p, nsLabels)
		if err != nil {
			res.Warnings = append(res.Warnings,
				fmt.Sprintf("policy %q is not being enforced: %s", p.Name, err))
			continue
		}
		if !ok {
			continue
		}
		record(&res, p, replicaViolations(app, p))
	}
	return res, nil
}

func record(res *Result, p *v1alpha1.WebAppPolicy, errs field.ErrorList) {
	if len(errs) == 0 {
		return
	}
	switch p.Spec.Enforcement {
	case v1alpha1.EnforcementWarn:
		for _, e := range errs {
			res.Warnings = append(res.Warnings, fmt.Sprintf("policy %q: %s", p.Name, e.Error()))
		}
	case v1alpha1.EnforcementAudit:
		for _, e := range errs {
			res.Audited = append(res.Audited, fmt.Sprintf("policy %q: %s", p.Name, e.Error()))
		}
	default:
		res.Violations = append(res.Violations, errs...)
	}
}

func replicaViolations(app *v1alpha1.WebApp, p *v1alpha1.WebAppPolicy) field.ErrorList {
	var errs field.ErrorList
	spec := field.NewPath("spec")

	if ceiling := p.Spec.MaxReplicas; ceiling != nil {
		if a := app.Spec.Autoscaling; a != nil && a.Enabled {
			if a.MaxReplicas != nil && *a.MaxReplicas > *ceiling {
				errs = append(errs, field.Invalid(spec.Child("autoscaling", "maxReplicas"), *a.MaxReplicas,
					fmt.Sprintf("policy %q allows at most %d replicas", p.Name, *ceiling)))
			}
		} else if app.Spec.Replicas != nil && *app.Spec.Replicas > *ceiling {
			errs = append(errs, field.Invalid(spec.Child("replicas"), *app.Spec.Replicas,
				fmt.Sprintf("policy %q allows at most %d replicas", p.Name, *ceiling)))
		}
	}
	return errs
}

func violations(app *v1alpha1.WebApp, p *v1alpha1.WebAppPolicy) field.ErrorList {
	var errs field.ErrorList
	spec := field.NewPath("spec")

	errs = append(errs, replicaViolations(app, p)...)

	sa := app.Spec.ServiceAccountName
	if sa == "" {
		sa = "default"
	}
	if len(p.Spec.AllowedServiceAccounts) > 0 && !slices.Contains(p.Spec.AllowedServiceAccounts, sa) {
		errs = append(errs, field.Invalid(spec.Child("serviceAccountName"), sa,
			fmt.Sprintf("policy %q allows only %v", p.Name, p.Spec.AllowedServiceAccounts)))
	}

	for _, prefix := range p.Spec.ForbiddenPodAnnotationPrefixes {
		for _, k := range slices.Sorted(maps.Keys(app.Spec.PodAnnotations)) {
			if strings.HasPrefix(k, prefix) {
				errs = append(errs, field.Invalid(spec.Child("podAnnotations").Key(k), k,
					fmt.Sprintf("policy %q forbids the prefix %q", p.Name, prefix)))
			}
		}
	}

	if cap := p.Spec.MaxScratchSize; cap != nil {
		for i, c := range app.Spec.Containers {
			for j, v := range c.ScratchVolumes {
				vp := spec.Child("containers").Index(i).Child("scratchVolumes").Index(j)
				if v.SizeLimit == nil {
					errs = append(errs, field.Required(vp.Child("sizeLimit"),
						fmt.Sprintf("policy %q caps scratch volumes at %s", p.Name, cap.String())))
					continue
				}
				if v.SizeLimit.Cmp(*cap) > 0 {
					errs = append(errs, field.Invalid(vp.Child("sizeLimit"), v.SizeLimit.String(),
						fmt.Sprintf("policy %q allows at most %s", p.Name, cap.String())))
				}
			}
		}
	}

	rp := p.Spec.Resources
	if rp == nil {
		return errs
	}

	for i, c := range app.Spec.Containers {
		cp := spec.Child("containers").Index(i).Child("resources")

		if rp.RequireRequests && len(c.Resources.Requests) == 0 {
			errs = append(errs, field.Required(cp.Child("requests"),
				fmt.Sprintf("policy %q requires resource requests", p.Name)))
		}
		if rp.RequireLimits && len(c.Resources.Limits) == 0 {
			errs = append(errs, field.Required(cp.Child("limits"),
				fmt.Sprintf("policy %q requires resource limits", p.Name)))
		}

		for _, name := range sortedNames(rp.MinRequests) {
			floor := rp.MinRequests[name]
			got, ok := c.Resources.Requests[name]
			if !ok {
				errs = append(errs, field.Required(cp.Child("requests").Key(string(name)),
					fmt.Sprintf("policy %q requires at least %s; an absent request is zero",
						p.Name, floor.String())))
				continue
			}
			if got.Cmp(floor) < 0 {
				errs = append(errs, field.Invalid(cp.Child("requests").Key(string(name)), got.String(),
					fmt.Sprintf("policy %q requires at least %s", p.Name, floor.String())))
			}
		}

		for _, name := range sortedNames(rp.MaxLimits) {
			ceiling := rp.MaxLimits[name]
			got, ok := c.Resources.Limits[name]
			if !ok {
				errs = append(errs, field.Required(cp.Child("limits").Key(string(name)),
					fmt.Sprintf("policy %q allows at most %s; an absent limit is unlimited",
						p.Name, ceiling.String())))
				continue
			}
			if got.Cmp(ceiling) > 0 {
				errs = append(errs, field.Invalid(cp.Child("limits").Key(string(name)), got.String(),
					fmt.Sprintf("policy %q allows at most %s", p.Name, ceiling.String())))
			}
		}
	}
	return errs
}

func Validate(p *v1alpha1.WebAppPolicy) field.ErrorList {
	var errs field.ErrorList
	spec := field.NewPath("spec")

	if p.Spec.NamespaceSelector != nil {
		if _, err := metav1.LabelSelectorAsSelector(p.Spec.NamespaceSelector); err != nil {
			errs = append(errs, field.Invalid(spec.Child("namespaceSelector"),
				p.Spec.NamespaceSelector, err.Error()))
		}
	}

	for i, sa := range p.Spec.AllowedServiceAccounts {
		if strings.TrimSpace(sa) == "" {
			errs = append(errs, field.Invalid(
				spec.Child("allowedServiceAccounts").Index(i), sa, "must not be empty"))
		}
	}
	for i, prefix := range p.Spec.ForbiddenPodAnnotationPrefixes {
		if strings.TrimSpace(prefix) == "" {
			errs = append(errs, field.Invalid(
				spec.Child("forbiddenPodAnnotationPrefixes").Index(i), prefix,
				"an empty prefix forbids every annotation"))
		}
	}

	rp := p.Spec.Resources
	if rp == nil {
		return errs
	}
	for _, name := range sortedNames(rp.MinRequests) {
		floor := rp.MinRequests[name]
		ceiling, ok := rp.MaxLimits[name]
		if !ok {
			continue
		}
		if floor.Cmp(ceiling) > 0 {
			errs = append(errs, field.Invalid(
				spec.Child("resources", "minRequests").Key(string(name)), floor.String(),
				fmt.Sprintf("must not exceed maxLimits[%s] (%s); no container could satisfy both",
					name, ceiling.String())))
		}
	}
	return errs
}

func sortedNames(l corev1.ResourceList) []corev1.ResourceName {
	return slices.Sorted(maps.Keys(l))
}
