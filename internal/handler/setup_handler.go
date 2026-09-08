package handler

import (
	"errors"
	"net/http"

	"github.com/mstgnz/cronsole/v2/internal/applog"

	"github.com/mstgnz/cronsole/v2/internal/middleware"
	"github.com/mstgnz/cronsole/v2/internal/service"
	"github.com/mstgnz/cronsole/v2/pkg/token"
)

// SetupHandler creates the first administrator on a fresh deployment.
//
// The only screen in the service that writes without a caller behind it, which
// is why so little of the decision lives here: middleware.SetupGate decides
// whether it exists at all, and the INSERT itself decides whether the table was
// empty. This handler reads a form and hands it over.
//
// It is rate limited, and by the router rather than here. The login form keeps
// a limiter of its own because it has a second thing to count, one bucket per
// address tried; this form has only the caller's own address, which is exactly
// what the middleware already counts. Two limiters over one key would read as
// two controls and drift into one.
type SetupHandler struct {
	auth    *service.AuthService
	cookies *middleware.Set
	render  *Renderer
	log     *applog.Logger
}

// NewSetupHandler wires the handler.
func NewSetupHandler(authService *service.AuthService, cookies *middleware.Set,
	render *Renderer, log *applog.Logger) *SetupHandler {

	return &SetupHandler{auth: authService, cookies: cookies, render: render, log: log}
}

// Show renders the form.
func (h *SetupHandler) Show(w http.ResponseWriter, r *http.Request) {
	h.render.Render(w, r, "setup", PageData{Title: "Set up Cronsole"})
}

// Save creates the account and signs the person in.
func (h *SetupHandler) Save(w http.ResponseWriter, r *http.Request) {
	password := r.PostFormValue("password")
	if password != r.PostFormValue("confirm_password") {
		h.fail(w, r, tr(r, "The two passwords do not match."))
		return
	}

	user, err := h.auth.CompleteSetup(r.Context(), service.UserInput{
		Fullname: r.PostFormValue("fullname"),
		Email:    r.PostFormValue("email"),
		Password: password,
	})
	if err != nil {
		var ve *service.ValidationError
		switch {
		case errors.Is(err, service.ErrSetupDone):
			// Somebody else got there first, which is a race this deployment
			// only has once. Send them to the login screen: the account exists
			// and this screen is gone.
			http.Redirect(w, r, "/login", http.StatusSeeOther)
		case errors.As(err, &ve):
			h.fail(w, r, ve.Errors[0].Message)
		default:
			h.log.Error("setup: the first administrator could not be created", err.Error())
			h.fail(w, r, tr(r, "The account could not be created."))
		}
		return
	}

	// Latched here rather than left to the next request's count, so the
	// redirect below does not bounce straight back to this screen.
	h.cookies.SetupCompleted()

	signed, err := h.auth.IssueToken(user)
	if err != nil {
		// The account exists, which is the part that mattered. Signing in is
		// the login screen's job from here.
		h.log.Error("setup: the session could not be issued", err.Error(), "user_id", user.ID)
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}

	h.cookies.IssueCookie(w, signed, int(token.TTL.Seconds()))
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// fail re-renders the form with a message, keeping what was typed. The message
// arrives translated: the catalogue is kept honest by a test that scans the
// handlers for tr call literals, and a string translated in here would be
// invisible to it.
func (h *SetupHandler) fail(w http.ResponseWriter, r *http.Request, message string) {
	w.WriteHeader(http.StatusBadRequest)
	h.render.Render(w, r, "setup", PageData{
		Title: "Set up Cronsole",
		Flash: &Flash{Kind: "danger", Message: message},
		Data: map[string]any{
			// The password is never echoed back; the rest saves retyping.
			"fullname": r.PostFormValue("fullname"),
			"email":    r.PostFormValue("email"),
		},
	})
}
