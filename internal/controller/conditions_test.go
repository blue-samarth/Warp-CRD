package controller

import (
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/blue-samarth/Warp-CRD/api/v1alpha1"
)

func deploymentWith(replicas int32, conds ...appsv1.DeploymentCondition) *appsv1.Deployment {
	return &appsv1.Deployment{
		Spec:   appsv1.DeploymentSpec{Replicas: new(replicas)},
		Status: appsv1.DeploymentStatus{Conditions: conds},
	}
}

func cond(t appsv1.DeploymentConditionType, s corev1.ConditionStatus, reason, msg string) appsv1.DeploymentCondition {
	return appsv1.DeploymentCondition{Type: t, Status: s, Reason: reason, Message: msg}
}

func statusOf(app *v1alpha1.WebApp, condType string) *metav1.Condition {
	return meta.FindStatusCondition(app.Status.Conditions, condType)
}

func TestDeploymentCondition_FindsAndMisses(t *testing.T) {
	dep := deploymentWith(1, cond(appsv1.DeploymentAvailable, corev1.ConditionTrue, "MinimumReplicasAvailable", "ok"))
	if got := deploymentCondition(dep, appsv1.DeploymentAvailable); got == nil {
		t.Fatal("want Available condition found")
	}
	if got := deploymentCondition(dep, appsv1.DeploymentProgressing); got != nil {
		t.Fatalf("want nil for absent condition, got %+v", got)
	}
}

func TestApplyDeploymentConditions_MirrorsAvailable(t *testing.T) {
	app := &v1alpha1.WebApp{}
	dep := deploymentWith(1,
		cond(appsv1.DeploymentAvailable, corev1.ConditionTrue, "MinimumReplicasAvailable", "ok"),
		cond(appsv1.DeploymentProgressing, corev1.ConditionTrue, "NewReplicaSetAvailable", "rolled out"),
	)
	applyDeploymentConditions(app, dep)

	if c := statusOf(app, v1alpha1.ConditionAvailable); c == nil || c.Status != metav1.ConditionTrue {
		t.Fatalf("want Available=True, got %+v", c)
	}
	if c := statusOf(app, v1alpha1.ConditionProgressing); c == nil || c.Status != metav1.ConditionTrue {
		t.Fatalf("want Progressing=True, got %+v", c)
	}
	if c := statusOf(app, v1alpha1.ConditionDegraded); c == nil || c.Status != metav1.ConditionFalse {
		t.Fatalf("want Degraded=False, got %+v", c)
	}
}

func TestApplyDeploymentConditions_UnknownWhenDeploymentSilent(t *testing.T) {
	app := &v1alpha1.WebApp{}
	applyDeploymentConditions(app, deploymentWith(1))

	for _, ct := range []string{v1alpha1.ConditionAvailable, v1alpha1.ConditionProgressing} {
		c := statusOf(app, ct)
		if c == nil || c.Status != metav1.ConditionUnknown {
			t.Fatalf("want %s=Unknown, got %+v", ct, c)
		}
		if c.Reason != ReasonNoDeployment {
			t.Fatalf("want reason %s, got %s", ReasonNoDeployment, c.Reason)
		}
	}
}

func TestApplyDeploymentConditions_StalledRolloutIsDegraded(t *testing.T) {
	app := &v1alpha1.WebApp{}
	dep := deploymentWith(3,
		cond(appsv1.DeploymentAvailable, corev1.ConditionFalse, "MinimumReplicasUnavailable", "down"),
		cond(appsv1.DeploymentProgressing, corev1.ConditionFalse, "ProgressDeadlineExceeded", "timed out"),
	)
	applyDeploymentConditions(app, dep)

	d := statusOf(app, v1alpha1.ConditionDegraded)
	if d == nil || d.Status != metav1.ConditionTrue || d.Reason != ReasonRolloutStalled {
		t.Fatalf("want Degraded=True/RolloutStalled, got %+v", d)
	}
	r := statusOf(app, v1alpha1.ConditionReady)
	if r == nil || r.Status != metav1.ConditionFalse || r.Reason != ReasonRolloutStalled {
		t.Fatalf("want Ready=False/RolloutStalled, got %+v", r)
	}
}

func TestSetReady_TrueWhenAllReplicasUpdatedAndReady(t *testing.T) {
	app := &v1alpha1.WebApp{}
	dep := deploymentWith(3)
	dep.Status.UpdatedReplicas = 3
	dep.Status.ReadyReplicas = 3
	setReady(app, dep, false)

	c := statusOf(app, v1alpha1.ConditionReady)
	if c == nil || c.Status != metav1.ConditionTrue || c.Reason != ReasonAllReplicas {
		t.Fatalf("want Ready=True/AllReplicasReady, got %+v", c)
	}
}

func TestSetReady_FalseWhenPartiallyRolledOut(t *testing.T) {
	app := &v1alpha1.WebApp{}
	dep := deploymentWith(3)
	dep.Status.UpdatedReplicas = 1
	dep.Status.ReadyReplicas = 1
	setReady(app, dep, false)

	c := statusOf(app, v1alpha1.ConditionReady)
	if c == nil || c.Status != metav1.ConditionFalse || c.Reason != ReasonNotReady {
		t.Fatalf("want Ready=False/ReplicasNotReady, got %+v", c)
	}
}

func TestSetReady_DefaultsDesiredToOneWhenReplicasNil(t *testing.T) {
	app := &v1alpha1.WebApp{}
	dep := &appsv1.Deployment{}
	dep.Status.UpdatedReplicas = 1
	dep.Status.ReadyReplicas = 1

	setReady(app, dep, false)
	if c := statusOf(app, v1alpha1.ConditionReady); c == nil || c.Status != metav1.ConditionTrue {
		t.Fatalf("want Ready=True with nil replicas treated as 1, got %+v", c)
	}
}

func TestSetCondition_RecordsObservedGeneration(t *testing.T) {
	app := &v1alpha1.WebApp{}
	app.Generation = 7
	setCondition(app, v1alpha1.ConditionReady, metav1.ConditionTrue, ReasonAllReplicas, "ok")

	c := statusOf(app, v1alpha1.ConditionReady)
	if c == nil || c.ObservedGeneration != 7 {
		t.Fatalf("want observedGeneration 7, got %+v", c)
	}
}

func TestReasonOr_FallsBackWhenEmpty(t *testing.T) {
	if got := reasonOr("", "fallback"); got != "fallback" {
		t.Fatalf("want fallback, got %q", got)
	}
	if got := reasonOr("actual", "fallback"); got != "actual" {
		t.Fatalf("want actual, got %q", got)
	}
}
