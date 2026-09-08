// Package middleware holds the request filters: who is calling, what they may
// reach, and what must be recorded when something goes wrong.
package middleware

import (
	"errors"
	"fmt"
	"net/http"
	"runtime/debug"
	"strings"
	"sync/atomic"

	"github.com/mstgnz/cronsole/v2/internal/applog"
	"github.com/mstgnz/cronsole/v2/internal/authz"
	"github.com/mstgnz/cronsole/v2/internal/httpx"
	"github.com/mstgnz/cronsole/v2/internal/service"
	"github.com/mstgnz/cronsole/v2/pkg/auth"
)

// CookieName is where the browser session token lives.
//
// A cookie, not localStorage. A token in localStorage is readable by any
// script that reaches the page, so one cross site scripting flaw becomes every
// account; an HttpOnly cookie is not readable by script at all.
const CookieName = "cj_session"

// Set holds the middleware, so the router does not have to build each one with
// its dependencies at the point of use.
type Set struct {
	auth     *service.AuthService
	projects *service.ProjectService
	authz    *authz.Service
	log      *applog.Logger
	secure   bool

	// setupDone latches once an account is seen to exist. See SetupGate for
	// why it only ever moves in that direction.
	setupDone atomic.Bool
}

// New wires the middleware. secure marks the session cookie Secure, which is
// on for every deployment reached over HTTPS and off only for local work,
// where the browser would otherwise discard the cookie.
func New(authService *service.AuthService, projects *service.ProjectService,
	authzService *authz.Service, log *applog.Logger, secure bool) *Set {
	return &Set{auth: authService, projects: projects, authz: authzService,
		log: log, secure: secure}
}

// SetupPath is the screen that creates the first administrator.
const SetupPath = "/setup"

// SetupGate sends every request to the setup screen until an account exists,
// and makes that screen vanish once one does.
//
// The screen it guards is the only unauthenticated write in the service, so the
// gate is the security control and not a convenience. Two properties matter,
// and they pull in opposite directions:
//
//   - It must never report "needs setup" once an account exists. That would
//     reopen the screen and hand the next visitor an administrator account, so
//     the answer is read from the DATABASE rather than from anything cached.
//   - It must not query on every request forever, on a console that is mostly
//     reads.
//
// Both hold because the transition is one way. While no account exists the gate
// asks the database, which costs a count on a deployment nobody is using yet.
// The first time it sees an account it latches, and from then on it asks
// nothing and /setup is closed for the life of the process. Latching the other
// way round is what would be dangerous, and it never happens.
//
// A read error latches nothing and refuses the request: it must not fall
// through to the setup screen, and it must not fall through to the application
// either, because either guess could be the wrong one.
func (s *Set) SetupGate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.setupDone.Load() {
			// Set up. The screen does not exist.
			if r.URL.Path == SetupPath {
				http.NotFound(w, r)
				return
			}
			next.ServeHTTP(w, r)
			return
		}

		needed, err := s.auth.NeedsSetup(r.Context())
		if err != nil {
			s.log.Error("setup: the account count could not be read", err.Error())
			http.Error(w, "the service is not ready", http.StatusServiceUnavailable)
			return
		}
		if !needed {
			s.setupDone.Store(true)
			if r.URL.Path == SetupPath {
				http.NotFound(w, r)
				return
			}
			next.ServeHTTP(w, r)
			return
		}

		// No account yet. Only the setup screen is reachable, so nothing else
		// can be probed while the deployment is at its most open.
		if r.URL.Path != SetupPath {
			// A deploy pipeline calling the API deserves an answer it can
			// read. Redirected, it would follow the 303 and try to parse a
			// sign-up form as its payload.
			if strings.HasPrefix(r.URL.Path, "/api/") {
				httpx.Fail(w, http.StatusServiceUnavailable, "the deployment has not been set up yet")
				return
			}
			http.Redirect(w, r, SetupPath, http.StatusSeeOther)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// SetupCompleted latches the gate shut, called by the handler that created the
// account so the same request's redirect does not bounce back to /setup.
func (s *Set) SetupCompleted() { s.setupDone.Store(true) }

// IssueCookie writes the session cookie.
func (s *Set) IssueCookie(w http.ResponseWriter, token string, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    token,
		Path:     "/",
		MaxAge:   maxAge,
		HttpOnly: true,
		Secure:   s.secure,
		// Lax, not None: the cookie is sent on ordinary navigation to this
		// site and not on a cross site form post, which is what makes the
		// state changing routes safe from a page the operator did not open.
		SameSite: http.SameSiteLaxMode,
	})
}

// ClearCookie removes the session cookie.
func (s *Set) ClearCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   s.secure,
		SameSite: http.SameSiteLaxMode,
	})
}

