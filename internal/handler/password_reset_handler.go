package handler

import (
	"errors"
	"net/http"

	"github.com/mstgnz/cronsole/v2/internal/service"
)

// The forgotten password screens.
//
// Two unauthenticated screens, and both are written around the same rule: the
// answer must not depend on whether the address is registered. An internal
// console's account list is also a staff list, and a form that says "no such
// account" hands it out to anybody who can reach the login page.
//
// The limits in front of these routes are the router's, alongside the login
// form's, so all three sit in one place.

// ForgotPage shows the "send me a link" form.
func (h *AuthHandler) ForgotPage(w http.ResponseWriter, r *http.Request) {
	h.render.Render(w, r, "forgot", PageData{Title: "Reset your password"})
}

// Forgot mails a reset link, when there is an account to mail it to.
func (h *AuthHandler) Forgot(w http.ResponseWriter, r *http.Request) {
	email := r.PostFormValue("email")

	// Counted against the address as well as the caller's own, so a spray
	// across many addresses from one place and a flood at one address from
	// many places are both bounded. Answered like the success case even when
	// it trips, or the limit itself becomes the oracle.
	if !h.loginAllowed(r, email) {
		h.forgotSent(w, r)
		return
	}

	err := h.auth.RequestPasswordReset(r.Context(), email)
	switch {
	case errors.Is(err, service.ErrResetUnavailable):
		// No mail server, so no link can arrive. Saying so is better than a
		// promise nobody can keep, and it is not a secret about any account.
		h.forgotError(w, r, tr(r, "Password reset is not available here. Ask an administrator to set a new password for you."))
		return
	case err != nil:
		h.log.Error("auth: the reset link could not be prepared", err.Error())
		h.forgotError(w, r, tr(r, "The link could not be sent. Try again in a moment."))
		return
	}

	h.forgotSent(w, r)
}

// forgotSent is the one answer both the found and the not-found case get.
func (h *AuthHandler) forgotSent(w http.ResponseWriter, r *http.Request) {
	message := tr(r, "If that address has an account, a link is on its way.")
	h.render.Render(w, r, "forgot", PageData{
		Title: "Reset your password",
		Flash: &Flash{Kind: "success", Message: message},
		Data:  map[string]any{"sent": true},
	})
}

// forgotError re-renders the form. The message arrives translated, because the
// catalogue is guarded by a test that scans for tr call literals.
func (h *AuthHandler) forgotError(w http.ResponseWriter, r *http.Request, message string) {
	w.WriteHeader(http.StatusBadRequest)
	h.render.Render(w, r, "forgot", PageData{
		Title: "Reset your password",
		Flash: &Flash{Kind: "danger", Message: message},
		Data:  map[string]any{"email": r.PostFormValue("email")},
	})
}

// ResetPage shows the new password form for a mailed link.
//
// The token is not checked here. Doing so would answer "is this link real"
// to anybody who can construct a URL, and the form is harmless without it:
// the check that matters happens on the submission.
func (h *AuthHandler) ResetPage(w http.ResponseWriter, r *http.Request) {
	h.render.Render(w, r, "reset", PageData{
		Title: "Choose a new password",
		Data:  map[string]any{"token": r.URL.Query().Get("token")},
	})
}

// Reset spends the link and sets the password.
func (h *AuthHandler) Reset(w http.ResponseWriter, r *http.Request) {
	rawToken := r.PostFormValue("token")
	password := r.PostFormValue("password")

	if !h.limiter.Allow(h.proxy.ClientIP(r)) {
		h.resetError(w, r, rawToken, tr(r, "Too many attempts. Try again in a few minutes."))
		return
	}
	if password != r.PostFormValue("confirm_password") {
		h.resetError(w, r, rawToken, tr(r, "The two passwords do not match."))
		return
	}

	err := h.auth.CompletePasswordReset(r.Context(), rawToken, password)
	if err != nil {
		var ve *service.ValidationError
		switch {
		case errors.Is(err, service.ErrResetLinkInvalid):
			// Unknown, expired, spent, or an account that has since been
			// disabled: one message, because separating them tells somebody
			// guessing which half of the guess was right.
			h.resetError(w, r, "", tr(r, "This link is no longer valid. Ask for a new one."))
		case errors.As(err, &ve):
			h.resetError(w, r, rawToken, ve.Errors[0].Message)
		case errors.Is(err, service.ErrResetUnavailable):
			h.resetError(w, r, "", tr(r, "Password reset is not available here. Ask an administrator to set a new password for you."))
		default:
			h.log.Error("auth: the password could not be reset", err.Error())
			h.resetError(w, r, rawToken, tr(r, "The password could not be set. Try again in a moment."))
		}
		return
	}

	// Deliberately not signed in here. Every session the account had was just
	// invalidated, including one an attacker may have been holding, and asking
	// for the new password once proves it arrived where it was meant to.
	h.render.Render(w, r, "login", PageData{
		Title: "Sign in",
		Flash: &Flash{Kind: "success", Message: tr(r, "Your password has been set. Sign in with it.")},
		Data:  map[string]any{"next": "/"},
	})
}

// resetError re-renders the form with an already translated message.
func (h *AuthHandler) resetError(w http.ResponseWriter, r *http.Request, token, message string) {
	w.WriteHeader(http.StatusBadRequest)
	h.render.Render(w, r, "reset", PageData{
		Title: "Choose a new password",
		Flash: &Flash{Kind: "danger", Message: message},
		// An empty token means the form is not worth re-offering: the link
		// itself is gone, and the way back is a new mail.
		Data: map[string]any{"token": token, "expired": token == ""},
	})
}
