package repository

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/lib/pq"

	"github.com/mstgnz/cronsole/v2/internal/domain"
)

// These tests run against a REAL Postgres, which is the only thing that can
// answer the questions this package raises. A mocked driver would prove the Go
// compiles; it would not prove that a scope narrows a WHERE clause, that a
// unique index refuses a second row, or that two processes claiming the same
// run leave exactly one winner. Those are properties of the database, and the
// database is what has to be asked.
//
//	make test-repo
//
// which starts a throwaway Postgres on its own port and applies cronsole.sql.
// Without CRONSOLE_TEST_DB every test here skips, so `go test ./...` on a
// machine with no Docker still passes.

// testDB is the pool, opened once and shared. Opening one per test would run
// the connection setup a hundred times for no benefit.
var (
	testDB   *sql.DB
	testOnce sync.Once
	testSkip string
)

// connect returns the shared pool, or skips the test.
func connect(t *testing.T) *sql.DB {
	t.Helper()

	testOnce.Do(func() {
		dsn := strings.TrimSpace(os.Getenv("CRONSOLE_TEST_DB"))
		if dsn == "" {
			testSkip = "CRONSOLE_TEST_DB is not set; run `make test-repo`"
			return
		}
		db, err := sql.Open("postgres", dsn)
		if err != nil {
			testSkip = "the test database could not be opened: " + err.Error()
			return
		}
		// Enough for the concurrency tests to actually be concurrent. With a
		// pool of one, two goroutines claiming the same run would serialise and
		// the race the test is written for could not happen.
		db.SetMaxOpenConns(20)

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := db.PingContext(ctx); err != nil {
			testSkip = "the test database is not answering: " + err.Error()
			return
		}
		testDB = db
	})

	if testSkip != "" {
		t.Skip(testSkip)
	}
	return testDB
}

// sharedTestLock serializes this package against cmd/cronsole, which boots a
// real application against the same database while these tests empty it.
//
// go test runs packages in parallel, so without this the two interleave and
// whichever loses sees a database that changed underneath it: a catalogue
// truncated mid-boot, or an account that vanished between the gate reading it
// and the request arriving. The number is arbitrary; it only has to match the
// one in cmd/cronsole/build_test.go.
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

// fresh empties every table and returns a store over the pool.
//
// TRUNCATE rather than DELETE, and RESTART IDENTITY, so each test sees the same
// ids and a failure reads the same way twice. CASCADE because the tables are
// joined by real foreign keys, which is the point.
func fresh(t *testing.T) *Store {
	t.Helper()

	db := connect(t)
	lockTestDatabase(t, db)
	_, err := db.Exec(`
		TRUNCATE
			job_runs, job_links, job_schedules, job_headers, jobs,
			notify_emails, notifications, projects,
			grantz_user_permissions, grantz_user_roles, grantz_role_permissions,
			grantz_roles, grantz_permissions,
			host_overrides, app_logs, password_resets, users
		RESTART IDENTITY CASCADE`)
	if err != nil {
		t.Fatalf("could not empty the database: %v", err)
	}
	// The heartbeat is one row with a fixed id, so it is reset rather than
	// truncated.
	if _, err := db.Exec(
		`INSERT INTO heartbeat (id, last_run) VALUES (1, now())
		 ON CONFLICT (id) DO UPDATE SET last_run = now(), drift_sec = 0, note = ''`); err != nil {
		t.Fatalf("could not reset the heartbeat: %v", err)
	}
	return New(db)
}

func ctx(t *testing.T) context.Context {
	t.Helper()
	return t.Context()
}

// --- fixtures ---------------------------------------------------------------

// seedUser inserts an account and returns its id.
func seedUser(t *testing.T, s *Store, email string, admin bool) int64 {
	t.Helper()

	var id int64
	err := s.db.QueryRow(
		`INSERT INTO users (fullname, email, password, is_admin, active)
		 VALUES ($1, $2, 'x', $3, true) RETURNING id`,
		email, email, admin).Scan(&id)
	if err != nil {
		t.Fatalf("seedUser: %v", err)
	}
	return id
}

// seedProject inserts a project and returns its id.
func seedProject(t *testing.T, s *Store, slug, baseURL string) int64 {
	t.Helper()

	var id int64
	err := s.db.QueryRow(
		`INSERT INTO projects (name, slug, base_url, api_key_prefix, api_key_hash, active)
		 VALUES ($1, $1, $2, $3, $4, true) RETURNING id`,
		slug, baseURL, "pfx_"+slug, "hash_"+slug).Scan(&id)
	if err != nil {
		t.Fatalf("seedProject: %v", err)
	}
	return id
}

// seedJob inserts a job and returns its id.
func seedJob(t *testing.T, s *Store, projectID int64, code string) int64 {
	t.Helper()

	var id int64
	err := s.db.QueryRow(
		`INSERT INTO jobs (project_id, code, name, method, url, timeout_sec,
			success_min, success_max, active)
		 VALUES ($1, $2, $2, 'GET', '/cron/'||$2, 30, 200, 299, true) RETURNING id`,
		projectID, code).Scan(&id)
	if err != nil {
		t.Fatalf("seedJob: %v", err)
	}
	return id
}

// seedRun inserts an execution record and returns its id.
func seedRun(t *testing.T, s *Store, jobID int64, status string, at time.Time) int64 {
	t.Helper()

	var id int64
	err := s.db.QueryRow(
		`INSERT INTO job_runs (job_id, trigger, status, created_at, duration_ms, http_status)
		 VALUES ($1, 'schedule', $2, $3, 10, 200) RETURNING id`,
		jobID, status, at).Scan(&id)
	if err != nil {
		t.Fatalf("seedRun: %v", err)
	}
	return id
}

// --- small assertions -------------------------------------------------------

func mustNoErr(t *testing.T, err error, what string) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
}

func codesOf(rows []domain.JobRow) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Code)
	}
	return out
}

func containsString(haystack []string, want string) bool {
	for _, s := range haystack {
		if s == want {
			return true
		}
	}
	return false
}
