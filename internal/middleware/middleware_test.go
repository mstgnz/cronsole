package middleware

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/mstgnz/cronsole/v2/internal/applog"
	"github.com/mstgnz/cronsole/v2/internal/domain"
	"github.com/mstgnz/cronsole/v2/internal/httpx"
	"github.com/mstgnz/cronsole/v2/internal/i18n"
	"github.com/mstgnz/cronsole/v2/internal/repository"
	"github.com/mstgnz/cronsole/v2/internal/service"
	"github.com/mstgnz/cronsole/v2/pkg/auth"
	"github.com/mstgnz/cronsole/v2/pkg/token"
)

// --- partial fakes ----------------------------------------------------------
//
// The interface is embedded rather than implemented in full: a method these
// tests do not expect to be called panics on a nil interface, which is exactly
// the signal wanted if the middleware starts reaching somewhere new.

type stubUserRepo struct {
	domain.UserRepository
	users map[int64]*domain.User
	err   error
}

func (s stubUserRepo) GetByID(_ context.Context, id int64) (*domain.User, error) {
	if s.err != nil {
		return nil, s.err
	}
	user, ok := s.users[id]
	if !ok {
		return nil, repository.ErrNotFound
	}
	return user, nil
}

type stubProjectRepo struct {
	domain.ProjectRepository
	byPrefix map[string]*domain.Project
	err      error
}

func (s stubProjectRepo) GetByKeyPrefix(_ context.Context, prefix string) (*domain.Project, error) {
	if s.err != nil {
		return nil, s.err
	}
	project, ok := s.byPrefix[prefix]
	if !ok {
		return nil, repository.ErrNotFound
	}
	return project, nil
}

const testSecret = "a-signing-secret-of-at-least-32-chars"

// harness builds a Set over stub repositories, plus the issuer that signs the
// tokens the tests present.
type harness struct {
	set    *Set
	issuer *token.Issuer
	users  map[int64]*domain.User
	keys   map[string]*domain.Project
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	h := &harness{
		issuer: token.NewIssuer(testSecret),
		users:  map[int64]*domain.User{},
		keys:   map[string]*domain.Project{},
	}

	logger := applog.New()
	// No reset repository and no mailer: these tests are about who a request
	// belongs to, and a reset cannot be requested without a session anyway.
	authService := service.NewAuthService(stubUserRepo{users: h.users}, nil, nil, h.issuer, nil, logger)
	projectService := service.NewProjectService(stubProjectRepo{byPrefix: h.keys}, nil, service.TargetPolicy{})

	h.set = New(authService, projectService, nil, logger, true)
	return h
}

// signedFor issues a token for an active user and registers that user.
func (h *harness) signedFor(t *testing.T, user *domain.User) string {
	t.Helper()
	h.users[user.ID] = user
	signed, err := h.issuer.Issue(user.ID)
	if err != nil {
		t.Fatalf("Issue = %v", err)
	}
	return signed
}

// ok is the handler at the end of the chain. It records that it was reached.
func ok(reached *bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if reached != nil {
			*reached = true
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("reached"))
	})
}

func activeUser(id int64, admin bool) *domain.User {
	return &domain.User{ID: id, Email: "a@b.c", Active: true, IsAdmin: admin}
}

// --- cookies ----------------------------------------------------------------

func TestTheSessionCookieIsNotReadableByScript(t *testing.T) {
	// A token in localStorage, or in a cookie without HttpOnly, means one cross
	// site scripting flaw is every account.
	s := New(nil, nil, nil, applog.New(), true)
	w := httptest.NewRecorder()
	s.IssueCookie(w, "a-token", 3600)

	cookies := w.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("%d cookies were set", len(cookies))
	}
	c := cookies[0]

	if c.Name != CookieName {
		t.Errorf("name = %q", c.Name)
	}
	if !c.HttpOnly {
		t.Error("the session cookie is readable by script")
	}
	if !c.Secure {
		t.Error("the session cookie is not marked Secure")
	}
	// Lax, not None: sent on ordinary navigation to this site and not on a
	// cross site form post, which is what makes the state changing routes safe
	// from a page the operator did not open.
	if c.SameSite != http.SameSiteLaxMode {
		t.Errorf("SameSite = %v, want Lax", c.SameSite)
	}
	if c.Path != "/" {
		t.Errorf("path = %q", c.Path)
	}
}

