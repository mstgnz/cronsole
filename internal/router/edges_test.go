package router

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/mstgnz/cronsole/v2/internal/authz"
	"github.com/mstgnz/cronsole/v2/internal/domain"
	"github.com/mstgnz/cronsole/v2/pkg/auth"
)

// The paths that are only taken when something is wrong, and the ones taken by
// htmx rather than by a browser navigation. Both are where a screen breaks in
// the way nobody sees during ordinary use: an error branch that renders a
// half-page, or a fragment swapped into the wrong place.

// htmx issues a request the way the interface does.
func (h *harness) htmx(method, path, token string, form url.Values) *httptest.ResponseRecorder {
	h.t.Helper()
	return h.do(request{
		method: method, path: path, token: token, form: form,
		headers: map[string]string{
			"HX-Request": "true",
			"Origin":     "http://example.com",
		},
	})
}

// --- ids that are not ids ---------------------------------------------------

func TestAnIdThatIsNotAnIdIsNotFound(t *testing.T) {
	// Every id reaches a repository. Zero, negative and unparseable are all
	// "not an id" and must be refused before they become a query.
	h, project, session := console(t)
	h.job(project.ID, "daily", "/cron/daily")

	for _, id := range []string{"0", "-1", "abc", "1.5", "99999999999999999999"} {
		for _, path := range []string{"/jobs/" + id, "/runs/" + id} {
			w := h.get(path, session)
			if w.Code != http.StatusNotFound {
				t.Errorf("GET %s answered %d, want 404", path, w.Code)
			}
		}
	}
}

func TestAnIdThatDoesNotExistIsNotFound(t *testing.T) {
	h, _, session := console(t)

	for _, path := range []string{"/jobs/999", "/runs/999", "/projects/999/members"} {
		if w := h.get(path, session); w.Code != http.StatusNotFound {
			t.Errorf("GET %s answered %d, want 404", path, w.Code)
		}
	}
}

func TestWritingToSomethingThatIsNotThere(t *testing.T) {
	h, _, session := console(t)

	writes := []string{
		"/jobs/999", "/jobs/999/toggle", "/jobs/999/run", "/jobs/999/delete",
		"/jobs/999/clone", "/jobs/999/schedules", "/jobs/999/links",
		"/projects/999", "/projects/999/rotate-key", "/projects/999/delete",
		"/settings/users/999/delete", "/settings/notifications/999/delete",
		"/settings/hosts/999/delete",
	}
	for _, path := range writes {
		w := h.post(path, session, url.Values{})
		if w.Code == http.StatusSeeOther {
			t.Errorf("POST %s succeeded against something that is not there", path)
		}
		if w.Code >= 500 {
			t.Errorf("POST %s answered %d; a missing row is not a server fault", path, w.Code)
		}
	}
}

// --- what a form refuses ----------------------------------------------------

func TestAJobFormWithNothingInItIsRefusedField(t *testing.T) {
	// Every failure at once rather than one per submission: a form that
	// reports one problem per round trip takes as many round trips as it has
	// mistakes.
	h, _, session := console(t)

	w := h.post("/jobs", session, url.Values{"project_id": {"1"}})
	if w.Code == http.StatusSeeOther {
		t.Fatal("an empty job form was accepted")
	}
	body := w.Body.String()
	for _, field := range []string{"code", "name", "url"} {
		if !strings.Contains(body, field) {
			t.Errorf("the answer does not mention %q: %s", field, firstLineOf(body))
		}
	}
}

func TestAJobFormRefusesValuesOutsideTheirRange(t *testing.T) {
	h, _, session := console(t)

	cases := []struct {
		name string
		form url.Values
	}{
		{"a timeout past the ceiling", url.Values{"timeout_sec": {"99999"}}},
		{"a negative timeout", url.Values{"timeout_sec": {"-1"}}},
		{"a success range that is backwards", url.Values{"success_min": {"500"}, "success_max": {"200"}}},
		{"a method that is not one", url.Values{"method": {"FETCH"}}},
		{"a code with spaces", url.Values{"code": {"not a code"}}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			form := url.Values{
				"project_id": {"1"}, "code": {"daily"}, "name": {"Daily"},
				"url": {"/cron/daily"}, "method": {"GET"}, "timeout_sec": {"30"},
				"success_min": {"200"}, "success_max": {"299"},
			}
			for k, v := range c.form {
				form[k] = v
			}

			w := h.post("/jobs", session, form)
			if w.Code == http.StatusSeeOther {
				t.Error("it was accepted")
			}
			if h.store.Job(1) != nil {
				t.Errorf("it was stored: %+v", h.store.Job(1))
			}
		})
	}
}

