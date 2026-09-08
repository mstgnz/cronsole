package router

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/mstgnz/cronsole/v2/internal/authz"
	"github.com/mstgnz/cronsole/v2/internal/domain"
	"github.com/mstgnz/cronsole/v2/internal/httpx"
	"github.com/mstgnz/cronsole/v2/internal/middleware"
	"github.com/mstgnz/cronsole/v2/internal/repository/memrepo"
	"github.com/mstgnz/cronsole/v2/pkg/auth"
)

// The ordinary work: signing in, the screens, and the whole life of a job. The
// access rules have their own file; these are the paths somebody uses all day.

// console is a harness with one project and an administrator signed in, which
// is the shape most of these need.
func console(t *testing.T) (*harness, *domain.Project, string) {
	t.Helper()

	h := newHarness(t)
	project, _ := h.project("shop", "https://shop.example.com")
	_, session := h.admin("admin@example.com")
	return h, project, session
}

// --- signing in -------------------------------------------------------------

func TestSignInAndOut(t *testing.T) {
	h := newHarness(t)
	h.store.AddUser(domain.User{
		Email: "admin@example.com", Active: true, IsAdmin: true,
		Password: auth.HashAndSalt("a-good-enough-password"),
	})

	// The form is reachable without a session.
	h.mustCode(h.get("/login", ""), http.StatusOK, "the login page")

	w := h.do(request{
		method: http.MethodPost, path: "/login",
		form: url.Values{"email": {"admin@example.com"}, "password": {"a-good-enough-password"}},
	})
	if w.Code != http.StatusSeeOther {
		t.Fatalf("sign in answered %d: %s", w.Code, firstLineOf(w.Body.String()))
	}

	var session string
	for _, c := range w.Result().Cookies() {
		if c.Name == middleware.CookieName {
			session = c.Value
		}
	}
	if session == "" {
		t.Fatal("no session cookie was issued")
	}

	h.mustCode(h.get("/", session), http.StatusOK, "the dashboard with a session")

	// Signing out retires the token, so the same cookie stops working.
	out := h.post("/logout", session, nil)
	if out.Code != http.StatusSeeOther {
		t.Errorf("logout answered %d", out.Code)
	}
	if again := h.get("/", session); again.Code != http.StatusSeeOther {
		t.Errorf("the session still works after signing out (%d)", again.Code)
	}
}

func TestWrongCredentialsAreIndistinguishable(t *testing.T) {
	// A wrong address and a wrong password answer the same thing, or the form
	// becomes an account enumerator.
	h := newHarness(t)
	h.store.AddUser(domain.User{
		Email: "admin@example.com", Active: true,
		Password: auth.HashAndSalt("a-good-enough-password"),
	})

	wrongPassword := h.do(request{
		method: http.MethodPost, path: "/login",
		form: url.Values{"email": {"admin@example.com"}, "password": {"wrong"}},
	})
	unknownAddress := h.do(request{
		method: http.MethodPost, path: "/login",
		form: url.Values{"email": {"nobody@example.com"}, "password": {"wrong"}},
	})

	if wrongPassword.Code != unknownAddress.Code {
		t.Errorf("a wrong password answered %d and an unknown address %d",
			wrongPassword.Code, unknownAddress.Code)
	}
	// The same message for both. The rendered pages differ only where the form
	// echoes back the address that was typed, which the caller already knew.
	const message = "Email or password is incorrect."
	for name, w := range map[string]string{
		"a wrong password":   wrongPassword.Body.String(),
		"an unknown address": unknownAddress.Body.String(),
	} {
		if !strings.Contains(w, message) {
			t.Errorf("%s answered %q, want the shared message", name, firstLineOf(w))
		}
	}
	// And nothing in either answer hints that one address exists.
	for _, giveaway := range []string{"no such", "not found", "unknown account", "no account"} {
		if strings.Contains(strings.ToLower(unknownAddress.Body.String()), giveaway) {
			t.Errorf("the answer for an unknown address contains %q", giveaway)
		}
	}
}

func TestADeactivatedAccountCannotSignIn(t *testing.T) {
	h := newHarness(t)
	h.store.AddUser(domain.User{
		Email: "gone@example.com", Active: false,
		Password: auth.HashAndSalt("a-good-enough-password"),
	})

	w := h.do(request{
		method: http.MethodPost, path: "/login",
		form: url.Values{"email": {"gone@example.com"}, "password": {"a-good-enough-password"}},
	})
	for _, c := range w.Result().Cookies() {
		if c.Name == middleware.CookieName && c.Value != "" {
			t.Fatal("a deactivated account was given a session")
		}
	}
}

