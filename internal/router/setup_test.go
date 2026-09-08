package router

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/mstgnz/cronsole/v2/internal/domain"
	"github.com/mstgnz/cronsole/v2/internal/middleware"
	"github.com/mstgnz/cronsole/v2/internal/repository/memrepo"
)

// The first run, driven through the real router.
//
// This is the one screen that writes without a caller behind it, so what has to
// be demonstrated is not that the form works but that the window closes: while
// no account exists nothing else is reachable, and the moment one does exist the
// screen is gone and cannot be reopened.

// goodSetupForm is a submission that should be accepted.
func goodSetupForm() url.Values {
	return url.Values{
		"fullname":         {"Site Owner"},
		"email":            {"owner@example.com"},
		"password":         {"a-long-enough-password"},
		"confirm_password": {"a-long-enough-password"},
	}
}

func TestNothingButTheSetupScreenIsReachableBeforeTheFirstAccount(t *testing.T) {
	h := newFreshHarness(t)

	// The login screen is in the list on purpose. There is nothing to log in
	// to yet, and leaving it reachable would mean the deployment answers two
	// different unauthenticated screens while it is at its most open.
	for _, path := range []string{"/", "/login", "/jobs", "/runs", "/projects", "/settings", "/profile", "/docs"} {
		w := h.get(path, "")
		if w.Code != http.StatusSeeOther {
			t.Errorf("GET %s answered %d, want a redirect to the setup screen", path, w.Code)
			continue
		}
		if to := w.Header().Get("Location"); to != middleware.SetupPath {
			t.Errorf("GET %s redirected to %q, want %q", path, to, middleware.SetupPath)
		}
	}
}

func TestAnUnknownPathBeforeSetupGoesToTheSetupScreen(t *testing.T) {
	h := newFreshHarness(t)

	// The catch-all sends unknown pages to the dashboard, and the dashboard is
	// behind the gate. Worth pinning: the gate is mounted as a group, and a
	// NotFound handler registered outside that group would answer without it.
	w := h.get("/nothing-here", "")
	if to := w.Header().Get("Location"); to != middleware.SetupPath {
		t.Errorf("an unknown path redirected to %q, want %q", to, middleware.SetupPath)
	}
}

func TestTheAPIAnswersJSONBeforeSetupRatherThanARedirect(t *testing.T) {
	h := newFreshHarness(t)

	// A deploy pipeline is the likeliest first caller of a new deployment, and
	// following a 303 would hand it a sign-up form to parse as its payload.
	w := h.apiGet("/api/v1/jobs", "cj_"+strings.Repeat("k", 44))
	h.mustCode(w, http.StatusServiceUnavailable, "the API before setup")
	body := h.envelope(w)
	if status, _ := body["status"].(bool); status {
		t.Errorf("the API reported success before setup: %s", w.Body.String())
	}
	if message, _ := body["message"].(string); message == "" {
		t.Errorf("the API answered no message before setup: %s", w.Body.String())
	}
}

func TestTheProbesAndStyleSheetStayOutsideTheSetupGate(t *testing.T) {
	h := newFreshHarness(t)

	// An orchestrator asking whether the process is alive must get an answer
	// rather than a redirect to a form, and the setup screen is unreadable
	// without its stylesheet.
	for _, path := range []string{"/healthz", "/static/app.css", "/static/app.js", "/static/favicon.svg"} {
		if w := h.get(path, ""); w.Code != http.StatusOK {
			t.Errorf("GET %s answered %d before setup, want 200", path, w.Code)
		}
	}
}

func TestTheTabIconIsServedAsAnImage(t *testing.T) {
	// The layout names this file, so a rename that misses one of the two ends
	// as a 404 nobody notices: a missing favicon looks exactly like a browser
	// that has not fetched it yet.
	h := newFreshHarness(t)

	w := h.get("/static/favicon.svg", "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET /static/favicon.svg answered %d", w.Code)
	}
	if got := w.Header().Get("Content-Type"); !strings.Contains(got, "image/svg+xml") {
		t.Errorf("Content-Type = %q, want image/svg+xml", got)
	}
}

func TestTheSetupScreenAsksForTheFirstAccount(t *testing.T) {
	h := newFreshHarness(t)

	w := h.get(middleware.SetupPath, "")
	h.mustCode(w, http.StatusOK, "the setup screen")

	for _, field := range []string{`name="fullname"`, `name="email"`, `name="password"`, `name="confirm_password"`} {
		if !bodyContains(w, field) {
			t.Errorf("the setup form has no %s", field)
		}
	}
}

