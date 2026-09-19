package controller_test

import . "github.com/blue-samarth/Warp-CRD/internal/controller"

import (
	"context"
	"errors"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/blue-samarth/Warp-CRD/api/v1alpha1"
)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := v1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

func testApp() *v1alpha1.WebApp {
	return &v1alpha1.WebApp{
		ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "ns", UID: "uid"},
		Spec: v1alpha1.WebAppSpec{
			Containers: []v1alpha1.Container{{
				Name:  "web",
				Image: "nginx:1.27",
				Ports: []v1alpha1.ContainerPort{{Name: "http", ContainerPort: 8080}},
			}},
		},
	}
}

func ownedMeta() metav1.ObjectMeta {
	return metav1.ObjectMeta{
		Name:      "app",
		Namespace: "ns",
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: v1alpha1.GroupVersion.String(),
			Kind:       "WebApp",
			Name:       "app",
			UID:        "uid",
			Controller: new(true),
		}},
	}
}

func newReconciler(t *testing.T, funcs interceptor.Funcs, objs ...client.Object) *WebAppReconciler {
	t.Helper()
	s := testScheme(t)
	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(objs...).
		WithStatusSubresource(&v1alpha1.WebApp{}).
		WithInterceptorFuncs(funcs).
		Build()
	return &WebAppReconciler{Client: c, Scheme: s, Recorder: record.NewFakeRecorder(64)}
}

var appKey = ctrl.Request{NamespacedName: types.NamespacedName{Name: "app", Namespace: "ns"}}

func TestReconcile_MissingWebAppIsNotAnError(t *testing.T) {
	r := newReconciler(t, interceptor.Funcs{})
	if _, err := r.Reconcile(t.Context(), appKey); err != nil {
		t.Fatalf("want nil error for a deleted WebApp, got %v", err)
	}
}

func TestReconcile_PropagatesGetErrors(t *testing.T) {
	r := newReconciler(t, interceptor.Funcs{
		Get: func(_ context.Context, _ client.WithWatch, _ client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
			return errors.New("apiserver unreachable")
		},
	})
	_, err := r.Reconcile(t.Context(), appKey)
	if err == nil || !strings.Contains(err.Error(), "apiserver unreachable") {
		t.Fatalf("want the Get error surfaced, got %v", err)
	}
}

func TestReconcile_ApplyFailureSetsDegraded(t *testing.T) {
	r := newReconciler(t, interceptor.Funcs{
		Apply: func(_ context.Context, _ client.WithWatch, _ runtime.ApplyConfiguration, _ ...client.ApplyOption) error {
			return errors.New("quota exceeded")
		},
	}, testApp())

	_, err := r.Reconcile(t.Context(), appKey)
	if err == nil || !strings.Contains(err.Error(), "quota exceeded") {
		t.Fatalf("want the apply error surfaced, got %v", err)
	}

	got := &v1alpha1.WebApp{}
	if err := r.Get(t.Context(), appKey.NamespacedName, got); err != nil {
		t.Fatal(err)
	}
	d := meta.FindStatusCondition(got.Status.Conditions, v1alpha1.ConditionDegraded)
	if d == nil || d.Status != metav1.ConditionTrue || d.Reason != ReasonReconcileFailed {
		t.Fatalf("want Degraded=True/ReconcileFailed, got %+v", d)
	}
	ready := meta.FindStatusCondition(got.Status.Conditions, v1alpha1.ConditionReady)
	if ready == nil || ready.Status != metav1.ConditionFalse {
		t.Fatalf("want Ready=False, got %+v", ready)
	}
}

func TestReconcile_AddsFinalizerOnFirstPass(t *testing.T) {
	r := newReconciler(t, interceptor.Funcs{}, testApp())
	if _, err := r.Reconcile(t.Context(), appKey); err != nil {
		t.Fatal(err)
	}
	got := &v1alpha1.WebApp{}
	if err := r.Get(t.Context(), appKey.NamespacedName, got); err != nil {
		t.Fatal(err)
	}
	if len(got.Finalizers) != 1 || got.Finalizers[0] != v1alpha1.Finalizer {
		t.Fatalf("want finalizer set, got %v", got.Finalizers)
	}
}

func TestFinalize_DeletesOwnedResourcesAndDropsFinalizer(t *testing.T) {
	now := metav1.Now()
	app := testApp()
	app.DeletionTimestamp = &now
	app.Finalizers = []string{v1alpha1.Finalizer}

	owned := []client.Object{
		&appsv1.Deployment{ObjectMeta: ownedMeta()},
		&corev1.Service{ObjectMeta: ownedMeta()},
		&networkingv1.Ingress{ObjectMeta: ownedMeta()},
		&autoscalingv2.HorizontalPodAutoscaler{ObjectMeta: ownedMeta()},
	}
	r := newReconciler(t, interceptor.Funcs{}, append([]client.Object{app}, owned...)...)

	if _, err := r.Reconcile(t.Context(), appKey); err != nil {
		t.Fatal(err)
	}

	for _, obj := range owned {
		err := r.Get(t.Context(), appKey.NamespacedName, obj)
		if !apierrors.IsNotFound(err) {
			t.Errorf("%T should have been deleted, got err=%v", obj, err)
		}
	}
	if err := r.Get(t.Context(), appKey.NamespacedName, &v1alpha1.WebApp{}); !apierrors.IsNotFound(err) {
		t.Fatalf("webapp should be gone once the finalizer is removed, got %v", err)
	}
}