func TestTheAPIIssuesATokenForTheSameCredentials(t *testing.T) {
	h := newHarness(t)
	h.store.AddUser(domain.User{
		Email: "admin@example.com", Active: true, IsAdmin: true,
		Password: auth.HashAndSalt("a-good-enough-password"),
	})

	w := h.do(request{
		method: http.MethodPost, path: "/api/v1/login",
		body: `{"email":"admin@example.com","password":"a-good-enough-password"}`,
	})
	h.mustCode(w, http.StatusOK, "the API login")

	body := h.envelope(w)
	data, ok := body["data"].(map[string]any)
	if !ok {
		t.Fatalf("data = %#v", body["data"])
	}
	token, _ := data["token"].(string)
	if token == "" {
		t.Fatal("no token was issued")
	}
	// It sets no cookie: a caller that wanted one would be using the form.
	for _, c := range w.Result().Cookies() {
		if c.Name == middleware.CookieName {
			t.Error("the API login set a session cookie")
		}
	}

	// And the token works on the operator-scoped API.
	h.mustCode(h.adminAPI("/api/v1/admin/me", token), http.StatusOK, "/admin/me with the issued token")
}

func TestTheAPILoginRefusesAFormBody(t *testing.T) {
	// JSONOnly doubles as cross site request forgery protection: a plain HTML
	// form cannot set Content-Type to application/json.
	h := newHarness(t)

	w := h.do(request{
		method: http.MethodPost, path: "/api/v1/login",
		form: url.Values{"email": {"a@b.c"}, "password": {"x"}},
	})
	if w.Code != http.StatusUnsupportedMediaType {
		t.Errorf("a form body answered %d, want 415", w.Code)
	}
}

// --- the profile ------------------------------------------------------------

func TestChangingThePassword(t *testing.T) {
	h := newHarness(t)
	h.store.AddUser(domain.User{
		Email: "admin@example.com", Active: true, IsAdmin: true,
		Password: auth.HashAndSalt("the-old-password"),
	})
	session := h.tokenFor(1)

	h.mustCode(h.get("/profile", session), http.StatusOK, "the profile screen")

	// The current password is required, so a stolen session alone cannot lock
	// the owner out.
	wrong := h.post("/profile/password", session, url.Values{
		"current_password": {"not-the-password"}, "new_password": {"a-new-good-password"},
		"confirm_password": {"a-new-good-password"},
	})
	if !auth.ComparePassword(h.userPassword(1), "the-old-password") {
		t.Fatal("the password changed without the current one")
	}
	_ = wrong

	h.post("/profile/password", session, url.Values{
		"current_password": {"the-old-password"}, "new_password": {"a-new-good-password"},
		"confirm_password": {"a-new-good-password"},
	})
	if !auth.ComparePassword(h.userPassword(1), "a-new-good-password") {
		t.Error("the password did not change")
	}

	// Changing it retires every token issued before now, which is what makes
	// the change end other sessions.
	if w := h.get("/", session); w.Code != http.StatusSeeOther {
		t.Errorf("the old session still works after a password change (%d)", w.Code)
	}
}

// --- the screens ------------------------------------------------------------

func TestEveryScreenRenders(t *testing.T) {
	// A template that fails renders a 500, and the one screen nobody opened
	// during a change is the one that breaks. This opens all of them.
	h, project, session := console(t)
	job := h.job(project.ID, "daily", "/cron/daily")
	h.store.AddSchedule(job.ID, "0 3 * * *")
	status := 200
	duration := 15
	h.store.AddRun(domain.Run{
		JobID: job.ID, Status: domain.StatusSuccess,
		HTTPStatus: &status, DurationMs: &duration,
	})
	h.store.AddLog(domain.AppLog{Level: "error", Message: "something failed"})

	for _, path := range []string{
		"/", "/?hours=6", "/?hours=168",
		"/jobs", "/jobs?q=daily", "/jobs?active=true", "/jobs?project=1",
		"/jobs/new", "/jobs/1",
		"/runs", "/runs?status=success", "/runs?job=1", "/runs?project=1", "/runs/1",
		"/projects", "/projects/1/members",
		"/settings", "/settings?level=error",
		"/profile", "/docs", "/openapi.yaml",
	} {
		w := h.get(path, session)
		if w.Code != http.StatusOK {
			t.Errorf("GET %s answered %d: %s", path, w.Code, firstLineOf(w.Body.String()))
		}
	}
}

func TestTheDashboardAnswersAFragmentToHTMX(t *testing.T) {
	// It refreshes itself on a timer, swapping the body rather than reloading
	// the whole document.
	h, _, session := console(t)

	w := h.do(request{
		method: http.MethodGet, path: "/", token: session,
		headers: map[string]string{"HX-Request": "true"},
	})
	h.mustCode(w, http.StatusOK, "the dashboard fragment")

	// A fragment, not a page: no layout, and therefore no navigation.
	if bodyContains(w, "<!doctype html>") || bodyContains(w, "<html") {
		t.Error("the refresh returned a whole document rather than the body")
	}
	if !bodyContains(w, "Dispatcher") {
		t.Error("the fragment is missing the dashboard body")
	}
}

