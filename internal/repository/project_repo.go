package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/lib/pq"
	"github.com/mstgnz/cronsole/v2/internal/domain"
)

// ProjectRepo is data access for projects.
type ProjectRepo struct{ *Store }

// NewProjectRepo wires the repository onto a store.
func NewProjectRepo(s *Store) *ProjectRepo { return &ProjectRepo{s} }

const projectColumns = `id, name, slug, description, base_url, api_key_prefix,
	api_key_hash, active, user_id, created_at, updated_at`

func scanProject(row interface{ Scan(...any) error }) (*domain.Project, error) {
	var (
		p         domain.Project
		userID    sql.NullInt64
		updatedAt sql.NullTime
	)
	err := row.Scan(&p.ID, &p.Name, &p.Slug, &p.Description, &p.BaseURL, &p.KeyPrefix,
		&p.KeyHash, &p.Active, &userID, &p.CreatedAt, &updatedAt)
	if err != nil {
		return nil, err
	}
	p.UserID = nullInt64(userID)
	p.UpdatedAt = nullTime(updatedAt)
	return &p, nil
}

// GetByID reads one project.
func (r *ProjectRepo) GetByID(ctx context.Context, id int64) (*domain.Project, error) {
	const q = `SELECT ` + projectColumns + ` FROM projects WHERE id = $1 AND deleted_at IS NULL`
	p, err := scanProject(r.db.QueryRowContext(ctx, q, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return p, err
}

// GetBySlug reads one project by its machine name.
func (r *ProjectRepo) GetBySlug(ctx context.Context, slug string) (*domain.Project, error) {
	const q = `SELECT ` + projectColumns + ` FROM projects WHERE slug = $1 AND deleted_at IS NULL`
	p, err := scanProject(r.db.QueryRowContext(ctx, q, slug))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return p, err
}

// GetByKeyPrefix finds the candidate project for an API key.
//
// The prefix narrows the search to one row; it authenticates nothing. The
// caller still compares the full hash in constant time, which is where the
// decision actually happens.
func (r *ProjectRepo) GetByKeyPrefix(ctx context.Context, prefix string) (*domain.Project, error) {
	const q = `SELECT ` + projectColumns + ` FROM projects
		WHERE api_key_prefix = $1 AND api_key_prefix <> '' AND deleted_at IS NULL`
	p, err := scanProject(r.db.QueryRowContext(ctx, q, prefix))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return p, err
}

// List returns every project with the counters the list screen shows.
//
// The counters are lateral subqueries rather than a join with GROUP BY,
// because a project with no jobs still has to appear with zeros, and because
// the run counts have their own time window.
func (r *ProjectRepo) List(ctx context.Context, scope domain.ProjectScope, search string) ([]domain.ProjectRow, error) {
	scopeClause := ""
	args := []any{search, "%" + search + "%"}
	if !scope.All {
		args = append(args, pq.Array(scope.IDs))
		scopeClause = fmt.Sprintf(" AND p.id = ANY($%d)", len(args))
	}

	q := `
		SELECT ` + projectColumns + `,
			COALESCE(j.total, 0), COALESCE(j.active_total, 0),
			COALESCE(rc.success, 0), COALESCE(rc.failed, 0)
		FROM projects p
		LEFT JOIN LATERAL (
			SELECT count(*) AS total, count(*) FILTER (WHERE active) AS active_total
			FROM jobs WHERE project_id = p.id AND deleted_at IS NULL
		) j ON true
		LEFT JOIN LATERAL (
			SELECT count(*) FILTER (WHERE r.status = 'success') AS success,
			       count(*) FILTER (WHERE r.status IN ('failed', 'timeout')) AS failed
			FROM job_runs r
			JOIN jobs jj ON jj.id = r.job_id
			WHERE jj.project_id = p.id AND r.created_at > now() - interval '24 hours'
		) rc ON true
		WHERE p.deleted_at IS NULL
		  AND ($1 = '' OR p.name ILIKE $2 OR p.slug ILIKE $2)` + scopeClause + `
		ORDER BY p.name`

	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []domain.ProjectRow
	for rows.Next() {
		var (
			row       domain.ProjectRow
			userID    sql.NullInt64
			updatedAt sql.NullTime
		)
		err := rows.Scan(&row.ID, &row.Name, &row.Slug, &row.Description, &row.BaseURL,
			&row.KeyPrefix, &row.KeyHash, &row.Active, &userID, &row.CreatedAt, &updatedAt,
			&row.JobTotal, &row.JobActive, &row.DaySuccess, &row.DayFailed)
		if err != nil {
			return nil, err
		}
		row.UserID = nullInt64(userID)
		row.UpdatedAt = nullTime(updatedAt)
		out = append(out, row)
	}
	return out, rows.Err()
}

// ListNames feeds the filter dropdown: identity only, no counters.
//
// Scoped like everything else. A dropdown naming projects the caller cannot
// open tells them those projects exist, and is also how somebody discovers an
// id worth trying in a URL.
func (r *ProjectRepo) ListNames(ctx context.Context, scope domain.ProjectScope) ([]domain.Project, error) {
	q := `SELECT id, name, slug, active FROM projects WHERE deleted_at IS NULL`
	var args []any
	if !scope.All {
		args = append(args, pq.Array(scope.IDs))
		q += fmt.Sprintf(" AND id = ANY($%d)", len(args))
	}
	q += ` ORDER BY name`

	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []domain.Project
	for rows.Next() {
		var p domain.Project
		if err := rows.Scan(&p.ID, &p.Name, &p.Slug, &p.Active); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// Create inserts a project and returns its id.
func (r *ProjectRepo) Create(ctx context.Context, p *domain.Project) (int64, error) {
	const q = `INSERT INTO projects (name, slug, description, base_url, api_key_prefix,
			api_key_hash, active, user_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8) RETURNING id`
	var id int64
	err := r.db.QueryRowContext(ctx, q, p.Name, p.Slug, p.Description, p.BaseURL,
		p.KeyPrefix, p.KeyHash, p.Active, p.UserID).Scan(&id)
	return id, mapWriteErr(err)
}

// Update writes the editable fields. The API key is not among them: rotating a
// key is a separate action with its own confirmation, so it can never be a
// side effect of saving a form.
func (r *ProjectRepo) Update(ctx context.Context, p *domain.Project) error {
	const q = `UPDATE projects SET name = $1, slug = $2, description = $3, base_url = $4,
			active = $5, updated_at = now()
		WHERE id = $6 AND deleted_at IS NULL`
	_, err := r.db.ExecContext(ctx, q, p.Name, p.Slug, p.Description, p.BaseURL, p.Active, p.ID)
	return mapWriteErr(err)
}

// SetKey stores a rotated API key.
func (r *ProjectRepo) SetKey(ctx context.Context, id int64, prefix, hash string) error {
	const q = `UPDATE projects SET api_key_prefix = $1, api_key_hash = $2, updated_at = now() WHERE id = $3`
	_, err := r.db.ExecContext(ctx, q, prefix, hash, id)
	return err
}

// SoftDelete marks a project deleted. Its jobs go with it, which is why the
// service refuses when the project still has active jobs rather than leaving
// that decision to a cascade.
func (r *ProjectRepo) SoftDelete(ctx context.Context, id int64, at time.Time) error {
	return r.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`UPDATE jobs SET active = false, deleted_at = $1, updated_at = $1 WHERE project_id = $2 AND deleted_at IS NULL`,
			at, id); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx,
			`UPDATE projects SET active = false, deleted_at = $1, updated_at = $1 WHERE id = $2`, at, id)
		return err
	})
}
