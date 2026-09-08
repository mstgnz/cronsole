package domain

import (
	"context"
	"time"
)

// User is an operator account. Accounts are created by an admin; there is no
// public sign up, so nothing here is self service.
type User struct {
	ID               int64      `json:"id"`
	Fullname         string     `json:"fullname"`
	Email            string     `json:"email"`
	Password         string     `json:"-"`
	Phone            string     `json:"phone"`
	IsAdmin          bool       `json:"is_admin"`
	Active           bool       `json:"active"`
	LastLogin        *time.Time `json:"last_login,omitempty"`
	TokensValidAfter *time.Time `json:"-"`
	CreatedAt        time.Time  `json:"created_at"`
	UpdatedAt        *time.Time `json:"updated_at,omitempty"`
}

// TokenRetired reports whether a token minted at issuedAt predates the user's
// last logout or password change.
//
// JWTs cannot be withdrawn once issued, so this cut-off is the only thing that
// makes those two actions end existing sessions. The comparison is strict:
// the iat claim has second precision and the cut-off does not, so a token
// minted in the same second as a logout counts as retired. Asking that user to
// sign in again is the cheap failure; honouring a token that should be gone is
// not.
func (u *User) TokenRetired(issuedAt time.Time) bool {
	if u.TokensValidAfter == nil {
		return false
	}
	return issuedAt.Before(*u.TokensValidAfter)
}

// UserRepository is data access for accounts.
type UserRepository interface {
	GetByID(ctx context.Context, id int64) (*User, error)
	GetByEmail(ctx context.Context, email string) (*User, error)
	List(ctx context.Context, search string, offset, limit int) ([]User, int64, error)
	Count(ctx context.Context) (int64, error)
	Create(ctx context.Context, u *User) (int64, error)
	// CreateFirstAdmin creates the account only while the table holds none.
	// The check and the insert are one statement, because two requests both
	// finding an empty table is how a stranger becomes the administrator.
	CreateFirstAdmin(ctx context.Context, u *User) (int64, error)
	UpdateProfile(ctx context.Context, u *User) error
	UpdatePassword(ctx context.Context, id int64, hash string, at time.Time) error
	InvalidateTokens(ctx context.Context, id int64, at time.Time) error
	TouchLogin(ctx context.Context, id int64, at time.Time) error
	SoftDelete(ctx context.Context, id int64, at time.Time) error
}