func TestSwitchingLanguage(t *testing.T) {
	h, _, session := console(t)

	w := h.do(request{
		method: http.MethodPost, path: "/lang", token: session,
		form:    url.Values{"lang": {"tr"}, "next": {"/jobs"}},
		headers: map[string]string{"Origin": "http://example.com"},
	})
	if w.Code != http.StatusSeeOther {
		t.Fatalf("the language switch answered %d", w.Code)
	}

	var lang string
	for _, c := range w.Result().Cookies() {
		if c.Name == httpx.LangCookie {
			lang = c.Value
		}
	}
	if lang != "tr" {
		t.Errorf("the language cookie is %q, want tr", lang)
	}

	// And the screens come back translated.
	page := h.do(request{method: http.MethodGet, path: "/jobs", token: session, lang: "tr"})
	h.mustCode(page, http.StatusOK, "the Turkish job list")
	if !bodyContains(page, "İşler") {
		t.Errorf("the Turkish screen is in English: %s", firstLineOf(page.Body.String()))
	}

	// The API stays English whatever the interface is set to: an error that
	// changes wording with a header is one nobody can grep for.
	api := h.do(request{
		method: http.MethodGet, path: "/api/v1/jobs", lang: "tr",
		headers: map[string]string{"Accept-Language": "tr"},
	})
	if !strings.Contains(api.Body.String(), "API key") {
		t.Errorf("the API answered in something other than English: %s",
			firstLineOf(api.Body.String()))
	}
}

func TestTheLanguageSwitchCannotSendSomebodyOffSite(t *testing.T) {
	h, _, session := console(t)

	w := h.do(request{
		method: http.MethodPost, path: "/lang", token: session,
		form:    url.Values{"lang": {"tr"}, "next": {"//evil.com/"}},
		headers: map[string]string{"Origin": "http://example.com"},
	})
	if location := w.Header().Get("Location"); strings.Contains(location, "evil.com") {
		t.Errorf("Location = %q", location)
	}
}

func TestAnUnknownLanguageIsIgnored(t *testing.T) {
	h, _, session := console(t)

	h.do(request{
		method: http.MethodPost, path: "/lang", token: session,
		form:    url.Values{"lang": {"../../etc/passwd"}, "next": {"/"}},
		headers: map[string]string{"Origin": "http://example.com"},
	})
	// The screens still render, which is the thing that would break if an
	// unknown language reached the template set lookup.
	h.mustCode(h.get("/jobs", session), http.StatusOK, "the job list after a bad language")
}

func TestTheAPIReferenceIsServedWithItsSpec(t *testing.T) {
	h, _, session := console(t)

	page := h.get("/docs", session)
	h.mustCode(page, http.StatusOK, "the API reference")
	if !bodyContains(page, "/openapi.yaml") {
		t.Error("the reference does not load the specification")
	}

	spec := h.get("/openapi.yaml", session)
	h.mustCode(spec, http.StatusOK, "the specification")
	if !bodyContains(spec, "openapi:") {
		t.Errorf("the specification does not look like one: %s", firstLineOf(spec.Body.String()))
	}
	// Served with an ETag so a reload is cheap, and no-cache so an edit is not
	// held by a browser for five minutes.
	if spec.Header().Get("ETag") == "" {
		t.Error("the specification has no ETag")
	}
	if cache := spec.Header().Get("Cache-Control"); !strings.Contains(cache, "no-cache") {
		t.Errorf("Cache-Control = %q, want no-cache", cache)
	}
}

// --- the life of a job ------------------------------------------------------

