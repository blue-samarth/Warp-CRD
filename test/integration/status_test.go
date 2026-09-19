package integration_test

import (
	"fmt"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv1 "k8s.io/api/autoscaling/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/blue-samarth/Warp-CRD/api/v1alpha1"
)

// envtest runs no Deployment controller, so nothing ever writes Deployment
// status. These tests write it by hand to drive the operator's status mapping.
func setDeploymentStatus(t *testing.T, key client.ObjectKey, st appsv1.DeploymentStatus) {
	t.Helper()
	dep := &appsv1.Deployment{}
	eventually(t, func() error { return k8sClient.Get(testCtx, key, dep) })
	dep.Status = st
	// A real Deployment controller stamps this; Ready now requires it, so a
	// caller that leaves it unset means "has observed the current template".
	if dep.Status.ObservedGeneration == 0 {
		dep.Status.ObservedGeneration = dep.Generation
	}
	if err := k8sClient.Status().Update(testCtx, dep); err != nil {
		t.Fatalf("update deployment status: %v", err)
	}
}

func TestStatus_ReadyWhenDeploymentFullyRolledOut(t *testing.T) {
	ns := newNamespace(t)
	app := baseWebApp(ns, "rolled-out")
	app.Spec.Replicas = new(int32(2))
	key := createApp(t, app)

	setDeploymentStatus(t, key, appsv1.DeploymentStatus{
		Replicas:          2,
		ReadyReplicas:     2,
		AvailableReplicas: 2,
		UpdatedReplicas:   2,
		Conditions: []appsv1.DeploymentCondition{
			{Type: appsv1.DeploymentAvailable, Status: corev1.ConditionTrue, Reason: "MinimumReplicasAvailable"},
			{Type: appsv1.DeploymentProgressing, Status: corev1.ConditionTrue, Reason: "NewReplicaSetAvailable"},
		},
	})

	got := &v1alpha1.WebApp{}
	eventually(t, func() error {
		if err := k8sClient.Get(testCtx, key, got); err != nil {
			return err
		}
		c := meta.FindStatusCondition(got.Status.Conditions, v1alpha1.ConditionReady)
		if c == nil || c.Status != metav1.ConditionTrue {
			return fmt.Errorf("Ready = %v", c)
		}
		if got.Status.ReadyReplicas != 2 || got.Status.AvailableReplicas != 2 {
			return fmt.Errorf("counters not mirrored: %+v", got.Status)
		}
		return nil
	})

	if c := meta.FindStatusCondition(got.Status.Conditions, v1alpha1.ConditionAvailable); c.Status != metav1.ConditionTrue {
		t.Fatalf("want Available=True, got %+v", c)
	}
	if c := meta.FindStatusCondition(got.Status.Conditions, v1alpha1.ConditionDegraded); c.Status != metav1.ConditionFalse {
		t.Fatalf("want Degraded=False, got %+v", c)
	}
}

func TestStatus_DegradedWhenRolloutStalls(t *testing.T) {
	ns := newNamespace(t)
	key := createApp(t, baseWebApp(ns, "stalled"))

	setDeploymentStatus(t, key, appsv1.DeploymentStatus{
		Replicas: 1,
		Conditions: []appsv1.DeploymentCondition{
			{Type: appsv1.DeploymentAvailable, Status: corev1.ConditionFalse, Reason: "MinimumReplicasUnavailable"},
			{
				Type:    appsv1.DeploymentProgressing,
				Status:  corev1.ConditionFalse,
				Reason:  "ProgressDeadlineExceeded",
				Message: "ReplicaSet has timed out progressing",
			},
		},
	})

	eventually(t, func() error {
		got := &v1alpha1.WebApp{}
		if err := k8sClient.Get(testCtx, key, got); err != nil {
			return err
		}
		d := meta.FindStatusCondition(got.Status.Conditions, v1alpha1.ConditionDegraded)
		if d == nil || d.Status != metav1.ConditionTrue {
			return fmt.Errorf("Degraded = %v", d)
		}
		r := meta.FindStatusCondition(got.Status.Conditions, v1alpha1.ConditionReady)
		if r == nil || r.Status != metav1.ConditionFalse {
			return fmt.Errorf("Ready = %v", r)
		}
		return nil
	})
}

func TestStatus_ScaleSubresourceWorks(t *testing.T) {
	ns := newNamespace(t)
	key := createApp(t, baseWebApp(ns, "scalable"))

	eventually(t, func() error {
		got := &v1alpha1.WebApp{}
		if err := k8sClient.Get(testCtx, key, got); err != nil {
			return err
		}
		if got.Status.Selector == "" {
			return fmt.Errorf("selector empty")
		}
		return nil
	})

	scale := &autoscalingv1.Scale{}
	if err := k8sClient.SubResource("scale").Get(testCtx, &v1alpha1.WebApp{
		ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace},
	}, scale); err != nil {
		t.Fatalf("read scale subresource: %v", err)
	}
	if scale.Spec.Replicas != 1 {
		t.Fatalf("want scale.spec.replicas 1, got %d", scale.Spec.Replicas)
	}
	if scale.Status.Selector == "" {
		t.Fatal("scale subresource reports no selector; kubectl scale and the HPA need it")
	}
}