func TestTheCookieIsNotSecureOnAPlainDevelopmentServer(t *testing.T) {
	// The browser would discard it, and signing in would appear to do nothing.
	s := New(nil, nil, nil, applog.New(), false)
	w := httptest.NewRecorder()
	s.IssueCookie(w, "a-token", 3600)

	if w.Result().Cookies()[0].Secure {
		t.Error("Secure was set for a plain HTTP deployment")
	}
}

func TestClearCookieExpiresIt(t *testing.T) {
	s := New(nil, nil, nil, applog.New(), true)
	w := httptest.NewRecorder()
	s.ClearCookie(w)

	c := w.Result().Cookies()[0]
	if c.Value != "" {
		t.Errorf("value = %q, want empty", c.Value)
	}
	if c.MaxAge >= 0 {
		t.Errorf("MaxAge = %d, want negative so the browser drops it", c.MaxAge)
	}
	// Still HttpOnly and Secure: the attributes have to match the cookie being
	// replaced, or some browsers keep the original.
	if !c.HttpOnly || !c.Secure {
		t.Error("the clearing cookie does not match the attributes of the one it replaces")
	}
}

// --- WebAuth ----------------------------------------------------------------

func TestWebAuthAdmitsAValidSession(t *testing.T) {
	h := newHarness(t)
	signed := h.signedFor(t, activeUser(7, false))

	var reached bool
	var seen *domain.User
	handler := h.set.WebAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		seen = httpx.UserFrom(r.Context())
	}))

	r := httptest.NewRequest(http.MethodGet, "/jobs", nil)
	r.AddCookie(&http.Cookie{Name: CookieName, Value: signed})
	handler.ServeHTTP(httptest.NewRecorder(), r)

	if !reached {
		t.Fatal("a valid session was refused")
	}
	if seen == nil || seen.ID != 7 {
		t.Errorf("the operator on the context = %+v, want the signed in user", seen)
	}
}

func TestWebAuthRefusesWhatIsNotASession(t *testing.T) {
	h := newHarness(t)
	h.signedFor(t, activeUser(7, false))

	// A token signed with a different secret, which is what a forged one is.
	forged, err := token.NewIssuer("a-completely-different-secret-key!!").Issue(7)
	if err != nil {
		t.Fatalf("Issue = %v", err)
	}

	cases := []struct {
		name   string
		cookie string
	}{
		{"no cookie at all", ""},
		{"an empty cookie", " "},
		{"rubbish", "not-a-token"},
		{"a token signed with another key", forged},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var reached bool
			handler := h.set.WebAuth(ok(&reached))

			r := httptest.NewRequest(http.MethodGet, "/jobs", nil)
			if c.cookie != "" {
				r.AddCookie(&http.Cookie{Name: CookieName, Value: c.cookie})
			}
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)

			if reached {
				t.Fatal("the handler was reached")
			}
			if w.Code != http.StatusSeeOther {
				t.Errorf("code = %d, want a redirect to the login page", w.Code)
			}
		})
	}
}

func TestASessionForARetiredOrInactiveAccountIsRefused(t *testing.T) {
	// A JWT cannot be withdrawn once signed, so ending a session means moving
	// the cut-off. If that were not enforced here, logging out would not.
	h := newHarness(t)

	retired := activeUser(8, false)
	signed := h.signedFor(t, retired)
	// The cut-off moved forward, so a token issued before it is retired. This
	// is what makes a logout or a password change end existing sessions.
	future := time.Now().Add(time.Minute)
	retired.TokensValidAfter = &future

	var reached bool
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/jobs", nil)
	r.AddCookie(&http.Cookie{Name: CookieName, Value: signed})
	h.set.WebAuth(ok(&reached)).ServeHTTP(w, r)

	if reached {
		t.Error("a retired token was accepted")
	}

	// And a deactivated account.
	h.users[9] = &domain.User{ID: 9, Active: false}
	signed9, _ := h.issuer.Issue(9)

	reached = false
	r = httptest.NewRequest(http.MethodGet, "/jobs", nil)
	r.AddCookie(&http.Cookie{Name: CookieName, Value: signed9})
	h.set.WebAuth(ok(&reached)).ServeHTTP(httptest.NewRecorder(), r)

	if reached {
		t.Error("a deactivated account was admitted")
	}
}