func TestAJobFromCreationToDeletion(t *testing.T) {
	h, project, session := console(t)

	// Create.
	create := h.post("/jobs", session, url.Values{
		"project_id": {"1"}, "code": {"daily-report"}, "name": {"Daily report"},
		"url": {"/cron/daily-report"}, "method": {"GET"}, "timeout_sec": {"120"},
		"success_min": {"200"}, "success_max": {"299"}, "active": {"true"},
		"schedules": {"0 3 * * *"},
	})
	if create.Code != http.StatusSeeOther && create.Code != http.StatusOK {
		t.Fatalf("create answered %d: %s", create.Code, firstLineOf(create.Body.String()))
	}

	job := h.store.Job(1)
	if job == nil || job.Code != "daily-report" {
		t.Fatalf("the job was not stored: %+v", job)
	}
	if job.TimeoutSec != 120 {
		t.Errorf("timeout = %d, want 120", job.TimeoutSec)
	}
	if job.ProjectID != project.ID {
		t.Errorf("project = %d, want %d", job.ProjectID, project.ID)
	}

	// Edit.
	h.post("/jobs/1", session, url.Values{
		"project_id": {"1"}, "code": {"daily-report"}, "name": {"Daily report, renamed"},
		"url": {"/cron/daily-report"}, "method": {"POST"}, "timeout_sec": {"60"},
		"success_min": {"200"}, "success_max": {"299"}, "active": {"true"},
	})
	if job = h.store.Job(1); job.Name != "Daily report, renamed" || job.Method != "POST" {
		t.Errorf("the edit did not land: %+v", job)
	}

	// Switch off, and on again. The target state is submitted rather than
	// flipped, so two clicks on a stale page cannot leave it in the state
	// neither of them intended.
	h.post("/jobs/1/toggle", session, url.Values{"active": {"false"}})
	if h.store.Job(1).Active {
		t.Error("the job is still active after being switched off")
	}
	h.post("/jobs/1/toggle", session, url.Values{"active": {"true"}})
	if !h.store.Job(1).Active {
		t.Error("the job did not come back on")
	}

	// Run now. Nothing executes here; the run row and the hand-off are what
	// this asserts.
	before := h.store.RunCount()
	h.post("/jobs/1/run", session, nil)
	if h.store.RunCount() != before+1 {
		t.Errorf("%d runs after a manual trigger, want %d", h.store.RunCount(), before+1)
	}
	if len(h.dispatched) == 0 {
		t.Error("the run was not handed to the runner")
	}

	// Clone.
	clone := h.post("/jobs/1/clone", session, url.Values{"code": {"daily-report-copy"}})
	cloned := h.store.Job(2)
	if cloned == nil {
		t.Fatalf("the clone was not created: %d %s", clone.Code, firstLineOf(clone.Body.String()))
	}
	if cloned.Code == "daily-report" {
		t.Error("the clone kept the original code, which the unique index forbids")
	}
	// A clone starts switched off: it is a draft, and a copy that fires the
	// moment it is made is a copy nobody reviewed.
	if cloned.Active {
		t.Error("the clone was created already switched on")
	}

	// Delete.
	h.post("/jobs/1/delete", session, nil)
	if h.store.Job(1) != nil {
		t.Error("the job is still there after being deleted")
	}
}

func TestAJobCodeIsUniqueWithinItsProject(t *testing.T) {
	h, _, session := console(t)
	h.job(1, "daily", "/cron/daily")

	w := h.post("/jobs", session, url.Values{
		"project_id": {"1"}, "code": {"daily"}, "name": {"Another"},
		"url": {"/cron/other"}, "method": {"GET"}, "timeout_sec": {"30"},
		"success_min": {"200"}, "success_max": {"299"},
	})
	if w.Code == http.StatusSeeOther {
		t.Error("a duplicate code was accepted")
	}
	if h.store.Job(2) != nil {
		t.Error("the duplicate was stored")
	}
}

func TestSchedulesAreAddedAndRemoved(t *testing.T) {
	// A real job frequently needs several expressions that cannot be folded
	// into one, which is why they are rows rather than a column.
	h, _, session := console(t)
	h.job(1, "daily", "/cron/daily")

	h.post("/jobs/1/schedules", session, url.Values{"expression": {"0 3 * * *"}})
	h.post("/jobs/1/schedules", session, url.Values{"expression": {"58,59 15 * * *"}})

	page := h.get("/jobs/1", session)
	for _, expression := range []string{"0 3 * * *", "58,59 15 * * *"} {
		if !bodyContains(page, expression) {
			t.Errorf("the schedule %q is not on the screen", expression)
		}
	}

	// A nonsense expression is refused rather than stored, because it would
	// simply never fire.
	h.post("/jobs/1/schedules", session, url.Values{"expression": {"not a cron expression"}})
	if bodyContains(h.get("/jobs/1", session), "not a cron expression") {
		t.Error("an invalid expression was stored")
	}
}

func TestThePreviewReadsAnExpressionBack(t *testing.T) {
	// The preview is the only thing that catches "0 16 * * 7" meant as Sunday
	// before it runs on the wrong day for a week.
	h, _, session := console(t)

	w := h.do(request{
		method: http.MethodGet, path: "/jobs/schedule-preview?expression=0+3+*+*+*",
		token: session,
	})
	h.mustCode(w, http.StatusOK, "the schedule preview")
	if strings.TrimSpace(w.Body.String()) == "" {
		t.Error("the preview is empty")
	}

	// An invalid expression previews as invalid rather than as an error: the
	// field is being typed into, and a 500 per keystroke is not feedback.
	bad := h.do(request{
		method: http.MethodGet, path: "/jobs/schedule-preview?expression=nonsense",
		token: session,
	})
	h.mustCode(bad, http.StatusOK, "the preview of an invalid expression")
	if !bodyContains(bad, "5 fields") {
		t.Errorf("the preview does not say what is wrong: %s", firstLineOf(bad.Body.String()))
	}
}

