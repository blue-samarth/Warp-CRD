package metrics

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestSetCondition_ReplacesStaleStatusSeries(t *testing.T) {
	t.Cleanup(func() { Forget("app", "ns") })

	SetCondition("app", "ns", "Ready", "True")
	if got := testutil.CollectAndCount(Conditions); got != 1 {
		t.Fatalf("want 1 series, got %d", got)
	}

	SetCondition("app", "ns", "Ready", "False")
	if got := testutil.CollectAndCount(Conditions); got != 1 {
		t.Fatalf("a flipped condition must not leave a stale series behind; got %d", got)
	}
	if v := testutil.ToFloat64(Conditions.WithLabelValues("app", "ns", "Ready", "False")); v != 1 {
		t.Fatalf("want current status recorded, got %v", v)
	}
}

func TestSetCondition_KeepsDistinctTypes(t *testing.T) {
	t.Cleanup(func() { Forget("app", "ns") })

	SetCondition("app", "ns", "Ready", "True")
	SetCondition("app", "ns", "Degraded", "False")
	if got := testutil.CollectAndCount(Conditions); got != 2 {
		t.Fatalf("want both condition types retained, got %d", got)
	}
}

func TestForget_DropsAllSeriesForAWebApp(t *testing.T) {
	SetCondition("gone", "ns", "Ready", "True")
	Replicas.WithLabelValues("gone", "ns").Set(3)
	ReadyReplicas.WithLabelValues("gone", "ns").Set(3)

	Forget("gone", "ns")

	if got := testutil.CollectAndCount(Conditions); got != 0 {
		t.Fatalf("want conditions cleared, got %d", got)
	}
	if got := testutil.CollectAndCount(Replicas); got != 0 {
		t.Fatalf("want replicas cleared, got %d", got)
	}
}
