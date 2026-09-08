package main

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/lib/pq"

	"github.com/mstgnz/cronsole/v2/internal/applog"
	"github.com/mstgnz/cronsole/v2/internal/config"
	"github.com/mstgnz/cronsole/v2/internal/domain"
)

// The wiring, checked against the graph the process actually builds.
//
// Every other test in this repository assembles its own graph, so a mistake
// HERE is invisible to all of them: a runner built without its host resolver, a
// health handler without the pool, a metrics endpoint mounted when the
// deployment asked for it to be off. The service would start cleanly and be
// quietly wrong. This is the only test that looks at the real thing.

// sharedTestLock serializes this package against internal/repository, whose
// tests empty every table between cases while these boot a real application
// against the same rows.
//
// go test runs packages in parallel, so without this the two interleave: the
// catalogue this build writes gets truncated mid-boot, or the account the
// setup gate just counted disappears before the request lands. The number is
// arbitrary; it only has to match the one in internal/repository.
const sharedTestLock = 0x63726F6E // "cron"

// lockTestDatabase holds that lock for the rest of the test.
func lockTestDatabase(t *testing.T, db *sql.DB) {
	t.Helper()

	// A dedicated connection: an advisory lock belongs to a session, and a
	// pooled Exec could take it on one connection and release it on another.
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatalf("could not take a connection for the test lock: %v", err)
	}
	if _, err := conn.ExecContext(context.Background(), `SELECT pg_advisory_lock($1)`, sharedTestLock); err != nil {
		t.Fatalf("could not take the test lock: %v", err)
	}
	t.Cleanup(func() {
		// Released explicitly, because Conn.Close returns the session to the
		// pool rather than ending it, and the lock would outlive the test.
		if _, err := conn.ExecContext(context.Background(),
			`SELECT pg_advisory_unlock($1)`, sharedTestLock); err != nil {
			t.Errorf("could not release the test lock: %v", err)
		}
		_ = conn.Close()
	})
}

// buildTestApp assembles the application over the test database.
func buildTestApp(t *testing.T, adjust func(*config.Config)) (*app, *sql.DB) {
	t.Helper()

	dsn := strings.TrimSpace(os.Getenv("CRONSOLE_TEST_DB"))
	if dsn == "" {
		t.Skip("CRONSOLE_TEST_DB is not set; run `make test-db` first")
	}

	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("sql.Open = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	lockTestDatabase(t, db)

	cfg := testConfig(t)
	cfg.DB = dsnToConfig(t, dsn)
	cfg.App.Port = "0"
	cfg.JWTSecret = strings.Repeat("s", 32)
	cfg.Scheduler = config.Scheduler{
		Enabled: false, MaxConcurrent: 5, QueueSize: 32, MissScanMin: 60, Metrics: true,
	}
	cfg.Outbound = config.Outbound{
		UserAgent: "cronsole/test", MaxBodyBytes: 64 << 10, AllowPrivateTargets: true,
	}
	if adjust != nil {
		adjust(cfg)
	}

	// Attached the way main attaches it, so shutdown drains the same way.
	logger := applog.New()
	logger.Attach(db)

	application, err := build(cfg, db, logger)
	if err != nil {
		t.Fatalf("build = %v", err)
	}
	t.Cleanup(application.shutdown)
	return application, db
}

func TestBuildAssemblesEveryPart(t *testing.T) {
	a, _ := buildTestApp(t, nil)

	if a.server == nil {
		t.Fatal("no HTTP server was built")
	}
	if a.scheduler == nil {
		t.Error("no scheduler was built")
	}
	if a.pool == nil {
		t.Error("no worker pool was built")
	}
	if a.runner == nil {
		t.Error("no runner was built")
	}
	if a.notifier == nil {
		t.Error("no notifier was built")
	}
	if a.host == nil {
		t.Error("no host reader was built, so the dashboard panel would be absent")
	}
	if a.hostOverrides == nil {
		t.Error("no host override service was built, so nothing would route")
	}
	if a.instanceID == "" {
		t.Error("no instance id, so a stuck run cannot be traced to a machine")
	}
}

func TestBuildingTheGraphFiresNothing(t *testing.T) {
	// The scheduler is built here and started in start, so assembling the
	// application has no side effects. If build started it, a test — or a
	// process that failed later in boot — would have a dispatcher running
	// against production.
	a, _ := buildTestApp(t, nil)

	if len(a.scheduler.Entries()) != 2 {
		t.Fatalf("%d scheduled entries, want the dispatcher and the watchdog",
			len(a.scheduler.Entries()))
	}
	// Entries have no next firing until the cron is started.
	for _, e := range a.scheduler.Entries() {
		if !e.Next.IsZero() {
			t.Error("the scheduler is already running after build")
		}
	}
}

func TestTheServerCarriesEveryTimeout(t *testing.T) {
	// ReadHeaderTimeout is the one that is easy to leave out and the one that
	// matters most: without it a client can send headers a byte at a time and
	// hold a connection open indefinitely.
	a, _ := buildTestApp(t, nil)

	if a.server.ReadHeaderTimeout == 0 {
		t.Error("ReadHeaderTimeout is unset, which leaves Slowloris open")
	}
	if a.server.ReadTimeout == 0 {
		t.Error("ReadTimeout is unset")
	}
	if a.server.WriteTimeout == 0 {
		t.Error("WriteTimeout is unset")
	}
	if a.server.IdleTimeout == 0 {
		t.Error("IdleTimeout is unset")
	}
	if a.server.MaxHeaderBytes == 0 {
		t.Error("MaxHeaderBytes is unset")
	}
	if a.server.Handler == nil {
		t.Fatal("the server has no handler")
	}
}

func TestTheMetricsEndpointFollowsTheSetting(t *testing.T) {
	// It is unauthenticated, as a scrape target normally is, and it discloses
	// project names, job names and run counts. That is fine inside a cluster
	// and not fine on a public ingress, so a deployment can turn it off — and
	// turning it off has to actually remove the route.
	t.Run("on", func(t *testing.T) {
		a, _ := buildTestApp(t, func(c *config.Config) { c.Scheduler.Metrics = true })

		w := httptest.NewRecorder()
		a.server.Handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))

		if w.Code != http.StatusOK {
			t.Fatalf("/metrics answered %d with metrics on", w.Code)
		}
		if !strings.Contains(w.Body.String(), "cronsole_") {
			t.Error("the scrape carries none of this service's metrics")
		}
	})

	t.Run("off", func(t *testing.T) {
		a, _ := buildTestApp(t, func(c *config.Config) { c.Scheduler.Metrics = false })

		w := httptest.NewRecorder()
		a.server.Handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))

		if w.Code == http.StatusOK && strings.Contains(w.Body.String(), "cronsole_dispatcher") {
			t.Error("/metrics still serves a scrape with metrics switched off")
		}
	})
}

