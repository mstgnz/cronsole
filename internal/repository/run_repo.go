package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/lib/pq"
	"github.com/mstgnz/cronsole/v2/internal/domain"
)

// RunRepo is data access for execution records from the management side. The
// execution path uses ExecRepo, which is deliberately a much narrower surface.
type RunRepo struct{ *Store }

// NewRunRepo wires the repository onto a store.
func NewRunRepo(s *Store) *RunRepo { return &RunRepo{s} }

const runColumns = `r.id, r.job_id, r.planned_minute, r.run_after, r.trigger, r.user_id,
	r.parent_run_id, r.status, r.attempt, r.instance_id, r.started_at, r.finished_at,
	r.duration_ms, r.http_status, r.request_url, r.output, r.error, r.created_at`

func scanRunRow(row interface{ Scan(...any) error }) (*domain.RunRow, error) {
	var (
		out      domain.RunRow
		planned  sql.NullTime
		runAfter sql.NullTime
		userID   sql.NullInt64
		parentID sql.NullInt64
		started  sql.NullTime
		finished sql.NullTime
		duration sql.NullInt64
		httpCode sql.NullInt64
	)
	err := row.Scan(&out.ID, &out.JobID, &planned, &runAfter, &out.Trigger, &userID,
		&parentID, &out.Status, &out.Attempt, &out.InstanceID, &started, &finished,
		&duration, &httpCode, &out.RequestURL, &out.Output, &out.Error, &out.CreatedAt,
		&out.JobCode, &out.JobName, &out.ProjectSlug, &out.ProjectName)
	if err != nil {
		return nil, err
	}
	out.PlannedMinute = nullTime(planned)
	out.RunAfter = nullTime(runAfter)
	out.UserID = nullInt64(userID)
	out.ParentRunID = nullInt64(parentID)
	out.StartedAt = nullTime(started)
	out.FinishedAt = nullTime(finished)
	out.DurationMs = nullInt(duration)
	out.HTTPStatus = nullInt(httpCode)
	return &out, nil
}

const runJoin = `FROM job_runs r
	JOIN jobs j ON j.id = r.job_id
	JOIN projects p ON p.id = j.project_id`

const runSelect = `SELECT ` + runColumns + `, j.code, j.name, p.slug, p.name ` + runJoin

// Get reads one run with its job identity.
func (r *RunRepo) Get(ctx context.Context, id int64) (*domain.RunRow, error) {
	row, err := scanRunRow(r.db.QueryRowContext(ctx, runSelect+` WHERE r.id = $1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return row, err
}

// runFilterClauses turns a filter into SQL fragments and bound values. Every
// fragment is a literal; only values are bound.
func runFilterClauses(f domain.RunFilter, args []any) ([]string, []any) {
	var where []string

	// See jobFilterClauses: applied whenever the scope is not "all", including
	// when the id list is empty, because empty means "reaches nothing".
	if !f.Scope.All {
		args = append(args, pq.Array(f.Scope.IDs))
		where = append(where, fmt.Sprintf("j.project_id = ANY($%d)", len(args)))
	}

	if f.JobID != nil {
		args = append(args, *f.JobID)
		where = append(where, fmt.Sprintf("r.job_id = $%d", len(args)))
	}
	if f.ProjectID != nil {
		args = append(args, *f.ProjectID)
		where = append(where, fmt.Sprintf("j.project_id = $%d", len(args)))
	}
	if f.Status != nil && *f.Status != "" {
		args = append(args, *f.Status)
		where = append(where, fmt.Sprintf("r.status = $%d", len(args)))
	}
	if f.Trigger != nil && *f.Trigger != "" {
		args = append(args, *f.Trigger)
		where = append(where, fmt.Sprintf("r.trigger = $%d", len(args)))
	}
	if f.Start != nil {
		args = append(args, *f.Start)
		where = append(where, fmt.Sprintf("r.created_at >= $%d", len(args)))
	}
	if f.End != nil {
		args = append(args, *f.End)
		where = append(where, fmt.Sprintf("r.created_at <= $%d", len(args)))
	}
	if f.Search != nil && *f.Search != "" {
		args = append(args, "%"+*f.Search+"%")
		where = append(where, fmt.Sprintf("(j.code ILIKE $%d OR j.name ILIKE $%d OR r.error ILIKE $%d)",
			len(args), len(args), len(args)))
	}
	return where, args
}

// List pages through runs, newest first.
func (r *RunRepo) List(ctx context.Context, f domain.RunFilter, offset, limit int) ([]domain.RunRow, int64, error) {
	limit = clamp(limit, 1, 500)

	where := []string{"true"}
	var args []any
	extra, args := runFilterClauses(f, args)
	where = append(where, extra...)
	whereSQL := strings.Join(where, " AND ")

	countQ := `SELECT count(*) ` + runJoin + ` WHERE ` + whereSQL
	var total int64
	if err := r.db.QueryRowContext(ctx, countQ, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	q := runSelect + ` WHERE ` + whereSQL +
		` ORDER BY r.id DESC OFFSET $` + fmt.Sprint(len(args)+1) + ` LIMIT $` + fmt.Sprint(len(args)+2)
	args = append(args, offset, limit)

	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var out []domain.RunRow
	for rows.Next() {
		row, err := scanRunRow(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, *row)
	}
	return out, total, rows.Err()
}

// Recent returns the last n runs of a job, for the sparkline on the job form.
func (r *RunRepo) Recent(ctx context.Context, jobID int64, n int) ([]domain.Run, error) {
	n = clamp(n, 1, 100)
	const q = `SELECT id, status, duration_ms, http_status, started_at, created_at
		FROM job_runs WHERE job_id = $1 ORDER BY id DESC LIMIT $2`
	rows, err := r.db.QueryContext(ctx, q, jobID, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []domain.Run
	for rows.Next() {
		var (
			run      domain.Run
			duration sql.NullInt64
			httpCode sql.NullInt64
			started  sql.NullTime
		)
		if err := rows.Scan(&run.ID, &run.Status, &duration, &httpCode, &started, &run.CreatedAt); err != nil {
			return nil, err
		}
		run.JobID = jobID
		run.DurationMs = nullInt(duration)
		run.HTTPStatus = nullInt(httpCode)
		run.StartedAt = nullTime(started)
		out = append(out, run)
	}
	return out, rows.Err()
}

// Children returns the runs this one triggered, so a chain can be walked
// forward from any point.
func (r *RunRepo) Children(ctx context.Context, runID int64) ([]domain.RunRow, error) {
	rows, err := r.db.QueryContext(ctx, runSelect+` WHERE r.parent_run_id = $1 ORDER BY r.id`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []domain.RunRow
	for rows.Next() {
		row, err := scanRunRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *row)
	}
	return out, rows.Err()
}

// Enqueue opens a pending row outside the dispatcher.
//
// The "run now" button and the API trigger both land here, and neither of them
// executes anything directly: they queue a row and let the ordinary path pick
// it up. That is what keeps a manual run subject to the same concurrency cap,
// the same single_run rule and the same logging as a scheduled one.
func (r *RunRepo) Enqueue(ctx context.Context, jobID int64, trigger string, userID *int64) (int64, error) {
	const q = `INSERT INTO job_runs (job_id, trigger, user_id, status) VALUES ($1, $2, $3, 'pending') RETURNING id`
	var id int64
	err := r.db.QueryRowContext(ctx, q, jobID, trigger, userID).Scan(&id)
	return id, mapWriteErr(err)
}
