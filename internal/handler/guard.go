package handler

import (
	"errors"
	"net/http"

	"github.com/mstgnz/cronsole/v2/internal/authz"
	"github.com/mstgnz/cronsole/v2/internal/domain"
	"github.com/mstgnz/cronsole/v2/internal/httpx"
	"github.com/mstgnz/cronsole/v2/internal/i18n"
	"github.com/mstgnz/cronsole/v2/internal/service"
)

// guard resolves what the caller may reach, and answers when they may not.
//
// Every handler that touches project-owned data goes through it. The pattern is
// deliberately two steps: this resolves the SCOPE, and the service then decides
// per row. Middleware in front of the route has already answered the verb, but
// middleware runs before a row is read and cannot answer "this one".
type guard struct {
	authz *authz.Service
}

// scope resolves the caller's project scope for a permission.
//
// The second return is false when the request has already been answered. A
// caller who does not hold the permission gets an EMPTY scope rather than an
// error, so a list renders as empty instead of failing: the screens are shared,
// and a reader opening the runs page should see their own runs, not a 403.
func (g guard) scope(w http.ResponseWriter, r *http.Request, key string) (domain.ProjectScope, bool) {
	user := httpx.UserFrom(r.Context())
	if user == nil {
		httpx.Fail(w, http.StatusUnauthorized, tr(r, "authentication required"))
		return domain.NoProjects(), false
	}

	scope, err := g.authz.Scope(r.Context(), user, key)
	if err != nil {
		// A store error resolves to denied, never allowed. A database blip must
		// not open every endpoint at once.
		httpx.Fail(w, http.StatusInternalServerError, tr(r, "permissions could not be read"))
		return domain.NoProjects(), false
	}
	return scope, true
}

// require answers the request unless the caller holds the permission somewhere.
//
// Used where an empty result would be misleading rather than correct: a save
// button that silently does nothing is worse than a refusal.
func (g guard) require(w http.ResponseWriter, r *http.Request, key string) (domain.ProjectScope, bool) {
	scope, ok := g.scope(w, r, key)
	if !ok {
		return scope, false
	}
	if scope.Empty() {
		httpx.Fail(w, http.StatusForbidden, tr(r, "you do not have permission to do that"))
		return scope, false
	}
	return scope, true
}

// user is the authenticated operator, or nil.
func (g guard) user(r *http.Request) *domain.User { return httpx.UserFrom(r.Context()) }

// permissions is what the interface draws itself from, resolved once per
// request by middleware. A convenience, never an authority: every endpoint
// still checks for itself.
func (g guard) permissions(r *http.Request) map[string]bool {
	return httpx.PermissionsFrom(r.Context())
}

// tr translates a message for this request's language.
//
// For the SCREENS only. The JSON API deliberately keeps its messages in English:
// a client parsing or logging them wants a stable string, and an error that
// changes wording with an Accept-Language header is an error nobody can grep
// for. api_handler.go therefore does not use this.
func tr(r *http.Request, text string, args ...any) string {
	return i18n.T(httpx.LangFrom(r.Context()), text, args...)
}

// denied maps a service error to a response, and reports whether it did.
//
// ErrNotFound and a scope refusal are the same answer on purpose: telling
// somebody that a job exists but is not theirs confirms an id, and an id is the
// only thing needed to probe the rest.
func denied(w http.ResponseWriter, r *http.Request, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, service.ErrNotFound):
		httpx.Fail(w, http.StatusNotFound, tr(r, "not found"))
	case errors.Is(err, service.ErrForbidden), errors.Is(err, authz.ErrDenied):
		httpx.Fail(w, http.StatusForbidden, tr(r, "you do not have permission to do that"))
	case errors.Is(err, authz.ErrNoUser):
		httpx.Fail(w, http.StatusUnauthorized, tr(r, "authentication required"))
	default:
		return false
	}
	return true
}
