package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	appsv1ac "k8s.io/client-go/applyconfigurations/apps/v1"
	autoscalingv2ac "k8s.io/client-go/applyconfigurations/autoscaling/v2"
	corev1ac "k8s.io/client-go/applyconfigurations/core/v1"
	networkingv1ac "k8s.io/client-go/applyconfigurations/networking/v1"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/blue-samarth/Warp-CRD/api/v1alpha1"
	"github.com/blue-samarth/Warp-CRD/internal/metrics"
	"github.com/blue-samarth/Warp-CRD/internal/policy"
	"github.com/blue-samarth/Warp-CRD/internal/resources"
)

type terminalError struct{ error }

type externalError struct{ error }

func terminal(format string, a ...any) error { return terminalError{fmt.Errorf(format, a...)} }

func external(format string, a ...any) error { return externalError{fmt.Errorf(format, a...)} }

const externalRetry = time.Minute

var (
	deploymentGVK = appsv1.SchemeGroupVersion.WithKind("Deployment")
	serviceGVK    = corev1.SchemeGroupVersion.WithKind("Service")
	ingressGVK    = networkingv1.SchemeGroupVersion.WithKind("Ingress")
	hpaGVK        = autoscalingv2.SchemeGroupVersion.WithKind("HorizontalPodAutoscaler")
)

type WebAppReconciler struct {
	client.Client
	// APIReader bypasses the cache. Adoption and policy decisions are made
	// against live state: a cached miss would let SSA with ForceOwnership take
	// over an object created in the gap.
	APIReader client.Reader
	Scheme    *runtime.Scheme
	Recorder  record.EventRecorder
}

func (r *WebAppReconciler) reader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

// +kubebuilder:rbac:groups=webapps.example.com,resources=webapps,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=webapps.example.com,resources=webapps/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=webapps.example.com,resources=webapps/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=networking.k8s.io,resources=ingresses,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=autoscaling,resources=horizontalpodautoscalers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=webapps.example.com,resources=webapppolicies,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

func (r *WebAppReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	start := time.Now()
	res, err := r.reconcile(ctx, req)
	outcome := metrics.ResultSuccess
	if err != nil {
		outcome = metrics.ResultError
	}
	metrics.ReconcileTotal.WithLabelValues(outcome).Inc()
	metrics.ReconcileDuration.WithLabelValues(outcome).Observe(time.Since(start).Seconds())
	return res, err
}

func (r *WebAppReconciler) reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	app := &v1alpha1.WebApp{}
	if err := r.Get(ctx, req.NamespacedName, app); err != nil {
		if apierrors.IsNotFound(err) {
			metrics.Forget(req.Name, req.Namespace)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	base := app.DeepCopy()

	if !app.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, r.finalize(ctx, app)
	}

	if controllerutil.AddFinalizer(app, v1alpha1.Finalizer) {
		if err := r.Update(ctx, app); err != nil {
			return ctrl.Result{}, err
		}
	}

	if err := r.sync(ctx, app); err != nil {
		ApplyReconcileFailure(app, err)
		r.Recorder.Event(app, corev1.EventTypeWarning, ReasonReconcileFailed, err.Error())
		if serr := r.updateStatus(ctx, app, base, nil); serr != nil {
			return ctrl.Result{}, fmt.Errorf("%w (status update also failed: %v)", err, serr)
		}
		var t terminalError
		if errors.As(err, &t) {
			return ctrl.Result{}, nil
		}
		var e externalError
		if errors.As(err, &e) {
			return ctrl.Result{RequeueAfter: externalRetry}, nil
		}
		return ctrl.Result{}, err
	}

	dep := &appsv1.Deployment{}
	if err := r.Get(ctx, client.ObjectKeyFromObject(app), dep); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		dep = nil
	}
	if app.Generation != app.Status.ObservedGeneration {
		r.Recorder.Eventf(app, corev1.EventTypeNormal, "Reconciled",
			"applied spec generation %d", app.Generation)
	}
	app.Status.ObservedGeneration = app.Generation

	if dep != nil {
		ApplyDeploymentConditions(app, dep)
	} else {
		ApplyNoDeploymentConditions(app)
	}
	return ctrl.Result{}, r.updateStatus(ctx, app, base, dep)
}