func TestAProjectFormRefusesABadSlug(t *testing.T) {
	// A slug ends up in URLs, log lines and alert subjects, so the set it may
	// contain is narrow on purpose.
	h := newHarness(t)
	_, session := h.admin("admin@example.com")

	for _, slug := range []string{"A B", "../etc", "Şirket", "-leading", strings.Repeat("x", 100)} {
		w := h.post("/projects", session, url.Values{
			"name": {"Brand"}, "slug": {slug}, "active": {"true"},
		})
		if w.Code == http.StatusSeeOther {
			t.Errorf("the slug %q was accepted", slug)
		}
	}
	if h.store.Project(1) != nil {
		t.Errorf("a project was created: %+v", h.store.Project(1))
	}
}

func TestAnEmptySlugIsDerivedFromTheName(t *testing.T) {
	// The form offers one rather than refusing: asking somebody to invent a
	// slug for "Shop" is a question with one sensible answer.
	h := newHarness(t)
	_, session := h.admin("admin@example.com")

	h.post("/projects", session, url.Values{
		"name": {"Şirket Mağaza"}, "slug": {""}, "active": {"true"},
	})

	project := h.store.Project(1)
	if project == nil {
		t.Fatal("no project was created")
	}
	if project.Slug == "" {
		t.Error("the project was stored with no slug")
	}
	// Whatever it derived has to be a usable slug, because it goes into a URL.
	for _, r := range project.Slug {
		ok := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_'
		if !ok {
			t.Errorf("the derived slug %q contains %q, which is not URL safe", project.Slug, r)
		}
	}
}

func TestAPasswordChangeRefusesWhatItShould(t *testing.T) {
	h := newHarness(t)
	h.store.AddUser(domain.User{
		Email: "admin@example.com", Active: true, IsAdmin: true,
		Password: auth.HashAndSalt("the-old-password"),
	})
	session := h.tokenFor(1)

	cases := []struct {
		name string
		form url.Values
	}{
		{"the two new passwords differ", url.Values{
			"current_password": {"the-old-password"},
			"new_password":     {"a-new-good-password"}, "confirm_password": {"something-else"},
		}},
		{"the new password is too short", url.Values{
			"current_password": {"the-old-password"},
			"new_password":     {"short"}, "confirm_password": {"short"},
		}},
		{"the current password is wrong", url.Values{
			"current_password": {"not-it"},
			"new_password":     {"a-new-good-password"}, "confirm_password": {"a-new-good-password"},
		}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h.post("/profile/password", session, c.form)
			if !auth.ComparePassword(h.userPassword(1), "the-old-password") {
				t.Error("the password changed")
			}
		})
	}
}

// --- htmx ------------------------------------------------------------------

func TestHTMXGetsAFragmentAndARedirectHeader(t *testing.T) {
	// A fragment is swapped into the page, so it must not carry a layout; and a
	// redirect for htmx is a header on a 2xx, because htmx does not follow a
	// 303 the way a browser does.
	h, project, session := console(t)
	h.job(project.ID, "daily", "/cron/daily")

	w := h.htmx(http.MethodPost, "/jobs/1/toggle", session, url.Values{"active": {"false"}})
	if w.Code != http.StatusOK {
		t.Errorf("an htmx write answered %d, want 200 with a header", w.Code)
	}
	if w.Header().Get("HX-Redirect") == "" && strings.Contains(w.Body.String(), "<html") {
		t.Error("htmx got a whole document rather than a fragment or a redirect header")
	}
	if h.store.Job(1).Active {
		t.Error("the write did not happen")
	}
}

func TestTheJobListAnswersAFragmentToHTMX(t *testing.T) {
	// The filters swap the table rather than reloading the page.
	h, project, session := console(t)
	h.job(project.ID, "daily", "/cron/daily")

	w := h.do(request{
		method: http.MethodGet, path: "/jobs?q=daily", token: session,
		headers: map[string]string{"HX-Request": "true"},
	})
	h.mustCode(w, http.StatusOK, "the filtered job list")
	if strings.Contains(w.Body.String(), "<!doctype html>") {
		t.Error("the filter returned a whole document rather than the table")
	}
	if !strings.Contains(w.Body.String(), "daily") {
		t.Error("the fragment does not contain the row it filtered to")
	}
}

func TestTheRunListAnswersAFragmentToHTMX(t *testing.T) {
	h, project, session := console(t)
	job := h.job(project.ID, "daily", "/cron/daily")
	h.store.AddRun(domain.Run{JobID: job.ID, Status: domain.StatusSuccess})

	w := h.do(request{
		method: http.MethodGet, path: "/runs?status=success", token: session,
		headers: map[string]string{"HX-Request": "true"},
	})
	h.mustCode(w, http.StatusOK, "the filtered run list")
	if strings.Contains(w.Body.String(), "<!doctype html>") {
		t.Error("the filter returned a whole document")
	}
}