func TestASchedduleIsRemovedThroughItsOwnJob(t *testing.T) {
	// The id in the URL is not enough on its own: the route also carries the
	// job, and the repository refuses the pair when they do not belong
	// together. Without that, an id is all somebody needs to delete another
	// project's schedule.
	h, _, session := console(t)
	h.job(1, "daily", "/cron/daily")
	h.job(1, "other", "/cron/other")
	schedule := h.store.AddSchedule(1, "0 3 * * *")

	// Asserted on the stored rows rather than on the page: the form carries
	// preset chips for the common expressions, so "0 3 * * *" appears on it
	// whether or not the job has that schedule.
	stored := func() int {
		h.t.Helper()
		rows, err := memrepo.Jobs{Store: h.store}.ListSchedules(t.Context(), 1)
		if err != nil {
			t.Fatalf("ListSchedules = %v", err)
		}
		return len(rows)
	}

	// Through the wrong job: refused, and the row survives.
	h.post("/jobs/2/schedules/"+itoa(schedule.ID)+"/delete", session, nil)
	if stored() != 1 {
		t.Fatal("a schedule was deleted through another job's id")
	}

	// Through its own: removed.
	h.post("/jobs/1/schedules/"+itoa(schedule.ID)+"/delete", session, nil)
	if stored() != 0 {
		t.Error("the schedule is still there")
	}
}

func TestChainsAreAddedAndRemoved(t *testing.T) {
	h, _, session := console(t)
	h.job(1, "first", "/cron/first")
	h.job(1, "second", "/cron/second")

	h.post("/jobs/1/links", session, url.Values{
		"target_job_id": {"2"}, "condition": {"success"}, "delay_sec": {"30"},
	})

	// The chain picker lists every job, so the target's name is on the page
	// either way. The edges themselves are what this asserts.
	edges := func() int {
		h.t.Helper()
		rows, err := memrepo.Jobs{Store: h.store}.ListLinks(t.Context(), 1)
		if err != nil {
			t.Fatalf("ListLinks = %v", err)
		}
		return len(rows)
	}

	if edges() != 1 {
		t.Fatalf("%d chain edges after adding one", edges())
	}
	if !bodyContains(h.get("/jobs/1", session), "second") {
		t.Error("the chain target is not on the screen")
	}

	h.post("/jobs/1/links/1/delete", session, nil)
	if edges() != 0 {
		t.Error("the chain edge is still there after being removed")
	}
}

func TestAMemberIsRemovedFromAProject(t *testing.T) {
	h, _, session := console(t)
	colleague, _ := h.operatorWithRole("colleague@example.com", authz.RoleProjectReader, 1)

	members, err := h.grants.ListProjectMembers(t.Context(), 1)
	if err != nil || len(members) != 1 {
		t.Fatalf("the fixture is not what the test assumes: %v %+v", err, members)
	}

	h.post("/projects/1/members/"+itoa(colleague.ID)+"/"+itoa(members[0].RoleID)+"/delete",
		session, nil)

	after, _ := h.grants.ListProjectMembers(t.Context(), 1)
	if len(after) != 0 {
		t.Errorf("the member is still there: %+v", after)
	}
	// And the account itself survives: removing access is not deleting a person.
	if h.store.User(colleague.ID) == nil {
		t.Error("removing access deleted the account")
	}
}

func TestAnAccountAndANotificationListAreDeleted(t *testing.T) {
	h, _, session := console(t)

	h.post("/settings/users", session, url.Values{
		"fullname": {"Temporary"}, "email": {"temp@example.com"},
		"password": {"a-good-enough-password"}, "active": {"true"},
	})
	created := h.store.UserByEmail("temp@example.com")
	if created == nil {
		t.Fatal("the account was not created")
	}

	h.post("/settings/users/"+itoa(created.ID)+"/delete", session, nil)
	if h.store.User(created.ID) != nil {
		t.Error("the account is still there")
	}

	h.post("/settings/notifications", session, url.Values{
		"name": {"Temporary list"}, "emails": {"ops@example.com"},
		"on_failure": {"true"}, "active": {"true"},
	})
	if !bodyContains(h.get("/settings", session), "Temporary list") {
		t.Fatal("the recipient list was not created")
	}

	h.post("/settings/notifications/1/delete", session, nil)
	if bodyContains(h.get("/settings", session), "Temporary list") {
		t.Error("the recipient list is still there")
	}
}

func TestAnAdministratorCannotDeleteTheirOwnAccount(t *testing.T) {
	// Locking yourself out of the console you administer is not a mistake
	// anything else can undo.
	h, _, session := console(t)
	me := h.store.UserByEmail("admin@example.com")
	if me == nil {
		t.Fatal("the fixture is not what the test assumes")
	}

	h.post("/settings/users/"+itoa(me.ID)+"/delete", session, nil)
	if h.store.User(me.ID) == nil {
		t.Error("an administrator deleted their own account")
	}
}