func TestARejectedSessionCookieIsCleared(t *testing.T) {
	// Otherwise the browser keeps presenting a token that will never work, and
	// every page load pays for the verification.
	h := newHarness(t)

	r := httptest.NewRequest(http.MethodGet, "/jobs", nil)
	r.AddCookie(&http.Cookie{Name: CookieName, Value: "not-a-token"})
	w := httptest.NewRecorder()
	h.set.WebAuth(ok(nil)).ServeHTTP(w, r)

	for _, c := range w.Result().Cookies() {
		if c.Name == CookieName && c.MaxAge < 0 {
			return
		}
	}
	t.Error("the rejected cookie was not cleared")
}

func TestWebAuthRemembersWhereTheVisitorWasGoing(t *testing.T) {
	h := newHarness(t)

	r := httptest.NewRequest(http.MethodGet, "/runs?status=failed&page=2", nil)
	w := httptest.NewRecorder()
	h.set.WebAuth(ok(nil)).ServeHTTP(w, r)

	location := w.Header().Get("Location")
	if !strings.HasPrefix(location, "/login?next=") {
		t.Fatalf("Location = %q, want the return path", location)
	}
	if !strings.Contains(location, "%3Fstatus%3Dfailed") && !strings.Contains(location, "status") {
		t.Errorf("Location = %q, want the filter preserved", location)
	}
}

func TestTheReturnPathCannotLeaveThisSite(t *testing.T) {
	// This is where an attacker controlled value enters: a link to
	// /jobs?next=//evil.com would otherwise put an off-site address into the
	// login form, and the redirect after signing in would follow it.
	h := newHarness(t)

	// Two shapes reach RequestURI as something that is not a path on this site:
	// a protocol relative target, which a browser reads as a host, and an
	// absolute request target, which a client can send directly.
	cases := []struct {
		name  string
		build func() *http.Request
	}{
		{
			name: "protocol relative",
			build: func() *http.Request {
				r := httptest.NewRequest(http.MethodGet, "/", nil)
				r.URL.Path = "//evil.com/"
				return r
			},
		},
		{
			name: "an absolute request target",
			build: func() *http.Request {
				r := httptest.NewRequest(http.MethodGet, "/", nil)
				r.URL.Scheme, r.URL.Host, r.URL.Path = "https", "evil.com", "/"
				return r
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			h.set.WebAuth(ok(nil)).ServeHTTP(w, c.build())

			location := w.Header().Get("Location")
			if location != "/login" {
				t.Errorf("Location = %q, want a bare /login with no return path", location)
			}
		})
	}
}

func TestTheReturnPathIsEscapedRatherThanMangled(t *testing.T) {
	// The return path goes into a query parameter, so it is percent-encoded.
	// Encoding it per RUNE rather than per BYTE produced "%15F" for 'ş': a two
	// digit escape for a control character with an F on the end, which is not
	// an escape at all. Any non-ASCII path came back wrong.
	h := newHarness(t)

	r := httptest.NewRequest(http.MethodGet, "/jobs?q=işler", nil)
	w := httptest.NewRecorder()
	h.set.WebAuth(ok(nil)).ServeHTTP(w, r)

	location := w.Header().Get("Location")
	next := strings.TrimPrefix(location, "/login?next=")
	if next == location {
		t.Fatalf("Location = %q, want a return path", location)
	}

	decoded, err := url.QueryUnescape(next)
	if err != nil {
		t.Fatalf("the return path is not decodable: %q: %v", next, err)
	}
	if !strings.Contains(decoded, "işler") {
		t.Errorf("the return path decoded to %q, want the original", decoded)
	}
}

