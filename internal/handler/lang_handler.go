package handler

import (
	"net/http"

	"github.com/mstgnz/cronsole/v2/internal/httpx"
	"github.com/mstgnz/cronsole/v2/internal/i18n"
)

// LangHandler switches the interface language.
type LangHandler struct{ secure bool }

// NewLangHandler wires the handler. secure marks the cookie Secure, matching
// the session cookie: off only for a plain HTTP development server, where the
// browser would discard it and the switch would appear to do nothing.
func NewLangHandler(secure bool) *LangHandler { return &LangHandler{secure: secure} }

// Set stores the choice and returns to where the request came from.
//
// A POST rather than a link, so a crawler or a prefetch cannot change somebody's
// language, and so it goes through the same origin check as every other state
// changing route.
func (h *LangHandler) Set(w http.ResponseWriter, r *http.Request) {
	lang := r.PostFormValue("lang")
	if !i18n.Valid(lang) {
		// An unknown language is a bad request, not a silent fall back to
		// English: a switcher that quietly ignores a value looks like it worked.
		httpx.Fail(w, http.StatusBadRequest, tr(r, "unsupported language"))
		return
	}

	http.SetCookie(w, &http.Cookie{
		Name:  httpx.LangCookie,
		Value: lang,
		Path:  "/",
		// A year. This is a preference, not a session: somebody who picked
		// Turkish should not be back in English next week.
		MaxAge: 365 * 24 * 60 * 60,
		// Not HttpOnly. Nothing here is a credential, and leaving it readable
		// lets the page reflect the choice without a round trip if that is ever
		// wanted.
		Secure:   h.secure,
		SameSite: http.SameSiteLaxMode,
	})

	// Back to the page they were on. Sanitised, because the return path is
	// caller supplied and an unchecked one turns the language switcher into an
	// open redirect.
	target := httpx.SanitizeRedirect(r.PostFormValue("next"), "/")
	if httpx.IsHTMX(r) {
		w.Header().Set("HX-Redirect", target)
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}