// itoa renders an id for a URL.
func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func TestAChainCannotBeMadeIntoACycle(t *testing.T) {
	// Two jobs each triggering the other would run forever. Refused when the
	// edge is created, because there is no good place to break it later.
	h, _, session := console(t)
	h.job(1, "first", "/cron/first")
	h.job(1, "second", "/cron/second")

	h.post("/jobs/1/links", session, url.Values{
		"target_job_id": {"2"}, "condition": {"success"},
	})
	w := h.post("/jobs/2/links", session, url.Values{
		"target_job_id": {"1"}, "condition": {"success"},
	})
	if w.Code == http.StatusSeeOther {
		t.Error("the cycle was accepted")
	}

	// And a job cannot chain to itself, which is the same fault with one edge.
	self := h.post("/jobs/1/links", session, url.Values{
		"target_job_id": {"1"}, "condition": {"success"},
	})
	if self.Code == http.StatusSeeOther {
		t.Error("a job was chained to itself")
	}
}

func TestAJobsTargetMustStayOnItsProjectsHost(t *testing.T) {
	// With a base address set, the host comes from the project. That is the
	// containment mechanism: an account that can edit jobs cannot move one to
	// a different host, because the host is not a field it can write.
	h, _, session := console(t)

	w := h.post("/jobs", session, url.Values{
		"project_id": {"1"}, "code": {"exfiltrate"}, "name": {"Exfiltrate"},
		"url": {"https://evil.com/collect"}, "method": {"GET"}, "timeout_sec": {"30"},
		"success_min": {"200"}, "success_max": {"299"},
	})
	if w.Code == http.StatusSeeOther {
		t.Error("a job pointing at another host was accepted")
	}
	if h.store.Job(1) != nil {
		t.Errorf("the job was stored: %+v", h.store.Job(1))
	}
}

// --- projects ---------------------------------------------------------------

func TestAProjectFromCreationToDeletion(t *testing.T) {
	h := newHarness(t)
	_, session := h.admin("admin@example.com")

	create := h.post("/projects", session, url.Values{
		"name": {"Shop"}, "slug": {"shop"}, "base_url": {"https://shop.example.com"},
		"active": {"true"},
	})
	if create.Code != http.StatusSeeOther && create.Code != http.StatusOK {
		t.Fatalf("create answered %d: %s", create.Code, firstLineOf(create.Body.String()))
	}

	project := h.store.Project(1)
	if project == nil || project.Slug != "shop" {
		t.Fatalf("the project was not stored: %+v", project)
	}
	// A key is issued on creation, so a deploy pipeline has something to use.
	if project.KeyPrefix == "" || project.KeyHash == "" {
		t.Error("no API key was issued")
	}

	firstKey := project.KeyHash
	h.post("/projects/1/rotate-key", session, nil)
	if h.store.Project(1).KeyHash == firstKey {
		t.Error("rotating the key did not change it")
	}

	h.post("/projects/1/delete", session, nil)
	if h.store.Project(1) != nil {
		t.Error("the project is still there after being deleted")
	}
}

func TestAProjectSlugIsUnique(t *testing.T) {
	h := newHarness(t)
	_, session := h.admin("admin@example.com")
	h.project("shop", "https://shop.example.com")

	w := h.post("/projects", session, url.Values{
		"name": {"Shop again"}, "slug": {"shop"}, "active": {"true"},
	})
	if w.Code == http.StatusSeeOther {
		t.Error("a duplicate slug was accepted")
	}
}

// --- settings ---------------------------------------------------------------

func TestAccountsAreManagedFromSettings(t *testing.T) {
	h, _, session := console(t)

	h.post("/settings/users", session, url.Values{
		"fullname": {"New operator"}, "email": {"new@example.com"},
		"password": {"a-good-enough-password"}, "active": {"true"},
	})

	created := h.store.UserByEmail("new@example.com")
	if created == nil {
		t.Fatal("the account was not created")
	}
	// The password is hashed, never stored as given.
	if created.Password == "a-good-enough-password" {
		t.Fatal("the password was stored in clear text")
	}
	if !auth.ComparePassword(created.Password, "a-good-enough-password") {
		t.Error("the stored hash does not verify against the password that was set")
	}
	// And an ordinary account, not an administrator, unless asked for.
	if created.IsAdmin {
		t.Error("a new account was created as an administrator")
	}

	page := h.get("/settings", session)
	if !bodyContains(page, "new@example.com") {
		t.Error("the new account is not on the settings screen")
	}
}