func TestAnExpiredSessionInsideAFragmentReloadsThePage(t *testing.T) {
	// Swapping a login form into a table cell is not a login.
	h := newHarness(t)

	r := httptest.NewRequest(http.MethodGet, "/jobs", nil)
	r.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()
	h.set.WebAuth(ok(nil)).ServeHTTP(w, r)

	if got := w.Header().Get("HX-Redirect"); got != "/login" {
		t.Errorf("HX-Redirect = %q, want /login", got)
	}
	if w.Code != http.StatusOK {
		t.Errorf("code = %d; htmx only follows HX-Redirect on a 2xx", w.Code)
	}
}

// --- GuestOnly --------------------------------------------------------------

func TestGuestOnly(t *testing.T) {
	h := newHarness(t)
	signed := h.signedFor(t, activeUser(7, false))

	// Signed in: bounced away from the login page.
	var reached bool
	r := httptest.NewRequest(http.MethodGet, "/login", nil)
	r.AddCookie(&http.Cookie{Name: CookieName, Value: signed})
	w := httptest.NewRecorder()
	h.set.GuestOnly(ok(&reached)).ServeHTTP(w, r)

	if reached {
		t.Error("a signed in operator reached the login page")
	}
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/" {
		t.Errorf("code = %d, Location = %q", w.Code, w.Header().Get("Location"))
	}

	// Not signed in, or holding something that is not a session: let through.
	for _, cookie := range []string{"", "not-a-token"} {
		reached = false
		r = httptest.NewRequest(http.MethodGet, "/login", nil)
		if cookie != "" {
			r.AddCookie(&http.Cookie{Name: CookieName, Value: cookie})
		}
		h.set.GuestOnly(ok(&reached)).ServeHTTP(httptest.NewRecorder(), r)
		if !reached {
			t.Errorf("a visitor with cookie %q could not reach the login page", cookie)
		}
	}
}

// --- AdminOnly --------------------------------------------------------------

func TestAdminOnly(t *testing.T) {
	s := New(nil, nil, nil, applog.New(), true)

	cases := []struct {
		name     string
		user     *domain.User
		path     string
		reached  bool
		wantCode int
	}{
		{"an administrator", activeUser(1, true), "/settings", true, http.StatusOK},
		{"an ordinary operator", activeUser(2, false), "/settings", false, http.StatusSeeOther},
		// The nil check comes first: a failed lookup leaves the operator nil,
		// and reading IsAdmin off it would panic rather than deny.
		{"nobody at all", nil, "/settings", false, http.StatusSeeOther},
		{"an ordinary operator on the API", activeUser(2, false), "/api/v1/admin/me", false, http.StatusForbidden},
		{"nobody at all on the API", nil, "/api/v1/admin/me", false, http.StatusForbidden},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var reached bool
			r := httptest.NewRequest(http.MethodGet, c.path, nil)
			if c.user != nil {
				r = r.WithContext(httpx.WithUser(r.Context(), c.user))
			}
			w := httptest.NewRecorder()
			s.AdminOnly(ok(&reached)).ServeHTTP(w, r)

			if reached != c.reached {
				t.Errorf("reached = %v, want %v", reached, c.reached)
			}
			if w.Code != c.wantCode {
				t.Errorf("code = %d, want %d", w.Code, c.wantCode)
			}
		})
	}
}

// --- APIAuth ----------------------------------------------------------------

func TestAPIAuth(t *testing.T) {
	h := newHarness(t)
	signed := h.signedFor(t, activeUser(7, false))

	t.Run("a valid bearer token", func(t *testing.T) {
		var seen *domain.User
		handler := h.set.APIAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			seen = httpx.UserFrom(r.Context())
		}))
		r := httptest.NewRequest(http.MethodGet, "/api/v1/admin/me", nil)
		r.Header.Set("Authorization", "Bearer "+signed)
		handler.ServeHTTP(httptest.NewRecorder(), r)

		if seen == nil || seen.ID != 7 {
			t.Errorf("the operator on the context = %+v", seen)
		}
	})

	for _, header := range []string{"", "Bearer ", "Bearer not-a-token", "Basic abc", signed} {
		var reached bool
		r := httptest.NewRequest(http.MethodGet, "/api/v1/admin/me", nil)
		if header != "" {
			r.Header.Set("Authorization", header)
		}
		w := httptest.NewRecorder()
		h.set.APIAuth(ok(&reached)).ServeHTTP(w, r)

		if header == signed {
			// A bare token without the Bearer prefix is accepted, because
			// TrimPrefix leaves it untouched. Pinned so it is a decision.
			if !reached {
				t.Error("a bare token was refused")
			}
			continue
		}
		if reached {
			t.Errorf("Authorization %q was accepted", header)
		}
		if w.Code != http.StatusUnauthorized {
			t.Errorf("Authorization %q answered %d, want 401", header, w.Code)
		}
	}
}

