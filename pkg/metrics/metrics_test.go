package metrics

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// scrape returns the exposition text, which is what Prometheus actually reads
// and therefore the only honest thing to assert against.
func scrape(t *testing.T, r *Recorder) string {
	t.Helper()

	w := httptest.NewRecorder()
	r.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("the metrics endpoint answered %d", w.Code)
	}
	return w.Body.String()
}

// sample reads one series out of a scrape.
func sample(t *testing.T, body, name string) (float64, bool) {
	t.Helper()

	// name{labels} value, or name value.
	pattern := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(name) + `(?:\{[^}]*\})? ([0-9.e+-]+)$`)
	m := pattern.FindStringSubmatch(body)
	if m == nil {
		return 0, false
	}
	v, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		t.Fatalf("%s = %q, which is not a number", name, m[1])
	}
	return v, true
}

func mustSample(t *testing.T, body, name string) float64 {
	t.Helper()
	v, ok := sample(t, body, name)
	if !ok {
		t.Fatalf("%s is not in the scrape", name)
	}
	return v
}

func TestTwoRecordersCanExistAtOnce(t *testing.T) {
	// Its own registry rather than the default one. A global registry makes a
	// second instance panic about duplicate registration, which turns an
	// ordinary test into a crash.
	a := New(nil, nil)
	b := New(nil, nil)

	a.TickFinished(TickObservation{At: 1})
	b.TickFinished(TickObservation{At: 2})

	if got := mustSample(t, scrape(t, a), "cronsole_dispatcher_last_tick_timestamp_seconds"); got != 1 {
		t.Errorf("the first recorder saw %v, want 1; the two share state", got)
	}
	if got := mustSample(t, scrape(t, b), "cronsole_dispatcher_last_tick_timestamp_seconds"); got != 2 {
		t.Errorf("the second recorder saw %v, want 2", got)
	}
}

func TestEveryDeclaredMetricIsPresentInAScrape(t *testing.T) {
	// The package's own claim is that every metric is written from a real call
	// site. This is the half that can be checked here: the series exist and the
	// dashboards that name them are not pointing at nothing.
	r := New(func() float64 { return 3 }, func() float64 { return 512 })

	r.RunFinished("shop", "daily-report", "success", 1.5, 1000)
	r.TickFinished(TickObservation{
		At: 1000, DurationSeconds: 0.2, DriftSeconds: 1,
		MinutesLost: 1, Queued: 2, Dispatched: 2, Skipped: 1,
		QuotaFull: true, ActiveJobs: 9,
	})

	body := scrape(t, r)
	for _, name := range []string{
		"cronsole_runs_total",
		"cronsole_run_duration_seconds_count",
		"cronsole_last_run_timestamp_seconds",
		"cronsole_last_success_timestamp_seconds",
		"cronsole_dispatcher_ticks_total",
		"cronsole_dispatcher_tick_failures_total",
		"cronsole_dispatcher_tick_duration_seconds_count",
		"cronsole_dispatcher_last_tick_timestamp_seconds",
		"cronsole_clock_drift_seconds",
		"cronsole_dispatcher_minutes_lost_total",
		"cronsole_runs_queued_total",
		"cronsole_runs_dispatched_total",
		"cronsole_runs_skipped_total",
		"cronsole_quota_exceeded_total",
		"cronsole_active_jobs",
		"cronsole_queue_depth",
		"cronsole_queue_capacity",
	} {
		if _, ok := sample(t, body, name); !ok {
			t.Errorf("%s is not in the scrape", name)
		}
	}
}

func TestQueueGaugesAreReadAtScrapeTime(t *testing.T) {
	// So the pool is reported as it is, rather than as it was when something
	// last remembered to record it.
	depth := 0.0
	r := New(func() float64 { return depth }, func() float64 { return 512 })

	if got := mustSample(t, scrape(t, r), "cronsole_queue_depth"); got != 0 {
		t.Errorf("depth = %v, want 0", got)
	}
	depth = 41
	if got := mustSample(t, scrape(t, r), "cronsole_queue_depth"); got != 41 {
		t.Errorf("depth = %v, want 41; the gauge was not re-read", got)
	}
}