func (r *WebAppReconciler) sync(ctx context.Context, app *v1alpha1.WebApp) error {
	if err := r.enforceScalePolicy(ctx, app); err != nil {
		return err
	}

	svc := resources.Service(app)
	if len(svc.Spec.Ports) == 0 {
		return terminal("no container ports declared; the generated Service would be rejected")
	}

	dep := resources.Deployment(app)
	depAC, err := toApplyConfig[appsv1ac.DeploymentApplyConfiguration](dep, deploymentGVK)
	if err != nil {
		return err
	}
	if resources.WantsHPA(app) {
		hold, err := r.replicaHold(ctx, app)
		if err != nil {
			return err
		}
		depAC.Spec.Replicas = hold
	}
	svcAC, err := toApplyConfig[corev1ac.ServiceApplyConfiguration](svc, serviceGVK)
	if err != nil {
		return err
	}

	if err := r.assertAdoptable(ctx, app, &appsv1.Deployment{}, "deployment"); err != nil {
		return err
	}
	if err := r.assertAdoptable(ctx, app, &corev1.Service{}, "service"); err != nil {
		return err
	}

	if err := r.apply(ctx, depAC); err != nil {
		return fmt.Errorf("apply deployment: %w", err)
	}
	if err := r.apply(ctx, svcAC); err != nil {
		return fmt.Errorf("apply service: %w", err)
	}

	if err := r.syncIngress(ctx, app); err != nil {
		return err
	}
	return r.syncHPA(ctx, app)
}

func (r *WebAppReconciler) enforceScalePolicy(ctx context.Context, app *v1alpha1.WebApp) error {
	var policies v1alpha1.WebAppPolicyList
	if err := r.reader().List(ctx, &policies); err != nil {
		return fmt.Errorf("list webapppolicies: %w", err)
	}
	if len(policies.Items) == 0 {
		return nil
	}
	var ns corev1.Namespace
	if err := r.reader().Get(ctx, types.NamespacedName{Name: app.Namespace}, &ns); err != nil {
		return fmt.Errorf("get namespace %q: %w", app.Namespace, err)
	}
	res, err := policy.EvaluateScale(app, policies.Items, ns.Labels)
	if err != nil {
		return err
	}
	for _, w := range res.Warnings {
		log.FromContext(ctx).Info("webapppolicy warning", "violation", w)
	}
	for _, a := range res.Audited {
		log.FromContext(ctx).Info("webapppolicy audit", "violation", a)
	}
	if len(res.Violations) > 0 {
		r.Recorder.Event(app, corev1.EventTypeWarning, "PolicyViolation", res.Violations.ToAggregate().Error())
		return externalError{fmt.Errorf("webapppolicy: %w", res.Violations.ToAggregate())}
	}
	return nil
}

func (r *WebAppReconciler) replicaHold(ctx context.Context, app *v1alpha1.WebApp) (*int32, error) {
	key := client.ObjectKeyFromObject(app)

	var dep appsv1.Deployment
	if err := r.Get(ctx, key, &dep); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("get deployment: %w", err)
	}
	// Releasing the field before another manager owns it lets SSA remove it,
	// and the Deployment API defaults the removed field back to 1. The HPA
	// writes spec.replicas only when desired differs from current, so its
	// status is not evidence of ownership; managedFields is.
	if ReplicasOwnedByOther(&dep) {
		return nil, nil
	}
	return dep.Spec.Replicas, nil
}

func ReplicasOwnedByOther(dep *appsv1.Deployment) bool {
	for _, e := range dep.ManagedFields {
		if e.Manager == FieldOwner || e.FieldsV1 == nil {
			continue
		}
		var fields map[string]any
		if err := json.Unmarshal(e.FieldsV1.Raw, &fields); err != nil {
			continue
		}
		spec, ok := fields["f:spec"].(map[string]any)
		if !ok {
			continue
		}
		if _, ok := spec["f:replicas"]; ok {
			return true
		}
	}
	return false
}

func (r *WebAppReconciler) syncIngress(ctx context.Context, app *v1alpha1.WebApp) error {
	if !resources.WantsIngress(app) {
		return r.deleteOwned(ctx, app, &networkingv1.Ingress{})
	}
	ing, err := resources.Ingress(app)
	if err != nil {
		r.Recorder.Event(app, corev1.EventTypeWarning, "IngressBuildFailed", err.Error())
		return terminalError{fmt.Errorf("build ingress: %w", err)}
	}
	ac, err := toApplyConfig[networkingv1ac.IngressApplyConfiguration](ing, ingressGVK)
	if err != nil {
		return err
	}
	if err := r.assertAdoptable(ctx, app, &networkingv1.Ingress{}, "ingress"); err != nil {
		return err
	}
	if err := r.apply(ctx, ac); err != nil {
		r.Recorder.Event(app, corev1.EventTypeWarning, "IngressApplyFailed", err.Error())
		return fmt.Errorf("apply ingress: %w", err)
	}
	return nil
}

