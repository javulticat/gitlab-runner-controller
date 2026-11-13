package controller

import (
	"net/http"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/javulticat/gitlab-runner-controller/internal/state"
	"github.com/javulticat/gitlab-runner-controller/pkg/provider"
)

type Metrics struct {
	registry *prometheus.Registry
	cycles   *prometheus.CounterVec
	duration prometheus.Histogram
	state    *stateMetrics
}

type metricSnapshot struct {
	metrics []prometheus.Metric
}

type stateMetrics struct {
	snapshot         atomic.Pointer[metricSnapshot]
	slots            *prometheus.Desc
	jobs             *prometheus.Desc
	lastSync         *prometheus.Desc
	backoff          *prometheus.Desc
	unresolved       *prometheus.Desc
	ready            *prometheus.Desc
	notReady         *prometheus.Desc
	bootstrapTimeout *prometheus.Desc
	bootstrapPending *prometheus.Desc
}

func NewMetrics() *Metrics {
	m := &Metrics{
		registry: prometheus.NewRegistry(),
		cycles:   prometheus.NewCounterVec(prometheus.CounterOpts{Name: "grc_reconciliations_total", Help: "Reconciliation cycles by result."}, []string{"result"}),
		duration: prometheus.NewHistogram(prometheus.HistogramOpts{Name: "grc_reconciliation_duration_seconds", Help: "Time spent reconciling.", Buckets: prometheus.ExponentialBuckets(0.1, 4, 8)}),
		state: &stateMetrics{
			slots:            prometheus.NewDesc("grc_instances", "Managed instances by pool and observed power state.", []string{"pool", "state"}, nil),
			jobs:             prometheus.NewDesc("grc_jobs", "Verified active jobs by status.", []string{"status"}, nil),
			lastSync:         prometheus.NewDesc("grc_gitlab_last_sync_timestamp_seconds", "Last complete GitLab synchronization.", nil, nil),
			backoff:          prometheus.NewDesc("grc_gitlab_backoff_until_timestamp_seconds", "End of GitLab synchronization backoff, or zero.", nil, nil),
			unresolved:       prometheus.NewDesc("grc_unresolved_operations", "Unresolved resource creations by pool.", []string{"pool"}, nil),
			ready:            prometheus.NewDesc("grc_runner_ready", "Running instances with a ready runner by pool.", []string{"pool"}, nil),
			notReady:         prometheus.NewDesc("grc_runner_not_ready", "Running instances without a ready runner by pool.", []string{"pool"}, nil),
			bootstrapTimeout: prometheus.NewDesc("grc_bootstrap_timeouts", "Instances awaiting runner readiness past the bootstrap deadline by pool.", []string{"pool"}, nil),
			bootstrapPending: prometheus.NewDesc("grc_bootstrap_pending_seconds", "Longest observed wait for runner readiness by pool.", []string{"pool"}, nil),
		},
	}
	m.state.observe(state.State{}, time.Time{})
	m.registry.MustRegister(m.cycles, m.duration, m.state, collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	return m
}

func (m *Metrics) Observe(saved state.State, elapsed time.Duration, err error) {
	m.ObserveAt(saved, time.Now(), elapsed, err)
}

func (m *Metrics) ObserveAt(saved state.State, observedAt time.Time, elapsed time.Duration, err error) {
	result := "success"
	if err != nil {
		result = "error"
	}
	m.cycles.WithLabelValues(result).Inc()
	m.duration.Observe(elapsed.Seconds())
	m.state.observe(saved, observedAt)
}

func (m *stateMetrics) Describe(ch chan<- *prometheus.Desc) {
	for _, desc := range []*prometheus.Desc{m.slots, m.jobs, m.lastSync, m.backoff, m.unresolved, m.ready, m.notReady, m.bootstrapTimeout, m.bootstrapPending} {
		ch <- desc
	}
}

func (m *stateMetrics) Collect(ch chan<- prometheus.Metric) {
	snapshot := m.snapshot.Load()
	for _, metric := range snapshot.metrics {
		ch <- metric
	}
}

func (m *stateMetrics) observe(saved state.State, observedAt time.Time) {
	metrics := make([]prometheus.Metric, 0, len(saved.Pools)*8+6)
	for name, pool := range saved.Pools {
		powers := map[provider.PowerState]float64{provider.Running: 0, provider.Stopped: 0, provider.Unknown: 0}
		var unresolved, ready, notReady, timeouts, pending float64
		for _, slot := range pool.Slots {
			powers[slot.Power]++
			if slot.Operation != nil && slot.Operation.Kind == "unresolved" {
				unresolved++
			}
			if slot.Power == provider.Running {
				if slot.Ready {
					ready++
				} else {
					notReady++
				}
			}
			if !slot.Ready && slot.ResourceID != "" {
				if !slot.BootstrapDeadline.IsZero() && !observedAt.Before(slot.BootstrapDeadline) {
					timeouts++
				}
				if !slot.BootstrapStartedAt.IsZero() {
					pending = max(pending, observedAt.Sub(slot.BootstrapStartedAt).Seconds())
				}
			}
		}
		for power, count := range powers {
			metrics = append(metrics, prometheus.MustNewConstMetric(m.slots, prometheus.GaugeValue, count, name, string(power)))
		}
		metrics = append(metrics,
			prometheus.MustNewConstMetric(m.unresolved, prometheus.GaugeValue, unresolved, name),
			prometheus.MustNewConstMetric(m.ready, prometheus.GaugeValue, ready, name),
			prometheus.MustNewConstMetric(m.notReady, prometheus.GaugeValue, notReady, name),
			prometheus.MustNewConstMetric(m.bootstrapTimeout, prometheus.GaugeValue, timeouts, name),
			prometheus.MustNewConstMetric(m.bootstrapPending, prometheus.GaugeValue, pending, name),
		)
	}
	jobs := map[string]float64{"pending": 0, "preparing": 0, "running": 0, "canceling": 0}
	for _, job := range saved.Jobs {
		if state.Active(job.Status) {
			jobs[job.Status]++
		}
	}
	for status, count := range jobs {
		metrics = append(metrics, prometheus.MustNewConstMetric(m.jobs, prometheus.GaugeValue, count, status))
	}
	var lastSync, backoff float64
	if !saved.LastSync.IsZero() {
		lastSync = float64(saved.LastSync.Unix())
	}
	if !saved.BackoffUntil.IsZero() {
		backoff = float64(saved.BackoffUntil.Unix())
	}
	metrics = append(metrics,
		prometheus.MustNewConstMetric(m.lastSync, prometheus.GaugeValue, lastSync),
		prometheus.MustNewConstMetric(m.backoff, prometheus.GaugeValue, backoff),
	)
	m.snapshot.Store(&metricSnapshot{metrics: metrics})
}

func (c *Controller) MetricsHandler() http.Handler {
	return promhttp.HandlerFor(c.metrics.registry, promhttp.HandlerOpts{MaxRequestsInFlight: 4, Timeout: 5 * time.Second})
}
