package router

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/mstgnz/cronsole/v2/internal/domain"
	"github.com/mstgnz/cronsole/v2/pkg/auth"
)

// The forgotten password flow, driven end to end.
//
// Everything here is reachable without a session, which is what makes it worth
// testing through the router rather than against the service: what has to hold
// is not only that the token works, but that the screens in front of it give
// nothing away and that a signed-in caller is sent elsewhere.

// requestLink asks for a reset and returns the token that would have been
// mailed. The database only holds a hash, so the mailer is the only place a
// test can read it, exactly as it is the only place a person can.
func requestLink(t *testing.T, h *harness, email string) string {
	t.Helper()

	w := h.post("/forgot", "", url.Values{"email": {email}})
	h.mustCode(w, http.StatusOK, "the reset request")

	to, token, sends := h.mail.last()
	if sends == 0 {
		t.Fatal("no link was mailed")
	}
	if to != email {
		t.Fatalf("the link went to %q, want %q", to, email)
	}
	return token
}

func TestTheLoginScreenOffersTheWayBackIn(t *testing.T) {
	h := newHarness(t)
	h.admin("admin@example.com")

	w := h.get("/login", "")
	h.mustCode(w, http.StatusOK, "the login screen")
	if !bodyContains(w, `href="/forgot"`) {
		t.Error("the login screen does not link to the reset form")
	}
}

func TestTheWholeResetFlow(t *testing.T) {
	h := newHarness(t)
	h.adminWithPassword("admin@example.com", "the-first-password")

	token := requestLink(t, h, "admin@example.com")

	// The form is reachable with the token in the URL.
	page := h.get("/reset?token="+url.QueryEscape(token), "")
	h.mustCode(page, http.StatusOK, "the reset form")
	if !bodyContains(page, token) {
		t.Error("the form does not carry the token back")
	}

	w := h.post("/reset", "", url.Values{
		"token":            {token},
		"password":         {"the-replacement-password"},
		"confirm_password": {"the-replacement-password"},
	})
	h.mustCode(w, http.StatusOK, "setting the new password")
	// Not signed in by the reset: the answer is the login screen.
	if !bodyContains(w, `action="/login"`) {
		t.Error("the answer is not the sign in screen")
	}
	for _, c := range w.Result().Cookies() {
		if c.Name == "cj_session" && c.Value != "" {
			t.Error("a session was issued by the reset itself")
		}
	}

	// And the new password is the one that works.
	signIn := h.post("/login", "", url.Values{
		"email": {"admin@example.com"}, "password": {"the-replacement-password"},
	})
	if signIn.Code != http.StatusSeeOther {
		t.Fatalf("signing in with the new password answered %d", signIn.Code)
	}
	old := h.post("/login", "", url.Values{
		"email": {"admin@example.com"}, "password": {"the-first-password"},
	})
	if old.Code == http.StatusSeeOther {
		t.Error("the old password still signs in")
	}
}

func TestTheResetFormAnswersTheSameForAnyAddress(t *testing.T) {
	// The page must not become a way to ask who has an account here.
	h := newHarness(t)
	h.admin("admin@example.com")

	known := h.post("/forgot", "", url.Values{"email": {"admin@example.com"}})
	unknown := h.post("/forgot", "", url.Values{"email": {"nobody@example.com"}})

	if known.Code != unknown.Code {
		t.Errorf("known answered %d and unknown %d; they have to match", known.Code, unknown.Code)
	}
	if known.Body.String() != unknown.Body.String() {
		t.Error("the two answers differ, so the form says which addresses are registered")
	}
	// One mail, for the address that exists.
	if _, _, sends := h.mail.last(); sends != 1 {
		t.Errorf("%d message(s) were sent, want exactly the one", sends)
	}
}

func TestASpentLinkIsRefusedByTheScreen(t *testing.T) {
	h := newHarness(t)
	h.adminWithPassword("admin@example.com", "the-first-password")

	token := requestLink(t, h, "admin@example.com")
	form := url.Values{
		"token":            {token},
		"password":         {"the-replacement-password"},
		"confirm_password": {"the-replacement-password"},
	}
	h.mustCode(h.post("/reset", "", form), http.StatusOK, "the first use")

	again := h.post("/reset", "", form)
	h.mustCode(again, http.StatusBadRequest, "the second use")
	// And the refusal offers the only thing that helps, rather than a form
	// that would be refused again.
	if !bodyContains(again, `href="/forgot"`) {
		t.Error("the refusal does not offer a new link")
	}
}

func TestAnInventedTokenIsRefused(t *testing.T) {
	h := newHarness(t)
	h.admin("admin@example.com")

	w := h.post("/reset", "", url.Values{
		"token":            {auth.RandomHex(32)},
		"password":         {"a-long-enough-password"},
		"confirm_password": {"a-long-enough-password"},
	})
	h.mustCode(w, http.StatusBadRequest, "an invented token")
}

