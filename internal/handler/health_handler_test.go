package handler

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	_ "github.com/lib/pq"
)

// The probes. Between them they decide whether the orchestrator restarts this
// process or takes it out of the load balancer, so the difference between them
// is the whole point.

func TestLivenessTouchesNothingElse(t *testing.T) {
	// A liveness probe that checks the database restarts the application every
	// time the database hiccups, which turns a brief outage into a restart
	// loop. It answers from the process alone, with no database at all.
	h := NewHealthHandler(nil, nil, "v1.2.3")

	w := httptest.NewRecorder()
	h.Live(w, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("liveness answered %d with no database", w.Code)
	}

	var body struct {
		Data struct {
			Status  string `json:"status"`
			Version string `json:"version"`
			Uptime  string `json:"uptime"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("the answer is not JSON: %v", err)
	}
	if body.Data.Status != "ok" {
		t.Errorf("status = %q", body.Data.Status)
	}
	// The version is what tells an operator which build is answering, which is
	// the first question during a rollout.
	if body.Data.Version != "v1.2.3" {
		t.Errorf("version = %q", body.Data.Version)
	}
	if body.Data.Uptime == "" {
		t.Error("no uptime was reported")
	}
}

func TestReadinessWithNoDatabase(t *testing.T) {
	// Not ready, and not a panic. A readiness probe is the one endpoint that
	// has to answer even when the wiring is wrong.
	h := NewHealthHandler(nil, nil, "test")

	w := httptest.NewRecorder()
	h.Ready(w, httptest.NewRequest(http.MethodGet, "/readyz", nil))

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("readiness answered %d with no database, want 503", w.Code)
	}
	if !strings.Contains(w.Body.String(), "database") {
		t.Errorf("the answer does not say what is missing: %s", w.Body.String())
	}
}

func TestReadinessWithADatabaseThatIsNotAnswering(t *testing.T) {
	// A pool pointed at a closed port. It has to fail rather than hang: the
	// ping carries its own deadline.
	db, err := sql.Open("postgres",
		"postgres://nobody:nothing@127.0.0.1:1/nothing?sslmode=disable&connect_timeout=1")
	if err != nil {
		t.Fatalf("sql.Open = %v", err)
	}
	defer func() { _ = db.Close() }()

	h := NewHealthHandler(db, nil, "test")

	w := httptest.NewRecorder()
	h.Ready(w, httptest.NewRequest(http.MethodGet, "/readyz", nil))

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("readiness answered %d against a closed port, want 503", w.Code)
	}
}

func TestReadinessWithARealDatabase(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("CRONSOLE_TEST_DB"))
	if dsn == "" {
		t.Skip("CRONSOLE_TEST_DB is not set; run `make test-db` first")
	}

	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("sql.Open = %v", err)
	}
	defer func() { _ = db.Close() }()

	h := NewHealthHandler(db, nil, "test")

	w := httptest.NewRecorder()
	h.Ready(w, httptest.NewRequest(http.MethodGet, "/readyz", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("readiness answered %d against a working database: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "ok") {
		t.Errorf("the answer does not report ready: %s", w.Body.String())
	}
	// No pool was wired, so the queue figures are absent rather than reported
	// as zero, which would read as an idle worker pool that does not exist.
	if strings.Contains(w.Body.String(), "queue_depth") {
		t.Error("queue figures were reported with no pool")
	}
}
