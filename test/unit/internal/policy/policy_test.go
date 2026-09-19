package policy_test

import . "github.com/blue-samarth/Warp-CRD/internal/policy"

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/blue-samarth/Warp-CRD/api/v1alpha1"
)

func appWithResources(req, lim corev1.ResourceList) *v1alpha1.WebApp {
	return &v1alpha1.WebApp{
		ObjectMeta: metav1.ObjectMeta{Name: "my-app", Namespace: "team-a"},
		Spec: v1alpha1.WebAppSpec{
			Replicas: new(int32(2)),
			Containers: []v1alpha1.Container{{
				Name:      "web",
				Image:     "nginx:1.27",
				Resources: corev1.ResourceRequirements{Requests: req, Limits: lim},
			}},
		},
	}
}

func basePolicy() v1alpha1.WebAppPolicy {
	return v1alpha1.WebAppPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "baseline"},
		Spec: v1alpha1.WebAppPolicySpec{
			Enforcement: v1alpha1.EnforcementEnforce,
		},
	}
}

func TestEvaluate_NoPoliciesIsClean(t *testing.T) {
	res, err := Evaluate(appWithResources(nil, nil), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Violations) != 0 || len(res.Warnings) != 0 {
		t.Fatalf("want clean result, got %+v", res)
	}
}

func TestEvaluate_MaxReplicasViolation(t *testing.T) {
	p := basePolicy()
	p.Spec.MaxReplicas = new(int32(1))
	res, err := Evaluate(appWithResources(nil, nil), []v1alpha1.WebAppPolicy{p}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Violations) != 1 {
		t.Fatalf("want 1 violation, got %v", res.Violations)
	}
	if !strings.Contains(res.Violations[0].Error(), "at most 1 replicas") {
		t.Fatalf("unexpected message: %s", res.Violations[0].Error())
	}
}

func TestEvaluate_MaxReplicasChecksAutoscalingCeiling(t *testing.T) {
	app := appWithResources(nil, nil)
	app.Spec.Autoscaling = &v1alpha1.AutoscalingSpec{Enabled: true, MaxReplicas: new(int32(20))}
	p := basePolicy()
	p.Spec.MaxReplicas = new(int32(5))

	res, err := Evaluate(app, []v1alpha1.WebAppPolicy{p}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Violations) != 1 {
		t.Fatalf("want 1 violation, got %v", res.Violations)
	}
	if !strings.Contains(res.Violations[0].Field, "autoscaling.maxReplicas") {
		t.Fatalf("want the autoscaling ceiling flagged, got %s", res.Violations[0].Field)
	}
}

func TestEvaluate_RequireRequests(t *testing.T) {
	p := basePolicy()
	p.Spec.Resources = &v1alpha1.ResourcePolicy{RequireRequests: true}
	res, err := Evaluate(appWithResources(nil, nil), []v1alpha1.WebAppPolicy{p}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Violations) != 1 || !strings.Contains(res.Violations[0].Error(), "requires resource requests") {
		t.Fatalf("want requests-required violation, got %v", res.Violations)
	}
}

func TestEvaluate_MinRequestsViolation(t *testing.T) {
	p := basePolicy()
	p.Spec.Resources = &v1alpha1.ResourcePolicy{
		MinRequests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")},
	}
	app := appWithResources(corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m")}, nil)

	res, err := Evaluate(app, []v1alpha1.WebAppPolicy{p}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Violations) != 1 || !strings.Contains(res.Violations[0].Error(), "at least 100m") {
		t.Fatalf("want min-request violation, got %v", res.Violations)
	}
}

func TestEvaluate_MinRequestsSatisfied(t *testing.T) {
	p := basePolicy()
	p.Spec.Resources = &v1alpha1.ResourcePolicy{
		MinRequests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")},
	}
	app := appWithResources(corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("250m")}, nil)

	res, err := Evaluate(app, []v1alpha1.WebAppPolicy{p}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Violations) != 0 {
		t.Fatalf("want no violations, got %v", res.Violations)
	}
}

func TestEvaluate_MaxLimitsViolation(t *testing.T) {
	p := basePolicy()
	p.Spec.Resources = &v1alpha1.ResourcePolicy{
		MaxLimits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("512Mi")},
	}
	app := appWithResources(nil, corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("2Gi")})

	res, err := Evaluate(app, []v1alpha1.WebAppPolicy{p}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Violations) != 1 || !strings.Contains(res.Violations[0].Error(), "at most 512Mi") {
		t.Fatalf("want max-limit violation, got %v", res.Violations)
	}
}

func TestEvaluate_WarnModeProducesWarningsNotViolations(t *testing.T) {
	p := basePolicy()
	p.Spec.Enforcement = v1alpha1.EnforcementWarn
	p.Spec.MaxReplicas = new(int32(1))

	res, err := Evaluate(appWithResources(nil, nil), []v1alpha1.WebAppPolicy{p}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Violations) != 0 {
		t.Fatalf("warn mode must not block, got %v", res.Violations)
	}
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "baseline") {
		t.Fatalf("want a warning naming the policy, got %v", res.Warnings)
	}
}

