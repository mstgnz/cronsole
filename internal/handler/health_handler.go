package handler

import (
	"context"
	"database/sql"
	"net/http"
	"time"

	"github.com/mstgnz/cronsole/v2/internal/httpx"
	"github.com/mstgnz/cronsole/v2/pkg/worker"
)

// HealthHandler answers the liveness and readiness probes.
type HealthHandler struct {
	db      *sql.DB
	pool    *worker.Pool
	version string
	started time.Time
}

// NewHealthHandler wires the handler.
func NewHealthHandler(db *sql.DB, pool *worker.Pool, version string) *HealthHandler {
	return &HealthHandler{db: db, pool: pool, version: version, started: time.Now()}
}

// Live reports that the process is up. It touches nothing else on purpose: a
// liveness probe that checks the database restarts the application every time
// the database hiccups, which turns a brief outage into a restart loop.
func (h *HealthHandler) Live(w http.ResponseWriter, r *http.Request) {
	httpx.OK(w, map[string]any{
		"status":  "ok",
		"version": h.version,
		"uptime":  time.Since(h.started).Round(time.Second).String(),
	})
}

// Ready reports whether the process can serve traffic, which here means the
// database answers.
func (h *HealthHandler) Ready(w http.ResponseWriter, r *http.Request) {
	// No pool means no database at all, which is not ready. Checked rather
	// than assumed: a readiness probe is the one endpoint that must answer
	// even when the wiring is wrong, and PingContext on a nil handle panics.
	if h.db == nil {
		httpx.Fail(w, http.StatusServiceUnavailable, "no database configured")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	if err := h.db.PingContext(ctx); err != nil {
		httpx.Fail(w, http.StatusServiceUnavailable, "database unreachable")
		return
	}

	data := map[string]any{"status": "ok", "version": h.version}
	if h.pool != nil {
		data["queue_depth"] = h.pool.Queued()
		data["queue_capacity"] = h.pool.Capacity()
	}
	httpx.OK(w, data)
}
