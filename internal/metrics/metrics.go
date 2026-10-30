// Package metrics exposes the handful of series that replace a control panel:
// the system stays quiet while it is healthy and speaks up only when it is not.
package metrics

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics owns every series this service exports.
type Metrics struct {
	// QueuedJobs is the queue depth per pool; a non-zero value that persists
	// means expansion is failing.
	QueuedJobs *prometheus.GaugeVec
	// Instances is the observed container group count per pool and state.
	Instances *prometheus.GaugeVec
	// CreateFailures counts failed launches by reason (throttling, stock, quota).
	CreateFailures *prometheus.CounterVec
	// QueueWait records how long a job waited before a runner picked it up.
	QueueWait *prometheus.HistogramVec
	// Orphans counts container groups that outlived their job.
	Orphans prometheus.Gauge

	registry *prometheus.Registry
}

// New builds the metrics and registers them on a private registry.
func New() *Metrics {
	m := &Metrics{
		QueuedJobs: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "eci_runner_queued_jobs",
			Help: "Workflow jobs waiting for a runner, per pool.",
		}, []string{"pool"}),
		Instances: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "eci_runner_instances",
			Help: "Observed ECI container groups, per pool and state.",
		}, []string{"pool", "state"}),
		CreateFailures: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "eci_runner_create_failures_total",
			Help: "Failed container group launches, by reason.",
		}, []string{"pool", "reason"}),
		QueueWait: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "eci_runner_queue_wait_seconds",
			Help:    "Time from job queueing to runner pickup.",
			Buckets: []float64{1, 5, 15, 30, 60, 120, 300, 600},
		}, []string{"pool"}),
		Orphans: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "eci_runner_orphan_instances",
			Help: "Container groups still present after their job finished.",
		}),
		registry: prometheus.NewRegistry(),
	}
	m.registry.MustRegister(
		m.QueuedJobs,
		m.Instances,
		m.CreateFailures,
		m.QueueWait,
		m.Orphans,
	)
	return m
}

// Handler serves the metrics in the Prometheus text format.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
}