// WebAuth requires a signed in operator, redirecting to the login page.
func (s *Set) WebAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(CookieName)
		if err != nil {
			s.toLogin(w, r)
			return
		}
		user, err := s.auth.Authenticate(r.Context(), cookie.Value)
		if err != nil {
			s.ClearCookie(w)
			s.toLogin(w, r)
			return
		}
		next.ServeHTTP(w, r.WithContext(httpx.WithUser(r.Context(), user)))
	})
}

// Language resolves the interface language and puts it on the context.
//
// Mounted in front of every HTML route including the login page, because the
// screen somebody cannot get past is the one they most need in their own
// language.
func Language(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r.WithContext(httpx.WithLang(r.Context(), httpx.ResolveLang(r))))
	})
}

// Permissions resolves what the operator may do, once per request.
//
// Resolved here rather than per handler so the layout can draw the navigation
// from the same answer the endpoints enforce. A failure is not fatal: the page
// renders offering nothing extra, and every endpoint still checks for itself.
// Refusing the request instead would take the whole interface down for what is
// usually a cache miss against a busy database.
func (s *Set) Permissions(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user := httpx.UserFrom(r.Context())
		if user == nil || s.authz == nil {
			next.ServeHTTP(w, r)
			return
		}

		perms, err := s.authz.Permissions(r.Context(), user)
		if err != nil {
			s.log.Warn("permissions could not be read", err.Error(), "user_id", user.ID)
			perms = map[string]bool{}
		}
		next.ServeHTTP(w, r.WithContext(httpx.WithPermissions(r.Context(), perms)))
	})
}

// toLogin sends an unauthenticated visitor to the login page, remembering
// where they were going.
//
// The return path is sanitised here rather than at the login handler, because
// this is where an attacker controlled value would enter: a link to
// /jobs?next=//evil.com would otherwise put an off-site address into the form.
func (s *Set) toLogin(w http.ResponseWriter, r *http.Request) {
	if httpx.IsHTMX(r) {
		// An expired session inside a fragment request must reload the whole
		// page. Swapping a login form into a table cell is not a login.
		w.Header().Set("HX-Redirect", "/login")
		w.WriteHeader(http.StatusOK)
		return
	}
	target := "/login"
	if next := httpx.SanitizeRedirect(r.URL.RequestURI(), ""); next != "" && next != "/" {
		target += "?next=" + urlQueryEscape(next)
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// GuestOnly bounces an already signed in operator away from the login page.
func (s *Set) GuestOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if cookie, err := r.Cookie(CookieName); err == nil {
			if _, err := s.auth.Authenticate(r.Context(), cookie.Value); err == nil {
				http.Redirect(w, r, "/", http.StatusSeeOther)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// AdminOnly restricts a route group to administrators.
func (s *Set) AdminOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user := httpx.UserFrom(r.Context())
		// The nil check comes first. A failed lookup leaves user nil, and
		// reading IsAdmin off it would panic instead of denying access.
		if user == nil || !user.IsAdmin {
			if strings.HasPrefix(r.URL.Path, "/api/") {
				httpx.Fail(w, http.StatusForbidden, "administrator access required")
				return
			}
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// APIAuth accepts an operator bearer token.
func (s *Set) APIAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		if raw == "" {
			httpx.Fail(w, http.StatusUnauthorized, "authentication required")
			return
		}
		user, err := s.auth.Authenticate(r.Context(), raw)
		if err != nil {
			httpx.Fail(w, http.StatusUnauthorized, "authentication required")
			return
		}
		next.ServeHTTP(w, r.WithContext(httpx.WithUser(r.Context(), user)))
	})
}

// ProjectKey authenticates a project by its API key.
//
// This is how a project registers its own jobs from a deploy pipeline. The key
// is scoped to one project, so a leaked key reaches that project's jobs and
// nothing else.
func (s *Set) ProjectKey(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimSpace(r.Header.Get("X-API-Key"))
		if key == "" {
			key = strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		}
		if key == "" {
			httpx.Fail(w, http.StatusUnauthorized, "an API key is required")
			return
		}

		project, err := s.projects.Authenticate(r.Context(), key)
		if err != nil {
			if !errors.Is(err, service.ErrForbidden) {
				s.log.Error("api: key lookup failed", err.Error())
				httpx.Fail(w, http.StatusInternalServerError, "the key could not be checked")
				return
			}
			httpx.Fail(w, http.StatusUnauthorized, "invalid API key")
			return
		}
		next.ServeHTTP(w, r.WithContext(httpx.WithProject(r.Context(), project)))
	})
}

