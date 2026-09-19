package integration_test

import (
	"fmt"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv1 "k8s.io/api/autoscaling/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/blue-samarth/Warp-CRD/api/v1alpha1"
)

// WebAppPolicy is cluster scoped, so every test here must remove it again or it
// silently changes admission for the rest of the suite.
func withPolicy(t *testing.T, p *v1alpha1.WebAppPolicy) {
	t.Helper()
	if err := k8sClient.Create(testCtx, p); err != nil {
		t.Fatalf("create policy: %v", err)
	}
	t.Cleanup(func() {
		if err := k8sClient.Delete(testCtx, p); err != nil {
			t.Errorf("delete policy: %v", err)
		}
	})
}

func TestPolicy_RejectsUnsatisfiableBounds(t *testing.T) {
	p := &v1alpha1.WebAppPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "impossible"},
		Spec: v1alpha1.WebAppPolicySpec{
			Enforcement: v1alpha1.EnforcementEnforce,
			Resources: &v1alpha1.ResourcePolicy{
				MinRequests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2")},
				MaxLimits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")},
			},
		},
	}
	err := k8sClient.Create(testCtx, p)
	if err == nil {
		t.Cleanup(func() { _ = k8sClient.Delete(testCtx, p) })
		t.Fatal("want rejection: no container could satisfy minRequests above maxLimits")
	}
	if !strings.Contains(err.Error(), "must not exceed maxLimits") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestPolicy_AuditModeAdmitsWithoutWarning(t *testing.T) {
	withPolicy(t, &v1alpha1.WebAppPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "audited"},
		Spec: v1alpha1.WebAppPolicySpec{
			Enforcement: v1alpha1.EnforcementAudit,
			MaxReplicas: new(int32(1)),
		},
	})

	app := baseWebApp(newNamespace(t), "audited-app")
	app.Spec.Replicas = new(int32(9))
	if err := k8sClient.Create(testCtx, app); err != nil {
		t.Fatalf("audit mode must admit, got %v", err)
	}
}

func TestPolicy_ScaleSubresourceCannotBypassMaxReplicas(t *testing.T) {
	withPolicy(t, &v1alpha1.WebAppPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "scale-cap"},
		Spec: v1alpha1.WebAppPolicySpec{
			Enforcement: v1alpha1.EnforcementEnforce,
			MaxReplicas: new(int32(2)),
		},
	})

	key := createApp(t, baseWebApp(newNamespace(t), "scaled"))
	eventually(t, func() error { return k8sClient.Get(testCtx, key, &appsv1.Deployment{}) })

	// The scale subresource writes a Scale, not a WebApp, so admission never
	// sees it. The reconciler has to refuse instead.
	scale := &autoscalingv1.Scale{
		ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace},
		Spec:       autoscalingv1.ScaleSpec{Replicas: 9},
	}
	if err := k8sClient.SubResource("scale").Update(testCtx, &v1alpha1.WebApp{
		ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace},
	}, client.WithSubResourceBody(scale)); err != nil {
		t.Fatalf("scale: %v", err)
	}

	got := &v1alpha1.WebApp{}
	eventually(t, func() error {
		if err := k8sClient.Get(testCtx, key, got); err != nil {
			return err
		}
		c := meta.FindStatusCondition(got.Status.Conditions, v1alpha1.ConditionDegraded)
		if c == nil || c.Status != metav1.ConditionTrue {
			return fmt.Errorf("Degraded = %v", c)
		}
		if !strings.Contains(c.Message, "at most 2 replicas") {
			return fmt.Errorf("unexpected message: %s", c.Message)
		}
		return nil
	})

	dep := &appsv1.Deployment{}
	if err := k8sClient.Get(testCtx, key, dep); err != nil {
		t.Fatal(err)
	}
	if *dep.Spec.Replicas > 2 {
		t.Fatalf("policy ceiling was bypassed: deployment has %d replicas", *dep.Spec.Replicas)
	}
}

func TestPolicy_EnforcesMaxReplicas(t *testing.T) {
	withPolicy(t, &v1alpha1.WebAppPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "max-two"},
		Spec: v1alpha1.WebAppPolicySpec{
			Enforcement: v1alpha1.EnforcementEnforce,
			MaxReplicas: new(int32(2)),
		},
	})

	ns := newNamespace(t)
	// The validator reads policies with the API reader, so the policy is in
	// force immediately; each attempt uses a fresh name so a success cannot be
	// mistaken for AlreadyExists.
	app := baseWebApp(ns, "too-many")
	app.Spec.Replicas = new(int32(9))

	err := k8sClient.Create(testCtx, app)
	if err == nil {
		t.Fatal("want the policy enforced on the first write")
	}
	if !strings.Contains(err.Error(), "at most 2 replicas") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestPolicy_AllowsCompliantWebApp(t *testing.T) {
	withPolicy(t, &v1alpha1.WebAppPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "max-five"},
		Spec: v1alpha1.WebAppPolicySpec{
			Enforcement: v1alpha1.EnforcementEnforce,
			MaxReplicas: new(int32(5)),
		},
	})

	ns := newNamespace(t)
	app := baseWebApp(ns, "compliant")
	app.Spec.Replicas = new(int32(3))

	if err := k8sClient.Create(testCtx, app); err != nil {
		t.Fatalf("compliant webapp should be admitted: %v", err)
	}
}

func TestPolicy_EnforcesMinimumRequests(t *testing.T) {
	withPolicy(t, &v1alpha1.WebAppPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "min-cpu"},
		Spec: v1alpha1.WebAppPolicySpec{
			Enforcement: v1alpha1.EnforcementEnforce,
			Resources: &v1alpha1.ResourcePolicy{
				MinRequests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")},
			},
		},
	})

	ns := newNamespace(t)
	app := baseWebApp(ns, "too-small")
	app.Spec.Containers[0].Resources = corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("10m")},
	}

	err := k8sClient.Create(testCtx, app)
	if err == nil {
		t.Fatal("want the policy enforced on the first write")
	}
	if !strings.Contains(err.Error(), "at least 100m") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestPolicy_NamespaceSelectorScopesEnforcement(t *testing.T) {
	withPolicy(t, &v1alpha1.WebAppPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "prod-only"},
		Spec: v1alpha1.WebAppPolicySpec{
			Enforcement:       v1alpha1.EnforcementEnforce,
			MaxReplicas:       new(int32(1)),
			NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"tier": "prod"}},
		},
	})

	ns := newNamespace(t)
	app := baseWebApp(ns, "dev-app")
	app.Spec.Replicas = new(int32(4))

	if err := k8sClient.Create(testCtx, app); err != nil {
		t.Fatalf("policy should not apply to an unlabelled namespace: %v", err)
	}
}
