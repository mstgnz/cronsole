package repository

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/lib/pq"
	"github.com/mstgnz/cronsole/v2/internal/domain"
)

// StatsRepo feeds the dashboard.
//
// Every query here takes a scope. The dashboard is the screen where a missing
// filter is least likely to be noticed: it shows numbers, not rows, so a reader
// on one project silently seeing the whole company's failure count looks like
// nothing at all.
type StatsRepo struct{ *Store }

// NewStatsRepo wires the repository onto a store.
func NewStatsRepo(s *Store) *StatsRepo { return &StatsRepo{s} }

// runScope limits run rows to a scope.
//
// A subquery on jobs rather than a join, so the caller's FROM clause does not
// change shape depending on who is asking. The clause is empty only for an
// unrestricted scope; an empty id list still produces `= ANY('{}')`, which
// matches nothing, and that is the right answer for somebody with no projects.
func runScope(alias string, scope domain.ProjectScope, args []any) (clause string, out []any) {
	if scope.All {
		return "", args
	}
	args = append(args, pq.Array(scope.IDs))
	column := "job_id"
	if alias != "" {
		column = alias + ".job_id"
	}
	return fmt.Sprintf(
		" AND %s IN (SELECT id FROM jobs WHERE project_id = ANY($%d))", column, len(args)), args
}

// jobScope limits job rows to a scope.
func jobScope(alias string, scope domain.ProjectScope, args []any) (clause string, out []any) {
	if scope.All {
		return "", args
	}
	args = append(args, pq.Array(scope.IDs))
	column := "project_id"
	if alias != "" {
		column = alias + ".project_id"
	}
	return fmt.Sprintf(" AND %s = ANY($%d)", column, len(args)), args
}

