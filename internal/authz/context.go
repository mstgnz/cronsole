package authz

import (
	"context"

	"github.com/mstgnz/cronsole/v2/internal/domain"
	"github.com/mstgnz/cronsole/v2/internal/httpx"
)

// The authenticated user lives under one context key for the whole service,
// the one httpx already owns. A second key here would be a second answer to
// "who is calling", and the two would eventually disagree.

// UserFrom returns the authenticated operator, or nil.
func UserFrom(ctx context.Context) *domain.User { return httpx.UserFrom(ctx) }

// userIDFromContext is what grantz calls to find the caller. It is the one line
// the library cannot write for us, because where a user lives is the host
// application's decision.
func userIDFromContext(ctx context.Context) (int64, bool) {
	user := httpx.UserFrom(ctx)
	if user == nil {
		return 0, false
	}
	return user.ID, true
}