func TestTheQueueGaugesAreOptional(t *testing.T) {
	// The composition root builds the recorder before the pool exists. Passing
	// nil must not register a gauge that would panic when scraped.
	body := scrape(t, New(nil, nil))

	if _, ok := sample(t, body, "cronsole_queue_depth"); ok {
		t.Error("a queue gauge was registered with no source")
	}
	if _, ok := sample(t, body, "cronsole_queue_capacity"); ok {
		t.Error("a queue capacity gauge was registered with no source")
	}
}

func TestRunFinishedCountsByOutcome(t *testing.T) {
	r := New(nil, nil)

	r.RunFinished("shop", "daily", "success", 1, 100)
	r.RunFinished("shop", "daily", "success", 2, 200)
	r.RunFinished("shop", "daily", "failed", 3, 300)

	body := scrape(t, r)
	if got := seriesValue(t, body, `cronsole_runs_total{job="daily",project="shop",status="success"}`); got != 2 {
		t.Errorf("successes = %v, want 2", got)
	}
	if got := seriesValue(t, body, `cronsole_runs_total{job="daily",project="shop",status="failed"}`); got != 1 {
		t.Errorf("failures = %v, want 1", got)
	}
	if got := seriesValue(t, body, `cronsole_run_duration_seconds_count{job="daily",project="shop"}`); got != 3 {
		t.Errorf("duration observations = %v, want 3", got)
	}
}

func TestOnlyASuccessMovesTheLastSuccessTimestamp(t *testing.T) {
	// The pair is what makes "this job has not succeeded since 03:00"
	// expressible. If a failure moved both, the alert could never fire.
	r := New(nil, nil)

	r.RunFinished("shop", "daily", "success", 1, 1000)
	r.RunFinished("shop", "daily", "failed", 1, 2000)

	body := scrape(t, r)
	if got := seriesValue(t, body, `cronsole_last_run_timestamp_seconds{job="daily",project="shop"}`); got != 2000 {
		t.Errorf("last run = %v, want the failure's timestamp", got)
	}
	if got := seriesValue(t, body, `cronsole_last_success_timestamp_seconds{job="daily",project="shop"}`); got != 1000 {
		t.Errorf("last success = %v, want the success's timestamp; a failure moved it", got)
	}

	// And every other non-success outcome behaves the same way.
	for _, status := range []string{"timeout", "skipped", "running", "pending"} {
		r.RunFinished("shop", "daily", status, 1, 9000)
	}
	body = scrape(t, r)
	if got := seriesValue(t, body, `cronsole_last_success_timestamp_seconds{job="daily",project="shop"}`); got != 1000 {
		t.Errorf("last success = %v after non-success outcomes, want 1000", got)
	}
}

func TestAFailedTickIsCountedAsAFailureAndNothingElse(t *testing.T) {
	// A tick that errored has no duration worth recording and no counts to
	// trust, and moving the last-tick timestamp would hide a dispatcher that is
	// failing every minute behind a fresh pulse.
	r := New(nil, nil)

	r.TickFinished(TickObservation{At: 1000, DurationSeconds: 0.2, ActiveJobs: 5})
	r.TickFinished(TickObservation{At: 2000, DurationSeconds: 0.3, ActiveJobs: 5, Failed: true})

	body := scrape(t, r)
	if got := mustSample(t, body, "cronsole_dispatcher_tick_failures_total"); got != 1 {
		t.Errorf("failures = %v, want 1", got)
	}
	if got := mustSample(t, body, "cronsole_dispatcher_ticks_total"); got != 1 {
		t.Errorf("successful ticks = %v, want 1", got)
	}
	if got := mustSample(t, body, "cronsole_dispatcher_last_tick_timestamp_seconds"); got != 1000 {
		t.Errorf("last tick = %v; a failed tick refreshed the pulse", got)
	}
	if got := mustSample(t, body, "cronsole_dispatcher_tick_duration_seconds_count"); got != 1 {
		t.Errorf("duration observations = %v, want 1", got)
	}
}

