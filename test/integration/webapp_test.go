package integration_test

import (
	"fmt"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/blue-samarth/Warp-CRD/api/v1alpha1"
)

func baseWebApp(ns, name string) *v1alpha1.WebApp {
	return &v1alpha1.WebApp{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: v1alpha1.WebAppSpec{
			Containers: []v1alpha1.Container{{
				Name:  "web",
				Image: "nginx:1.27",
				Ports: []v1alpha1.ContainerPort{{Name: "http", ContainerPort: 8080}},
			}},
		},
	}
}

func autoscale(app *v1alpha1.WebApp, minReplicas, maxReplicas int32) *v1alpha1.WebApp {
	app.Spec.Autoscaling = &v1alpha1.AutoscalingSpec{
		Enabled:     true,
		MinReplicas: new(minReplicas),
		MaxReplicas: new(maxReplicas),
	}
	app.Spec.Containers[0].Resources.Requests = corev1.ResourceList{
		corev1.ResourceCPU: resource.MustParse("100m"),
	}
	return app
}

func createApp(t *testing.T, app *v1alpha1.WebApp) types.NamespacedName {
	t.Helper()
	if err := k8sClient.Create(testCtx, app); err != nil {
		t.Fatalf("create webapp: %v", err)
	}
	return client.ObjectKeyFromObject(app)
}

func TestReconcile_CreatesDeploymentAndService(t *testing.T) {
	ns := newNamespace(t)
	key := createApp(t, baseWebApp(ns, "app-basic"))

	dep := &appsv1.Deployment{}
	eventually(t, func() error { return k8sClient.Get(testCtx, key, dep) })

	if got := *dep.Spec.Replicas; got != 1 {
		t.Fatalf("want 1 replica, got %d", got)
	}
	if got := dep.Spec.Template.Spec.Containers[0].Image; got != "nginx:1.27" {
		t.Fatalf("unexpected image %q", got)
	}
	if len(dep.OwnerReferences) != 1 || dep.OwnerReferences[0].Kind != "WebApp" {
		t.Fatalf("missing owner reference: %+v", dep.OwnerReferences)
	}

	svc := &corev1.Service{}
	eventually(t, func() error { return k8sClient.Get(testCtx, key, svc) })
	if len(svc.Spec.Ports) != 1 || svc.Spec.Ports[0].Port != 8080 {
		t.Fatalf("unexpected service ports: %+v", svc.Spec.Ports)
	}
}

func TestReconcile_IsIdempotent(t *testing.T) {
	ns := newNamespace(t)
	key := createApp(t, baseWebApp(ns, "app-idempotent"))

	dep := &appsv1.Deployment{}
	eventually(t, func() error { return k8sClient.Get(testCtx, key, dep) })
	rv := dep.ResourceVersion

	// Force several reconciles by touching the WebApp's labels.
	for i := range 3 {
		app := &v1alpha1.WebApp{}
		if err := k8sClient.Get(testCtx, key, app); err != nil {
			t.Fatal(err)
		}
		if app.Labels == nil {
			app.Labels = map[string]string{}
		}
		app.Labels["touch"] = fmt.Sprint(i)
		if err := k8sClient.Update(testCtx, app); err != nil {
			t.Fatal(err)
		}
		time.Sleep(300 * time.Millisecond)
	}

	consistently(t, 3*time.Second, func() error {
		cur := &appsv1.Deployment{}
		if err := k8sClient.Get(testCtx, key, cur); err != nil {
			return err
		}
		if cur.ResourceVersion != rv {
			return fmt.Errorf("deployment rewritten: resourceVersion %s -> %s", rv, cur.ResourceVersion)
		}
		return nil
	})
}

func TestReconcile_CreatesIngressWhenDomainSet(t *testing.T) {
	ns := newNamespace(t)
	app := baseWebApp(ns, "app-ingress")
	app.Spec.Domain = "app.example.com"
	key := createApp(t, app)

	ing := &networkingv1.Ingress{}
	eventually(t, func() error { return k8sClient.Get(testCtx, key, ing) })

	rule := ing.Spec.Rules[0]
	if rule.Host != "app.example.com" {
		t.Fatalf("unexpected host %q", rule.Host)
	}
	if got := rule.HTTP.Paths[0].Backend.Service.Port.Name; got != "http" {
		t.Fatalf("unexpected backend port %q", got)
	}

	got := &v1alpha1.WebApp{}
	eventually(t, func() error {
		if err := k8sClient.Get(testCtx, key, got); err != nil {
			return err
		}
		if got.Status.IngressURL != "http://app.example.com" {
			return fmt.Errorf("ingressURL = %q", got.Status.IngressURL)
		}
		return nil
	})
}

func TestReconcile_DeletesIngressWhenDomainCleared(t *testing.T) {
	ns := newNamespace(t)
	app := baseWebApp(ns, "app-domain-cleared")
	app.Spec.Domain = "gone.example.com"
	key := createApp(t, app)

	eventually(t, func() error { return k8sClient.Get(testCtx, key, &networkingv1.Ingress{}) })

	cur := &v1alpha1.WebApp{}
	if err := k8sClient.Get(testCtx, key, cur); err != nil {
		t.Fatal(err)
	}
	cur.Spec.Domain = ""
	if err := k8sClient.Update(testCtx, cur); err != nil {
		t.Fatal(err)
	}

	eventually(t, func() error {
		err := k8sClient.Get(testCtx, key, &networkingv1.Ingress{})
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("ingress still present (err=%v)", err)
	})
}