func TestCompletingSetupCreatesAnAdministratorAndSignsThemIn(t *testing.T) {
	h := newFreshHarness(t)

	w := h.post(middleware.SetupPath, "", goodSetupForm())
	h.mustCode(w, http.StatusSeeOther, "completing setup")
	if to := w.Header().Get("Location"); to != "/" {
		t.Errorf("setup redirected to %q, want the dashboard", to)
	}

	// The account is an active administrator, and neither of those came from
	// the form: the repository decides the shape of the first account.
	users := memrepo.Users{Store: h.store}
	count, err := users.Count(t.Context())
	if err != nil {
		t.Fatalf("Count = %v", err)
	}
	if count != 1 {
		t.Fatalf("the deployment holds %d accounts, want exactly the one just created", count)
	}
	user, err := users.GetByEmail(t.Context(), "owner@example.com")
	if err != nil {
		t.Fatalf("GetByEmail = %v", err)
	}
	if !user.IsAdmin || !user.Active {
		t.Errorf("the first account is admin=%v active=%v, want both true", user.IsAdmin, user.Active)
	}
	if user.Password == "" || strings.Contains(user.Password, "a-long-enough-password") {
		t.Error("the password was not hashed")
	}

	// Signed in by the same response, so the operator lands on the dashboard
	// rather than on a login form they have no session for.
	session := sessionCookie(t, w.Result().Cookies())
	if got := h.get("/", session); got.Code != http.StatusOK {
		t.Errorf("the dashboard answered %d to the session setup issued, want 200", got.Code)
	}
}

func TestTheSetupScreenIsGoneOnceTheAccountExists(t *testing.T) {
	h := newFreshHarness(t)

	h.mustCode(h.post(middleware.SetupPath, "", goodSetupForm()), http.StatusSeeOther, "completing setup")

	// Not a redirect and not a form: the screen does not exist any more.
	h.mustCode(h.get(middleware.SetupPath, ""), http.StatusNotFound, "the setup screen after setup")
	h.mustCode(h.post(middleware.SetupPath, "", goodSetupForm()), http.StatusNotFound, "a second setup submission")

	// And the rest of the service is reachable again.
	h.mustCode(h.get("/login", ""), http.StatusOK, "the login screen after setup")
}

func TestSetupIsClosedOnADeploymentThatAlreadyHasAnAccount(t *testing.T) {
	// The gate's own latch is not what closes the screen here: this process has
	// never seen the deployment set up, so it has to read the answer from
	// storage. A restarted instance is exactly this case.
	h := newFreshHarness(t)
	h.store.AddUser(domain.User{Fullname: "Someone", Email: "someone@example.com", Active: true})

	h.mustCode(h.get(middleware.SetupPath, ""), http.StatusNotFound, "the setup screen")
	h.mustCode(h.post(middleware.SetupPath, "", goodSetupForm()), http.StatusNotFound, "a setup submission")
	h.mustCode(h.get("/login", ""), http.StatusOK, "the login screen")
}

func TestSetupRefusesWhatItCannotAccept(t *testing.T) {
	cases := []struct {
		name string
		form url.Values
	}{
		{"the two passwords differ", url.Values{
			"fullname": {"Site Owner"}, "email": {"owner@example.com"},
			"password": {"a-long-enough-password"}, "confirm_password": {"a-long-enough-passwerd"},
		}},
		{"the password is too short", url.Values{
			"fullname": {"Site Owner"}, "email": {"owner@example.com"},
			"password": {"short"}, "confirm_password": {"short"},
		}},
		{"the address is not one", url.Values{
			"fullname": {"Site Owner"}, "email": {"owner"},
			"password": {"a-long-enough-password"}, "confirm_password": {"a-long-enough-password"},
		}},
		{"there is no name", url.Values{
			"fullname": {"   "}, "email": {"owner@example.com"},
			"password": {"a-long-enough-password"}, "confirm_password": {"a-long-enough-password"},
		}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newFreshHarness(t)

			w := h.post(middleware.SetupPath, "", c.form)
			h.mustCode(w, http.StatusBadRequest, "the refused submission")

			users := memrepo.Users{Store: h.store}
			count, err := users.Count(t.Context())
			if err != nil {
				t.Fatalf("Count = %v", err)
			}
			if count != 0 {
				t.Fatalf("%d accounts exist after a refused submission, want none", count)
			}

			// The screen is still open, and what was typed is still in it.
			if !bodyContains(w, `name="confirm_password"`) {
				t.Error("the form was not rendered again")
			}
			if !bodyContains(w, "Site Owner") && !bodyContains(w, "owner@example.com") {
				t.Error("the form came back empty; it should keep what was typed")
			}
			// Never the password, whichever way the submission failed.
			if bodyContains(w, "a-long-enough-password") {
				t.Error("the password was echoed back into the page")
			}
		})
	}
}

func TestSetupRefusesACrossOriginSubmission(t *testing.T) {
	h := newFreshHarness(t)

	w := h.do(request{
		method: http.MethodPost, path: middleware.SetupPath, form: goodSetupForm(),
		headers: map[string]string{"Origin": "https://evil.example"},
	})
	h.mustCode(w, http.StatusForbidden, "a cross origin setup submission")

	users := memrepo.Users{Store: h.store}
	if count, _ := users.Count(t.Context()); count != 0 {
		t.Fatalf("%d accounts exist after a cross origin submission, want none", count)
	}
}

// sessionCookie pulls the session out of a response, failing when there is none.
func sessionCookie(t *testing.T, cookies []*http.Cookie) string {
	t.Helper()

	for _, c := range cookies {
		if c.Name != middleware.CookieName {
			continue
		}
		if c.Value == "" {
			t.Fatal("the session cookie was cleared, not issued")
		}
		if !c.HttpOnly {
			t.Error("the session cookie is readable by script")
		}
		return c.Value
	}
	t.Fatal("no session cookie was issued")
	return ""
}
