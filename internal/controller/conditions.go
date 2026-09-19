package controller

import (
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/blue-samarth/Warp-CRD/api/v1alpha1"
)

const (
	ReasonReconcileFailed = "ReconcileFailed"
	ReasonRolloutStalled  = "RolloutStalled"
	ReasonMinimumReplicas = "MinimumReplicasAvailable"
	ReasonRollingOut      = "RollingOut"
	ReasonAllReplicas     = "AllReplicasReady"
	ReasonNotReady        = "ReplicasNotReady"
	ReasonNoDeployment    = "DeploymentNotFound"
	ReasonReconciled      = "Reconciled"
	ReasonRolloutPending  = "RolloutPending"
	ReasonScaledToZero    = "ScaledToZero"
)

func SetCondition(app *v1alpha1.WebApp, condType string, status metav1.ConditionStatus, reason, msg string) {
	meta.SetStatusCondition(&app.Status.Conditions, metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            msg,
		ObservedGeneration: app.Generation,
	})
}

func DeploymentCondition(dep *appsv1.Deployment, t appsv1.DeploymentConditionType) *appsv1.DeploymentCondition {
	for i := range dep.Status.Conditions {
		if dep.Status.Conditions[i].Type == t {
			return &dep.Status.Conditions[i]
		}
	}
	return nil
}

func ApplyDeploymentConditions(app *v1alpha1.WebApp, dep *appsv1.Deployment) {
	avail := DeploymentCondition(dep, appsv1.DeploymentAvailable)
	if avail != nil {
		SetCondition(app, v1alpha1.ConditionAvailable,
			metav1.ConditionStatus(avail.Status), ReasonOr(avail.Reason, ReasonMinimumReplicas), avail.Message)
	} else {
		SetCondition(app, v1alpha1.ConditionAvailable, metav1.ConditionUnknown, ReasonNoDeployment,
			"deployment has not reported availability yet")
	}

	prog := DeploymentCondition(dep, appsv1.DeploymentProgressing)
	stalled := prog != nil && prog.Status == corev1.ConditionFalse && prog.Reason == "ProgressDeadlineExceeded"
	if prog != nil {
		SetCondition(app, v1alpha1.ConditionProgressing,
			metav1.ConditionStatus(prog.Status), ReasonOr(prog.Reason, ReasonRollingOut), prog.Message)
	} else {
		SetCondition(app, v1alpha1.ConditionProgressing, metav1.ConditionUnknown, ReasonNoDeployment,
			"deployment has not reported progress yet")
	}

	if stalled {
		SetCondition(app, v1alpha1.ConditionDegraded, metav1.ConditionTrue, ReasonRolloutStalled, prog.Message)
	} else {
		SetCondition(app, v1alpha1.ConditionDegraded, metav1.ConditionFalse, ReasonReconciled,
			"no degraded state detected")
	}

	SetReady(app, dep, stalled)
}

func SetReady(app *v1alpha1.WebApp, dep *appsv1.Deployment, stalled bool) {
	desired := int32(1)
	if dep.Spec.Replicas != nil {
		desired = *dep.Spec.Replicas
	}
	switch {
	case stalled:
		SetCondition(app, v1alpha1.ConditionReady, metav1.ConditionFalse, ReasonRolloutStalled,
			"rollout stalled; see Degraded condition")
	case dep.Status.ObservedGeneration < dep.Generation:
		SetCondition(app, v1alpha1.ConditionReady, metav1.ConditionFalse, ReasonRolloutPending,
			"deployment has not yet observed the current template")
	case desired == 0:
		SetCondition(app, v1alpha1.ConditionReady, metav1.ConditionFalse, ReasonScaledToZero,
			"scaled to zero replicas")
	case dep.Status.UpdatedReplicas == desired && dep.Status.ReadyReplicas == desired:
		SetCondition(app, v1alpha1.ConditionReady, metav1.ConditionTrue, ReasonAllReplicas,
			"all replicas are ready and running the current template")
	default:
		SetCondition(app, v1alpha1.ConditionReady, metav1.ConditionFalse, ReasonNotReady,
			"waiting for replicas to become ready")
	}
}

func ApplyNoDeploymentConditions(app *v1alpha1.WebApp) {
	const msg = "deployment has not been created yet"
	SetCondition(app, v1alpha1.ConditionAvailable, metav1.ConditionUnknown, ReasonNoDeployment, msg)
	SetCondition(app, v1alpha1.ConditionProgressing, metav1.ConditionUnknown, ReasonNoDeployment, msg)
	SetCondition(app, v1alpha1.ConditionDegraded, metav1.ConditionFalse, ReasonReconciled,
		"no degraded state detected")
	SetCondition(app, v1alpha1.ConditionReady, metav1.ConditionFalse, ReasonNotReady, msg)
}

func ApplyReconcileFailure(app *v1alpha1.WebApp, err error) {
	SetCondition(app, v1alpha1.ConditionDegraded, metav1.ConditionTrue, ReasonReconcileFailed, err.Error())
	SetCondition(app, v1alpha1.ConditionReady, metav1.ConditionFalse, ReasonReconcileFailed, err.Error())
	for _, t := range []string{v1alpha1.ConditionAvailable, v1alpha1.ConditionProgressing} {
		if meta.FindStatusCondition(app.Status.Conditions, t) == nil {
			SetCondition(app, t, metav1.ConditionUnknown, ReasonNoDeployment,
				"deployment has not been created yet")
		}
	}
}

func ReasonOr(reason, fallback string) string {
	if reason == "" {
		return fallback
	}
	return reason
}
