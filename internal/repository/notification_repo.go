package repository

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/lib/pq"
	"github.com/mstgnz/cronsole/v2/internal/domain"
)

// NotificationRepo is data access for recipient sets.
type NotificationRepo struct{ *Store }

// NewNotificationRepo wires the repository onto a store.
func NewNotificationRepo(s *Store) *NotificationRepo { return &NotificationRepo{s} }

const notificationSelect = `
	SELECT n.id, n.user_id, n.name, n.on_success, n.on_failure, n.active, n.created_at, n.updated_at,
		COALESCE(array_agg(e.email ORDER BY e.email) FILTER (WHERE e.email IS NOT NULL), ARRAY[]::text[])
	FROM notifications n
	LEFT JOIN notify_emails e ON e.notification_id = n.id AND e.active
	WHERE n.deleted_at IS NULL`

func scanNotification(row interface{ Scan(...any) error }) (*domain.Notification, error) {
	var (
		n         domain.Notification
		userID    sql.NullInt64
		updatedAt sql.NullTime
		emails    pq.StringArray
	)
	err := row.Scan(&n.ID, &userID, &n.Name, &n.OnSuccess, &n.OnFailure, &n.Active,
		&n.CreatedAt, &updatedAt, &emails)
	if err != nil {
		return nil, err
	}
	n.UserID = nullInt64(userID)
	n.UpdatedAt = nullTime(updatedAt)
	n.Emails = emails
	return &n, nil
}

// Get reads one recipient set with its addresses.
func (r *NotificationRepo) Get(ctx context.Context, id int64) (*domain.Notification, error) {
	const q = notificationSelect + ` AND n.id = $1
		GROUP BY n.id, n.user_id, n.name, n.on_success, n.on_failure, n.active, n.created_at, n.updated_at`
	n, err := scanNotification(r.db.QueryRowContext(ctx, q, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return n, err
}

// List returns every recipient set.
func (r *NotificationRepo) List(ctx context.Context) ([]domain.Notification, error) {
	const q = notificationSelect + `
		GROUP BY n.id, n.user_id, n.name, n.on_success, n.on_failure, n.active, n.created_at, n.updated_at
		ORDER BY n.name`
	rows, err := r.db.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []domain.Notification
	for rows.Next() {
		n, err := scanNotification(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *n)
	}
	return out, rows.Err()
}

// Create inserts a recipient set and its addresses in one transaction.
func (r *NotificationRepo) Create(ctx context.Context, n *domain.Notification) (int64, error) {
	var id int64
	err := r.tx(ctx, func(tx *sql.Tx) error {
		const q = `INSERT INTO notifications (user_id, name, on_success, on_failure, active)
			VALUES ($1, $2, $3, $4, $5) RETURNING id`
		if err := tx.QueryRowContext(ctx, q, n.UserID, n.Name, n.OnSuccess, n.OnFailure, n.Active).Scan(&id); err != nil {
			return err
		}
		return insertEmails(ctx, tx, id, n.Emails)
	})
	return id, mapWriteErr(err)
}

// Update writes the set and replaces its addresses.
func (r *NotificationRepo) Update(ctx context.Context, n *domain.Notification) error {
	return mapWriteErr(r.tx(ctx, func(tx *sql.Tx) error {
		const q = `UPDATE notifications SET name = $1, on_success = $2, on_failure = $3,
				active = $4, updated_at = now()
			WHERE id = $5 AND deleted_at IS NULL`
		if _, err := tx.ExecContext(ctx, q, n.Name, n.OnSuccess, n.OnFailure, n.Active, n.ID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM notify_emails WHERE notification_id = $1`, n.ID); err != nil {
			return err
		}
		return insertEmails(ctx, tx, n.ID, n.Emails)
	}))
}

func insertEmails(ctx context.Context, tx *sql.Tx, notificationID int64, emails []string) error {
	seen := make(map[string]bool, len(emails))
	for _, e := range emails {
		e = strings.ToLower(strings.TrimSpace(e))
		if e == "" || seen[e] {
			continue
		}
		seen[e] = true
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO notify_emails (notification_id, email, active) VALUES ($1, $2, true)
			 ON CONFLICT (notification_id, lower(email)) DO NOTHING`, notificationID, e); err != nil {
			return err
		}
	}
	return nil
}

// SoftDelete marks a recipient set deleted. Jobs pointing at it keep running;
// the foreign key clears their notification_id, so losing an on-call list
// never stops the work.
func (r *NotificationRepo) SoftDelete(ctx context.Context, id int64, at time.Time) error {
	const q = `UPDATE notifications SET active = false, deleted_at = $1, updated_at = $1 WHERE id = $2`
	_, err := r.db.ExecContext(ctx, q, at, id)
	return err
}

// ReplaceEmails swaps the address list.
func (r *NotificationRepo) ReplaceEmails(ctx context.Context, notificationID int64, emails []string) error {
	return r.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM notify_emails WHERE notification_id = $1`, notificationID); err != nil {
			return err
		}
		return insertEmails(ctx, tx, notificationID, emails)
	})
}

// --- application logs ---

// AppLogRepo is data access for application logs.
type AppLogRepo struct{ *Store }

// NewAppLogRepo wires the repository onto a store.
func NewAppLogRepo(s *Store) *AppLogRepo { return &AppLogRepo{s} }

// List pages through application logs, newest first.
func (r *AppLogRepo) List(ctx context.Context, level string, offset, limit int) ([]domain.AppLog, int64, error) {
	limit = clamp(limit, 1, 200)

	const countQ = `SELECT count(*) FROM app_logs WHERE ($1 = '' OR level = $1)`
	var total int64
	if err := r.db.QueryRowContext(ctx, countQ, level).Scan(&total); err != nil {
		return nil, 0, err
	}

	const q = `SELECT id, level, message, detail, created_at FROM app_logs
		WHERE ($1 = '' OR level = $1) ORDER BY id DESC OFFSET $2 LIMIT $3`
	rows, err := r.db.QueryContext(ctx, q, level, offset, limit)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var out []domain.AppLog
	for rows.Next() {
		var l domain.AppLog
		if err := rows.Scan(&l.ID, &l.Level, &l.Message, &l.Detail, &l.CreatedAt); err != nil {
			return nil, 0, err
		}
		out = append(out, l)
	}
	return out, total, rows.Err()
}

// DeleteBefore prunes application logs by age.
func (r *AppLogRepo) DeleteBefore(ctx context.Context, before time.Time) (int64, error) {
	res, err := r.db.ExecContext(ctx, `DELETE FROM app_logs WHERE created_at < $1`, before)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