func TestAnExpiredSessionInsideAFragmentAsksForAReload(t *testing.T) {
	// Swapping a login form into a table cell is not a login.
	h := newHarness(t)

	w := h.do(request{
		method: http.MethodGet, path: "/jobs",
		headers: map[string]string{"HX-Request": "true"},
	})
	if got := w.Header().Get("HX-Redirect"); got != "/login" {
		t.Errorf("HX-Redirect = %q, want /login", got)
	}
	if w.Code != http.StatusOK {
		t.Errorf("code = %d; htmx only follows HX-Redirect on a 2xx", w.Code)
	}
}

func TestTheMemberPanelIsAFragmentForHTMX(t *testing.T) {
	h, _, session := console(t)
	colleague := h.newAccount("colleague@example.com")

	w := h.htmx(http.MethodPost, "/projects/1/members", session, url.Values{
		"email": {colleague.Email}, "role": {authz.RoleProjectReader},
	})
	h.mustCode(w, http.StatusOK, "adding a member over htmx")
	if strings.Contains(w.Body.String(), "<!doctype html>") {
		t.Error("the member panel came back as a whole document")
	}
	if !strings.Contains(w.Body.String(), colleague.Email) {
		t.Error("the panel does not show the member that was just added")
	}
}

// --- the API's error shapes -------------------------------------------------

func TestTheAPIAnswersJSONForEveryFailure(t *testing.T) {
	// A machine client parses the body. An HTML error page here reads as a
	// successful response with unexpected content.
	h, project, session := console(t)
	_, key := h.project("keyed", "https://keyed.example.com")
	_ = project

	cases := []struct {
		name string
		req  request
	}{
		{"no key", request{method: http.MethodGet, path: "/api/v1/jobs"}},
		{"a bad key", request{method: http.MethodGet, path: "/api/v1/jobs", apiKey: "cj_" + strings.Repeat("z", 44)}},
		{"a job that is not there", request{method: http.MethodPost, path: "/api/v1/jobs/nope/run", apiKey: key}},
		{"no token", request{method: http.MethodGet, path: "/api/v1/admin/jobs"}},
		{"an id that is not one", request{method: http.MethodGet, path: "/api/v1/admin/jobs/abc", bearer: session}},
		{"a job that is not there, by id", request{method: http.MethodGet, path: "/api/v1/admin/jobs/999", bearer: session}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := h.do(c.req)
			if w.Code < 400 {
				t.Fatalf("answered %d, want a failure", w.Code)
			}
			if got := w.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
				t.Errorf("Content-Type = %q, want JSON", got)
			}
			body := h.envelope(w)
			if body["status"] != false {
				t.Errorf("status = %v, want false", body["status"])
			}
			if message, _ := body["message"].(string); message == "" {
				t.Error("the failure carries no message")
			}
		})
	}
}

func TestTheAPIRefusesAMalformedBody(t *testing.T) {
	h, _, session := console(t)

	bodies := []string{
		``,
		`{`,
		`{"project_id":1,"code":"daily"}{"project_id":1,"code":"other"}`,
		`{"project_id":1,"code":"daily","unknown_field":true}`,
	}
	for _, body := range bodies {
		w := h.do(request{
			method: http.MethodPost, path: "/api/v1/admin/jobs", bearer: session, body: body,
		})
		if w.Code < 400 {
			t.Errorf("the body %q was accepted (%d)", firstLineOf(body), w.Code)
		}
	}
	if h.store.Job(1) != nil {
		t.Error("a job was created from a malformed body")
	}
}

func TestTheAPIRefusesAValidationFailureWithItsFields(t *testing.T) {
	// The field level detail is what lets a pipeline print which value it got
	// wrong instead of "invalid".
	h, _, session := console(t)

	w := h.do(request{
		method: http.MethodPost, path: "/api/v1/admin/jobs", bearer: session,
		body: `{"project_id":1,"code":"","name":"","url":"","method":"GET"}`,
	})
	if w.Code != http.StatusBadRequest && w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("answered %d, want a validation failure", w.Code)
	}
	body := h.envelope(w)
	if body["errors"] == nil {
		t.Errorf("the answer carries no field detail: %s", firstLineOf(w.Body.String()))
	}
}

func TestSyncRefusesAPayloadWithNoJobs(t *testing.T) {
	h := newHarness(t)
	_, key := h.project("shop", "https://shop.example.com")

	for _, body := range []string{`{}`, `{"jobs":[]}`} {
		w := h.apiPost("/api/v1/sync", key, body)
		if w.Code < 400 {
			t.Errorf("the payload %s was accepted (%d)", body, w.Code)
		}
	}
}

