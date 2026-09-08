package repository

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/mstgnz/cronsole/v2/internal/domain"
)

// PasswordResetRepo is data access for outstanding reset links.
type PasswordResetRepo struct{ *Store }

// NewPasswordResetRepo wires the repository onto a store.
func NewPasswordResetRepo(s *Store) *PasswordResetRepo { return &PasswordResetRepo{s} }

const passwordResetColumns = `id, user_id, token_hash, expires_at, used_at, created_at`

func scanPasswordReset(row interface{ Scan(...any) error }) (*domain.PasswordReset, error) {
	var (
		r      domain.PasswordReset
		usedAt sql.NullTime
	)
	if err := row.Scan(&r.ID, &r.UserID, &r.TokenHash, &r.ExpiresAt, &usedAt, &r.CreatedAt); err != nil {
		return nil, err
	}
	if usedAt.Valid {
		r.UsedAt = &usedAt.Time
	}
	return &r, nil
}

// Create records a new link.
func (r *PasswordResetRepo) Create(ctx context.Context, reset *domain.PasswordReset) (int64, error) {
	const q = `INSERT INTO password_resets (user_id, token_hash, expires_at) VALUES ($1, $2, $3) RETURNING id`
	var id int64
	err := r.db.QueryRowContext(ctx, q, reset.UserID, reset.TokenHash, reset.ExpiresAt).Scan(&id)
	return id, mapWriteErr(err)
}

// FindByTokenHash resolves a link by the hash of the token that was mailed.
//
// Live or not: a spent link and an expired one are refused by the service, and
// they have to be refused the same way an unknown one is.
func (r *PasswordResetRepo) FindByTokenHash(ctx context.Context, tokenHash string) (*domain.PasswordReset, error) {
	q := `SELECT ` + passwordResetColumns + ` FROM password_resets WHERE token_hash = $1`
	reset, err := scanPasswordReset(r.db.QueryRowContext(ctx, q, tokenHash))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return reset, err
}

// MarkUsed spends this link and every other one the account is holding.
//
// One statement rather than two, so there is no moment where the account has
// a new password and an older link that still works. An unused link left live
// after a successful reset is the classic way a stolen mail keeps working.
func (r *PasswordResetRepo) MarkUsed(ctx context.Context, id int64, at time.Time) error {
	const q = `UPDATE password_resets SET used_at = $1
	           WHERE used_at IS NULL
	             AND user_id = (SELECT user_id FROM password_resets WHERE id = $2)`
	_, err := r.db.ExecContext(ctx, q, at, id)
	return err
}

// DeleteExpired removes links that can no longer be used. Retention, run by
// the watchdog sweep alongside the run and log pruning.
func (r *PasswordResetRepo) DeleteExpired(ctx context.Context, before time.Time) (int64, error) {
	const q = `DELETE FROM password_resets WHERE expires_at < $1 OR used_at IS NOT NULL`
	result, err := r.db.ExecContext(ctx, q, before)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}