func TestCountersOnlyMoveWhenThereIsSomethingToCount(t *testing.T) {
	// A tick with nothing to do is the common case. Adding zero is harmless for
	// a counter and the guards exist anyway; what matters is that a quiet tick
	// does not increment quota_exceeded, which is an alerting signal.
	r := New(nil, nil)

	r.TickFinished(TickObservation{At: 1000})
	body := scrape(t, r)

	for _, name := range []string{
		"cronsole_dispatcher_minutes_lost_total",
		"cronsole_runs_queued_total",
		"cronsole_runs_dispatched_total",
		"cronsole_runs_skipped_total",
		"cronsole_quota_exceeded_total",
	} {
		if got := mustSample(t, body, name); got != 0 {
			t.Errorf("%s = %v after an idle tick, want 0", name, got)
		}
	}
}

func TestTickCountersAccumulate(t *testing.T) {
	r := New(nil, nil)

	r.TickFinished(TickObservation{At: 1, MinutesLost: 2, Queued: 5, Dispatched: 4, Skipped: 1, QuotaFull: true})
	r.TickFinished(TickObservation{At: 2, MinutesLost: 1, Queued: 3, Dispatched: 3, Skipped: 2, QuotaFull: true})

	body := scrape(t, r)
	want := map[string]float64{
		"cronsole_dispatcher_minutes_lost_total": 3,
		"cronsole_runs_queued_total":             8,
		"cronsole_runs_dispatched_total":         7,
		"cronsole_runs_skipped_total":            3,
		"cronsole_quota_exceeded_total":          2,
	}
	for name, expected := range want {
		if got := mustSample(t, body, name); got != expected {
			t.Errorf("%s = %v, want %v", name, got, expected)
		}
	}
}

func TestActiveJobsAndDriftAreTheLastValueNotASum(t *testing.T) {
	// Both are gauges: a sum would climb forever and mean nothing.
	r := New(nil, nil)

	r.TickFinished(TickObservation{At: 1, ActiveJobs: 10, DriftSeconds: 5})
	r.TickFinished(TickObservation{At: 2, ActiveJobs: 7, DriftSeconds: -2})

	body := scrape(t, r)
	if got := mustSample(t, body, "cronsole_active_jobs"); got != 7 {
		t.Errorf("active jobs = %v, want 7", got)
	}
	if got := mustSample(t, body, "cronsole_clock_drift_seconds"); got != -2 {
		t.Errorf("drift = %v, want -2; a negative drift has to survive", got)
	}
}

func TestRuntimeCollectorsArePresent(t *testing.T) {
	// A run duration means little without knowing whether the process was
	// starved, so these are what make the rest readable.
	body := scrape(t, New(nil, nil))
	for _, name := range []string{"go_goroutines", "go_memstats_alloc_bytes"} {
		if !strings.Contains(body, name) {
			t.Errorf("%s is not in the scrape", name)
		}
	}
}

func TestDurationBucketsReachBeyondTenSeconds(t *testing.T) {
	// The library default stops at 10s, and a scheduled job that takes a minute
	// is entirely ordinary. Without the upper buckets every slow job lands in
	// +Inf and the percentiles say nothing.
	r := New(nil, nil)
	r.RunFinished("shop", "slow", "success", 200, 1)

	body := scrape(t, r)
	for _, le := range []string{"30", "60", "120", "300", "600"} {
		if !strings.Contains(body, `le="`+le+`"`) {
			t.Errorf("there is no bucket at %s seconds", le)
		}
	}
	// A 200 second run belongs in the 300 second bucket and not in the 120 one.
	if got := seriesValue(t, body, `cronsole_run_duration_seconds_bucket{job="slow",project="shop",le="300"}`); got != 1 {
		t.Errorf("a 200 second run did not land in the 300 second bucket: %v", got)
	}
	if got := seriesValue(t, body, `cronsole_run_duration_seconds_bucket{job="slow",project="shop",le="120"}`); got != 0 {
		t.Errorf("a 200 second run landed in the 120 second bucket: %v", got)
	}
}

// seriesValue reads one exact series line, labels included. The exposition
// format writes the declared labels in order and puts `le` last, so the caller
// writes them that way.
func seriesValue(t *testing.T, body, series string) float64 {
	t.Helper()

	for _, line := range strings.Split(body, "\n") {
		name, value, found := strings.Cut(line, " ")
		if !found || name != series {
			continue
		}
		v, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
		if err != nil {
			t.Fatalf("%s = %q, which is not a number", series, value)
		}
		return v
	}
	t.Fatalf("%s is not in the scrape", series)
	return 0
}