func TestPruningAgainstAnEmptyDeclarationIsRefused(t *testing.T) {
	// It would switch off every job the project has, and answer success. That
	// is indistinguishable from the mistake it usually is: a template that
	// rendered nothing, or a variable that was never set. Somebody who means to
	// stop everything sends the jobs with active false.
	h := newHarness(t)
	project, key := h.project("shop", "https://shop.example.com")
	h.job(project.ID, "one", "/cron/one")
	h.job(project.ID, "two", "/cron/two")

	w := h.apiPost("/api/v1/sync", key, `{"prune":true,"jobs":[]}`)
	if w.Code < 400 {
		t.Errorf("pruning against nothing answered %d", w.Code)
	}

	for id := int64(1); id <= 2; id++ {
		job := h.store.Job(id)
		if job == nil {
			t.Fatalf("job %d disappeared", id)
		}
		if !job.Active {
			t.Errorf("job %q was switched off by a payload declaring nothing", job.Code)
		}
	}
}

func TestPruningAgainstARealDeclarationStillWorks(t *testing.T) {
	// The guard above must not have closed the feature itself.
	h := newHarness(t)
	project, key := h.project("shop", "https://shop.example.com")
	h.job(project.ID, "kept", "/cron/kept")
	h.job(project.ID, "retired", "/cron/retired")

	w := h.apiPost("/api/v1/sync", key,
		`{"prune":true,"jobs":[{"code":"kept","name":"Kept","url":"/cron/kept"}]}`)
	h.mustCode(w, http.StatusOK, "sync with prune")

	if kept := h.store.Job(1); kept == nil || !kept.Active {
		t.Errorf("the declared job was switched off: %+v", kept)
	}
	if retired := h.store.Job(2); retired == nil || retired.Active {
		t.Errorf("the undeclared job was not switched off: %+v", retired)
	}
}

// --- the same origin check --------------------------------------------------

func TestACrossOriginWriteIsRefused(t *testing.T) {
	// The session cookie is SameSite=Lax, which already blocks a cross site
	// POST in current browsers. This is the second layer, and it is the one
	// that keeps working when a route is reached in a way nobody predicted.
	h, project, session := console(t)
	h.job(project.ID, "daily", "/cron/daily")

	w := h.do(request{
		method: http.MethodPost, path: "/jobs/1/delete", token: session,
		form:    url.Values{},
		headers: map[string]string{"Origin": "https://evil.com"},
	})
	if w.Code != http.StatusForbidden {
		t.Errorf("a cross origin write answered %d, want 403", w.Code)
	}
	if h.store.Job(1) == nil {
		t.Error("the job was deleted from another origin")
	}
}

func TestAReadIsNotOriginChecked(t *testing.T) {
	// A GET changes nothing, and refusing one on its Origin would break every
	// link somebody follows from a chat window.
	h, _, session := console(t)

	w := h.do(request{
		method: http.MethodGet, path: "/jobs", token: session,
		headers: map[string]string{"Origin": "https://evil.com"},
	})
	h.mustCode(w, http.StatusOK, "a read with a foreign Origin")
}

// --- pagination -------------------------------------------------------------

func TestPagingThroughALongList(t *testing.T) {
	h, project, session := console(t)
	for i := 0; i < 120; i++ {
		job := h.job(project.ID, "job-"+itoa(int64(i)), "/cron/x")
		h.store.AddRun(domain.Run{JobID: job.ID, Status: domain.StatusSuccess})
	}

	for _, path := range []string{
		"/jobs?page=1", "/jobs?page=2", "/jobs?page=99",
		"/runs?page=1", "/runs?page=2", "/runs?page=99",
		"/settings?page=2",
	} {
		w := h.get(path, session)
		if w.Code != http.StatusOK {
			t.Errorf("GET %s answered %d", path, w.Code)
		}
	}

	// A page number that is not one falls back rather than failing.
	for _, path := range []string{"/jobs?page=0", "/jobs?page=-1", "/jobs?page=abc"} {
		if w := h.get(path, session); w.Code != http.StatusOK {
			t.Errorf("GET %s answered %d", path, w.Code)
		}
	}
}

func TestTheDashboardWindowIsClamped(t *testing.T) {
	// An unbounded window is a query over the whole history on a screen that
	// refreshes every thirty seconds.
	h, _, session := console(t)

	for _, hours := range []string{"0", "-5", "100000", "abc", ""} {
		w := h.get("/?hours="+hours, session)
		if w.Code != http.StatusOK {
			t.Errorf("GET /?hours=%s answered %d", hours, w.Code)
		}
	}
}
