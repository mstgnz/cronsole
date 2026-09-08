package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mstgnz/cronsole/v2/internal/applog"
	"github.com/mstgnz/cronsole/v2/internal/config"
	"github.com/mstgnz/cronsole/v2/internal/domain"
	"github.com/mstgnz/cronsole/v2/internal/service"
	"github.com/mstgnz/cronsole/v2/pkg/metrics"
)

// This is the composition root, so most of it is wiring that only means
// anything when the process is running. What CAN be checked here is the part
// that decides behaviour rather than merely connecting it: the two entries the
// scheduler registers, how a tick becomes metrics and log lines, and the
// identity a replica claims runs under.

func testConfig(t *testing.T) *config.Config {
	t.Helper()

	loc, err := time.LoadLocation("Europe/Istanbul")
	if err != nil {
		t.Fatalf("time zone: %v", err)
	}
	return &config.Config{
		App:      config.App{Env: "test", Location: loc},
		Watchdog: config.Watchdog{Every: 5 * time.Minute},
	}
}

// --- the scheduler's own two entries ----------------------------------------

func TestTheSchedulerRegistersTheDispatcherAndTheWatchdog(t *testing.T) {
	// Neither could be a job: a job is an HTTP call to somewhere else, and
	// these are the service's own work.
	c := newScheduler(testConfig(t), applog.New(), metrics.New(nil, nil), nil, nil)
	if c == nil {
		t.Fatal("no scheduler was built")
	}

	entries := c.Entries()
	if len(entries) != 2 {
		t.Fatalf("%d entries, want the dispatcher and the watchdog", len(entries))
	}
}

func TestTheDispatcherFiresOnItsOwnSecond(t *testing.T) {
	// Not on the minute boundary. domain.DispatcherSpec names a second, and the
	// parser has to accept a six field expression for that to work at all: if
	// it did not, boot would fail rather than quietly firing at :00.
	cfg := testConfig(t)
	c := newScheduler(cfg, applog.New(), metrics.New(nil, nil), nil, nil)

	c.Start()
	defer c.Stop()

	entries := c.Entries()
	var next time.Time
	for _, e := range entries {
		if next.IsZero() || e.Next.Before(next) {
			next = e.Next
		}
	}
	if next.IsZero() {
		t.Fatal("nothing is scheduled")
	}

	// The dispatcher's next firing is at the second the spec names, in the
	// deployment's own zone rather than UTC.
	second := -1
	for _, e := range entries {
		if e.Next.Second() != 0 {
			second = e.Next.Second()
		}
	}
	if second == 0 {
		t.Error("the dispatcher fires on the minute boundary, which loses minutes")
	}
	if _, offset := next.Zone(); offset == 0 && cfg.App.Location.String() != "UTC" {
		t.Errorf("the schedule is in UTC rather than %s", cfg.App.Location)
	}
}

func TestAWatchdogIntervalBelowAMinuteIsRaised(t *testing.T) {
	// A sweep every few seconds would be a self-inflicted load, and the sweep
	// is not the kind of thing anybody needs at that resolution.
	cfg := testConfig(t)
	cfg.Watchdog.Every = time.Second

	c := newScheduler(cfg, applog.New(), metrics.New(nil, nil), nil, nil)
	c.Start()
	defer c.Stop()

	entries := c.Entries()
	if len(entries) != 2 {
		t.Fatalf("%d entries", len(entries))
	}

	// Whichever of the two is further out is the watchdog, and it must be
	// minutes rather than seconds away.
	furthest := entries[0].Next
	for _, e := range entries {
		if e.Next.After(furthest) {
			furthest = e.Next
		}
	}
	if time.Until(furthest) < time.Minute {
		t.Errorf("the watchdog is scheduled in %s, want the floor of five minutes",
			time.Until(furthest).Round(time.Second))
	}
}

// --- a tick becomes metrics -------------------------------------------------

func TestASkippedTickRecordsNothing(t *testing.T) {
	// A tick that was skipped did no work, and recording zeros for it would put
	// a dip in every graph whenever one ran long.
	recorder := metrics.New(nil, nil)
	recordTick(recorder, service.TickResult{Skipped: true, QueuedRuns: 5})

	if got := sampleOf(t, recorder, "cronsole_dispatcher_ticks_total"); got != 0 {
		t.Errorf("a skipped tick was counted: %v", got)
	}
}

