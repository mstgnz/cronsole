package repository

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/lib/pq"
	"github.com/mstgnz/cronsole/v2/internal/domain"
)

// ExecRepo is data access for the execution path: the dispatcher's minute
// scan, one run's lifecycle, and the watchdog sweep.
//
// It is one type implementing three narrow interfaces rather than three types,
// because all of it is the same handful of tables and the statements have to
// agree with each other. The interfaces are what keeps each caller from
// depending on the whole surface.
type ExecRepo struct{ *Store }

// NewExecRepo wires the repository onto a store.
func NewExecRepo(s *Store) *ExecRepo { return &ExecRepo{s} }

// DBTime reads the database clock.
//
// This is the time authority for the whole scheduler. Replicas run on separate
// machines, and every one of them has to derive the same minute value,
// otherwise the unique index on (job_id, planned_minute) does not line up and
// the same job runs twice in one minute.
func (r *ExecRepo) DBTime(ctx context.Context) (time.Time, error) {
	var t time.Time
	err := r.db.QueryRowContext(ctx, `SELECT now()`).Scan(&t)
	return t, err
}

// --- heartbeat ---

// ReadHeartbeat returns the dispatcher pulse, or (nil, nil) when the row is
// missing.
func (r *ExecRepo) ReadHeartbeat(ctx context.Context) (*domain.Heartbeat, error) {
	const q = `SELECT last_run, app_time, drift_sec, instance_id, note FROM heartbeat WHERE id = 1`
	var (
		h       domain.Heartbeat
		appTime sql.NullTime
	)
	err := r.db.QueryRowContext(ctx, q).Scan(&h.LastRun, &appTime, &h.DriftSec, &h.InstanceID, &h.Note)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	h.AppTime = nullTime(appTime)
	return &h, nil
}

// TouchHeartbeat refreshes the pulse. An upsert rather than an update, so a
// database restored without the seed row heals itself on the next tick.
func (r *ExecRepo) TouchHeartbeat(ctx context.Context, dbTime, appTime time.Time, driftSec int, instanceID string) error {
	const q = `INSERT INTO heartbeat (id, last_run, app_time, drift_sec, instance_id)
		VALUES (1, $1, $2, $3, $4)
		ON CONFLICT (id) DO UPDATE
		SET last_run = EXCLUDED.last_run, app_time = EXCLUDED.app_time,
		    drift_sec = EXCLUDED.drift_sec, instance_id = EXCLUDED.instance_id`
	_, err := r.db.ExecContext(ctx, q, dbTime, appTime, driftSec, instanceID)
	return err
}

// --- dispatcher ---