func TestAPIAuthAnswersJSON(t *testing.T) {
	// A machine client parses the body. An HTML redirect here would be read as
	// a successful response with unexpected content.
	h := newHarness(t)

	w := httptest.NewRecorder()
	h.set.APIAuth(ok(nil)).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/admin/me", nil))

	if got := w.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Errorf("Content-Type = %q", got)
	}
}

// --- ProjectKey -------------------------------------------------------------

// keyPrefixLen mirrors the service's own constant: how much of a key is stored
// in clear, for lookup.
const keyPrefixLen = 8

func projectWithKey(t *testing.T, h *harness) (*domain.Project, string) {
	t.Helper()

	raw := "cj_" + strings.Repeat("k", 40)
	sum := sha256.Sum256([]byte(raw))

	project := &domain.Project{ID: 3, Slug: "shop", Active: true}
	project.KeyHash = hex.EncodeToString(sum[:])
	h.keys[raw[:keyPrefixLen]] = project
	return project, raw
}

func TestProjectKeyAdmitsAValidKey(t *testing.T) {
	h := newHarness(t)
	project, raw := projectWithKey(t, h)

	var seen *domain.Project
	handler := h.set.ProjectKey(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = httpx.ProjectFrom(r.Context())
	}))

	r := httptest.NewRequest(http.MethodPost, "/api/v1/sync", nil)
	r.Header.Set("X-API-Key", raw)
	handler.ServeHTTP(httptest.NewRecorder(), r)

	if seen == nil || seen.ID != project.ID {
		t.Errorf("the project on the context = %+v", seen)
	}
}

func TestProjectKeyAcceptsTheKeyAsABearerToken(t *testing.T) {
	// A deploy pipeline that already has an Authorization header should not
	// have to learn a second convention.
	h := newHarness(t)
	_, raw := projectWithKey(t, h)

	var reached bool
	r := httptest.NewRequest(http.MethodPost, "/api/v1/sync", nil)
	r.Header.Set("Authorization", "Bearer "+raw)
	h.set.ProjectKey(ok(&reached)).ServeHTTP(httptest.NewRecorder(), r)

	if !reached {
		t.Error("a key presented as a bearer token was refused")
	}
}

func TestProjectKeyRefusals(t *testing.T) {
	h := newHarness(t)
	_, raw := projectWithKey(t, h)

	cases := []struct {
		name string
		key  string
		code int
	}{
		{"no key", "", http.StatusUnauthorized},
		{"too short to be a key", "cj_short", http.StatusUnauthorized},
		{"a prefix that matches nothing", "cj_" + strings.Repeat("z", 40), http.StatusUnauthorized},
		{"the right prefix and the wrong key", raw[:keyPrefixLen] + strings.Repeat("x", 40), http.StatusUnauthorized},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var reached bool
			r := httptest.NewRequest(http.MethodPost, "/api/v1/sync", nil)
			if c.key != "" {
				r.Header.Set("X-API-Key", c.key)
			}
			w := httptest.NewRecorder()
			h.set.ProjectKey(ok(&reached)).ServeHTTP(w, r)

			if reached {
				t.Error("the handler was reached")
			}
			if w.Code != c.code {
				t.Errorf("code = %d, want %d", w.Code, c.code)
			}
		})
	}
}