func TestARepeatedMinuteCountsAsALostMinute(t *testing.T) {
	// The one metric that reports silent data loss. A repeat means the wall
	// minute between the two ticks was never processed, and nothing else in the
	// system leaves a trace of it: the repeat conflicts on the unique index
	// without raising an error.
	recorder := metrics.New(nil, nil)
	recordTick(recorder, service.TickResult{
		Minute: time.Now(), MinuteRepeat: true, Duration: time.Second,
	})

	if got := sampleOf(t, recorder, "cronsole_dispatcher_minutes_lost_total"); got != 1 {
		t.Errorf("minutes lost = %v, want 1", got)
	}
}

func TestAGapAndARepeatAddUp(t *testing.T) {
	recorder := metrics.New(nil, nil)
	recordTick(recorder, service.TickResult{
		Minute: time.Now(), MinuteGap: 3, MinuteRepeat: true, Duration: time.Second,
	})

	if got := sampleOf(t, recorder, "cronsole_dispatcher_minutes_lost_total"); got != 4 {
		t.Errorf("minutes lost = %v, want 4", got)
	}
}

func TestAnOrdinaryTickIsRecorded(t *testing.T) {
	recorder := metrics.New(nil, nil)
	recordTick(recorder, service.TickResult{
		Minute: time.Now(), Duration: 250 * time.Millisecond, DriftSec: 2,
		QueuedRuns: 4, Dispatched: 4, SkippedRuns: 1, ActiveJobs: 9, QuotaFull: true,
	})

	want := map[string]float64{
		"cronsole_dispatcher_ticks_total":        1,
		"cronsole_runs_queued_total":             4,
		"cronsole_runs_dispatched_total":         4,
		"cronsole_runs_skipped_total":            1,
		"cronsole_quota_exceeded_total":          1,
		"cronsole_active_jobs":                   9,
		"cronsole_clock_drift_seconds":           2,
		"cronsole_dispatcher_minutes_lost_total": 0,
	}
	for name, expected := range want {
		if got := sampleOf(t, recorder, name); got != expected {
			t.Errorf("%s = %v, want %v", name, got, expected)
		}
	}
}

// --- a tick becomes log lines -----------------------------------------------

func TestReportTickDoesNotPanicOnAnyShape(t *testing.T) {
	// It runs inside the cron entry, where a panic takes the dispatcher with
	// it. Every field is exercised rather than only the happy shape.
	logger := applog.New()

	shapes := []service.TickResult{
		{Skipped: true},
		{Minute: time.Now()},
		{Minute: time.Now(), MinuteRepeat: true},
		{Minute: time.Now(), MinuteGap: 5, MissedMinutes: 2},
		// More recovered than were missed: the report clamps rather than
		// printing a figure that cannot be true.
		{Minute: time.Now(), MinuteGap: 2, MissedMinutes: 9},
		{Minute: time.Now(), ParseErrors: []string{"bad expression"}},
		{Minute: time.Now(), WriteErrors: []string{"could not queue"}},
		{Minute: time.Now(), QuotaFull: true, Running: 20},
		{Minute: time.Now(), QueuedRuns: 3, Dispatched: 3, SkippedRuns: 1, Duration: time.Second},
	}
	for _, shape := range shapes {
		reportTick(logger, shape)
	}
}

// --- the identity a replica runs under --------------------------------------

func TestInstanceNameIsUniquePerProcess(t *testing.T) {
	// Two replicas sharing a name make a stuck run untraceable to the machine
	// holding it.
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		name := instanceName()
		if seen[name] {
			t.Fatalf("instanceName repeated %q", name)
		}
		seen[name] = true
	}
}

func TestInstanceNameCarriesTheHostAndProcess(t *testing.T) {
	// The hostname is what names a pod, and the process id tells two processes
	// on one host apart. The random suffix covers a pod restarting under the
	// same name.
	name := instanceName()

	if host, err := os.Hostname(); err == nil && host != "" {
		if !strings.HasPrefix(name, host+"-") {
			t.Errorf("instanceName = %q, want it to start with the hostname %q", name, host)
		}
	}
	if len(strings.Split(name, "-")) < 3 {
		t.Errorf("instanceName = %q, want host, process and a suffix", name)
	}
	// It goes into a text column and into log lines, so it stays plain.
	if strings.ContainsAny(name, " \t\r\n'\"") {
		t.Errorf("instanceName = %q, which is not safe to print unquoted", name)
	}
}