func TestAnAccountAddressIsUnique(t *testing.T) {
	h, _, session := console(t)

	h.post("/settings/users", session, url.Values{
		"fullname": {"Second"}, "email": {"admin@example.com"},
		"password": {"a-good-enough-password"}, "active": {"true"},
	})
	// Only the original remains: the unique index is on lower(email).
	if h.store.User(2) != nil {
		t.Error("a second account was created with an address already in use")
	}
}

func TestNotificationListsAreManaged(t *testing.T) {
	h, _, session := console(t)

	h.post("/settings/notifications", session, url.Values{
		"name": {"On call"}, "emails": {"ops@example.com\noncall@example.com"},
		"on_failure": {"true"}, "active": {"true"},
	})

	page := h.get("/settings", session)
	if !bodyContains(page, "On call") {
		t.Error("the recipient list is not on the settings screen")
	}
}

func TestHostRoutesAreManagedFromSettings(t *testing.T) {
	// The Network tab: where a host name is dialled, without changing the jobs
	// that name it.
	h, _, session := console(t)

	h.post("/settings/hosts", session, url.Values{
		"hostname": {"shop.example.com"}, "address": {"10.10.0.5"},
		"note": {"through the private network"}, "active": {"true"},
	})

	routes := h.store.HostOverrides()
	if len(routes) != 1 {
		t.Fatalf("%d routes were stored, want 1", len(routes))
	}
	if routes[0].Hostname != "shop.example.com" || routes[0].Address != "10.10.0.5" {
		t.Errorf("the route was stored as %+v", routes[0])
	}

	page := h.get("/settings", session)
	if !bodyContains(page, "10.10.0.5") {
		t.Error("the route is not on the settings screen")
	}

	// Edit, then remove.
	h.post("/settings/hosts/1", session, url.Values{
		"hostname": {"shop.example.com"}, "address": {"10.10.0.6"}, "active": {"true"},
	})
	if routes = h.store.HostOverrides(); routes[0].Address != "10.10.0.6" {
		t.Errorf("the edit did not land: %+v", routes[0])
	}

	h.post("/settings/hosts/1/delete", session, nil)
	if len(h.store.HostOverrides()) != 0 {
		t.Error("the route is still there after being removed")
	}
}

func TestAHostRouteToANameIsRefused(t *testing.T) {
	// A name would mean a second DNS lookup, which is the round trip the route
	// exists to avoid.
	h, _, session := console(t)

	h.post("/settings/hosts", session, url.Values{
		"hostname": {"shop.example.com"}, "address": {"internal.example.com"}, "active": {"true"},
	})
	if len(h.store.HostOverrides()) != 0 {
		t.Error("a route to a host name was stored")
	}
}

func TestRunVisibilityIsSetPerRole(t *testing.T) {
	h, _, session := console(t)

	h.post("/settings/roles/run-visibility", session, url.Values{
		"role": {authz.RoleProjectReader}, "error": {"true"},
	})

	fields, restricted, err := h.grants.RoleFields(t.Context(),
		h.grants.RoleID(authz.RoleProjectReader), authz.RunsRead)
	if err != nil {
		t.Fatalf("RoleFields = %v", err)
	}
	if !restricted {
		t.Fatal("the role was not restricted")
	}
	if len(fields) != 1 || fields[0] != authz.RunFieldError {
		t.Errorf("fields = %v, want just the error text", fields)
	}
}

// --- the API ----------------------------------------------------------------

func TestTheProjectAPIListsJobsAndRuns(t *testing.T) {
	h := newHarness(t)
	project, key := h.project("shop", "https://shop.example.com")
	job := h.job(project.ID, "daily", "/cron/daily")
	h.store.AddRun(domain.Run{JobID: job.ID, Status: domain.StatusSuccess})

	jobs := h.apiGet("/api/v1/jobs", key)
	h.mustCode(jobs, http.StatusOK, "the API job list")
	if !strings.Contains(jobs.Body.String(), "daily") {
		t.Error("the job is not in the answer")
	}

	runs := h.apiGet("/api/v1/runs", key)
	h.mustCode(runs, http.StatusOK, "the API run list")
	if !strings.Contains(runs.Body.String(), "success") {
		t.Error("the run is not in the answer")
	}
}

func TestTheProjectAPITriggersAJobByCode(t *testing.T) {
	// A code rather than an id, because a deploy pipeline knows the code it
	// registered and has no reason to learn an id.
	h := newHarness(t)
	project, key := h.project("shop", "https://shop.example.com")
	h.job(project.ID, "daily", "/cron/daily")

	w := h.apiPost("/api/v1/jobs/daily/run", key, "")
	// 202, not 200: the run is queued and handed to a worker, and has not
	// happened yet. Answering 200 would tell a pipeline the job finished.
	h.mustCode(w, http.StatusAccepted, "the API trigger")
	if h.store.RunCount() != 1 {
		t.Errorf("%d runs were queued, want 1", h.store.RunCount())
	}

	// A code on another project is not found, whatever the key.
	other, _ := h.project("alpha", "https://alpha.example.com")
	h.job(other.ID, "alpha-only", "/cron/alpha")
	if got := h.apiPost("/api/v1/jobs/alpha-only/run", key, ""); got.Code != http.StatusNotFound {
		t.Errorf("another project's job answered %d, want 404", got.Code)
	}
}