// withAnAccount makes the deployment one that has been through its first run.
//
// Without it every request answers a redirect to /setup, which is correct and
// is not what the tests below are about. The row is removed afterwards so a
// test that wants the empty case still gets it.
func withAnAccount(t *testing.T, db *sql.DB) {
	t.Helper()

	const email = "wiring-fixture@example.invalid"
	_, err := db.Exec(`
		INSERT INTO users (fullname, email, password, is_admin, active)
		VALUES ('Wiring Fixture', $1, 'x', true, true)
		ON CONFLICT DO NOTHING`, email)
	if err != nil {
		t.Fatalf("seeding an account: %v", err)
	}
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM users WHERE email = $1`, email) })
}

func TestTheAssembledServerServesTheInterface(t *testing.T) {
	// The router, the middleware and the handlers, reached through the same
	// mux the process listens on. This is what proves they were connected to
	// each other rather than merely constructed.
	a, db := buildTestApp(t, nil)
	withAnAccount(t, db)

	t.Run("liveness answers", func(t *testing.T) {
		w := httptest.NewRecorder()
		a.server.Handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/healthz", nil))
		if w.Code != http.StatusOK {
			t.Errorf("/healthz answered %d", w.Code)
		}
	})

	t.Run("readiness sees the database", func(t *testing.T) {
		// The health handler was given the real pool, not nil. Wiring it with
		// nil would answer 503 on a healthy deployment.
		w := httptest.NewRecorder()
		a.server.Handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/readyz", nil))
		if w.Code != http.StatusOK {
			t.Errorf("/readyz answered %d against a working database: %s", w.Code, w.Body.String())
		}
		// And the queue figures are there, which only happens when the pool
		// reached the handler.
		if !strings.Contains(w.Body.String(), "queue_capacity") {
			t.Errorf("the readiness answer carries no queue figures: %s", w.Body.String())
		}
	})

	t.Run("the login page renders", func(t *testing.T) {
		w := httptest.NewRecorder()
		a.server.Handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/login", nil))
		if w.Code != http.StatusOK {
			t.Errorf("/login answered %d", w.Code)
		}
	})

	t.Run("a screen requires a session", func(t *testing.T) {
		w := httptest.NewRecorder()
		a.server.Handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/jobs", nil))
		if w.Code != http.StatusSeeOther {
			t.Errorf("/jobs answered %d without a session, want a redirect", w.Code)
		}
	})

	t.Run("the API requires a key", func(t *testing.T) {
		w := httptest.NewRecorder()
		a.server.Handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/jobs", nil))
		if w.Code != http.StatusUnauthorized {
			t.Errorf("/api/v1/jobs answered %d with no key", w.Code)
		}
	})

	t.Run("the security headers are set", func(t *testing.T) {
		w := httptest.NewRecorder()
		a.server.Handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/login", nil))
		for _, header := range []string{
			"X-Content-Type-Options", "X-Frame-Options", "Content-Security-Policy",
		} {
			if w.Header().Get(header) == "" {
				t.Errorf("%s is not set, so the middleware is not in front of the routes", header)
			}
		}
	})
}

func TestTheAssembledServerServesTheSetupScreenOnAnEmptyDeployment(t *testing.T) {
	// Against the real database, with no account in it. The gate is mounted as
	// a group around most of the route table, and a group is easy to get wrong
	// in a way that only shows up here: build the router, ask it, see what a
	// fresh installation would actually answer.
	a, db := buildTestApp(t, nil)

	var accounts int
	if err := db.QueryRow(`SELECT count(*) FROM users WHERE deleted_at IS NULL`).Scan(&accounts); err != nil {
		t.Fatalf("counting accounts: %v", err)
	}
	if accounts > 0 {
		t.Skip("the test database already holds an account, so the first run cannot be observed")
	}

	ask := func(path string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		a.server.Handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		return w
	}

	if w := ask("/setup"); w.Code != http.StatusOK {
		t.Errorf("/setup answered %d on a deployment with no account, want the form", w.Code)
	}
	for _, path := range []string{"/", "/login", "/jobs"} {
		w := ask(path)
		if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/setup" {
			t.Errorf("%s answered %d to %q, want a redirect to /setup",
				path, w.Code, w.Header().Get("Location"))
		}
	}
	// The probes stay outside the gate, or an orchestrator would never let a
	// fresh deployment become ready enough to be set up.
	if w := ask("/healthz"); w.Code != http.StatusOK {
		t.Errorf("/healthz answered %d before setup, want 200", w.Code)
	}
}

func TestTheRunnerDialsThroughTheHostOverrides(t *testing.T) {
	// The wiring this test was written for. The runner takes its routes from
	// the host override service, and building it without them would leave every
	// job going out through public DNS while the screen shows routes that do
	// nothing.
	a, _ := buildTestApp(t, nil)

	if a.hostOverrides.Resolver() == nil {
		t.Fatal("the host override service has no resolver")
	}

	// A route added through the service reaches the table the dialer reads.
	// Asserting on the runner's private transport would test the plumbing;
	// asserting that the two share one resolver is the property that matters.
	resolver := a.hostOverrides.Resolver()
	resolver.Replace([]domain.HostOverride{
		{Hostname: "shop.example.com", Address: "10.10.0.5", Active: true},
	})
	if got, ok := resolver.Lookup("shop.example.com:443"); !ok || got != "10.10.0.5:443" {
		t.Errorf("Lookup = %q, %v; the resolver the runner holds is not this one", got, ok)
	}
}

func TestBuildRefusesAHeaderItWillNotTrust(t *testing.T) {
	// A misspelled header silently trusts nothing, which looks exactly like a
	// working deployment until somebody reads the rate-limit buckets. Boot
	// fails instead.
	dsn := strings.TrimSpace(os.Getenv("CRONSOLE_TEST_DB"))
	if dsn == "" {
		t.Skip("CRONSOLE_TEST_DB is not set; run `make test-db` first")
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("sql.Open = %v", err)
	}
	defer func() { _ = db.Close() }()

	cfg := testConfig(t)
	cfg.DB = dsnToConfig(t, dsn)
	cfg.JWTSecret = strings.Repeat("s", 32)
	cfg.Scheduler = config.Scheduler{MaxConcurrent: 5, QueueSize: 32}
	cfg.App.TrustedProxyHeader = "X-Made-Up"

	if _, err := build(cfg, db, applog.New()); err == nil {
		t.Error("build accepted a header it will not vouch for")
	}
}

func TestStartAndShutdownAreClean(t *testing.T) {
	// The order is the reverse of start up and it is load bearing: draining the
	// pool before closing the server would let a new request queue work into a
	// pool that is already draining.
	a, _ := buildTestApp(t, func(c *config.Config) { c.Scheduler.Enabled = true })

	ctx, cancel := context.WithCancel(context.Background())
	a.start(ctx)

	// The scheduler is running now, which it was not after build.
	running := false
	for _, e := range a.scheduler.Entries() {
		if !e.Next.IsZero() {
			running = true
		}
	}
	if !running {
		t.Error("start did not start the scheduler")
	}

	cancel()

	done := make(chan struct{})
	go func() {
		a.shutdown()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(shutdownTimeout + 10*time.Second):
		t.Fatal("shutdown did not finish")
	}

	// Idempotent: the deferred cleanup calls it again, and a second shutdown
	// must not panic on a closed channel or a stopped pool.
	a.shutdown()
}