func TestReconcile_CreatesHPAAndLeavesReplicasAlone(t *testing.T) {
	ns := newNamespace(t)
	key := createApp(t, autoscale(baseWebApp(ns, "app-hpa"), 2, 7))

	hpa := &autoscalingv2.HorizontalPodAutoscaler{}
	eventually(t, func() error { return k8sClient.Get(testCtx, key, hpa) })
	if hpa.Spec.MaxReplicas != 7 || *hpa.Spec.MinReplicas != 2 {
		t.Fatalf("unexpected hpa bounds: %+v", hpa.Spec)
	}

	// Simulate the HPA scaling the Deployment; the operator must not revert it.
	dep := &appsv1.Deployment{}
	eventually(t, func() error { return k8sClient.Get(testCtx, key, dep) })
	dep.Spec.Replicas = new(int32(5))
	if err := k8sClient.Update(testCtx, dep); err != nil {
		t.Fatal(err)
	}

	consistently(t, 3*time.Second, func() error {
		cur := &appsv1.Deployment{}
		if err := k8sClient.Get(testCtx, key, cur); err != nil {
			return err
		}
		if *cur.Spec.Replicas != 5 {
			return fmt.Errorf("operator reverted HPA scaling to %d", *cur.Spec.Replicas)
		}
		return nil
	})
}

func TestReconcile_SetsFinalizerAndStatus(t *testing.T) {
	ns := newNamespace(t)
	key := createApp(t, baseWebApp(ns, "app-status"))

	got := &v1alpha1.WebApp{}
	eventually(t, func() error {
		if err := k8sClient.Get(testCtx, key, got); err != nil {
			return err
		}
		if len(got.Finalizers) == 0 {
			return fmt.Errorf("finalizer not set")
		}
		if got.Status.ObservedGeneration == 0 {
			return fmt.Errorf("observedGeneration not set")
		}
		if got.Status.Selector == "" {
			return fmt.Errorf("selector not set; scale subresource needs it")
		}
		return nil
	})

	if got.Finalizers[0] != v1alpha1.Finalizer {
		t.Fatalf("unexpected finalizer %q", got.Finalizers[0])
	}

	for _, c := range []string{
		v1alpha1.ConditionReady,
		v1alpha1.ConditionAvailable,
		v1alpha1.ConditionProgressing,
		v1alpha1.ConditionDegraded,
	} {
		if meta.FindStatusCondition(got.Status.Conditions, c) == nil {
			t.Errorf("missing required condition %q", c)
		}
	}
}

func TestReconcile_DeploymentIsRestrictedPSACompliant(t *testing.T) {
	key := createApp(t, baseWebApp(newNamespace(t), "app-psa"))

	dep := &appsv1.Deployment{}
	eventually(t, func() error { return k8sClient.Get(testCtx, key, dep) })

	pod := dep.Spec.Template.Spec
	if pod.SecurityContext == nil || pod.SecurityContext.RunAsNonRoot == nil || !*pod.SecurityContext.RunAsNonRoot {
		t.Fatalf("want runAsNonRoot, got %+v", pod.SecurityContext)
	}
	sc := pod.Containers[0].SecurityContext
	if sc == nil || sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation {
		t.Fatalf("want allowPrivilegeEscalation=false, got %+v", sc)
	}
	if sc.Capabilities == nil || len(sc.Capabilities.Drop) != 1 {
		t.Fatalf("want capabilities dropped, got %+v", sc.Capabilities)
	}
}

func TestReconcile_ConditionsExistBeforeDeploymentReports(t *testing.T) {
	key := createApp(t, baseWebApp(newNamespace(t), "app-conds"))

	got := &v1alpha1.WebApp{}
	eventually(t, func() error {
		if err := k8sClient.Get(testCtx, key, got); err != nil {
			return err
		}
		for _, c := range []string{
			v1alpha1.ConditionReady, v1alpha1.ConditionAvailable,
			v1alpha1.ConditionProgressing, v1alpha1.ConditionDegraded,
		} {
			if meta.FindStatusCondition(got.Status.Conditions, c) == nil {
				return fmt.Errorf("condition %q not set yet", c)
			}
		}
		return nil
	})
}

func TestDelete_RemovesOwnedResources(t *testing.T) {
	ns := newNamespace(t)
	app := baseWebApp(ns, "app-delete")
	app.Spec.Domain = "delete.example.com"
	key := createApp(t, autoscale(app, 1, 3))

	eventually(t, func() error { return k8sClient.Get(testCtx, key, &appsv1.Deployment{}) })
	eventually(t, func() error { return k8sClient.Get(testCtx, key, &corev1.Service{}) })
	eventually(t, func() error { return k8sClient.Get(testCtx, key, &networkingv1.Ingress{}) })
	eventually(t, func() error {
		return k8sClient.Get(testCtx, key, &autoscalingv2.HorizontalPodAutoscaler{})
	})

	cur := &v1alpha1.WebApp{}
	if err := k8sClient.Get(testCtx, key, cur); err != nil {
		t.Fatal(err)
	}
	if err := k8sClient.Delete(testCtx, cur); err != nil {
		t.Fatal(err)
	}

	// envtest runs no garbage collector, so this proves the finalizer deleted them.
	for name, obj := range map[string]client.Object{
		"deployment": &appsv1.Deployment{},
		"service":    &corev1.Service{},
		"ingress":    &networkingv1.Ingress{},
		"hpa":        &autoscalingv2.HorizontalPodAutoscaler{},
		"webapp":     &v1alpha1.WebApp{},
	} {
		eventually(t, func() error {
			if err := k8sClient.Get(testCtx, key, obj); apierrors.IsNotFound(err) {
				return nil
			}
			return fmt.Errorf("%s still present", name)
		})
	}
}
