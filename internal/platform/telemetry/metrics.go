// Package telemetry owns the metrics and tracing NEXUS emits about itself.
//
// A delivery platform that cannot answer "how long do deployments take, and how often
// do they fail" is not observable, whatever it exposes about the workloads it deploys.
// Everything defined here is about NEXUS itself.
package telemetry

import (
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Metrics holds every instrument the control plane exports.
//
// Held on a struct and injected rather than registered into the default registry as
// package globals: globals make two instances impossible, which makes tests that assert
// on metric values flaky and order-dependent.
type Metrics struct {
	registry *prometheus.Registry

	// HTTP surface
	HTTPRequests *prometheus.CounterVec
	HTTPDuration *prometheus.HistogramVec
	HTTPInFlight prometheus.Gauge

	// Pipeline execution
	PipelineRuns     *prometheus.CounterVec
	PipelineDuration *prometheus.HistogramVec
	JobsTotal        *prometheus.CounterVec
	JobDuration      *prometheus.HistogramVec
	JobRetries       *prometheus.CounterVec
	QueueDepth       *prometheus.GaugeVec
	WorkerJobsActive *prometheus.GaugeVec
	WorkersActive    prometheus.Gauge
	LeasesReaped     prometheus.Counter

	// Delivery
	Deployments        *prometheus.CounterVec
	DeploymentDuration *prometheus.HistogramVec
	HealthChecks       *prometheus.CounterVec
	Rollbacks          *prometheus.CounterVec
	DriftDetected      prometheus.Counter

	// Eventing
	EventsPublished *prometheus.CounterVec
	OutboxPending   prometheus.Gauge
	OutboxFailures  prometheus.Counter
	WebhooksTotal   *prometheus.CounterVec
}

// Duration buckets are chosen per instrument rather than using the default set.
//
// Prometheus defaults top out at 10s, which is useless for a deployment that takes four
// minutes: every real observation lands in +Inf and the histogram can no longer answer
// what a p95 deployment costs.
var (
	httpBuckets       = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}
	jobBuckets        = []float64{0.5, 1, 2.5, 5, 10, 30, 60, 120, 300, 600, 1800}
	pipelineBuckets   = []float64{5, 15, 30, 60, 120, 300, 600, 1200, 3600}
	deploymentBuckets = []float64{1, 5, 10, 30, 60, 120, 300, 600}
)