func TestTheOperatorAPIServesTheSameDataAsTheScreens(t *testing.T) {
	h, project, session := console(t)
	h.job(project.ID, "daily", "/cron/daily")

	for _, path := range []string{
		"/api/v1/admin/me", "/api/v1/admin/summary", "/api/v1/admin/projects",
		"/api/v1/admin/jobs", "/api/v1/admin/jobs/1", "/api/v1/admin/runs",
		"/api/v1/admin/schedule-preview?expression=0+3+*+*+*",
	} {
		w := h.adminAPI(path, session)
		if w.Code != http.StatusOK {
			t.Errorf("GET %s answered %d: %s", path, w.Code, firstLineOf(w.Body.String()))
		}
	}
}

func TestTheOperatorAPICreatesUpdatesAndDeletesAJob(t *testing.T) {
	h, _, session := console(t)

	create := h.do(request{
		method: http.MethodPost, path: "/api/v1/admin/jobs", bearer: session,
		body: `{"project_id":1,"code":"daily","name":"Daily","url":"/cron/daily",
			"method":"GET","timeout_sec":30,"success_min":200,"success_max":299,"active":true}`,
	})
	if create.Code != http.StatusOK && create.Code != http.StatusCreated {
		t.Fatalf("create answered %d: %s", create.Code, firstLineOf(create.Body.String()))
	}
	if job := h.store.Job(1); job == nil || job.Code != "daily" {
		t.Fatalf("the job was not stored: %+v", job)
	}

	update := h.do(request{
		method: http.MethodPut, path: "/api/v1/admin/jobs/1", bearer: session,
		body: `{"project_id":1,"code":"daily","name":"Renamed","url":"/cron/daily",
			"method":"GET","timeout_sec":30,"success_min":200,"success_max":299,"active":true}`,
	})
	h.mustCode(update, http.StatusOK, "the API update")
	if job := h.store.Job(1); job.Name != "Renamed" {
		t.Errorf("the update did not land: %+v", job)
	}

	trigger := h.do(request{
		method: http.MethodPost, path: "/api/v1/admin/jobs/1/run", bearer: session,
	})
	h.mustCode(trigger, http.StatusAccepted, "the API trigger")

	del := h.do(request{
		method: http.MethodDelete, path: "/api/v1/admin/jobs/1", bearer: session,
	})
	h.mustCode(del, http.StatusOK, "the API delete")
	if h.store.Job(1) != nil {
		t.Error("the job is still there after being deleted")
	}
}

func TestSyncReportsAPartialFailure(t *testing.T) {
	// A pipeline must not read "eleven of twelve registered" as clean.
	h := newHarness(t)
	_, key := h.project("shop", "https://shop.example.com")

	w := h.apiPost("/api/v1/sync", key, `{
		"jobs": [
			{"code": "good", "name": "Good", "url": "/cron/good"},
			{"code": "BAD CODE", "name": "Bad", "url": "/cron/bad"}
		]
	}`)
	if w.Code != http.StatusMultiStatus {
		t.Errorf("a partial failure answered %d, want 207", w.Code)
	}
	if !strings.Contains(w.Body.String(), "BAD CODE") && !strings.Contains(w.Body.String(), "bad code") {
		t.Errorf("the answer does not say which job was refused: %s", firstLineOf(w.Body.String()))
	}
}

func TestSyncPruneDeactivatesRatherThanDeletes(t *testing.T) {
	// Never deletes: a job dropped from a payload by mistake would take its
	// whole history with it.
	h := newHarness(t)
	project, key := h.project("shop", "https://shop.example.com")
	h.job(project.ID, "retired", "/cron/retired")

	w := h.apiPost("/api/v1/sync", key,
		`{"prune":true,"jobs":[{"code":"kept","name":"Kept","url":"/cron/kept"}]}`)
	h.mustCode(w, http.StatusOK, "sync with prune")

	retired := h.store.Job(1)
	if retired == nil {
		t.Fatal("prune deleted the job")
	}
	if retired.Active {
		t.Error("prune did not deactivate the job")
	}
}

// userPassword is the stored hash, for the profile test.
func (h *harness) userPassword(id int64) string {
	h.t.Helper()

	user := h.store.User(id)
	if user == nil {
		h.t.Fatalf("no account %d", id)
	}
	return user.Password
}