func TestADeactivatedProjectsKeyStopsWorking(t *testing.T) {
	h := newHarness(t)
	project, raw := projectWithKey(t, h)
	project.Active = false

	var reached bool
	r := httptest.NewRequest(http.MethodPost, "/api/v1/sync", nil)
	r.Header.Set("X-API-Key", raw)
	w := httptest.NewRecorder()
	h.set.ProjectKey(ok(&reached)).ServeHTTP(w, r)

	if reached {
		t.Error("a deactivated project's key still works")
	}
	if w.Code != http.StatusUnauthorized {
		t.Errorf("code = %d, want 401", w.Code)
	}
}

func TestALookupFailureIsNotAnInvalidKey(t *testing.T) {
	// A database blip must not read as "your key is wrong", which would send an
	// operator to rotate a key that was fine.
	logger := applog.New()
	projectService := service.NewProjectService(
		stubProjectRepo{err: errors.New("connection refused")}, nil, service.TargetPolicy{})
	s := New(nil, projectService, nil, logger, true)

	r := httptest.NewRequest(http.MethodPost, "/api/v1/sync", nil)
	r.Header.Set("X-API-Key", "cj_"+strings.Repeat("k", 40))
	w := httptest.NewRecorder()
	s.ProjectKey(ok(nil)).ServeHTTP(w, r)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("code = %d, want 500 for a lookup failure", w.Code)
	}
}

// --- Permissions ------------------------------------------------------------

func TestPermissionsIsSkippedWithoutAnOperator(t *testing.T) {
	s := New(nil, nil, nil, applog.New(), true)

	var reached bool
	s.Permissions(ok(&reached)).ServeHTTP(httptest.NewRecorder(),
		httptest.NewRequest(http.MethodGet, "/", nil))

	if !reached {
		t.Error("the request was refused rather than passed through")
	}
}

func TestPermissionsDoesNotTakeTheInterfaceDownWhenItCannotResolve(t *testing.T) {
	// A failure renders a page offering nothing extra. Refusing the request
	// would take the whole interface down for what is usually a cache miss
	// against a busy database, and every endpoint still checks for itself.
	s := New(nil, nil, nil, applog.New(), true) // nil authz stands in for unavailable

	var seen map[string]bool
	var reached bool
	handler := s.Permissions(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		seen = httpx.PermissionsFrom(r.Context())
	}))

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r = r.WithContext(httpx.WithUser(r.Context(), activeUser(1, false)))
	handler.ServeHTTP(httptest.NewRecorder(), r)

	if !reached {
		t.Fatal("the request was refused")
	}
	if len(seen) != 0 {
		t.Errorf("permissions = %v, want none", seen)
	}
}

// --- Language ---------------------------------------------------------------

func TestLanguageResolvesOntoTheContext(t *testing.T) {
	var seen i18n.Lang
	handler := Language(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = httpx.LangFrom(r.Context())
	}))

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.AddCookie(&http.Cookie{Name: httpx.LangCookie, Value: "tr"})
	handler.ServeHTTP(httptest.NewRecorder(), r)

	if seen != i18n.TR {
		t.Errorf("language = %q, want tr", seen)
	}
}

// --- JSONOnly ---------------------------------------------------------------

func TestJSONOnly(t *testing.T) {
	// It doubles as cross site request forgery protection for the API: a plain
	// HTML form cannot set Content-Type to application/json, so a request
	// carrying it went through a script that passed the same origin policy.
	cases := []struct {
		method      string
		contentType string
		reached     bool
	}{
		{http.MethodGet, "", true},
		{http.MethodDelete, "", true},
		{http.MethodPost, "application/json", true},
		{http.MethodPost, "application/json; charset=utf-8", true},
		{http.MethodPost, "", false},
		{http.MethodPost, "application/x-www-form-urlencoded", false},
		{http.MethodPost, "multipart/form-data", false},
		{http.MethodPost, "text/plain", false},
		{http.MethodPut, "text/plain", false},
		{http.MethodPatch, "text/plain", false},
	}

	for _, c := range cases {
		var reached bool
		r := httptest.NewRequest(c.method, "/api/v1/sync", nil)
		if c.contentType != "" {
			r.Header.Set("Content-Type", c.contentType)
		}
		w := httptest.NewRecorder()
		JSONOnly(ok(&reached)).ServeHTTP(w, r)

		if reached != c.reached {
			t.Errorf("%s with %q: reached = %v, want %v", c.method, c.contentType, reached, c.reached)
		}
		if !c.reached && w.Code != http.StatusUnsupportedMediaType {
			t.Errorf("%s with %q answered %d, want 415", c.method, c.contentType, w.Code)
		}
	}
}