// Summary builds the dashboard's top band.
//
// Three separate reads rather than one query: the pulse is a single row, the
// job counts scan jobs, and the run counts scan a 24 hour slice of job_runs.
// Forcing them into one statement would make the planner choose one strategy
// for three unrelated shapes.
//
// The pulse is NOT scoped, and that is deliberate. The dispatcher is one
// process serving every project, so its health is the same fact for everyone;
// hiding it from a project reader would leave them looking at a page of zeros
// with no way to tell a quiet day from a dead scheduler.
func (r *StatsRepo) Summary(ctx context.Context, scope domain.ProjectScope) (*domain.Summary, error) {
	out := &domain.Summary{}

	var (
		lastRun sql.NullTime
		elapsed sql.NullFloat64
	)
	const pulseQ = `SELECT last_run, EXTRACT(EPOCH FROM (now() - last_run)), drift_sec, instance_id
		FROM heartbeat WHERE id = 1`
	err := r.db.QueryRowContext(ctx, pulseQ).Scan(&lastRun, &elapsed, &out.DriftSec, &out.InstanceID)
	if err != nil && err != sql.ErrNoRows {
		return nil, err
	}
	if lastRun.Valid {
		t := lastRun.Time
		out.LastRun = &t
		if elapsed.Valid {
			secs := int(elapsed.Float64)
			out.ElapsedSec = &secs
		}
	}

	// One bound argument reused by all three subqueries. Binding it once keeps
	// the three counts talking about the same set of projects, which is the
	// whole point of the band: "5 of 6 jobs active across 2 projects" has to
	// add up.
	//
	// The project count filters on the ids directly rather than through jobs,
	// because a project with no jobs still belongs to the caller.
	jobQ := `SELECT
			(SELECT count(*) FROM projects WHERE deleted_at IS NULL%[1]s),
			(SELECT count(*) FROM jobs WHERE deleted_at IS NULL%[2]s),
			(SELECT count(*) FROM jobs WHERE deleted_at IS NULL AND active%[2]s)`

	var jobArgs []any
	projectFilter, jobFilter := "", ""
	if !scope.All {
		jobArgs = append(jobArgs, pq.Array(scope.IDs))
		projectFilter = " AND id = ANY($1)"
		jobFilter = " AND project_id = ANY($1)"
	}
	jobQ = fmt.Sprintf(jobQ, projectFilter, jobFilter)

	if err := r.db.QueryRowContext(ctx, jobQ, jobArgs...).Scan(
		&out.ProjectTotal, &out.JobTotal, &out.JobActive); err != nil {
		return nil, err
	}

	var runArgs []any
	runClause, runArgs := runScope("", scope, runArgs)

	runQ := `SELECT
			count(*) FILTER (WHERE created_at > now() - interval '24 hours'),
			count(*) FILTER (WHERE created_at > now() - interval '24 hours' AND status = 'success'),
			count(*) FILTER (WHERE created_at > now() - interval '24 hours' AND status IN ('failed','timeout')),
			count(*) FILTER (WHERE created_at > now() - interval '24 hours' AND status = 'timeout'),
			count(*) FILTER (WHERE created_at > now() - interval '24 hours' AND status = 'skipped'),
			count(*) FILTER (WHERE status = 'running'),
			count(*) FILTER (WHERE status = 'pending'),
			COALESCE(avg(duration_ms) FILTER (WHERE created_at > now() - interval '24 hours' AND status = 'success'), 0)::int
		FROM job_runs
		WHERE (created_at > now() - interval '24 hours' OR status IN ('running','pending'))` + runClause

	err = r.db.QueryRowContext(ctx, runQ, runArgs...).Scan(&out.DayTotal, &out.DaySuccess, &out.DayFailed,
		&out.DayTimeout, &out.DaySkipped, &out.Running, &out.Pending, &out.AvgDurationMs)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Activity returns per hour run counts over the window, oldest first.
//
// The hour series is generated rather than derived from the rows, so an hour
// with no activity is a zero column and not a missing one. A chart that closes
// its own gaps hides exactly the outage it should be showing.
func (r *StatsRepo) Activity(ctx context.Context, scope domain.ProjectScope, hours int) ([]domain.HourBucket, error) {
	args := []any{clamp(hours, 1, 168)}
	scopeClause, args := runScope("r", scope, args)

	// The scope goes in the JOIN condition, not a WHERE clause. In a WHERE it
	// would filter away the generated hours that have no matching run and turn
	// the gap-free series back into a gap-full one.
	q := `
		WITH series AS (
			SELECT generate_series(
				date_trunc('hour', now()) - make_interval(hours => $1::int - 1),
				date_trunc('hour', now()),
				interval '1 hour'
			) AS hour
		)
		SELECT s.hour,
			count(r.id) FILTER (WHERE r.status = 'success')::int,
			count(r.id) FILTER (WHERE r.status = 'failed')::int,
			count(r.id) FILTER (WHERE r.status = 'timeout')::int,
			count(r.id) FILTER (WHERE r.status = 'skipped')::int
		FROM series s
		LEFT JOIN job_runs r ON date_trunc('hour', r.created_at) = s.hour` + scopeClause + `
		GROUP BY s.hour
		ORDER BY s.hour`

	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []domain.HourBucket
	for rows.Next() {
		var b domain.HourBucket
		if err := rows.Scan(&b.Hour, &b.Success, &b.Failed, &b.Timeout, &b.Skipped); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// SlowestJobs ranks jobs by average duration over the window.
//
// This is the question an operator actually has when a minute is congested:
// not which job failed, but which one is holding the concurrency slots.
func (r *StatsRepo) SlowestJobs(ctx context.Context, scope domain.ProjectScope, hours, limit int) ([]domain.JobDuration, error) {
	args := []any{clamp(hours, 1, 168), clamp(limit, 1, 50)}
	scopeClause, args := jobScope("j", scope, args)

	q := `SELECT j.id, j.code, j.name, p.slug, count(*)::int,
			avg(r.duration_ms)::int, max(r.duration_ms)::int
		FROM job_runs r
		JOIN jobs j ON j.id = r.job_id
		JOIN projects p ON p.id = j.project_id
		WHERE r.created_at > now() - make_interval(hours => $1::int)
		  AND r.duration_ms IS NOT NULL
		  AND r.status IN ('success','failed','timeout')` + scopeClause + `
		GROUP BY j.id, j.code, j.name, p.slug
		ORDER BY avg(r.duration_ms) DESC
		LIMIT $2`

	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []domain.JobDuration
	for rows.Next() {
		var d domain.JobDuration
		if err := rows.Scan(&d.JobID, &d.Code, &d.Name, &d.ProjectSlug, &d.Runs, &d.AvgMs, &d.MaxMs); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// JobHistory returns one job's recent runs, oldest first, for its trend line.
//
// Newest-first in the query and reversed on the way out: an index on
// (job_id, created_at DESC) answers "the last fifty" without sorting the job's
// whole history, and a chart reads left to right.
//
// Skipped runs are kept. A job that is constantly skipping is exactly what the
// chart should show, and dropping them would draw a healthy line over a job
// that has not actually run in a week.
func (r *StatsRepo) JobHistory(ctx context.Context, scope domain.ProjectScope, jobID int64, limit int) ([]domain.RunPoint, error) {
	args := []any{jobID, clamp(limit, 1, 200)}
	scopeClause, args := jobScope("j", scope, args)

	q := `SELECT r.id, r.created_at, r.status,
			COALESCE(r.duration_ms, 0), COALESCE(r.http_status, 0)
		FROM job_runs r
		JOIN jobs j ON j.id = r.job_id
		WHERE r.job_id = $1
		  AND r.status <> 'pending'` + scopeClause + `
		ORDER BY r.created_at DESC
		LIMIT $2`

	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []domain.RunPoint
	for rows.Next() {
		var p domain.RunPoint
		if err := rows.Scan(&p.RunID, &p.At, &p.Status, &p.DurationMs, &p.HTTPStatus); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

// RunningNow lists the runs currently in flight, with how long they have been
// going. It is the dashboard panel that answers "is anything stuck right now".
func (r *StatsRepo) RunningNow(ctx context.Context, scope domain.ProjectScope, limit int) ([]domain.RunRow, error) {
	args := []any{clamp(limit, 1, 100)}
	scopeClause, args := jobScope("j", scope, args)

	q := `SELECT r.id, r.job_id, r.trigger, r.status, r.instance_id, r.started_at,
			j.code, j.name, p.slug, p.name
		FROM job_runs r
		JOIN jobs j ON j.id = r.job_id
		JOIN projects p ON p.id = j.project_id
		WHERE r.status = 'running'` + scopeClause + `
		ORDER BY r.started_at
		LIMIT $1`

	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []domain.RunRow
	for rows.Next() {
		var (
			row     domain.RunRow
			started sql.NullTime
		)
		err := rows.Scan(&row.ID, &row.JobID, &row.Trigger, &row.Status, &row.InstanceID,
			&started, &row.JobCode, &row.JobName, &row.ProjectSlug, &row.ProjectName)
		if err != nil {
			return nil, err
		}
		row.StartedAt = nullTime(started)
		out = append(out, row)
	}
	return out, rows.Err()
}

// RecentFailures lists the latest failed and timed out runs the caller may see.
func (r *StatsRepo) RecentFailures(ctx context.Context, scope domain.ProjectScope, limit int) ([]domain.RunRow, error) {
	args := []any{clamp(limit, 1, 100)}
	scopeClause, args := jobScope("j", scope, args)

	q := `SELECT r.id, r.job_id, r.status, r.http_status, r.duration_ms, r.error, r.created_at,
			j.code, j.name, p.slug, p.name
		FROM job_runs r
		JOIN jobs j ON j.id = r.job_id
		JOIN projects p ON p.id = j.project_id
		WHERE r.status IN ('failed','timeout')` + scopeClause + `
		ORDER BY r.id DESC
		LIMIT $1`

	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []domain.RunRow
	for rows.Next() {
		var (
			row      domain.RunRow
			httpCode sql.NullInt64
			duration sql.NullInt64
		)
		err := rows.Scan(&row.ID, &row.JobID, &row.Status, &httpCode, &duration, &row.Error,
			&row.CreatedAt, &row.JobCode, &row.JobName, &row.ProjectSlug, &row.ProjectName)
		if err != nil {
			return nil, err
		}
		row.HTTPStatus = nullInt(httpCode)
		row.DurationMs = nullInt(duration)
		out = append(out, row)
	}
	return out, rows.Err()
}
