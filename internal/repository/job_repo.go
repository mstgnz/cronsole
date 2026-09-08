package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/lib/pq"
	"github.com/mstgnz/cronsole/v2/internal/domain"
)

// JobRepo is data access for job definitions, their headers, schedules and
// chain links.
type JobRepo struct{ *Store }

// NewJobRepo wires the repository onto a store.
func NewJobRepo(s *Store) *JobRepo { return &JobRepo{s} }

const jobColumns = `id, project_id, code, name, description, tag, method, url, body,
	timeout_sec, max_duration_sec, retries, single_run, run_missed, max_delay_min,
	priority, success_min, success_max, notification_id, active, last_run_at,
	last_status, last_duration_ms, created_at, updated_at`

func scanJob(row interface{ Scan(...any) error }) (*domain.Job, error) {
	var (
		j          domain.Job
		notifyID   sql.NullInt64
		lastRun    sql.NullTime
		lastStatus sql.NullString
		lastMs     sql.NullInt64
		updatedAt  sql.NullTime
	)
	err := row.Scan(&j.ID, &j.ProjectID, &j.Code, &j.Name, &j.Description, &j.Tag,
		&j.Method, &j.URL, &j.Body, &j.TimeoutSec, &j.MaxDurationSec, &j.Retries,
		&j.SingleRun, &j.RunMissed, &j.MaxDelayMin, &j.Priority, &j.SuccessMin,
		&j.SuccessMax, &notifyID, &j.Active, &lastRun, &lastStatus, &lastMs,
		&j.CreatedAt, &updatedAt)
	if err != nil {
		return nil, err
	}
	j.NotificationID = nullInt64(notifyID)
	j.LastRunAt = nullTime(lastRun)
	j.LastStatus = lastStatus.String
	j.LastDurationMs = nullInt(lastMs)
	j.UpdatedAt = nullTime(updatedAt)
	return &j, nil
}