func TestTheResetFormRefusesWhatItShould(t *testing.T) {
	cases := []struct {
		name     string
		password string
		confirm  string
	}{
		{"the two passwords differ", "a-long-enough-password", "a-long-enough-passwerd"},
		{"the password is too short", "short", "short"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			h.adminWithPassword("admin@example.com", "the-first-password")
			token := requestLink(t, h, "admin@example.com")

			w := h.post("/reset", "", url.Values{
				"token": {token}, "password": {c.password}, "confirm_password": {c.confirm},
			})
			h.mustCode(w, http.StatusBadRequest, "the refused reset")
			// The link survives a refused attempt, so the same mail still works.
			if !bodyContains(w, token) {
				t.Error("the form dropped the token, so the mail has to be requested again")
			}
			if bodyContains(w, c.password) && c.password != "short" {
				t.Error("the password was echoed back into the page")
			}

			// And the old password still signs in, because nothing was set.
			signIn := h.post("/login", "", url.Values{
				"email": {"admin@example.com"}, "password": {"the-first-password"},
			})
			if signIn.Code != http.StatusSeeOther {
				t.Error("the account's password was changed by a refused attempt")
			}
		})
	}
}

func TestASignedInOperatorIsNotOfferedTheResetScreens(t *testing.T) {
	// They are behind GuestOnly with the login form: somebody with a session
	// changes their password on the profile screen, where the current one is
	// asked for.
	h := newHarness(t)
	_, session := h.admin("admin@example.com")

	for _, path := range []string{"/forgot", "/reset"} {
		w := h.get(path, session)
		if w.Code != http.StatusSeeOther {
			t.Errorf("GET %s answered %d for a signed in operator, want a redirect", path, w.Code)
		}
	}
}

func TestTheResetPostIsOriginChecked(t *testing.T) {
	h := newHarness(t)
	h.admin("admin@example.com")

	for _, path := range []string{"/forgot", "/reset"} {
		w := h.do(request{
			method: http.MethodPost, path: path,
			form:    url.Values{"email": {"admin@example.com"}, "token": {"x"}},
			headers: map[string]string{"Origin": "https://evil.example"},
		})
		h.mustCode(w, http.StatusForbidden, "a cross origin post to "+path)
	}
}

func TestTheMailedLinkPointsAtTheResetScreen(t *testing.T) {
	// The token has to survive the round trip through a URL, which is the one
	// part of the flow a person performs by hand.
	h := newHarness(t)
	h.admin("admin@example.com")
	token := requestLink(t, h, "admin@example.com")

	if strings.TrimSpace(token) == "" {
		t.Fatal("an empty token was mailed")
	}
	w := h.get("/reset?token="+url.QueryEscape(token), "")
	h.mustCode(w, http.StatusOK, "the link from the mail")
}

func TestTheResetFormIsReachableWithoutASession(t *testing.T) {
	h := newHarness(t)
	h.admin("admin@example.com")

	w := h.get("/forgot", "")
	h.mustCode(w, http.StatusOK, "the reset request form")
	if !bodyContains(w, `action="/forgot"`) {
		t.Error("the page carries no form to submit")
	}
}

// brokenResets fails the write that records a link.
type brokenResets struct{ domain.PasswordResetRepository }

func (b brokenResets) Create(context.Context, *domain.PasswordReset) (int64, error) {
	return 0, errNotAnswering
}

func TestAResetIsNotPromisedWhenTheLinkCannotBeRecorded(t *testing.T) {
	// The alternative is telling somebody to wait for a message that was never
	// going to arrive, and they would wait rather than ask an administrator.
	h := newHarnessWith(t, func(s *repoSet) {
		s.resets = brokenResets{PasswordResetRepository: s.resets}
	})
	h.mw.SetupCompleted()
	h.adminWithPassword("admin@example.com", "the-first-password")

	w := h.post("/forgot", "", url.Values{"email": {"admin@example.com"}})
	h.mustCode(w, http.StatusBadRequest, "a reset request that could not be recorded")
	if _, _, sends := h.mail.last(); sends != 0 {
		t.Error("a link was mailed that nothing recorded")
	}
	if strings.Contains(w.Body.String(), "pq:") {
		t.Error("the database's own error reached the page")
	}
}

func TestADeploymentWithNoMailSaysSoRatherThanPromising(t *testing.T) {
	// resets left nil is what a deployment with no mail server produces: the
	// service refuses instead of pretending.
	h := newHarnessWith(t, func(s *repoSet) { s.resets = nil })
	h.mw.SetupCompleted()
	h.adminWithPassword("admin@example.com", "the-first-password")

	w := h.post("/forgot", "", url.Values{"email": {"admin@example.com"}})
	h.mustCode(w, http.StatusBadRequest, "a reset request with no mail server")
	if !bodyContains(w, "administrator") && !bodyContains(w, "yöneticiden") {
		t.Errorf("the page does not say what to do instead: %s", firstLineOf(w.Body.String()))
	}

	// And the second screen refuses for the same reason rather than pretending
	// a token could ever be resolved.
	spend := h.post("/reset", "", url.Values{
		"token": {"whatever"}, "password": {"a-long-enough-password"},
		"confirm_password": {"a-long-enough-password"},
	})
	h.mustCode(spend, http.StatusBadRequest, "spending a link with no mail server")
}
