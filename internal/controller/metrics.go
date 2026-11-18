package controller

import (
	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	appsv1alpha1 "github.com/gracefulset-io/gracefulset/api/v1alpha1"
)

var (
	podsGauge = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "gracefulset_pods",
		Help: "Pods per GracefulSet, version and state (active or draining).",
	}, []string{"namespace", "gracefulset", "version", "state"})

	drainDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name: "gracefulset_drain_duration_seconds",
		Help: "Time from a pod starting to drain until it was removed, by removal reason.",
		// 1 minute to ~3 days
		Buckets: prometheus.ExponentialBuckets(60, 3, 9),
	}, []string{"namespace", "gracefulset", "reason"})

	drainCheckFailures = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "gracefulset_drain_check_failures_total",
		Help: "Drain check HTTP calls that failed or returned an unusable response.",
	}, []string{"namespace", "gracefulset"})
)

func init() {
	metrics.Registry.MustRegister(podsGauge, drainDuration, drainCheckFailures)
}

func recordPodMetrics(gs *appsv1alpha1.GracefulSet, active, draining []corev1.Pod) {
	podsGauge.DeletePartialMatch(prometheus.Labels{"namespace": gs.Namespace, "gracefulset": gs.Name})
	for state, pods := range map[string][]corev1.Pod{"active": active, "draining": draining} {
		for i := range pods {
			podsGauge.WithLabelValues(gs.Namespace, gs.Name, pods[i].Labels[versionLabel], state).Inc()
		}
	}
}
