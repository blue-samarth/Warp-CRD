package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

const (
	ResultSuccess = "success"
	ResultError   = "error"
)

var (
	ReconcileTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "webapp_reconcile_total",
		Help: "Total number of WebApp reconciliations by result.",
	}, []string{"result"})

	ReconcileDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "webapp_reconcile_duration_seconds",
		Help:    "Duration of WebApp reconciliations in seconds.",
		Buckets: prometheus.DefBuckets,
	}, []string{"result"})

	Replicas = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "webapp_replicas",
		Help: "Replicas reported by the Deployment owned by a WebApp.",
	}, []string{"name", "namespace"})

	ReadyReplicas = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "webapp_ready_replicas",
		Help: "Ready replicas reported by the Deployment owned by a WebApp.",
	}, []string{"name", "namespace"})

	Conditions = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "webapp_conditions",
		Help: "Current WebApp conditions; 1 when the condition holds the given status.",
	}, []string{"name", "namespace", "type", "status"})
)

func init() {
	ctrlmetrics.Registry.MustRegister(
		ReconcileTotal, ReconcileDuration, Replicas, ReadyReplicas, Conditions,
	)
}

func SetCondition(name, namespace, condType, status string) {
	Conditions.DeletePartialMatch(prometheus.Labels{
		"name": name, "namespace": namespace, "type": condType,
	})
	Conditions.WithLabelValues(name, namespace, condType, status).Set(1)
}

func Forget(name, namespace string) {
	Replicas.DeleteLabelValues(name, namespace)
	ReadyReplicas.DeleteLabelValues(name, namespace)
	Conditions.DeletePartialMatch(prometheus.Labels{"name": name, "namespace": namespace})
}
