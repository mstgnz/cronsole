package repository

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/mstgnz/cronsole/v2/internal/domain"
)

// UserRepo is data access for accounts.
type UserRepo struct{ *Store }

// NewUserRepo wires the repository onto a store.
func NewUserRepo(s *Store) *UserRepo { return &UserRepo{s} }

const userColumns = `id, fullname, email, password, phone, is_admin, active,
	last_login, tokens_valid_after, created_at, updated_at`

func scanUser(row interface{ Scan(...any) error }) (*domain.User, error) {
	var (
		u          domain.User
		lastLogin  sql.NullTime
		validAfter sql.NullTime
		updatedAt  sql.NullTime
	)
	err := row.Scan(&u.ID, &u.Fullname, &u.Email, &u.Password, &u.Phone, &u.IsAdmin,
		&u.Active, &lastLogin, &validAfter, &u.CreatedAt, &updatedAt)
	if err != nil {
		return nil, err
	}
	u.LastLogin = nullTime(lastLogin)
	u.TokensValidAfter = nullTime(validAfter)
	u.UpdatedAt = nullTime(updatedAt)
	return &u, nil
}

// GetByID reads one account.
func (r *UserRepo) GetByID(ctx context.Context, id int64) (*domain.User, error) {
	const q = `SELECT ` + userColumns + ` FROM users WHERE id = $1 AND deleted_at IS NULL`
	u, err := scanUser(r.db.QueryRowContext(ctx, q, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return u, err
}

// GetByEmail reads one account by address. The lookup is case insensitive
// because the unique index is, and a mismatch between the two would let a
// second account be created under a different casing.
func (r *UserRepo) GetByEmail(ctx context.Context, email string) (*domain.User, error) {
	const q = `SELECT ` + userColumns + ` FROM users WHERE lower(email) = lower($1) AND deleted_at IS NULL`
	u, err := scanUser(r.db.QueryRowContext(ctx, q, email))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return u, err
}

// List pages through accounts, optionally filtered by a search term.
func (r *UserRepo) List(ctx context.Context, search string, offset, limit int) ([]domain.User, int64, error) {
	limit = clamp(limit, 1, 200)
	pattern := "%" + search + "%"

	const countQ = `SELECT count(*) FROM users
		WHERE deleted_at IS NULL AND ($1 = '' OR fullname ILIKE $2 OR email ILIKE $2 OR phone ILIKE $2)`
	var total int64
	if err := r.db.QueryRowContext(ctx, countQ, search, pattern).Scan(&total); err != nil {
		return nil, 0, err
	}

	const q = `SELECT ` + userColumns + ` FROM users
		WHERE deleted_at IS NULL AND ($1 = '' OR fullname ILIKE $2 OR email ILIKE $2 OR phone ILIKE $2)
		ORDER BY id DESC OFFSET $3 LIMIT $4`
	rows, err := r.db.QueryContext(ctx, q, search, pattern, offset, limit)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var out []domain.User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, *u)
	}
	return out, total, rows.Err()
}

// Count reports live accounts, and is what decides whether the setup screen is
// still open. Soft-deleted rows are not counted; nobody can delete their own
// account, so a deployment cannot be emptied back into its first run.
func (r *UserRepo) Count(ctx context.Context) (int64, error) {
	var n int64
	err := r.db.QueryRowContext(ctx, `SELECT count(*) FROM users WHERE deleted_at IS NULL`).Scan(&n)
	return n, err
}

// Create inserts an account and returns its id.
func (r *UserRepo) Create(ctx context.Context, u *domain.User) (int64, error) {
	const q = `INSERT INTO users (fullname, email, password, phone, is_admin, active)
		VALUES ($1, $2, $3, $4, $5, $6) RETURNING id`
	var id int64
	err := r.db.QueryRowContext(ctx, q, u.Fullname, u.Email, u.Password, u.Phone, u.IsAdmin, u.Active).Scan(&id)
	return id, mapWriteErr(err)
}

// CreateFirstAdmin creates the account only while the table holds none.
//
// ONE statement, because the check and the insert have to be the same
// operation. Reading the count and then inserting is a race with a stranger as
// the prize: two requests both see an empty table, both create an
// administrator, and the second one is somebody who found the URL. The database
// decides here, and ErrForbidden is what it decided.
func (r *UserRepo) CreateFirstAdmin(ctx context.Context, u *domain.User) (int64, error) {
	const q = `INSERT INTO users (fullname, email, password, phone, is_admin, active)
		SELECT $1, $2, $3, $4, true, true
		WHERE NOT EXISTS (SELECT 1 FROM users WHERE deleted_at IS NULL)
		RETURNING id`

	var id int64
	err := r.db.QueryRowContext(ctx, q, u.Fullname, u.Email, u.Password, u.Phone).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		// The WHERE NOT EXISTS matched nothing, so somebody already holds the
		// first account. Not a duplicate address: the table simply is not empty.
		return 0, ErrForbidden
	}
	return id, mapWriteErr(err)
}

// UpdateProfile writes the fields an admin may change.
//
// The column list is explicit rather than a struct update, so a field the user
// must never set from a request body (password, is_admin on self service,
// tokens_valid_after) cannot arrive here by accident.
func (r *UserRepo) UpdateProfile(ctx context.Context, u *domain.User) error {
	const q = `UPDATE users SET fullname = $1, email = $2, phone = $3, is_admin = $4,
		active = $5, updated_at = now() WHERE id = $6 AND deleted_at IS NULL`
	_, err := r.db.ExecContext(ctx, q, u.Fullname, u.Email, u.Phone, u.IsAdmin, u.Active, u.ID)
	return mapWriteErr(err)
}

// UpdatePassword stores a new hash and retires every token issued before now,
// in one statement so a session cannot survive the change.
func (r *UserRepo) UpdatePassword(ctx context.Context, id int64, hash string, at time.Time) error {
	const q = `UPDATE users SET password = $1, updated_at = $2, tokens_valid_after = $2 WHERE id = $3`
	_, err := r.db.ExecContext(ctx, q, hash, at, id)
	return err
}

// InvalidateTokens ends every existing session for the account.
func (r *UserRepo) InvalidateTokens(ctx context.Context, id int64, at time.Time) error {
	const q = `UPDATE users SET tokens_valid_after = $1 WHERE id = $2`
	_, err := r.db.ExecContext(ctx, q, at, id)
	return err
}

// TouchLogin records a successful sign in.
func (r *UserRepo) TouchLogin(ctx context.Context, id int64, at time.Time) error {
	const q = `UPDATE users SET last_login = $1 WHERE id = $2`
	_, err := r.db.ExecContext(ctx, q, at, id)
	return err
}

// SoftDelete deactivates an account and marks it deleted. The row stays
// because job_runs.user_id points at it and the history of who triggered what
// has to survive the account.
func (r *UserRepo) SoftDelete(ctx context.Context, id int64, at time.Time) error {
	const q = `UPDATE users SET active = false, deleted_at = $1, updated_at = $1, tokens_valid_after = $1 WHERE id = $2`
	_, err := r.db.ExecContext(ctx, q, at, id)
	return err
}