// --- SameOrigin -------------------------------------------------------------

func TestSameOrigin(t *testing.T) {
	cases := []struct {
		name    string
		method  string
		origin  string
		host    string
		reached bool
	}{
		{"a read is never checked", http.MethodGet, "https://evil.com", "cron.example.com", true},
		{"a HEAD is never checked", http.MethodHead, "https://evil.com", "cron.example.com", true},
		{"an OPTIONS is never checked", http.MethodOptions, "https://evil.com", "cron.example.com", true},
		{"same origin over https", http.MethodPost, "https://cron.example.com", "cron.example.com", true},
		{"same origin over http", http.MethodPost, "http://cron.example.com", "cron.example.com", true},
		{"same origin, different case", http.MethodPost, "https://CRON.example.com", "cron.example.com", true},
		{"same host, different port", http.MethodPost, "https://cron.example.com:8443", "cron.example.com", false},
		{"another site", http.MethodPost, "https://evil.com", "cron.example.com", false},
		{"a subdomain is a different origin", http.MethodPost, "https://a.cron.example.com", "cron.example.com", false},
		{"a sandboxed frame", http.MethodPost, "null", "cron.example.com", false},
		// No Origin at all is sent by some non-browser clients and by same
		// origin navigation in older browsers, so it is not by itself evidence.
		{"no Origin header", http.MethodPost, "", "cron.example.com", true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var reached bool
			r := httptest.NewRequest(c.method, "/jobs", nil)
			r.Host = c.host
			if c.origin != "" {
				r.Header.Set("Origin", c.origin)
			}
			w := httptest.NewRecorder()
			SameOrigin(ok(&reached)).ServeHTTP(w, r)

			if reached != c.reached {
				t.Errorf("reached = %v, want %v", reached, c.reached)
			}
			if !c.reached && w.Code != http.StatusForbidden {
				t.Errorf("code = %d, want 403", w.Code)
			}
		})
	}
}

// --- SecurityHeaders --------------------------------------------------------

func TestSecurityHeaders(t *testing.T) {
	w := httptest.NewRecorder()
	SecurityHeaders(ok(nil)).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))

	want := map[string]string{
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "DENY",
		"Referrer-Policy":        "same-origin",
	}
	for header, value := range want {
		if got := w.Header().Get(header); got != value {
			t.Errorf("%s = %q, want %q", header, got, value)
		}
	}

	csp := w.Header().Get("Content-Security-Policy")
	for _, directive := range []string{
		"default-src 'self'",
		"frame-ancestors 'none'",
		"base-uri 'self'",
		"form-action 'self'",
		"connect-src 'self'",
	} {
		if !strings.Contains(csp, directive) {
			t.Errorf("the policy is missing %q: %s", directive, csp)
		}
	}
}

func TestTheContentPolicyNamesOneScriptHost(t *testing.T) {
	// Every additional origin here is another party that can execute script on
	// this page, so the interface loads everything from one CDN.
	w := httptest.NewRecorder()
	SecurityHeaders(ok(nil)).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))

	csp := w.Header().Get("Content-Security-Policy")
	scriptSrc := ""
	for _, part := range strings.Split(csp, ";") {
		if strings.Contains(part, "script-src") {
			scriptSrc = part
		}
	}
	if scriptSrc == "" {
		t.Fatalf("there is no script-src: %s", csp)
	}

	hosts := 0
	for _, field := range strings.Fields(scriptSrc) {
		if strings.HasPrefix(field, "https://") {
			hosts++
		}
	}
	if hosts != 1 {
		t.Errorf("script-src names %d hosts: %s", hosts, scriptSrc)
	}
}

// --- PanicLogger ------------------------------------------------------------