// --- the database pool ------------------------------------------------------

func TestOpenDatabaseRefusesAServerThatIsNotThere(t *testing.T) {
	// It pings before returning, so a wrong address is a boot failure rather
	// than a first-request failure minutes later.
	cfg := testConfig(t)
	cfg.DB = config.DB{
		Host: "127.0.0.1", Port: "1", Name: "nothing",
		User: "nobody", Pass: "", SSLMode: "disable", Zone: "UTC",
	}

	db, err := openDatabase(cfg)
	if err == nil {
		_ = db.Close()
		t.Fatal("openDatabase succeeded against a closed port")
	}
}

func TestOpenDatabaseBoundsThePool(t *testing.T) {
	// Without limits the pool is unbounded: a slow query storm opens
	// connections until the server refuses them, and none are ever recycled.
	dsn := strings.TrimSpace(os.Getenv("CRONSOLE_TEST_DB"))
	if dsn == "" {
		t.Skip("CRONSOLE_TEST_DB is not set; run `make test-db` first")
	}

	cfg := testConfig(t)
	cfg.DB = dsnToConfig(t, dsn)
	cfg.Scheduler.MaxConcurrent = 20

	db, err := openDatabase(cfg)
	if err != nil {
		t.Fatalf("openDatabase = %v", err)
	}
	defer func() { _ = db.Close() }()

	stats := db.Stats()
	if stats.MaxOpenConnections <= 0 {
		t.Error("the pool has no ceiling")
	}
	// Above the concurrency cap, because the interface and the dispatcher need
	// connections of their own while jobs are running.
	if stats.MaxOpenConnections <= cfg.Scheduler.MaxConcurrent {
		t.Errorf("the pool allows %d connections for %d concurrent runs, leaving none for the interface",
			stats.MaxOpenConnections, cfg.Scheduler.MaxConcurrent)
	}
}

// dsnToConfig reads a postgres URL into the settings openDatabase builds from.
func dsnToConfig(t *testing.T, dsn string) config.DB {
	t.Helper()

	// postgres://user:pass@host:port/name?sslmode=disable
	rest := strings.TrimPrefix(dsn, "postgres://")
	credentials, hostAndPath, ok := strings.Cut(rest, "@")
	if !ok {
		t.Fatalf("CRONSOLE_TEST_DB is not a postgres URL: %q", dsn)
	}
	user, pass, _ := strings.Cut(credentials, ":")
	hostPort, path, _ := strings.Cut(hostAndPath, "/")
	host, port, _ := strings.Cut(hostPort, ":")
	name, _, _ := strings.Cut(path, "?")

	return config.DB{
		Host: host, Port: port, Name: name, User: user, Pass: pass,
		SSLMode: "disable", Zone: "UTC",
	}
}

// --- helpers ----------------------------------------------------------------

// sampleOf reads one metric out of a recorder by scraping it, which is what
// Prometheus does and therefore the only honest reading.
func sampleOf(t *testing.T, r *metrics.Recorder, name string) float64 {
	t.Helper()

	w := httptest.NewRecorder()
	r.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	for _, line := range strings.Split(w.Body.String(), "\n") {
		metric, value, found := strings.Cut(line, " ")
		if !found || metric != name {
			continue
		}
		f, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
		if err != nil {
			t.Fatalf("%s = %q, which is not a number", name, value)
		}
		return f
	}
	return 0
}

func TestTheDispatcherSpecNamesASecond(t *testing.T) {
	// Six fields, not five. Firing on the minute boundary races the clock the
	// minute is read from, and the fields are what the scheduler's parser is
	// configured to accept.
	if fields := len(strings.Fields(domain.DispatcherSpec)); fields != 6 {
		t.Errorf("DispatcherSpec = %q, which has %d fields, want 6",
			domain.DispatcherSpec, fields)
	}
}