func (r *WebAppReconciler) syncHPA(ctx context.Context, app *v1alpha1.WebApp) error {
	if !resources.WantsHPA(app) {
		return r.deleteOwned(ctx, app, &autoscalingv2.HorizontalPodAutoscaler{})
	}
	hpa, err := resources.HPA(app)
	if err != nil {
		r.Recorder.Event(app, corev1.EventTypeWarning, "HPABuildFailed", err.Error())
		return terminalError{fmt.Errorf("build hpa: %w", err)}
	}
	ac, err := toApplyConfig[autoscalingv2ac.HorizontalPodAutoscalerApplyConfiguration](hpa, hpaGVK)
	if err != nil {
		return err
	}
	if err := r.assertAdoptable(ctx, app, &autoscalingv2.HorizontalPodAutoscaler{}, "hpa"); err != nil {
		return err
	}
	if err := r.apply(ctx, ac); err != nil {
		return fmt.Errorf("apply hpa: %w", err)
	}
	return nil
}

func (r *WebAppReconciler) deleteOwned(ctx context.Context, app *v1alpha1.WebApp, obj client.Object) error {
	if err := r.Get(ctx, client.ObjectKeyFromObject(app), obj); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("get %T: %w", obj, err)
	}
	if !metav1.IsControlledBy(obj, app) {
		return nil
	}
	if err := r.Delete(ctx, obj); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete %T: %w", obj, err)
	}
	return nil
}

func (r *WebAppReconciler) assertAdoptable(ctx context.Context, app *v1alpha1.WebApp, obj client.Object, kind string) error {
	if err := r.reader().Get(ctx, client.ObjectKeyFromObject(app), obj); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("get %s: %w", kind, err)
	}
	if !metav1.IsControlledBy(obj, app) {
		return external("%s %q already exists and is not controlled by this WebApp", kind, app.Name)
	}
	return nil
}

func (r *WebAppReconciler) finalize(ctx context.Context, app *v1alpha1.WebApp) error {
	if !controllerutil.ContainsFinalizer(app, v1alpha1.Finalizer) {
		return nil
	}
	for _, obj := range []client.Object{
		&networkingv1.Ingress{},
		&autoscalingv2.HorizontalPodAutoscaler{},
		&corev1.Service{},
		&appsv1.Deployment{},
	} {
		if err := r.deleteOwned(ctx, app, obj); err != nil {
			return err
		}
	}

	r.Recorder.Event(app, corev1.EventTypeNormal, "Deleted", "owned resources removed")
	metrics.Forget(app.Name, app.Namespace)

	controllerutil.RemoveFinalizer(app, v1alpha1.Finalizer)
	return r.Update(ctx, app)
}

func (r *WebAppReconciler) updateStatus(ctx context.Context, app *v1alpha1.WebApp, base *v1alpha1.WebApp, dep *appsv1.Deployment) error {
	if dep != nil && dep.Status.Replicas != base.Status.Replicas {
		r.Recorder.Eventf(app, corev1.EventTypeNormal, "Scaled",
			"replicas %d -> %d", base.Status.Replicas, dep.Status.Replicas)
	}

	app.Status.IngressURL = resources.IngressURL(app)
	app.Status.Selector = labels.Set(resources.SelectorLabels(app)).String()

	if dep != nil {
		app.Status.Replicas = dep.Status.Replicas
		app.Status.ReadyReplicas = dep.Status.ReadyReplicas
		app.Status.AvailableReplicas = dep.Status.AvailableReplicas
		app.Status.UpdatedReplicas = dep.Status.UpdatedReplicas
	}

	metrics.Replicas.WithLabelValues(app.Name, app.Namespace).Set(float64(app.Status.Replicas))
	metrics.ReadyReplicas.WithLabelValues(app.Name, app.Namespace).Set(float64(app.Status.ReadyReplicas))
	for _, c := range app.Status.Conditions {
		metrics.SetCondition(app.Name, app.Namespace, c.Type, string(c.Status))
	}

	// MergeFrom carries no resourceVersion, so a reconcile racing the Deployment
	// watch patches status instead of failing with a conflict.
	return r.Status().Patch(ctx, app, client.MergeFrom(base))
}

func (r *WebAppReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.WebApp{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Owns(&networkingv1.Ingress{}).
		Owns(&autoscalingv2.HorizontalPodAutoscaler{}).
		Named("webapp").
		Complete(r)
}
