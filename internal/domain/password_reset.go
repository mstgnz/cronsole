package domain

import (
	"context"
	"time"
)

// PasswordReset is one outstanding "I forgot my password" link.
//
// TokenHash rather than the token: what was mailed is a bearer credential for
// the account, and this table would otherwise be a list of them. The raw value
// is generated, sent, and never written down.
type PasswordReset struct {
	ID        int64
	UserID    int64
	TokenHash string
	ExpiresAt time.Time
	UsedAt    *time.Time
	CreatedAt time.Time
}

// Live reports whether the link can still be used at the given moment. Both
// halves matter: a link is spent by use and by time, and a check that only
// asks one of them leaves the other open.
func (r PasswordReset) Live(at time.Time) bool {
	return r.UsedAt == nil && at.Before(r.ExpiresAt)
}

// PasswordResetRepository stores the outstanding links.
type PasswordResetRepository interface {
	Create(ctx context.Context, r *PasswordReset) (int64, error)
	// FindByTokenHash returns the row whatever its state, because refusing a
	// spent link and refusing an unknown one have to look the same to the
	// caller and the service is where that decision belongs.
	FindByTokenHash(ctx context.Context, tokenHash string) (*PasswordReset, error)
	// MarkUsed spends this link and every other one the account holds. A reset
	// that leaves the previous links live means a stolen older mail still
	// works after the owner has recovered the account.
	MarkUsed(ctx context.Context, id int64, at time.Time) error
	// DeleteExpired is retention: a spent or expired row is a record of a
	// request, not of an account, and there is no reason to keep it for long.
	DeleteExpired(ctx context.Context, before time.Time) (int64, error)
}