func TestPanicLoggerRecordsAndRePanics(t *testing.T) {
	// It has to re-panic: chi's Recoverer is mounted around it and is what
	// writes the 500. Swallowing the panic here would return a 200 with an
	// empty body.
	logger := applog.New()

	handler := PanicLogger(logger)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	}))

	defer func() {
		if rec := recover(); rec == nil {
			t.Error("the panic was swallowed; nothing would write the 500")
		} else if rec != "boom" {
			t.Errorf("recovered %v, want the original value", rec)
		}
	}()
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/jobs", nil))
}

func TestPanicLoggerLeavesAbortHandlerAlone(t *testing.T) {
	// http.ErrAbortHandler is how a handler says "stop, quietly". Logging it as
	// a crash would fill the log with entries about clients that hung up.
	handler := PanicLogger(applog.New())(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic(http.ErrAbortHandler)
	}))

	defer func() {
		if rec := recover(); rec != http.ErrAbortHandler {
			t.Errorf("recovered %v, want ErrAbortHandler", rec)
		}
	}()
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
}

func TestPanicLoggerPassesAnOrdinaryRequestThrough(t *testing.T) {
	var reached bool
	PanicLogger(applog.New())(ok(&reached)).ServeHTTP(
		httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	if !reached {
		t.Error("an ordinary request did not reach the handler")
	}
}

// --- RateLimit --------------------------------------------------------------

func TestRateLimitRefusesPastTheLimit(t *testing.T) {
	limiter := auth.NewLimiter(2, time.Minute)
	handler := RateLimit(limiter, auth.TrustedProxy{}, "too many")(ok(nil))

	for i := 0; i < 2; i++ {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, "/login", nil)
		r.RemoteAddr = "203.0.113.9:1000"
		handler.ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("attempt %d answered %d", i+1, w.Code)
		}
	}

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/login", nil)
	r.RemoteAddr = "203.0.113.9:1000"
	handler.ServeHTTP(w, r)

	if w.Code != http.StatusTooManyRequests {
		t.Errorf("code = %d, want 429", w.Code)
	}
	if !strings.Contains(w.Body.String(), "too many") {
		t.Errorf("body = %q, want the message", w.Body.String())
	}
}

func TestRateLimitAnswersJSONOnTheAPI(t *testing.T) {
	limiter := auth.NewLimiter(0, time.Minute) // a limit of zero permits nothing
	handler := RateLimit(limiter, auth.TrustedProxy{}, "too many")(ok(nil))

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/api/v1/login", nil)
	r.RemoteAddr = "203.0.113.9:1000"
	handler.ServeHTTP(w, r)

	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("code = %d", w.Code)
	}
	if got := w.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Errorf("Content-Type = %q, want JSON for an API route", got)
	}
}

func TestRateLimitCannotBeEvadedWithAForwardingHeader(t *testing.T) {
	// The whole point of TrustedProxy's default. Without it these ten requests
	// are ten buckets.
	limiter := auth.NewLimiter(2, time.Minute)
	handler := RateLimit(limiter, auth.TrustedProxy{}, "too many")(ok(nil))

	allowed := 0
	for i := 0; i < 10; i++ {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, "/login", nil)
		r.RemoteAddr = "203.0.113.9:1000"
		r.Header.Set("X-Forwarded-For", "10.0.0."+string(rune('0'+i)))
		r.Header.Set("CF-Connecting-IP", "10.1.0."+string(rune('0'+i)))
		handler.ServeHTTP(w, r)
		if w.Code == http.StatusOK {
			allowed++
		}
	}
	if allowed != 2 {
		t.Errorf("%d requests got through a limit of 2", allowed)
	}
}

// --- urlQueryEscape ---------------------------------------------------------

func TestURLQueryEscape(t *testing.T) {
	cases := map[string]string{
		"/jobs":               "/jobs",
		"/runs?status=failed": "/runs%3Fstatus%3Dfailed",
		"/a b":                "/a%20b",
		"/x\r\ny":             "/x%0D%0Ay",
		"/tr/işler":           "/tr/i%C5%9Fler",
	}
	for in, want := range cases {
		if got := urlQueryEscape(in); got != want {
			t.Errorf("urlQueryEscape(%q) = %q, want %q", in, got, want)
		}
	}
}