func TestFinalize_ToleratesAlreadyDeletedResources(t *testing.T) {
	now := metav1.Now()
	app := testApp()
	app.DeletionTimestamp = &now
	app.Finalizers = []string{v1alpha1.Finalizer}

	r := newReconciler(t, interceptor.Funcs{}, app)
	if _, err := r.Reconcile(t.Context(), appKey); err != nil {
		t.Fatalf("finalize must tolerate missing owned resources, got %v", err)
	}
}

func TestFinalize_SurfacesDeleteErrors(t *testing.T) {
	now := metav1.Now()
	app := testApp()
	app.DeletionTimestamp = &now
	app.Finalizers = []string{v1alpha1.Finalizer}

	r := newReconciler(t, interceptor.Funcs{
		Delete: func(_ context.Context, _ client.WithWatch, _ client.Object, _ ...client.DeleteOption) error {
			return errors.New("webhook denied delete")
		},
	}, app, &appsv1.Deployment{ObjectMeta: ownedMeta()})

	_, err := r.Reconcile(t.Context(), appKey)
	if err == nil || !strings.Contains(err.Error(), "webhook denied delete") {
		t.Fatalf("want delete error surfaced so the finalizer is retained, got %v", err)
	}

	got := &v1alpha1.WebApp{}
	if err := r.Get(t.Context(), appKey.NamespacedName, got); err != nil {
		t.Fatal(err)
	}
	if len(got.Finalizers) == 0 {
		t.Fatal("finalizer must not be removed while cleanup is failing")
	}
}

func TestSyncHPA_RemovesHPAWhenAutoscalingDisabled(t *testing.T) {
	app := testApp()
	hpa := &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: ownedMeta(),
	}
	r := newReconciler(t, interceptor.Funcs{}, app, hpa)

	if _, err := r.Reconcile(t.Context(), appKey); err != nil {
		t.Fatal(err)
	}
	err := r.Get(t.Context(), appKey.NamespacedName, &autoscalingv2.HorizontalPodAutoscaler{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("hpa should be deleted when autoscaling is off, got %v", err)
	}
}

func TestSyncIngress_SurfacesBuildError(t *testing.T) {
	app := testApp()
	app.Spec.Domain = "example.com"
	app.Spec.IngressPortName = "nope"

	r := newReconciler(t, interceptor.Funcs{}, app)
	_, err := r.Reconcile(t.Context(), appKey)
	if err == nil || !strings.Contains(err.Error(), "build ingress") {
		t.Fatalf("want a build error when no port can back the ingress, got %v", err)
	}
}

func TestSyncIngress_SurfacesApplyError(t *testing.T) {
	app := testApp()
	app.Spec.Domain = "example.com"

	r := newReconciler(t, interceptor.Funcs{
		Apply: func(_ context.Context, _ client.WithWatch, _ runtime.ApplyConfiguration, _ ...client.ApplyOption) error {
			return errors.New("ingress admission denied")
		},
	}, app)

	_, err := r.Reconcile(t.Context(), appKey)
	if err == nil || !strings.Contains(err.Error(), "ingress admission denied") {
		t.Fatalf("want the apply error surfaced, got %v", err)
	}
}

func TestSyncIngress_RemovesIngressWhenDomainCleared(t *testing.T) {
	app := testApp()
	ing := &networkingv1.Ingress{ObjectMeta: ownedMeta()}
	r := newReconciler(t, interceptor.Funcs{}, app, ing)

	if _, err := r.Reconcile(t.Context(), appKey); err != nil {
		t.Fatal(err)
	}
	if err := r.Get(t.Context(), appKey.NamespacedName, &networkingv1.Ingress{}); !apierrors.IsNotFound(err) {
		t.Fatalf("ingress should be removed when domain is empty, got %v", err)
	}
}

func foreignMeta() metav1.ObjectMeta {
	return metav1.ObjectMeta{
		Name:      "app",
		Namespace: "ns",
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: "v1", Kind: "ConfigMap", Name: "someone-else", UID: "other-uid",
			Controller: new(true),
		}},
	}
}

func TestDeleteOwned_LeavesForeignObjectAlone(t *testing.T) {
	app := testApp()
	r := newReconciler(t, interceptor.Funcs{}, app,
		&networkingv1.Ingress{ObjectMeta: foreignMeta()})

	if _, err := r.Reconcile(t.Context(), appKey); err != nil {
		t.Fatal(err)
	}
	if err := r.Get(t.Context(), appKey.NamespacedName, &networkingv1.Ingress{}); err != nil {
		t.Fatalf("an ingress this WebApp does not control must survive, got %v", err)
	}
}