// ListScheduledJobs returns active jobs in priority order.
//
// A job under an inactive project is excluded. Deactivating a project has to
// stop its work, otherwise "inactive" means nothing on the screen where it
// matters most.
func (r *ExecRepo) ListScheduledJobs(ctx context.Context) ([]domain.ScheduledJob, error) {
	const q = `SELECT j.id, j.code, j.priority, j.run_missed, j.max_delay_min
		FROM jobs j JOIN projects p ON p.id = j.project_id
		WHERE j.active AND j.deleted_at IS NULL AND p.active AND p.deleted_at IS NULL
		ORDER BY j.priority, j.id`
	rows, err := r.db.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []domain.ScheduledJob
	for rows.Next() {
		var j domain.ScheduledJob
		if err := rows.Scan(&j.ID, &j.Code, &j.Priority, &j.RunMissed, &j.MaxDelayMin); err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// ListActiveSchedules returns the active expressions grouped by job id.
func (r *ExecRepo) ListActiveSchedules(ctx context.Context) (map[int64][]string, error) {
	const q = `SELECT s.job_id, s.expression
		FROM job_schedules s
		JOIN jobs j ON j.id = s.job_id
		JOIN projects p ON p.id = j.project_id
		WHERE s.active AND j.active AND j.deleted_at IS NULL AND p.active AND p.deleted_at IS NULL`
	rows, err := r.db.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[int64][]string)
	for rows.Next() {
		var (
			jobID int64
			expr  string
		)
		if err := rows.Scan(&jobID, &expr); err != nil {
			return nil, err
		}
		out[jobID] = append(out[jobID], expr)
	}
	return out, rows.Err()
}

// QueueRun opens a pending row for a job and minute.
//
// ON CONFLICT DO NOTHING against the partial unique index is the whole
// duplicate protection of the system. Two dispatchers reaching the same minute
// both run this statement; exactly one row exists afterwards, and neither
// caller has to know which of them wrote it.
func (r *ExecRepo) QueueRun(ctx context.Context, jobID int64, plannedMinute time.Time) error {
	const q = `INSERT INTO job_runs (job_id, planned_minute, trigger, status)
		VALUES ($1, $2, 'schedule', 'pending')
		ON CONFLICT (job_id, planned_minute) WHERE planned_minute IS NOT NULL DO NOTHING`
	_, err := r.db.ExecContext(ctx, q, jobID, plannedMinute)
	return err
}

// CountRunning is the number of runs currently in flight, across every job.
func (r *ExecRepo) CountRunning(ctx context.Context) (int, error) {
	var n int
	err := r.db.QueryRowContext(ctx, `SELECT count(*) FROM job_runs WHERE status = 'running'`).Scan(&n)
	return n, err
}

// RunningJobIDs is the set of jobs with a run in flight.
func (r *ExecRepo) RunningJobIDs(ctx context.Context) (map[int64]bool, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT DISTINCT job_id FROM job_runs WHERE status = 'running'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[int64]bool)
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// ListPending returns queued rows whose run_after has arrived, in job priority
// order so a congested minute serves the important work first.
func (r *ExecRepo) ListPending(ctx context.Context, now time.Time, limit int) ([]domain.PendingRun, error) {
	const q = `SELECT r.id, r.job_id, j.code, j.single_run
		FROM job_runs r JOIN jobs j ON j.id = r.job_id
		WHERE r.status = 'pending' AND (r.run_after IS NULL OR r.run_after <= $1)
		ORDER BY j.priority, r.id
		LIMIT $2`
	rows, err := r.db.QueryContext(ctx, q, now, clamp(limit, 1, 500))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []domain.PendingRun
	for rows.Next() {
		var p domain.PendingRun
		if err := rows.Scan(&p.ID, &p.JobID, &p.Code, &p.SingleRun); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// SkipRun closes a queued row as skipped.
//
// The status guard is what makes this safe to call from two places: it can
// only ever close a row that has not started, so it cannot overwrite a result.
func (r *ExecRepo) SkipRun(ctx context.Context, runID int64, reason string) error {
	const q = `UPDATE job_runs SET status = 'skipped', finished_at = now(), error = $2
		WHERE id = $1 AND status = 'pending'`
	_, err := r.db.ExecContext(ctx, q, runID, truncate(reason, domain.ErrorMaxLen))
	return err
}

// --- one run's lifecycle ---

// GetRun reads the run together with everything needed to fire it.
func (r *ExecRepo) GetRun(ctx context.Context, runID int64) (*domain.RunTarget, error) {
	const q = `SELECT r.id, r.job_id, r.status, r.attempt,
			j.code, j.name, j.project_id, p.slug, p.base_url,
			j.method, j.url, j.body, j.timeout_sec, j.retries, j.single_run,
			j.success_min, j.success_max, j.notification_id
		FROM job_runs r
		JOIN jobs j ON j.id = r.job_id
		JOIN projects p ON p.id = j.project_id
		WHERE r.id = $1`

	var (
		t        domain.RunTarget
		notifyID sql.NullInt64
	)
	err := r.db.QueryRowContext(ctx, q, runID).Scan(&t.ID, &t.JobID, &t.Status, &t.Attempt,
		&t.Code, &t.Name, &t.ProjectID, &t.ProjectSlug, &t.BaseURL,
		&t.Method, &t.URL, &t.Body, &t.TimeoutSec, &t.Retries, &t.SingleRun,
		&t.SuccessMin, &t.SuccessMax, &notifyID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	t.NotificationID = nullInt64(notifyID)

	headers, err := r.headersFor(ctx, t.JobID)
	if err != nil {
		return nil, err
	}
	t.Headers = headers
	return &t, nil
}

func (r *ExecRepo) headersFor(ctx context.Context, jobID int64) ([]domain.JobHeader, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT key, value FROM job_headers WHERE job_id = $1`, jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []domain.JobHeader
	for rows.Next() {
		var h domain.JobHeader
		if err := rows.Scan(&h.Key, &h.Value); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// CountRunningForJob counts a job's in flight runs, excluding one id so a run
// can leave itself out of the count.
func (r *ExecRepo) CountRunningForJob(ctx context.Context, jobID, excludeRunID int64) (int, error) {
	const q = `SELECT count(*) FROM job_runs WHERE job_id = $1 AND status = 'running' AND id <> $2`
	var n int
	err := r.db.QueryRowContext(ctx, q, jobID, excludeRunID).Scan(&n)
	return n, err
}

// ClaimRun takes the row atomically.
//
// The WHERE clause carries the guard: only a pending row can be claimed, and
// the update is a single statement, so two processes racing for the same run
// produce exactly one winner. This is the ONLY thing preventing a run from
// executing twice; nothing above it in the stack repeats the check.
func (r *ExecRepo) ClaimRun(ctx context.Context, runID int64, instanceID string, at time.Time) (bool, error) {
	const q = `UPDATE job_runs SET status = 'running', started_at = $2, instance_id = $3
		WHERE id = $1 AND status = 'pending'`
	res, err := r.db.ExecContext(ctx, q, runID, at, instanceID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

// FinishRun writes the outcome of a run.
func (r *ExecRepo) FinishRun(ctx context.Context, runID int64, result domain.RunResult) error {
	const q = `UPDATE job_runs SET status = $2, finished_at = now(), duration_ms = $3,
			http_status = $4, request_url = $5, output = $6, error = $7, attempt = $8
		WHERE id = $1`

	// Zero means no response was received at all, which is not the same claim
	// as "the server answered 0", so it is stored as NULL.
	var httpStatus any
	if result.HTTPStatus > 0 {
		httpStatus = result.HTTPStatus
	}

	_, err := r.db.ExecContext(ctx, q, runID, result.Status, result.DurationMs, httpStatus,
		truncate(result.RequestURL, 2000),
		truncate(result.Output, domain.OutputMaxLen),
		truncate(result.Error, domain.ErrorMaxLen),
		result.Attempt)
	return err
}

// WriteJobSummary refreshes the denormalised columns the list screen reads.
//
// They are a convenience, not a source of truth: everything here can be
// recomputed from job_runs, which is why a failure to write them degrades into
// a note on the run rather than failing the run.
func (r *ExecRepo) WriteJobSummary(ctx context.Context, jobID int64, status string, at time.Time, durationMs int) error {
	const q = `UPDATE jobs SET last_run_at = $2, last_status = $3, last_duration_ms = $4 WHERE id = $1`
	_, err := r.db.ExecContext(ctx, q, jobID, at, status, durationMs)
	return err
}

// AppendRunNote adds to the error text without overwriting what is there.
//
// Overwriting would replace the reason the run failed with a note about
// bookkeeping, which is exactly backwards.
func (r *ExecRepo) AppendRunNote(ctx context.Context, runID int64, message string) error {
	const q = `UPDATE job_runs
		SET error = left(CASE WHEN error = '' THEN $2 ELSE error || ' | ' || $2 END, $3)
		WHERE id = $1`
	_, err := r.db.ExecContext(ctx, q, runID, message, domain.ErrorMaxLen)
	return err
}

// --- chain ---

// ListChainTargets returns the active links leaving this job that match the
// outcome, plus the ones marked always.
func (r *ExecRepo) ListChainTargets(ctx context.Context, jobID int64, outcome string) ([]domain.ChainTarget, error) {
	const q = `SELECT l.target_job_id, t.code, t.active AND p.active, t.single_run, l.delay_sec, l.condition
		FROM job_links l
		JOIN jobs t ON t.id = l.target_job_id
		JOIN projects p ON p.id = t.project_id
		WHERE l.job_id = $1 AND l.active AND t.deleted_at IS NULL AND p.deleted_at IS NULL
		  AND (l.condition = $2 OR l.condition = 'always')`
	rows, err := r.db.QueryContext(ctx, q, jobID, outcome)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []domain.ChainTarget
	for rows.Next() {
		var c domain.ChainTarget
		if err := rows.Scan(&c.JobID, &c.Code, &c.Active, &c.SingleRun, &c.DelaySec, &c.Condition); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ChainStepCount reports how deep in a chain this run sits, by walking
// parent_run_id upwards.
//
// The walk is bounded independently of the depth cap. A cycle written straight
// into the table would otherwise make this query run forever, and the guard
// against that cannot be the same number the guard is checking.
func (r *ExecRepo) ChainStepCount(ctx context.Context, runID int64) (int, error) {
	const q = `
		WITH RECURSIVE up(id, parent_run_id, depth) AS (
			SELECT id, parent_run_id, 0 FROM job_runs WHERE id = $1
			UNION ALL
			SELECT r.id, r.parent_run_id, u.depth + 1
			FROM job_runs r JOIN up u ON r.id = u.parent_run_id
			WHERE u.depth < 50
		)
		SELECT COALESCE(max(depth), 0) FROM up`
	var depth int
	err := r.db.QueryRowContext(ctx, q, runID).Scan(&depth)
	return depth, err
}

// CreateChainRun opens a pending row for a chain step.
//
// run_after carries the delay, so the row is durable the moment it is written:
// if the in process timer that was meant to hand it over is lost to a restart,
// the dispatcher still picks it up.
func (r *ExecRepo) CreateChainRun(ctx context.Context, jobID int64, delaySec int, parentRunID int64) (int64, error) {
	const q = `INSERT INTO job_runs (job_id, trigger, parent_run_id, run_after, status)
		VALUES ($1, 'chain', $2, now() + make_interval(secs => $3::double precision), 'pending')
		RETURNING id`
	var id int64
	err := r.db.QueryRowContext(ctx, q, jobID, parentRunID, delaySec).Scan(&id)
	return id, err
}

// CreateChainSkipped records a chain step that was not run.
//
// A step that vanishes without a trace is far harder to notice than one that
// failed, which is why this writes a row rather than only a log line.
func (r *ExecRepo) CreateChainSkipped(ctx context.Context, jobID int64, parentRunID int64, reason string) error {
	const q = `INSERT INTO job_runs (job_id, trigger, parent_run_id, status, finished_at, error)
		VALUES ($1, 'chain', $2, 'skipped', now(), $3)`
	_, err := r.db.ExecContext(ctx, q, jobID, parentRunID, truncate(reason, domain.ErrorMaxLen))
	return err
}

// --- watchdog ---

// CloseStuckRuns closes runs that have been running past their job's
// max_duration_sec.
//
// Not cosmetic. A job marked single_run whose run row is stuck in running
// never executes again: every subsequent tick decides the previous execution
// is still going and skips.
func (r *ExecRepo) CloseStuckRuns(ctx context.Context) (int64, error) {
	const q = `UPDATE job_runs r
		SET status = 'timeout',
		    finished_at = now(),
		    duration_ms = COALESCE(r.duration_ms,
		        (EXTRACT(EPOCH FROM (now() - r.started_at)) * 1000)::int),
		    error = CASE WHEN r.error = '' THEN $1 ELSE r.error || ' | ' || $1 END
		FROM jobs j
		WHERE j.id = r.job_id
		  AND r.status = 'running'
		  AND r.started_at IS NOT NULL
		  AND r.started_at < now() - make_interval(secs => j.max_duration_sec::double precision)`
	res, err := r.db.ExecContext(ctx, q,
		"Still running past max_duration_sec; closed by the watchdog. The target may have finished.")
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// CountStalePending counts rows nothing ever handed over.
func (r *ExecRepo) CountStalePending(ctx context.Context, threshold time.Duration) (int, error) {
	const q = `SELECT count(*) FROM job_runs
		WHERE status = 'pending' AND created_at < now() - make_interval(secs => $1::double precision)`
	var n int
	err := r.db.QueryRowContext(ctx, q, threshold.Seconds()).Scan(&n)
	return n, err
}

// ListFailingJobs returns jobs at or over the failure threshold in the window.
func (r *ExecRepo) ListFailingJobs(ctx context.Context, window time.Duration, threshold int) ([]domain.ErrorSummary, error) {
	const q = `SELECT j.code, j.name, count(*)::int, COALESCE(max(r.error), '')
		FROM job_runs r JOIN jobs j ON j.id = r.job_id
		WHERE r.status IN ('failed', 'timeout')
		  AND r.created_at > now() - make_interval(secs => $1::double precision)
		GROUP BY j.code, j.name
		HAVING count(*) >= $2
		ORDER BY count(*) DESC`
	rows, err := r.db.QueryContext(ctx, q, window.Seconds(), threshold)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []domain.ErrorSummary
	for rows.Next() {
		var e domain.ErrorSummary
		if err := rows.Scan(&e.Code, &e.Name, &e.Count, &e.LastError); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// PurgeRuns deletes old run rows, bounded per call.
//
// The bound is what keeps the delete from holding a long lock on the busiest
// table in the schema. The sweep runs often enough that a backlog drains over
// a few passes.
func (r *ExecRepo) PurgeRuns(ctx context.Context, days, limit int) (int64, error) {
	const q = `DELETE FROM job_runs WHERE id IN (
			SELECT id FROM job_runs
			WHERE created_at < now() - make_interval(days => $1::int)
			ORDER BY id LIMIT $2
		)`
	res, err := r.db.ExecContext(ctx, q, days, clamp(limit, 1, 50000))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// PurgeAppLogs deletes old application log rows.
func (r *ExecRepo) PurgeAppLogs(ctx context.Context, days int) (int64, error) {
	const q = `DELETE FROM app_logs WHERE created_at < now() - make_interval(days => $1::int)`
	res, err := r.db.ExecContext(ctx, q, days)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// SetWarningMarker stores the alert repeat fingerprint.
func (r *ExecRepo) SetWarningMarker(ctx context.Context, marker string) error {
	_, err := r.db.ExecContext(ctx, `UPDATE heartbeat SET note = $1 WHERE id = 1`, marker)
	return err
}

// --- notification recipients ---

// TargetForJob reads the recipient set attached to a job.
func (r *ExecRepo) TargetForJob(ctx context.Context, notificationID int64) (*domain.NotifyTarget, error) {
	const q = `SELECT n.on_success, n.on_failure,
			COALESCE(array_agg(e.email) FILTER (WHERE e.email IS NOT NULL AND e.active), ARRAY[]::text[])
		FROM notifications n
		LEFT JOIN notify_emails e ON e.notification_id = n.id
		WHERE n.id = $1 AND n.active AND n.deleted_at IS NULL
		GROUP BY n.on_success, n.on_failure`

	var (
		t      domain.NotifyTarget
		emails pq.StringArray
	)
	err := r.db.QueryRowContext(ctx, q, notificationID).Scan(&t.OnSuccess, &t.OnFailure, &emails)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	t.Emails = emails
	return &t, nil
}