// Get reads one job definition.
func (r *JobRepo) Get(ctx context.Context, id int64) (*domain.Job, error) {
	const q = `SELECT ` + jobColumns + ` FROM jobs WHERE id = $1 AND deleted_at IS NULL`
	j, err := scanJob(r.db.QueryRowContext(ctx, q, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return j, err
}

// GetByCode reads a job by its machine name inside a project. This is the
// lookup the sync API uses to decide between insert and update.
func (r *JobRepo) GetByCode(ctx context.Context, projectID int64, code string) (*domain.Job, error) {
	const q = `SELECT ` + jobColumns + ` FROM jobs
		WHERE project_id = $1 AND lower(code) = lower($2) AND deleted_at IS NULL`
	j, err := scanJob(r.db.QueryRowContext(ctx, q, projectID, code))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return j, err
}

// jobFilterClauses turns a filter into SQL fragments and bound values.
//
// The fragments are literals chosen from a fixed set; only the values are
// bound. Nothing the caller supplies ever reaches the statement text.
func jobFilterClauses(f domain.JobFilter, args []any) ([]string, []any) {
	var where []string

	// The scope clause first, and unconditionally when the scope is not "all".
	// An empty id list produces `= ANY('{}')`, which matches nothing: that is
	// the correct answer for a caller who may reach no project, and the reason
	// this is not skipped when the list is empty.
	if !f.Scope.All {
		args = append(args, pq.Array(f.Scope.IDs))
		where = append(where, fmt.Sprintf("j.project_id = ANY($%d)", len(args)))
	}

	if f.ProjectID != nil {
		args = append(args, *f.ProjectID)
		where = append(where, fmt.Sprintf("j.project_id = $%d", len(args)))
	}
	if f.Tag != nil && *f.Tag != "" {
		args = append(args, *f.Tag)
		where = append(where, fmt.Sprintf("j.tag = $%d", len(args)))
	}
	if f.Active != nil {
		args = append(args, *f.Active)
		where = append(where, fmt.Sprintf("j.active = $%d", len(args)))
	}
	if f.Search != nil && *f.Search != "" {
		args = append(args, "%"+*f.Search+"%")
		where = append(where, fmt.Sprintf(
			"(j.code ILIKE $%d OR j.name ILIKE $%d OR j.url ILIKE $%d OR j.description ILIKE $%d)",
			len(args), len(args), len(args), len(args)))
	}
	return where, args
}

// List pages through jobs with the aggregates the list screen shows.
func (r *JobRepo) List(ctx context.Context, f domain.JobFilter, offset, limit int) ([]domain.JobRow, int64, error) {
	limit = clamp(limit, 1, 500)

	where := []string{"j.deleted_at IS NULL"}
	var args []any
	extra, args := jobFilterClauses(f, args)
	where = append(where, extra...)
	whereSQL := strings.Join(where, " AND ")

	countQ := `SELECT count(*) FROM jobs j WHERE ` + whereSQL
	var total int64
	if err := r.db.QueryRowContext(ctx, countQ, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	// The aggregates are lateral subqueries so a job with no schedules and no
	// runs still appears. A join with GROUP BY would either drop those rows or
	// multiply the run counts by the number of schedules.
	q := `
		SELECT ` + prefixColumns("j", jobColumns) + `,
			p.name, p.slug,
			COALESCE(s.exprs, ARRAY[]::text[]),
			COALESCE(rc.success, 0), COALESCE(rc.failed, 0), COALESCE(rc.timeout, 0),
			COALESCE(lc.outgoing, 0), COALESCE(lc.incoming, 0)
		FROM jobs j
		JOIN projects p ON p.id = j.project_id
		LEFT JOIN LATERAL (
			SELECT array_agg(expression ORDER BY expression) AS exprs
			FROM job_schedules WHERE job_id = j.id AND active
		) s ON true
		LEFT JOIN LATERAL (
			SELECT count(*) FILTER (WHERE status = 'success') AS success,
			       count(*) FILTER (WHERE status IN ('failed', 'timeout')) AS failed,
			       count(*) FILTER (WHERE status = 'timeout') AS timeout
			FROM job_runs WHERE job_id = j.id AND created_at > now() - interval '24 hours'
		) rc ON true
		LEFT JOIN LATERAL (
			SELECT count(*) FILTER (WHERE job_id = j.id) AS outgoing,
			       count(*) FILTER (WHERE target_job_id = j.id) AS incoming
			FROM job_links WHERE active AND (job_id = j.id OR target_job_id = j.id)
		) lc ON true
		WHERE ` + whereSQL + `
		ORDER BY p.name, j.priority, j.code
		OFFSET $` + fmt.Sprint(len(args)+1) + ` LIMIT $` + fmt.Sprint(len(args)+2)

	args = append(args, offset, limit)
	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var out []domain.JobRow
	for rows.Next() {
		var (
			row        domain.JobRow
			notifyID   sql.NullInt64
			lastRun    sql.NullTime
			lastStatus sql.NullString
			lastMs     sql.NullInt64
			updatedAt  sql.NullTime
			exprs      pq.StringArray
		)
		err := rows.Scan(&row.ID, &row.ProjectID, &row.Code, &row.Name, &row.Description,
			&row.Tag, &row.Method, &row.URL, &row.Body, &row.TimeoutSec, &row.MaxDurationSec,
			&row.Retries, &row.SingleRun, &row.RunMissed, &row.MaxDelayMin, &row.Priority,
			&row.SuccessMin, &row.SuccessMax, &notifyID, &row.Active, &lastRun, &lastStatus,
			&lastMs, &row.CreatedAt, &updatedAt,
			&row.ProjectName, &row.ProjectSlug, &exprs,
			&row.DaySuccess, &row.DayFailed, &row.DayTimeout, &row.LinkCount, &row.TriggerCount)
		if err != nil {
			return nil, 0, err
		}
		row.NotificationID = nullInt64(notifyID)
		row.LastRunAt = nullTime(lastRun)
		row.LastStatus = lastStatus.String
		row.LastDurationMs = nullInt(lastMs)
		row.UpdatedAt = nullTime(updatedAt)
		row.Schedules = exprs
		out = append(out, row)
	}
	return out, total, rows.Err()
}

// prefixColumns qualifies a column list with a table alias. The list is a
// compile time constant, so nothing user supplied passes through here.
func prefixColumns(alias, columns string) string {
	parts := strings.Split(columns, ",")
	for i, p := range parts {
		parts[i] = alias + "." + strings.TrimSpace(p)
	}
	return strings.Join(parts, ", ")
}

// ListOptions feeds the chain picker. excludeID drops the job itself, because
// a link from a job to itself is rejected by the schema and offering it in the
// list would only produce an error message.
//
// The scope here is the caller's WRITE scope, not their read scope. A link is a
// change to both ends: the target starts running because of something the
// source did, so offering a job somebody may only read would be offering them a
// way to trigger it.
func (r *JobRepo) ListOptions(ctx context.Context, scope domain.ProjectScope, excludeID int64) ([]domain.JobOption, error) {
	q := `SELECT j.id, j.code, j.name, p.slug, j.active
		FROM jobs j JOIN projects p ON p.id = j.project_id
		WHERE j.deleted_at IS NULL AND j.id <> $1`
	args := []any{excludeID}
	if !scope.All {
		args = append(args, pq.Array(scope.IDs))
		q += fmt.Sprintf(" AND j.project_id = ANY($%d)", len(args))
	}
	q += ` ORDER BY p.slug, j.code`

	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []domain.JobOption
	for rows.Next() {
		var o domain.JobOption
		if err := rows.Scan(&o.ID, &o.Code, &o.Name, &o.ProjectSlug, &o.Active); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// ListTags returns the tags in use, for the filter dropdown.
//
// Scoped as well. A dropdown listing another project's tags leaks its taxonomy,
// which is small but is exactly the kind of edge a filter is forgotten on.
func (r *JobRepo) ListTags(ctx context.Context, scope domain.ProjectScope) ([]string, error) {
	q := `SELECT DISTINCT tag FROM jobs WHERE deleted_at IS NULL AND tag <> ''`
	var args []any
	if !scope.All {
		args = append(args, pq.Array(scope.IDs))
		q += fmt.Sprintf(" AND project_id = ANY($%d)", len(args))
	}
	q += ` ORDER BY tag`

	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// Create inserts a job definition and returns its id.
func (r *JobRepo) Create(ctx context.Context, j *domain.Job) (int64, error) {
	const q = `INSERT INTO jobs (project_id, code, name, description, tag, method, url, body,
			timeout_sec, max_duration_sec, retries, single_run, run_missed, max_delay_min,
			priority, success_min, success_max, notification_id, active)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19)
		RETURNING id`
	var id int64
	err := r.db.QueryRowContext(ctx, q, j.ProjectID, j.Code, j.Name, j.Description, j.Tag,
		j.Method, j.URL, j.Body, j.TimeoutSec, j.MaxDurationSec, j.Retries, j.SingleRun,
		j.RunMissed, j.MaxDelayMin, j.Priority, j.SuccessMin, j.SuccessMax,
		j.NotificationID, j.Active).Scan(&id)
	return id, mapWriteErr(err)
}

// Update writes the editable fields.
//
// code is deliberately absent. History, chain links and every alert message
// are keyed by it, so renaming one would orphan the trail that explains what
// the job did.
func (r *JobRepo) Update(ctx context.Context, j *domain.Job) error {
	const q = `UPDATE jobs SET name = $1, description = $2, tag = $3, method = $4, url = $5,
			body = $6, timeout_sec = $7, max_duration_sec = $8, retries = $9,
			single_run = $10, run_missed = $11, max_delay_min = $12, priority = $13,
			success_min = $14, success_max = $15, notification_id = $16, active = $17,
			project_id = $18, updated_at = now()
		WHERE id = $19 AND deleted_at IS NULL`
	_, err := r.db.ExecContext(ctx, q, j.Name, j.Description, j.Tag, j.Method, j.URL,
		j.Body, j.TimeoutSec, j.MaxDurationSec, j.Retries, j.SingleRun, j.RunMissed,
		j.MaxDelayMin, j.Priority, j.SuccessMin, j.SuccessMax, j.NotificationID,
		j.Active, j.ProjectID, j.ID)
	return mapWriteErr(err)
}

// SetActive flips a job on or off.
func (r *JobRepo) SetActive(ctx context.Context, id int64, active bool) error {
	const q = `UPDATE jobs SET active = $1, updated_at = now() WHERE id = $2 AND deleted_at IS NULL`
	_, err := r.db.ExecContext(ctx, q, active, id)
	return err
}

// SoftDelete deactivates a job and marks it deleted. The row stays because
// job_runs references it and the execution history has to survive.
func (r *JobRepo) SoftDelete(ctx context.Context, id int64, at time.Time) error {
	const q = `UPDATE jobs SET active = false, deleted_at = $1, updated_at = $1 WHERE id = $2`
	_, err := r.db.ExecContext(ctx, q, at, id)
	return err
}

// --- headers ---

// ListHeaders returns the request headers for a job.
func (r *JobRepo) ListHeaders(ctx context.Context, jobID int64) ([]domain.JobHeader, error) {
	const q = `SELECT id, job_id, key, value, is_secret FROM job_headers WHERE job_id = $1 ORDER BY key`
	rows, err := r.db.QueryContext(ctx, q, jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []domain.JobHeader
	for rows.Next() {
		var h domain.JobHeader
		if err := rows.Scan(&h.ID, &h.JobID, &h.Key, &h.Value, &h.IsSecret); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// ReplaceHeaders swaps the whole header set in one transaction.
//
// Replace rather than diff: the form posts the complete set, and a partial
// update would leave a header the operator deleted on screen still being sent
// on the next run.
func (r *JobRepo) ReplaceHeaders(ctx context.Context, jobID int64, headers []domain.JobHeader) error {
	return r.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM job_headers WHERE job_id = $1`, jobID); err != nil {
			return err
		}
		for _, h := range headers {
			if strings.TrimSpace(h.Key) == "" {
				continue
			}
			_, err := tx.ExecContext(ctx,
				`INSERT INTO job_headers (job_id, key, value, is_secret) VALUES ($1, $2, $3, $4)
				 ON CONFLICT (job_id, lower(key)) DO UPDATE SET value = EXCLUDED.value, is_secret = EXCLUDED.is_secret`,
				jobID, strings.TrimSpace(h.Key), h.Value, h.IsSecret)
			if err != nil {
				return err
			}
		}
		return nil
	})
}

// --- schedules ---

// ListSchedules returns a job's cron expressions.
func (r *JobRepo) ListSchedules(ctx context.Context, jobID int64) ([]domain.JobSchedule, error) {
	const q = `SELECT id, job_id, expression, active, created_at FROM job_schedules
		WHERE job_id = $1 ORDER BY id`
	rows, err := r.db.QueryContext(ctx, q, jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []domain.JobSchedule
	for rows.Next() {
		var s domain.JobSchedule
		if err := rows.Scan(&s.ID, &s.JobID, &s.Expression, &s.Active, &s.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// CreateSchedule adds one expression to a job.
func (r *JobRepo) CreateSchedule(ctx context.Context, s *domain.JobSchedule) (int64, error) {
	const q = `INSERT INTO job_schedules (job_id, expression, active) VALUES ($1, $2, $3) RETURNING id`
	var id int64
	err := r.db.QueryRowContext(ctx, q, s.JobID, s.Expression, s.Active).Scan(&id)
	return id, mapWriteErr(err)
}

// DeleteSchedule removes one expression.
//
// jobID is part of the WHERE clause, not just a convenience: it is the
// ownership check, so a schedule id guessed from another job cannot be deleted
// through this route.
func (r *JobRepo) DeleteSchedule(ctx context.Context, id, jobID int64) error {
	const q = `DELETE FROM job_schedules WHERE id = $1 AND job_id = $2`
	res, err := r.db.ExecContext(ctx, q, id, jobID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// CountSchedules counts a job's active expressions. Activation depends on it:
// a job with no schedule would sit active and never run.
func (r *JobRepo) CountSchedules(ctx context.Context, jobID int64) (int, error) {
	var n int
	err := r.db.QueryRowContext(ctx,
		`SELECT count(*) FROM job_schedules WHERE job_id = $1 AND active`, jobID).Scan(&n)
	return n, err
}

// ReplaceSchedules swaps the whole expression set, for the declarative sync
// API where the caller sends the complete desired state.
func (r *JobRepo) ReplaceSchedules(ctx context.Context, jobID int64, expressions []string) error {
	return r.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM job_schedules WHERE job_id = $1`, jobID); err != nil {
			return err
		}
		for _, e := range expressions {
			e = strings.Join(strings.Fields(e), " ")
			if e == "" {
				continue
			}
			_, err := tx.ExecContext(ctx,
				`INSERT INTO job_schedules (job_id, expression, active) VALUES ($1, $2, true)
				 ON CONFLICT (job_id, expression) DO UPDATE SET active = true`, jobID, e)
			if err != nil {
				return err
			}
		}
		return nil
	})
}

// --- chain links ---

const linkColumns = `l.id, l.job_id, l.target_job_id, l.condition, l.delay_sec, l.active, l.created_at`

func scanLinkRows(rows *sql.Rows) ([]domain.JobLinkRow, error) {
	var out []domain.JobLinkRow
	for rows.Next() {
		var row domain.JobLinkRow
		err := rows.Scan(&row.ID, &row.JobID, &row.TargetJobID, &row.Condition,
			&row.DelaySec, &row.Active, &row.CreatedAt,
			&row.TargetCode, &row.TargetName, &row.TargetActive,
			&row.SourceCode, &row.SourceName)
		if err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// ListLinks returns the chain edges leaving a job.
func (r *JobRepo) ListLinks(ctx context.Context, jobID int64) ([]domain.JobLinkRow, error) {
	const q = `SELECT ` + linkColumns + `, t.code, t.name, t.active, s.code, s.name
		FROM job_links l
		JOIN jobs t ON t.id = l.target_job_id
		JOIN jobs s ON s.id = l.job_id
		WHERE l.job_id = $1 AND t.deleted_at IS NULL
		ORDER BY l.delay_sec, t.code`
	rows, err := r.db.QueryContext(ctx, q, jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanLinkRows(rows)
}

// ListTriggers is the chain in reverse: which jobs trigger this one.
func (r *JobRepo) ListTriggers(ctx context.Context, jobID int64) ([]domain.JobLinkRow, error) {
	const q = `SELECT ` + linkColumns + `, t.code, t.name, t.active, s.code, s.name
		FROM job_links l
		JOIN jobs t ON t.id = l.target_job_id
		JOIN jobs s ON s.id = l.job_id
		WHERE l.target_job_id = $1 AND s.deleted_at IS NULL
		ORDER BY s.code`
	rows, err := r.db.QueryContext(ctx, q, jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanLinkRows(rows)
}

// CreateLink adds a chain edge.
func (r *JobRepo) CreateLink(ctx context.Context, l *domain.JobLink) (int64, error) {
	const q = `INSERT INTO job_links (job_id, target_job_id, condition, delay_sec, active)
		VALUES ($1, $2, $3, $4, $5) RETURNING id`
	var id int64
	err := r.db.QueryRowContext(ctx, q, l.JobID, l.TargetJobID, l.Condition, l.DelaySec, l.Active).Scan(&id)
	return id, mapWriteErr(err)
}

// DeleteLink removes a chain edge. jobID scopes the delete to the owning job.
func (r *JobRepo) DeleteLink(ctx context.Context, id, jobID int64) error {
	const q = `DELETE FROM job_links WHERE id = $1 AND job_id = $2`
	res, err := r.db.ExecContext(ctx, q, id, jobID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ReachesJob reports whether a chain starting at fromID reaches targetID
// within the depth cap.
//
// It is the loop guard for the form. The runner has its own depth cap and does
// not rely on this: a cycle created outside the application would still stop
// there. This one exists so the operator is told at the moment they build it,
// rather than discovering it from a truncated chain days later.
func (r *JobRepo) ReachesJob(ctx context.Context, fromID, targetID int64, maxDepth int) (bool, error) {
	const q = `
		WITH RECURSIVE walk(job_id, depth) AS (
			SELECT $1::bigint, 0
			UNION ALL
			SELECT l.target_job_id, w.depth + 1
			FROM walk w
			JOIN job_links l ON l.job_id = w.job_id AND l.active
			WHERE w.depth < $3
		)
		SELECT EXISTS (SELECT 1 FROM walk WHERE job_id = $2)`
	var found bool
	err := r.db.QueryRowContext(ctx, q, fromID, targetID, maxDepth).Scan(&found)
	return found, err
}
