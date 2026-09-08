package handler

import (
	"errors"
	"net/http"
	"strings"

	"github.com/mstgnz/cronsole/v2/internal/applog"
	"github.com/mstgnz/cronsole/v2/internal/httpx"
	"github.com/mstgnz/cronsole/v2/internal/middleware"
	"github.com/mstgnz/cronsole/v2/internal/service"
	"github.com/mstgnz/cronsole/v2/pkg/auth"
	"github.com/mstgnz/cronsole/v2/pkg/token"
)

// AuthHandler serves signing in, signing out and the profile screen.
type AuthHandler struct {
	auth    *service.AuthService
	render  *Renderer
	cookies *middleware.Set
	limiter *auth.Limiter
	// proxy decides which address an attempt is attributed to. Its zero value
	// trusts no forwarding header, which is what makes the limiter a limit.
	proxy auth.TrustedProxy
	log   *applog.Logger
}

// NewAuthHandler wires the handler.
func NewAuthHandler(authService *service.AuthService, render *Renderer, cookies *middleware.Set,
	limiter *auth.Limiter, proxy auth.TrustedProxy, log *applog.Logger) *AuthHandler {
	return &AuthHandler{auth: authService, render: render, cookies: cookies,
		limiter: limiter, proxy: proxy, log: log}
}

// loginAllowed records a sign in attempt against two buckets and reports
// whether it may proceed.
//
// Two, because they stop different attacks. The address bucket stops a spray
// across many accounts from one place. The address bucket alone does nothing
// about a distributed attempt on ONE account, which is what credential stuffing
// is, so the address being tried gets a bucket of its own.
func (h *AuthHandler) loginAllowed(r *http.Request, email string) bool {
	if !h.limiter.Allow(h.proxy.ClientIP(r)) {
		return false
	}
	if key := emailKey(email); key != "" {
		return h.limiter.Allow(key)
	}
	return true
}

// loginSucceeded clears both buckets, so somebody who eventually types the
// right password is not still being counted for the tries before it.
func (h *AuthHandler) loginSucceeded(r *http.Request, email string) {
	h.limiter.Reset(h.proxy.ClientIP(r))
	if key := emailKey(email); key != "" {
		h.limiter.Reset(key)
	}
}

// emailKey namespaces the address bucket so it cannot collide with an address
// bucket, which is what keeps the two counts independent.
func emailKey(email string) string {
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" {
		return ""
	}
	return "email:" + email
}

// LoginPage shows the sign in form.
func (h *AuthHandler) LoginPage(w http.ResponseWriter, r *http.Request) {
	h.render.Render(w, r, "login", PageData{
		Title: "Sign in",
		Data: map[string]any{
			"next": httpx.SanitizeRedirect(r.URL.Query().Get("next"), "/"),
		},
	})
}

// Login verifies credentials and starts a session.
func (h *AuthHandler) Login(w http.ResponseWriter, r *http.Request) {
	email := r.PostFormValue("email")
	password := r.PostFormValue("password")
	next := httpx.SanitizeRedirect(r.PostFormValue("next"), "/")

	if !h.loginAllowed(r, email) {
		h.loginError(w, r, next, "Too many attempts. Try again in a few minutes.")
		return
	}

	user, signed, err := h.auth.Login(r.Context(), email, password)
	if err != nil {
		switch {
		case errors.Is(err, service.ErrInvalidLogin):
			h.loginError(w, r, next, "Email or password is incorrect.")
		case errors.Is(err, service.ErrInactiveUser):
			h.loginError(w, r, next, "This account is not active.")
		default:
			h.log.Error("auth: login failed", err.Error())
			h.loginError(w, r, next, "Sign in could not be completed.")
		}
		return
	}

	h.loginSucceeded(r, email)
	h.cookies.IssueCookie(w, signed, int(token.TTL.Seconds()))
	h.log.Info("auth: signed in", "user_id", user.ID, "email", user.Email)
	http.Redirect(w, r, next, http.StatusSeeOther)
}

