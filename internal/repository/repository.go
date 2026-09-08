// Package repository is the only place that speaks SQL.
//
// Queries are Go constants next to the method that runs them, not entries in a
// shared file: a query and its scan have to change together, and splitting
// them is how a column gets added to one and forgotten in the other.
//
// Every statement is parameterised. There is no path in this package that
// concatenates a caller supplied value into SQL; where a filter set is
// dynamic, the clauses are appended from a fixed set of literals and only the
// values are bound.
package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrNotFound is returned when a lookup by identity finds nothing. It is a
// sentinel so the service layer can map it to a 404 without inspecting driver
// errors.
var ErrNotFound = errors.New("record not found")

// ErrDuplicate is returned when a unique constraint rejects a write. The
// service turns it into a message naming the field, so uniqueness is enforced
// by the database and merely explained by the application.
var ErrDuplicate = errors.New("record already exists")

// ErrForbidden is returned when a conditional write matched no row because the
// condition was not met, rather than because the row was missing. Only the
// first-administrator insert uses it: the table was not empty, which is a
// refusal and not a lookup that failed.
var ErrForbidden = errors.New("the write was not permitted")

// uniqueViolation is the SQLSTATE for a unique constraint failure.
const uniqueViolation = "23505"

// Store owns the connection pool and is embedded by every repository.
type Store struct {
	db *sql.DB
}

// New wires a store onto an open pool.
func New(db *sql.DB) *Store { return &Store{db: db} }

// DB exposes the pool for the few callers that legitimately need it, such as
// the health check.
func (s *Store) DB() *sql.DB { return s.db }

// tx runs fn inside a transaction, rolling back on error or panic.
//
// A raw Begin with a deferred Rollback works too, and is exactly the shape
// that gets copied once with the defer left out. One helper means one place
// where the rollback can be missing, and it is not missing here.
func (s *Store) tx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback()
			panic(p)
		}
	}()
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// isDuplicate reports whether an error is a unique constraint violation.
//
// The check is on the SQLSTATE, reached through the driver's own error
// interface rather than a type assertion on *pq.Error, so the repository does
// not have to name the driver in every method.
func isDuplicate(err error) bool {
	if err == nil {
		return false
	}
	var coded interface{ SQLState() string }
	if errors.As(err, &coded) {
		return coded.SQLState() == uniqueViolation
	}
	// Older lib/pq releases expose the code on a Code field rather than
	// through SQLState, so fall back to the text form.
	return strings.Contains(err.Error(), uniqueViolation)
}

// mapWriteErr turns a duplicate key error into the sentinel and leaves
// everything else alone.
func mapWriteErr(err error) error {
	if isDuplicate(err) {
		return ErrDuplicate
	}
	return err
}

// nullTime converts a nullable timestamp for scanning.
func nullTime(t sql.NullTime) *time.Time {
	if !t.Valid {
		return nil
	}
	v := t.Time
	return &v
}

// nullInt64 converts a nullable bigint for scanning.
func nullInt64(v sql.NullInt64) *int64 {
	if !v.Valid {
		return nil
	}
	out := v.Int64
	return &out
}

// nullInt converts a nullable integer for scanning.
func nullInt(v sql.NullInt64) *int {
	if !v.Valid {
		return nil
	}
	out := int(v.Int64)
	return &out
}

// clamp keeps a page size inside sane bounds. An unbounded limit on a table
// that grows with every execution is a denial of service with extra steps.
func clamp(v, min, max int) int {
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}

// truncate cuts a string to n runes. Cutting on bytes would split a multi byte
// character and produce invalid UTF-8 in the column.
func truncate(s string, n int) string {
	if n <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + fmt.Sprintf(" ... [%d characters truncated]", len(r)-n)
}
