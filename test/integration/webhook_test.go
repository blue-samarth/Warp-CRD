package integration_test

import (
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/blue-samarth/Warp-CRD/api/v1alpha1"
)

func TestWebhook_RejectsDuplicatePortNames(t *testing.T) {
	ns := newNamespace(t)
	app := baseWebApp(ns, "dup-ports")
	app.Spec.Containers = append(app.Spec.Containers, v1alpha1.Container{
		Name:  "sidecar",
		Image: "envoy:v1.30",
		Ports: []v1alpha1.ContainerPort{{Name: "http", ContainerPort: 9901}},
	})

	err := k8sClient.Create(testCtx, app)
	if err == nil {
		t.Fatal("want admission rejection for duplicate port name")
	}
	if !strings.Contains(err.Error(), "Duplicate value") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestWebhook_RejectsAutoscalingWithoutMaxReplicas(t *testing.T) {
	ns := newNamespace(t)
	app := baseWebApp(ns, "no-max")
	app.Spec.Autoscaling = &v1alpha1.AutoscalingSpec{Enabled: true}

	err := k8sClient.Create(testCtx, app)
	if err == nil {
		t.Fatal("want admission rejection when maxReplicas is missing")
	}
	if !strings.Contains(err.Error(), "maxReplicas") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestWebhook_RejectsMaxBelowMin(t *testing.T) {
	ns := newNamespace(t)
	app := baseWebApp(ns, "bad-bounds")
	app.Spec.Autoscaling = &v1alpha1.AutoscalingSpec{
		Enabled:     true,
		MinReplicas: new(int32(5)),
		MaxReplicas: new(int32(2)),
	}

	err := k8sClient.Create(testCtx, app)
	if err == nil {
		t.Fatal("want admission rejection when maxReplicas < minReplicas")
	}
	if !strings.Contains(err.Error(), "minReplicas") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestWebhook_RejectsRecreateWithRollingUpdate(t *testing.T) {
	ns := newNamespace(t)
	app := baseWebApp(ns, "bad-strategy")
	app.Spec.Strategy = &v1alpha1.StrategySpec{
		Type:          appsv1.RecreateDeploymentStrategyType,
		RollingUpdate: &appsv1.RollingUpdateDeployment{},
	}

	err := k8sClient.Create(testCtx, app)
	if err == nil {
		t.Fatal("want admission rejection for rollingUpdate under Recreate")
	}
	if !strings.Contains(err.Error(), "rollingUpdate") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestWebhook_DefaultsAreApplied(t *testing.T) {
	ns := newNamespace(t)
	app := &v1alpha1.WebApp{
		ObjectMeta: metav1.ObjectMeta{Name: "defaulted", Namespace: ns},
		Spec: v1alpha1.WebAppSpec{
			Containers: []v1alpha1.Container{{
				Name:  "web",
				Image: "nginx:1.27",
				Ports: []v1alpha1.ContainerPort{{Name: "http", ContainerPort: 8080}},
			}},
		},
	}
	if err := k8sClient.Create(testCtx, app); err != nil {
		t.Fatalf("create: %v", err)
	}

	if app.Spec.Replicas == nil || *app.Spec.Replicas != 1 {
		t.Fatalf("want replicas defaulted to 1, got %v", app.Spec.Replicas)
	}
	if app.Spec.ServiceType != corev1.ServiceTypeClusterIP {
		t.Fatalf("want ClusterIP, got %q", app.Spec.ServiceType)
	}
	// The operator labels what it generates, not the user's own object:
	// rewriting these fights Helm ownership checks and GitOps drift.
	if _, ok := app.Labels["app.kubernetes.io/managed-by"]; ok {
		t.Fatalf("webapp labels must be left alone, got %v", app.Labels)
	}
}

func TestWebhook_RejectsInvalidDomain(t *testing.T) {
	ns := newNamespace(t)
	app := baseWebApp(ns, "bad-domain")
	app.Spec.Domain = "Not_A_Domain"

	err := k8sClient.Create(testCtx, app)
	if err == nil {
		t.Fatal("want admission rejection for invalid domain")
	}
	if !strings.Contains(err.Error(), "domain") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestWebhook_RejectsWebAppWithoutPorts(t *testing.T) {
	ns := newNamespace(t)
	app := baseWebApp(ns, "no-ports")
	app.Spec.Containers[0].Ports = nil

	err := k8sClient.Create(testCtx, app)
	if err == nil {
		t.Fatal("want rejection: a Service with no ports is invalid, so the WebApp could never reconcile")
	}
	if !strings.Contains(err.Error(), "at least one container port") {
		t.Fatalf("unexpected error: %v", err)
	}
}
