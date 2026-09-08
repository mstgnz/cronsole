package repository

import (
	"context"
	"database/sql"
	"errors"

	"github.com/mstgnz/cronsole/v2/internal/domain"
)

// HostOverrideRepo is data access for the dialer's host routes.
type HostOverrideRepo struct{ *Store }

// NewHostOverrideRepo wires the repository onto a store.
func NewHostOverrideRepo(s *Store) *HostOverrideRepo { return &HostOverrideRepo{s} }

// host(address) renders inet as text without the mask that inet carries, so a
// value read back is the literal that was written and can go straight into a
// dial address.
const hostOverrideSelect = `
	SELECT id, hostname, host(address), port, note, active, created_by, created_at, updated_at
	FROM host_overrides`

func scanHostOverride(row interface{ Scan(...any) error }) (*domain.HostOverride, error) {
	var (
		o         domain.HostOverride
		port      sql.NullInt64
		createdBy sql.NullInt64
	)
	err := row.Scan(&o.ID, &o.Hostname, &o.Address, &port, &o.Note, &o.Active,
		&createdBy, &o.CreatedAt, &o.UpdatedAt)
	if err != nil {
		return nil, err
	}
	if port.Valid {
		p := int(port.Int64)
		o.Port = &p
	}
	o.CreatedBy = nullInt64(createdBy)
	return &o, nil
}

func collectHostOverrides(rows *sql.Rows) ([]domain.HostOverride, error) {
	defer func() { _ = rows.Close() }()

	out := []domain.HostOverride{}
	for rows.Next() {
		o, err := scanHostOverride(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *o)
	}
	return out, rows.Err()
}

// List returns every route, including the disabled ones.
//
// Ordered so a row naming a port sorts before the "every port" row for the same
// hostname, which is the order they are applied in.
func (r *HostOverrideRepo) List(ctx context.Context) ([]domain.HostOverride, error) {
	const q = hostOverrideSelect + `
		ORDER BY lower(hostname), coalesce(port, 0) DESC`
	rows, err := r.db.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	return collectHostOverrides(rows)
}

// ListActive returns the routes the dialer should apply.
func (r *HostOverrideRepo) ListActive(ctx context.Context) ([]domain.HostOverride, error) {
	const q = hostOverrideSelect + `
		WHERE active
		ORDER BY lower(hostname), coalesce(port, 0) DESC`
	rows, err := r.db.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	return collectHostOverrides(rows)
}

// Get reads one route.
func (r *HostOverrideRepo) Get(ctx context.Context, id int64) (*domain.HostOverride, error) {
	const q = hostOverrideSelect + ` WHERE id = $1`
	o, err := scanHostOverride(r.db.QueryRowContext(ctx, q, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return o, err
}

// Create stores a route.
//
// A duplicate hostname and port is refused by the unique index and returned as
// ErrDuplicate rather than as a driver error: read-then-write is a race, and
// the constraint is the thing that actually holds.
func (r *HostOverrideRepo) Create(ctx context.Context, o *domain.HostOverride) (int64, error) {
	const q = `
		INSERT INTO host_overrides (hostname, address, port, note, active, created_by)
		VALUES ($1, $2::inet, $3, $4, $5, $6)
		RETURNING id`

	var id int64
	err := r.db.QueryRowContext(ctx, q,
		o.Hostname, o.Address, o.Port, o.Note, o.Active, o.CreatedBy).Scan(&id)
	if err != nil {
		return 0, mapWriteErr(err)
	}
	return id, nil
}

// Update changes a route.
func (r *HostOverrideRepo) Update(ctx context.Context, o *domain.HostOverride) error {
	const q = `
		UPDATE host_overrides
		SET hostname = $2, address = $3::inet, port = $4, note = $5, active = $6, updated_at = now()
		WHERE id = $1`

	res, err := r.db.ExecContext(ctx, q, o.ID, o.Hostname, o.Address, o.Port, o.Note, o.Active)
	if err != nil {
		return mapWriteErr(err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// Delete removes a route.
func (r *HostOverrideRepo) Delete(ctx context.Context, id int64) error {
	res, err := r.db.ExecContext(ctx, `DELETE FROM host_overrides WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