// JSONOnly refuses a state changing request that is not JSON.
//
// It doubles as cross site request forgery protection for the API: a plain
// HTML form cannot set Content-Type to application/json, so a request carrying
// it went through a script that had to pass the same origin policy first.
func JSONOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost, http.MethodPut, http.MethodPatch:
			if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
				httpx.Fail(w, http.StatusUnsupportedMediaType, "Content-Type must be application/json")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// SameOrigin rejects a state changing browser request whose Origin is not this
// site.
//
// The session cookie is SameSite=Lax, which already blocks a cross site POST
// in current browsers. This is the second layer, and it is the one that keeps
// working when a route is later reached in a way nobody predicted.
func SameOrigin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			next.ServeHTTP(w, r)
			return
		}

		origin := r.Header.Get("Origin")
		if origin == "" {
			// No Origin header at all. Sent by some non-browser clients and by
			// same origin navigation in older browsers, so it is not by itself
			// evidence of anything.
			next.ServeHTTP(w, r)
			return
		}
		if sameHost(origin, r) {
			next.ServeHTTP(w, r)
			return
		}
		httpx.Fail(w, http.StatusForbidden, "cross origin request refused")
	})
}

func sameHost(origin string, r *http.Request) bool {
	trimmed := origin
	for _, prefix := range []string{"https://", "http://"} {
		trimmed = strings.TrimPrefix(trimmed, prefix)
	}
	return strings.EqualFold(trimmed, r.Host)
}

// SecurityHeaders sets the response headers that constrain what a browser will
// do with the page.
func SecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "same-origin")
		// The interface loads Bootstrap, htmx and Chart.js from ONE CDN, so
		// script-src names that host rather than 'self' alone. One host on
		// purpose: every additional origin here is another party that can
		// execute script on this page. Inline scripts are allowed because the
		// page templates carry them; tightening that to a nonce is the next
		// step, and is noted here rather than left as a silently missing
		// control.
		h.Set("Content-Security-Policy", strings.Join([]string{
			"default-src 'self'",
			"script-src 'self' 'unsafe-inline' https://cdn.jsdelivr.net",
			"style-src 'self' 'unsafe-inline' https://cdn.jsdelivr.net",
			"font-src 'self' https://cdn.jsdelivr.net data:",
			"img-src 'self' data:",
			"connect-src 'self'",
			"frame-ancestors 'none'",
			"base-uri 'self'",
			"form-action 'self'",
		}, "; "))
		next.ServeHTTP(w, r)
	})
}

// PanicLogger records a panic and re-panics so the recoverer above it can
// write the response.
//
// MOUNT ORDER IS LOAD BEARING AND IS THE OPPOSITE OF HOW IT READS.
// r.Use(A); r.Use(B) builds A(B(handler)), so a panic unwinds through B first,
// and chi's Recoverer recovers WITHOUT re-panicking. This one therefore has to
// be the INNER of the two: it logs, re-panics, and the Recoverer wrapped
// around it produces the 500.
//
// Mounted the other way round, chi swallows every panic before this deferred
// recover runs, and a production crash appears only on stdout. The symptom is
// an absence: app_logs simply never gets a panic row, which is exactly the
// kind of gap nobody notices.
func PanicLogger(log *applog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if rec := recover(); rec != nil {
					if rec == http.ErrAbortHandler {
						panic(rec)
					}
					log.Error("http: handler panicked", fmt.Sprintf("%v\n%s", rec, debug.Stack()),
						"path", r.URL.Path, "method", r.Method)
					panic(rec)
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// RateLimit throttles a route group by client address.
//
// The address comes from the configured TrustedProxy, which by default is the
// socket and nothing else. Reading a forwarding header without knowing a
// trusted proxy set it lets the caller choose their own bucket, and a caller
// who chooses their own bucket is not limited at all.
func RateLimit(limiter *auth.Limiter, proxy auth.TrustedProxy, message string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !limiter.Allow(proxy.ClientIP(r)) {
				if strings.HasPrefix(r.URL.Path, "/api/") {
					httpx.Fail(w, http.StatusTooManyRequests, message)
					return
				}
				http.Error(w, message, http.StatusTooManyRequests)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// urlQueryEscape percent-encodes a return path for a query parameter.
//
// BYTE by byte, not rune by rune. Percent-encoding is defined on octets, and
// encoding a rune's code point produces something that is not an escape at all:
// 'ş' is U+015F, and "%15F" is a two digit escape for a control character with
// an F stuck on the end. Any non-ASCII path came back mangled.
//
// url.QueryEscape is not used because it turns a space into '+', which is
// correct for a form body and wrong inside a path that is read back with
// url.Path semantics.
func urlQueryEscape(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9',
			c == '-', c == '_', c == '.', c == '~', c == '/':
			b.WriteByte(c)
		default:
			b.WriteString(fmt.Sprintf("%%%02X", c))
		}
	}
	return b.String()
}