func (h *AuthHandler) loginError(w http.ResponseWriter, r *http.Request, next, message string) {
	w.WriteHeader(http.StatusUnauthorized)
	h.render.Render(w, r, "login", PageData{
		Title: "Sign in",
		Flash: &Flash{Kind: "danger", Message: message},
		Data:  map[string]any{"next": next, "email": r.PostFormValue("email")},
	})
}

// APILogin issues a session token for a script.
//
// It exists because the operator API is authenticated by the same token the
// browser holds, and without this the only way to obtain one was to read the
// cookie out of a browser. An API nobody can authenticate to is not an API.
//
// It sets no cookie: a caller that wanted one would be using the form.
func (h *AuthHandler) APILogin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := httpx.DecodeJSON(w, r, &body); err != nil {
		httpx.Fail(w, http.StatusBadRequest, tr(r, "the request body could not be read"))
		return
	}

	if !h.loginAllowed(r, body.Email) {
		httpx.Fail(w, http.StatusTooManyRequests, tr(r, "too many attempts"))
		return
	}

	user, signed, err := h.auth.Login(r.Context(), body.Email, body.Password)
	if err != nil {
		// One answer for a wrong address and a wrong password, the same as the
		// form: telling them apart turns this into an account enumerator.
		if errors.Is(err, service.ErrInvalidLogin) || errors.Is(err, service.ErrInactiveUser) {
			httpx.Fail(w, http.StatusUnauthorized, tr(r, "email or password is incorrect"))
			return
		}
		h.log.Error("auth: API login failed", err.Error())
		httpx.Fail(w, http.StatusInternalServerError, tr(r, "sign in could not be completed"))
		return
	}

	h.loginSucceeded(r, body.Email)
	h.log.Info("auth: API token issued", "user_id", user.ID)
	httpx.OK(w, map[string]any{
		"token":      signed,
		"expires_in": int(token.TTL.Seconds()),
		"user":       user,
	})
}

// Logout ends the session everywhere, not just in this browser.
//
// POST only. A logout reachable by GET can be fired by any page that embeds
// the URL as an image.
func (h *AuthHandler) Logout(w http.ResponseWriter, r *http.Request) {
	if user := httpx.UserFrom(r.Context()); user != nil {
		if err := h.auth.Logout(r.Context(), user.ID); err != nil {
			h.log.Warn("auth: sessions could not be retired", err.Error(), "user_id", user.ID)
		}
	}
	h.cookies.ClearCookie(w)
	if httpx.IsHTMX(r) {
		w.Header().Set("HX-Redirect", "/login")
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// Profile shows the signed in operator's own account.
func (h *AuthHandler) Profile(w http.ResponseWriter, r *http.Request) {
	h.render.Render(w, r, "profile", PageData{Title: "Profile", Active: "profile"})
}

// ChangePassword updates the operator's own password.
func (h *AuthHandler) ChangePassword(w http.ResponseWriter, r *http.Request) {
	user := httpx.UserFrom(r.Context())
	if user == nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}

	current := r.PostFormValue("current_password")
	next := r.PostFormValue("new_password")
	confirm := r.PostFormValue("confirm_password")

	if next != confirm {
		h.profileFlash(w, r, "danger", "The two new passwords do not match.")
		return
	}

	if err := h.auth.ChangePassword(r.Context(), user.ID, current, next); err != nil {
		var ve *service.ValidationError
		if errors.As(err, &ve) {
			h.profileFlash(w, r, "danger", ve.Errors[0].Message)
			return
		}
		h.log.Error("auth: password change failed", err.Error(), "user_id", user.ID)
		h.profileFlash(w, r, "danger", "The password could not be changed.")
		return
	}

	// The password change retired every token, including this one, so the
	// browser has to sign in again. Leaving the dead cookie in place would
	// show a confusing redirect loop instead.
	h.cookies.ClearCookie(w)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (h *AuthHandler) profileFlash(w http.ResponseWriter, r *http.Request, kind, message string) {
	h.render.Render(w, r, "profile", PageData{
		Title:  "Profile",
		Active: "profile",
		Flash:  &Flash{Kind: kind, Message: message},
	})
}