func TestEvaluate_NamespaceSelectorSkipsNonMatching(t *testing.T) {
	p := basePolicy()
	p.Spec.MaxReplicas = new(int32(1))
	p.Spec.NamespaceSelector = &metav1.LabelSelector{MatchLabels: map[string]string{"tier": "prod"}}

	res, err := Evaluate(appWithResources(nil, nil), []v1alpha1.WebAppPolicy{p}, map[string]string{"tier": "dev"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Violations) != 0 {
		t.Fatalf("policy should not apply to non-matching namespace, got %v", res.Violations)
	}
}

func TestEvaluate_NamespaceSelectorAppliesOnMatch(t *testing.T) {
	p := basePolicy()
	p.Spec.MaxReplicas = new(int32(1))
	p.Spec.NamespaceSelector = &metav1.LabelSelector{MatchLabels: map[string]string{"tier": "prod"}}

	res, err := Evaluate(appWithResources(nil, nil), []v1alpha1.WebAppPolicy{p}, map[string]string{"tier": "prod"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Violations) != 1 {
		t.Fatalf("want violation in matching namespace, got %v", res.Violations)
	}
}

func TestMatches_NilSelectorMatchesEverything(t *testing.T) {
	p := basePolicy()
	ok, err := Matches(&p, nil)
	if err != nil || !ok {
		t.Fatalf("want match, got ok=%v err=%v", ok, err)
	}
}

func TestMatches_RejectsInvalidSelector(t *testing.T) {
	p := basePolicy()
	p.Spec.NamespaceSelector = &metav1.LabelSelector{
		MatchExpressions: []metav1.LabelSelectorRequirement{{Key: "tier", Operator: "NotAnOperator"}},
	}
	if _, err := Matches(&p, nil); err == nil {
		t.Fatal("want an error for an invalid namespaceSelector")
	}
}

func TestEvaluate_SurfacesInvalidSelector(t *testing.T) {
	p := basePolicy()
	p.Spec.NamespaceSelector = &metav1.LabelSelector{
		MatchExpressions: []metav1.LabelSelectorRequirement{{Key: "tier", Operator: "NotAnOperator"}},
	}
	if _, err := Evaluate(appWithResources(nil, nil), []v1alpha1.WebAppPolicy{p}, nil); err == nil {
		t.Fatal("want the selector error propagated to the caller")
	}
}

func TestEvaluate_RequireLimits(t *testing.T) {
	p := basePolicy()
	p.Spec.Resources = &v1alpha1.ResourcePolicy{RequireLimits: true}
	res, err := Evaluate(appWithResources(nil, nil), []v1alpha1.WebAppPolicy{p}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Violations) != 1 || !strings.Contains(res.Violations[0].Error(), "requires resource limits") {
		t.Fatalf("want limits-required violation, got %v", res.Violations)
	}
}

func TestEvaluate_AbsentRequestViolatesAMinimum(t *testing.T) {
	p := basePolicy()
	p.Spec.Resources = &v1alpha1.ResourcePolicy{
		MinRequests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("64Mi")},
	}
	app := appWithResources(corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")}, nil)

	res, err := Evaluate(app, []v1alpha1.WebAppPolicy{p}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Violations) != 1 || !strings.Contains(res.Violations[0].Error(), "absent request is zero") {
		t.Fatalf("an absent request is below any floor, got %v", res.Violations)
	}
}

func TestEvaluate_AbsentLimitViolatesAMaximum(t *testing.T) {
	p := basePolicy()
	p.Spec.Resources = &v1alpha1.ResourcePolicy{
		MaxLimits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")},
	}
	app := appWithResources(nil, corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")})

	res, err := Evaluate(app, []v1alpha1.WebAppPolicy{p}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Violations) != 1 || !strings.Contains(res.Violations[0].Error(), "absent limit is unlimited") {
		t.Fatalf("an absent limit is unbounded, got %v", res.Violations)
	}
}

func TestEvaluate_ResourcesNotNamedByThePolicyAreIgnored(t *testing.T) {
	p := basePolicy()
	p.Spec.Resources = &v1alpha1.ResourcePolicy{
		MinRequests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m")},
	}
	app := appWithResources(corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")}, nil)

	res, err := Evaluate(app, []v1alpha1.WebAppPolicy{p}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Violations) != 0 {
		t.Fatalf("memory is unconstrained by this policy, got %v", res.Violations)
	}
}

func TestEvaluateScale_ChecksOnlyTheReplicaCeiling(t *testing.T) {
	p := basePolicy()
	p.Spec.MaxReplicas = new(int32(1))
	p.Spec.Resources = &v1alpha1.ResourcePolicy{RequireRequests: true}

	res, err := EvaluateScale(appWithResources(nil, nil), []v1alpha1.WebAppPolicy{p}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Violations) != 1 || !strings.Contains(res.Violations[0].Field, "replicas") {
		t.Fatalf("want only the replica ceiling, got %v", res.Violations)
	}
}

func TestEvaluateScale_HonoursWarnMode(t *testing.T) {
	p := basePolicy()
	p.Spec.Enforcement = v1alpha1.EnforcementWarn
	p.Spec.MaxReplicas = new(int32(1))

	res, err := EvaluateScale(appWithResources(nil, nil), []v1alpha1.WebAppPolicy{p}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Violations) != 0 || len(res.Warnings) != 1 {
		t.Fatalf("warn mode must not block a scale, got %+v", res)
	}
}

func TestEvaluate_MultiplePoliciesAccumulate(t *testing.T) {
	a := basePolicy()
	a.Name = "replicas"
	a.Spec.MaxReplicas = new(int32(1))

	b := basePolicy()
	b.Name = "resources"
	b.Spec.Resources = &v1alpha1.ResourcePolicy{RequireRequests: true}

	res, err := Evaluate(appWithResources(nil, nil), []v1alpha1.WebAppPolicy{a, b}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Violations) != 2 {
		t.Fatalf("want violations from both policies, got %v", res.Violations)
	}
}