func NewMetrics() *Metrics {
	reg := prometheus.NewRegistry()
	f := promauto.With(reg)

	// Process and Go runtime metrics: without these there is no way to tell a slow
	// pipeline from a control plane that is garbage collecting itself to death.
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)

	return &Metrics{
		registry: reg,

		HTTPRequests: f.NewCounterVec(prometheus.CounterOpts{
			Name: "nexus_http_requests_total",
			Help: "HTTP requests handled by the control plane API.",
		}, []string{"method", "route", "status"}),
		HTTPDuration: f.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "nexus_http_request_duration_seconds",
			Help:    "HTTP request latency.",
			Buckets: httpBuckets,
		}, []string{"method", "route"}),
		HTTPInFlight: f.NewGauge(prometheus.GaugeOpts{
			Name: "nexus_http_requests_in_flight",
			Help: "HTTP requests currently being served.",
		}),

		PipelineRuns: f.NewCounterVec(prometheus.CounterOpts{
			Name: "nexus_pipeline_runs_total",
			Help: "Pipeline runs by terminal state.",
		}, []string{"project", "trigger", "state"}),
		PipelineDuration: f.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "nexus_pipeline_duration_seconds",
			Help:    "Wall-clock duration of completed pipeline runs.",
			Buckets: pipelineBuckets,
		}, []string{"project", "state"}),
		JobsTotal: f.NewCounterVec(prometheus.CounterOpts{
			Name: "nexus_jobs_total",
			Help: "Jobs by step type and terminal state.",
		}, []string{"type", "state"}),
		JobDuration: f.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "nexus_job_duration_seconds",
			Help:    "Job execution duration.",
			Buckets: jobBuckets,
		}, []string{"type", "state"}),
		JobRetries: f.NewCounterVec(prometheus.CounterOpts{
			Name: "nexus_job_retries_total",
			Help: "Job attempts beyond the first, by reason.",
		}, []string{"type", "reason"}),
		QueueDepth: f.NewGaugeVec(prometheus.GaugeOpts{
			Name: "nexus_queue_depth",
			Help: "Jobs currently claimable.",
		}, []string{"state"}),
		WorkerJobsActive: f.NewGaugeVec(prometheus.GaugeOpts{
			Name: "nexus_worker_jobs_active",
			Help: "Jobs currently executing on a worker.",
		}, []string{"worker"}),
		WorkersActive: f.NewGauge(prometheus.GaugeOpts{
			Name: "nexus_workers_active",
			Help: "Workers that have heartbeated within the liveness window.",
		}),
		LeasesReaped: f.NewCounter(prometheus.CounterOpts{
			Name: "nexus_leases_reaped_total",
			Help: "Jobs recovered from workers that stopped renewing their lease.",
		}),

		Deployments: f.NewCounterVec(prometheus.CounterOpts{
			Name: "nexus_deployments_total",
			Help: "Deployments by environment and terminal state.",
		}, []string{"environment", "state"}),
		DeploymentDuration: f.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "nexus_deployment_duration_seconds",
			Help:    "Time from deployment request to terminal state.",
			Buckets: deploymentBuckets,
		}, []string{"environment", "state"}),
		HealthChecks: f.NewCounterVec(prometheus.CounterOpts{
			Name: "nexus_health_checks_total",
			Help: "Health probes performed during deployment verification.",
		}, []string{"result"}),
		Rollbacks: f.NewCounterVec(prometheus.CounterOpts{
			Name: "nexus_rollbacks_total",
			Help: "Rollbacks by environment and how they were initiated.",
		}, []string{"environment", "initiator"}),
		DriftDetected: f.NewCounter(prometheus.CounterOpts{
			Name: "nexus_drift_detected_total",
			Help: "Occasions the reconciler found actual state diverged from desired.",
		}),

		EventsPublished: f.NewCounterVec(prometheus.CounterOpts{
			Name: "nexus_events_published_total",
			Help: "Events relayed from the outbox to the transport.",
		}, []string{"type"}),
		OutboxPending: f.NewGauge(prometheus.GaugeOpts{
			Name: "nexus_outbox_pending",
			Help: "Outbox rows awaiting publication. Sustained growth means the transport is unreachable.",
		}),
		OutboxFailures: f.NewCounter(prometheus.CounterOpts{
			Name: "nexus_outbox_publish_failures_total",
			Help: "Failed attempts to publish an outbox row.",
		}),
		WebhooksTotal: f.NewCounterVec(prometheus.CounterOpts{
			Name: "nexus_webhooks_total",
			Help: "Inbound webhook deliveries by provider and outcome.",
		}, []string{"provider", "outcome"}),
	}
}

func (m *Metrics) Registry() *prometheus.Registry { return m.registry }

// ObserveHTTP records one completed request.
func (m *Metrics) ObserveHTTP(method, route string, status int, d time.Duration) {
	m.HTTPRequests.WithLabelValues(method, route, strconv.Itoa(status)).Inc()
	m.HTTPDuration.WithLabelValues(method, route).Observe(d.Seconds())
}

func (m *Metrics) ObserveJob(stepType, state string, d time.Duration) {
	m.JobsTotal.WithLabelValues(stepType, state).Inc()
	m.JobDuration.WithLabelValues(stepType, state).Observe(d.Seconds())
}

func (m *Metrics) ObservePipeline(project, trigger, state string, d time.Duration) {
	m.PipelineRuns.WithLabelValues(project, trigger, state).Inc()
	m.PipelineDuration.WithLabelValues(project, state).Observe(d.Seconds())
}

func (m *Metrics) ObserveDeployment(environment, state string, d time.Duration) {
	m.Deployments.WithLabelValues(environment, state).Inc()
	m.DeploymentDuration.WithLabelValues(environment, state).Observe(d.Seconds())
}
