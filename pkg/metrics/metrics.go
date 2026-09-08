// Package metrics exposes what this scheduler can actually measure.
//
// It replaces a set of fifteen collectors that nothing ever incremented and no
// endpoint ever served, including per job memory and CPU gauges this process
// has no way to observe and a leader gauge for an election that does not
// exist. Metrics that are always zero are worse than absent: somebody builds
// an alert on one and it never fires.
//
// Every metric below is written from a real call site. If a metric is here,
// something sets it.
package metrics

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Recorder holds the collectors and the registry they live in.
//
// Its own registry rather than the default one: a global registry makes two
// instances impossible, which turns an ordinary test into a panic about
// duplicate registration.
type Recorder struct {
	registry *prometheus.Registry

	runsTotal   *prometheus.CounterVec
	runDuration *prometheus.HistogramVec
	lastRun     *prometheus.GaugeVec
	lastSuccess *prometheus.GaugeVec

	ticksTotal    prometheus.Counter
	tickFailures  prometheus.Counter
	tickDuration  prometheus.Histogram
	lastTick      prometheus.Gauge
	clockDrift    prometheus.Gauge
	minutesLost   prometheus.Counter
	queuedRuns    prometheus.Counter
	dispatched    prometheus.Counter
	skippedRuns   prometheus.Counter
	quotaExceeded prometheus.Counter
	activeJobs    prometheus.Gauge
}

// New builds a recorder. queueDepth and queueCapacity are read at scrape time,
// so the pool is observed as it is rather than as it was when something last
// remembered to record it.
func New(queueDepth, queueCapacity func() float64) *Recorder {
	registry := prometheus.NewRegistry()

	// The process and Go runtime collectors are what make the rest readable:
	// a run duration means little without knowing the process was starved.
	registry.MustRegister(
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		collectors.NewGoCollector(),
	)

	// project and job, not an id: a dashboard shows names, and an id becomes
	// meaningless the moment the job is deleted. Cardinality is bounded by the
	// number of jobs, which is dozens to low hundreds.
	jobLabels := []string{"project", "job"}

	r := &Recorder{
		registry: registry,

		runsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "cronsole_runs_total",
			Help: "Executions by outcome.",
		}, append(append([]string{}, jobLabels...), "status")),

		runDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "cronsole_run_duration_seconds",
			Help: "How long an execution took, from request to response.",
			// The default buckets stop at 10 seconds, and a scheduled job that
			// takes a minute is entirely ordinary. Without the upper buckets
			// every slow job lands in +Inf and the percentiles say nothing.
			Buckets: []float64{.1, .25, .5, 1, 2.5, 5, 10, 30, 60, 120, 300, 600},
		}, jobLabels),

		lastRun: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "cronsole_last_run_timestamp_seconds",
			Help: "When a job last finished, whatever the outcome.",
		}, jobLabels),

		// The pair above and below is what makes "this job has not succeeded
		// since 03:00" expressible. One timestamp cannot say it.
		lastSuccess: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "cronsole_last_success_timestamp_seconds",
			Help: "When a job last succeeded.",
		}, jobLabels),

		ticksTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "cronsole_dispatcher_ticks_total",
			Help: "Minute ticks completed.",
		}),
		tickFailures: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "cronsole_dispatcher_tick_failures_total",
			Help: "Minute ticks that ended in an error.",
		}),
		tickDuration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "cronsole_dispatcher_tick_duration_seconds",
			Help:    "How long a minute tick took. Approaching 60 means ticks are about to overlap.",
			Buckets: []float64{.01, .05, .1, .25, .5, 1, 2.5, 5, 15, 30, 60},
		}),
		lastTick: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "cronsole_dispatcher_last_tick_timestamp_seconds",
			Help: "When the dispatcher last ran. Stale means nothing is being queued.",
		}),
		clockDrift: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "cronsole_clock_drift_seconds",
			Help: "Application clock minus database clock.",
		}),

		// The one metric that reports silent data loss. A repeated or skipped
		// minute leaves no other trace: a repeat conflicts on the unique index
		// without raising an error, and a gap writes nothing at all.
		minutesLost: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "cronsole_dispatcher_minutes_lost_total",
			Help: "Wall minutes that were never processed. Any increase is a fault.",
		}),

		queuedRuns: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "cronsole_runs_queued_total",
			Help: "Runs opened by the dispatcher.",
		}),
		dispatched: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "cronsole_runs_dispatched_total",
			Help: "Runs handed to the worker pool.",
		}),
		skippedRuns: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "cronsole_runs_skipped_total",
			Help: "Runs skipped because the previous one was still going.",
		}),
		quotaExceeded: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "cronsole_quota_exceeded_total",
			Help: "Ticks where the concurrency cap was already full.",
		}),
		activeJobs: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "cronsole_active_jobs",
			Help: "Jobs the dispatcher considers, as of the last tick.",
		}),
	}

	registry.MustRegister(
		r.runsTotal, r.runDuration, r.lastRun, r.lastSuccess,
		r.ticksTotal, r.tickFailures, r.tickDuration, r.lastTick, r.clockDrift,
		r.minutesLost, r.queuedRuns, r.dispatched, r.skippedRuns,
		r.quotaExceeded, r.activeJobs,
	)

	if queueDepth != nil {
		registry.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: "cronsole_queue_depth",
			Help: "Runs waiting for a worker.",
		}, queueDepth))
	}
	if queueCapacity != nil {
		registry.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: "cronsole_queue_capacity",
			Help: "How many runs the queue can hold before work is refused.",
		}, queueCapacity))
	}

	return r
}

// Handler serves the metrics endpoint.
func (r *Recorder) Handler() http.Handler {
	return promhttp.HandlerFor(r.registry, promhttp.HandlerOpts{})
}

// RunFinished records one execution. It satisfies service.RunObserver.
func (r *Recorder) RunFinished(project, job, status string, durationSeconds float64, at float64) {
	r.runsTotal.WithLabelValues(project, job, status).Inc()
	r.runDuration.WithLabelValues(project, job).Observe(durationSeconds)
	r.lastRun.WithLabelValues(project, job).Set(at)
	if status == "success" {
		r.lastSuccess.WithLabelValues(project, job).Set(at)
	}
}

// TickObservation is what one dispatcher tick reports.
type TickObservation struct {
	At              float64
	DurationSeconds float64
	DriftSeconds    float64
	MinutesLost     int
	Queued          int
	Dispatched      int
	Skipped         int
	QuotaFull       bool
	ActiveJobs      int
	Failed          bool
}

// TickFinished records one dispatcher tick.
func (r *Recorder) TickFinished(o TickObservation) {
	if o.Failed {
		r.tickFailures.Inc()
		return
	}
	r.ticksTotal.Inc()
	r.tickDuration.Observe(o.DurationSeconds)
	r.lastTick.Set(o.At)
	r.clockDrift.Set(o.DriftSeconds)
	r.activeJobs.Set(float64(o.ActiveJobs))

	if o.MinutesLost > 0 {
		r.minutesLost.Add(float64(o.MinutesLost))
	}
	if o.Queued > 0 {
		r.queuedRuns.Add(float64(o.Queued))
	}
	if o.Dispatched > 0 {
		r.dispatched.Add(float64(o.Dispatched))
	}
	if o.Skipped > 0 {
		r.skippedRuns.Add(float64(o.Skipped))
	}
	if o.QuotaFull {
		r.quotaExceeded.Inc()
	}
}