func TestReconcile_DoesNotDeleteForeignIngressWhenDomainUnset(t *testing.T) {
	r := newReconciler(t, interceptor.Funcs{}, testApp(),
		&networkingv1.Ingress{ObjectMeta: foreignMeta()})

	if _, err := r.Reconcile(t.Context(), appKey); err != nil {
		t.Fatal(err)
	}
	if err := r.Get(t.Context(), appKey.NamespacedName, &networkingv1.Ingress{}); err != nil {
		t.Fatalf("reconcile deleted an object it does not own: %v", err)
	}
}

func TestFinalize_LeavesForeignObjectsAlone(t *testing.T) {
	now := metav1.Now()
	app := testApp()
	app.DeletionTimestamp = &now
	app.Finalizers = []string{v1alpha1.Finalizer}

	r := newReconciler(t, interceptor.Funcs{}, app,
		&corev1.Service{ObjectMeta: foreignMeta()})

	if _, err := r.Reconcile(t.Context(), appKey); err != nil {
		t.Fatal(err)
	}
	if err := r.Get(t.Context(), appKey.NamespacedName, &corev1.Service{}); err != nil {
		t.Fatalf("deleting a WebApp must not delete a same-named foreign Service: %v", err)
	}
}

func TestSync_RefusesToAdoptForeignDeployment(t *testing.T) {
	r := newReconciler(t, interceptor.Funcs{}, testApp(),
		&appsv1.Deployment{ObjectMeta: foreignMeta()})

	_, err := r.Reconcile(t.Context(), appKey)
	if err == nil || !strings.Contains(err.Error(), "not controlled by this WebApp") {
		t.Fatalf("want adoption refused, got %v", err)
	}
}

func TestDeleteOwned_SurfacesNonNotFoundErrors(t *testing.T) {
	app := testApp()
	r := newReconciler(t, interceptor.Funcs{
		Delete: func(_ context.Context, _ client.WithWatch, _ client.Object, _ ...client.DeleteOption) error {
			return errors.New("forbidden")
		},
	}, app, &networkingv1.Ingress{ObjectMeta: ownedMeta()})

	_, err := r.Reconcile(t.Context(), appKey)
	if err == nil || !strings.Contains(err.Error(), "forbidden") {
		t.Fatalf("want delete error surfaced, got %v", err)
	}
}

func TestReconcile_SurfacesFinalizerUpdateError(t *testing.T) {
	r := newReconciler(t, interceptor.Funcs{
		Update: func(_ context.Context, _ client.WithWatch, _ client.Object, _ ...client.UpdateOption) error {
			return errors.New("conflict adding finalizer")
		},
	}, testApp())

	_, err := r.Reconcile(t.Context(), appKey)
	if err == nil || !strings.Contains(err.Error(), "conflict adding finalizer") {
		t.Fatalf("want the finalizer update error surfaced, got %v", err)
	}
}

func TestFinalize_SurfacesScaleToZeroError(t *testing.T) {
	now := metav1.Now()
	app := testApp()
	app.DeletionTimestamp = &now
	app.Finalizers = []string{v1alpha1.Finalizer}

	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "ns"},
		Spec:       appsv1.DeploymentSpec{Replicas: new(int32(3))},
	}
	r := newReconciler(t, interceptor.Funcs{
		Update: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.UpdateOption) error {
			if _, ok := obj.(*appsv1.Deployment); ok {
				return errors.New("scale rejected")
			}
			return nil
		},
	}, app, dep)

	_, err := r.Reconcile(t.Context(), appKey)
	if err == nil || !strings.Contains(err.Error(), "scale deployment to zero") {
		t.Fatalf("want scale error surfaced, got %v", err)
	}
}

func recordedEvents(t *testing.T, r *WebAppReconciler) []string {
	t.Helper()
	rec, ok := r.Recorder.(*record.FakeRecorder)
	if !ok {
		t.Fatal("recorder is not a FakeRecorder")
	}
	var out []string
	for {
		select {
		case e := <-rec.Events:
			out = append(out, e)
		default:
			return out
		}
	}
}

func TestReconcile_EmitsReconciledEventOnSpecChange(t *testing.T) {
	app := testApp()
	app.Generation = 2
	r := newReconciler(t, interceptor.Funcs{}, app)

	if _, err := r.Reconcile(t.Context(), appKey); err != nil {
		t.Fatal(err)
	}
	events := strings.Join(recordedEvents(t, r), "\n")
	if !strings.Contains(events, "Reconciled") {
		t.Fatalf("want a Reconciled event, got %q", events)
	}
}

func TestReconcile_EmitsScaledEventWhenReplicasChange(t *testing.T) {
	app := testApp()
	dep := &appsv1.Deployment{
		ObjectMeta: ownedMeta(),
		Status:     appsv1.DeploymentStatus{Replicas: 4},
	}
	r := newReconciler(t, interceptor.Funcs{}, app, dep)

	if _, err := r.Reconcile(t.Context(), appKey); err != nil {
		t.Fatal(err)
	}
	events := strings.Join(recordedEvents(t, r), "\n")
	if !strings.Contains(events, "Scaled") || !strings.Contains(events, "0 -> 4") {
		t.Fatalf("want a Scaled event reporting the transition, got %q", events)
	}
}
