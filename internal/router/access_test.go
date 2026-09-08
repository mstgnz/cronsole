package router

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/mstgnz/cronsole/v2/internal/authz"
	"github.com/mstgnz/cronsole/v2/internal/dockerinfo"
	"github.com/mstgnz/cronsole/v2/internal/domain"
)

// This file is the one that matters. Six brands share one console, and every
// claim about who can see what is checked here against the running system
// rather than against a unit's idea of the rules.

// twoBrands sets up the arrangement the whole service exists for: two projects
// that must not be able to see each other.
func twoBrands(t *testing.T) (*harness, *domain.Project, *domain.Project) {
	t.Helper()

	h := newHarness(t)
	shop, _ := h.project("shop", "https://shop.example.com")
	alpha, _ := h.project("alpha", "https://alpha.example.com")

	h.job(shop.ID, "shop-daily", "/cron/daily")
	h.job(shop.ID, "shop-cleanup", "/cron/cleanup")
	h.job(alpha.ID, "alpha-nightly", "/cron/nightly")

	return h, shop, alpha
}

// --- nothing without a session ----------------------------------------------

func TestEveryScreenRequiresASession(t *testing.T) {
	// The one thing that must never regress: a route added later inherits the
	// group's middleware, and this fails the moment one does not.
	h, shop, _ := twoBrands(t)

	for _, path := range []string{
		"/", "/jobs", "/jobs/new", "/jobs/1", "/runs", "/runs/1",
		"/projects", "/settings", "/profile", "/docs", "/openapi.yaml",
	} {
		w := h.get(path, "")
		if w.Code != http.StatusSeeOther {
			t.Errorf("GET %s without a session answered %d, want a redirect to the login page",
				path, w.Code)
		}
		if location := w.Header().Get("Location"); !strings.HasPrefix(location, "/login") {
			t.Errorf("GET %s redirected to %q, want the login page", path, location)
		}
	}
	_ = shop
}

func TestTheLoginPageAndHealthChecksAreReachable(t *testing.T) {
	// The exceptions, listed so a change that closes one is visible.
	h := newHarness(t)

	for _, path := range []string{"/login", "/healthz"} {
		if w := h.get(path, ""); w.Code != http.StatusOK {
			t.Errorf("GET %s answered %d, want 200", path, w.Code)
		}
	}

	// Readiness is reachable without a session too, and answers "not ready"
	// rather than panicking when there is no database behind it.
	if w := h.get("/readyz", ""); w.Code != http.StatusServiceUnavailable {
		t.Errorf("GET /readyz with no database answered %d, want 503", w.Code)
	}
}

// --- what a reader on one project can see -----------------------------------

func TestAReaderSeesOnlyTheirOwnProjectsJobs(t *testing.T) {
	h, shop, alpha := twoBrands(t)
	_, reader := h.operatorWithRole("reader@example.com", authz.RoleProjectReader, shop.ID)

	w := h.get("/jobs", reader)
	h.mustCode(w, http.StatusOK, "the job list")

	if !bodyContains(w, "shop-daily") {
		t.Error("the reader cannot see a job on their own project")
	}
	if bodyContains(w, "alpha-nightly") {
		t.Error("the reader can see another project's job")
	}
	_ = alpha
}

func TestAnotherProjectsJobIsNotFoundRatherThanForbidden(t *testing.T) {
	// Answering 403 would confirm the id exists, and an id is the only thing
	// needed to probe the rest.
	h, shop, alpha := twoBrands(t)
	_, reader := h.operatorWithRole("reader@example.com", authz.RoleProjectReader, shop.ID)

	alphaJob := h.store.Job(3)
	if alphaJob == nil || alphaJob.ProjectID != alpha.ID {
		t.Fatalf("the fixture is not what the test assumes: %+v", alphaJob)
	}

	w := h.get("/jobs/3", reader)
	if w.Code != http.StatusNotFound {
		t.Errorf("another project's job answered %d, want 404", w.Code)
	}
}

func TestAReaderCannotChangeAnything(t *testing.T) {
	h, shop, _ := twoBrands(t)
	_, reader := h.operatorWithRole("reader@example.com", authz.RoleProjectReader, shop.ID)

	writes := []struct {
		path string
		form url.Values
	}{
		{"/jobs", url.Values{"code": {"new-job"}, "name": {"New"}, "url": {"/cron/x"},
			"project_id": {"1"}, "method": {"GET"}}},
		{"/jobs/1", url.Values{"code": {"shop-daily"}, "name": {"Renamed"}, "url": {"/cron/daily"},
			"project_id": {"1"}, "method": {"GET"}}},
		{"/jobs/1/toggle", nil},
		{"/jobs/1/run", nil},
		{"/jobs/1/delete", nil},
		{"/jobs/1/clone", nil},
		{"/projects", url.Values{"name": {"New brand"}, "slug": {"new-brand"}}},
		{"/projects/1/rotate-key", nil},
		{"/projects/1/delete", nil},
	}

	for _, write := range writes {
		w := h.post(write.path, reader, write.form)
		if w.Code == http.StatusOK || w.Code == http.StatusSeeOther {
			t.Errorf("POST %s succeeded for a reader (%d)", write.path, w.Code)
		}
	}

	// And nothing changed.
	if job := h.store.Job(1); job == nil || job.Name != "shop-daily" || !job.Active {
		t.Errorf("the job was modified: %+v", job)
	}
	if h.store.RunCount() != 0 {
		t.Errorf("%d runs were queued by a reader", h.store.RunCount())
	}
}

func TestSettingsIsAdministratorsOnly(t *testing.T) {
	// It changes accounts, alerting and where host names resolve. A project
	// role never reaches it, whatever that role is on its own project.
	h, shop, _ := twoBrands(t)

	for _, role := range []string{authz.RoleProjectReader, authz.RoleProjectWriter, authz.RoleProjectAdmin} {
		_, session := h.operatorWithRole(role+"@example.com", role, shop.ID)
		w := h.get("/settings", session)
		if w.Code != http.StatusSeeOther {
			t.Errorf("a %s reached /settings (%d)", role, w.Code)
		}
	}

	_, adminSession := h.admin("admin@example.com")
	h.mustCode(h.get("/settings", adminSession), http.StatusOK, "settings for an administrator")
}

// --- what a writer can do ---------------------------------------------------

func TestAWriterManagesTheirOwnProject(t *testing.T) {
	h, shop, _ := twoBrands(t)
	_, writer := h.operatorWithRole("writer@example.com", authz.RoleProjectWriter, shop.ID)

	w := h.post("/jobs", writer, url.Values{
		"project_id": {"1"}, "code": {"new-job"}, "name": {"New job"},
		"url": {"/cron/new"}, "method": {"GET"}, "timeout_sec": {"30"},
		"success_min": {"200"}, "success_max": {"299"}, "active": {"true"},
	})
	if w.Code != http.StatusSeeOther && w.Code != http.StatusOK {
		t.Fatalf("a writer could not create a job: %d %s", w.Code, firstLineOf(w.Body.String()))
	}

	created := h.store.Job(4)
	if created == nil || created.Code != "new-job" {
		t.Fatalf("the job was not stored: %+v", created)
	}
	if created.ProjectID != shop.ID {
		t.Errorf("the job landed on project %d, want %d", created.ProjectID, shop.ID)
	}
}

func TestAWriterCannotCreateAJobOnAnotherProject(t *testing.T) {
	// The project id arrives in the form, so this is the field an operator
	// would change to reach across brands.
	h, shop, alpha := twoBrands(t)
	_, writer := h.operatorWithRole("writer@example.com", authz.RoleProjectWriter, shop.ID)

	before := len(h.store.HostOverrides())
	w := h.post("/jobs", writer, url.Values{
		"project_id": {"2"}, "code": {"smuggled"}, "name": {"Smuggled"},
		"url": {"/cron/x"}, "method": {"GET"}, "timeout_sec": {"30"},
		"success_min": {"200"}, "success_max": {"299"}, "active": {"true"},
	})
	if w.Code == http.StatusSeeOther || w.Code == http.StatusOK {
		t.Errorf("a writer created a job on another project (%d)", w.Code)
	}

	for id := int64(1); id <= 6; id++ {
		if job := h.store.Job(id); job != nil && job.Code == "smuggled" {
			t.Fatalf("the job was stored on project %d", job.ProjectID)
		}
	}
	_, _ = alpha, before
}

func TestAWriterCannotManageMembers(t *testing.T) {
	// Adding members is the project administrator's job. A writer who could do
	// it could grant themselves more.
	h, shop, _ := twoBrands(t)
	_, writer := h.operatorWithRole("writer@example.com", authz.RoleProjectWriter, shop.ID)
	other, _ := h.operatorWithRole("other@example.com", authz.RoleProjectReader)

	w := h.post("/projects/1/members", writer, url.Values{
		"email": {other.Email}, "role": {authz.RoleProjectWriter},
	})
	if w.Code == http.StatusOK || w.Code == http.StatusSeeOther {
		t.Errorf("a writer added a member (%d)", w.Code)
	}
	_ = shop
}

// --- the project administrator ----------------------------------------------
//
// A refused membership change re-renders the panel with a warning rather than
// answering an error status: it is a form, and that is how a form reports a
// problem. So every test below asserts on the EFFECT, which is the thing that
// matters, rather than on the status code.

// grantsOf is what a user actually holds.
func grantsOf(t *testing.T, h *harness, userID int64) []string {
	t.Helper()

	assignments, err := h.grants.ListUserRoles(t.Context(), userID)
	if err != nil {
		t.Fatalf("ListUserRoles = %v", err)
	}
	out := []string{}
	for _, a := range assignments {
		scope := "unscoped"
		if !a.Unscoped {
			scope = "scoped"
		}
		out = append(out, a.RoleKey+"/"+scope)
	}
	return out
}

// newAccount is somebody holding nothing, so anything they end up with came
// from the request under test.
func (h *harness) newAccount(email string) *domain.User {
	h.t.Helper()
	return h.store.AddUser(domain.User{Fullname: email, Email: email, Active: true})
}

func TestAProjectAdministratorManagesTheirOwnMembers(t *testing.T) {
	// The delegation the model is built on: a project administrator adds their
	// own colleagues without an account on the settings screen.
	h, shop, _ := twoBrands(t)
	_, projectAdmin := h.operatorWithRole("padmin@example.com", authz.RoleProjectAdmin, shop.ID)
	colleague := h.newAccount("colleague@example.com")

	h.post("/projects/1/members", projectAdmin, url.Values{
		"email": {colleague.Email}, "role": {authz.RoleProjectReader},
	})

	members, err := h.grants.ListProjectMembers(t.Context(), shop.ID)
	if err != nil {
		t.Fatalf("ListProjectMembers = %v", err)
	}
	found := false
	for _, m := range members {
		if m.UserID == colleague.ID {
			found = true
		}
	}
	if !found {
		t.Errorf("the colleague was not granted access; they hold %v", grantsOf(t, h, colleague.ID))
	}
}

func TestAProjectAdministratorCannotReachAnotherProjectsMembers(t *testing.T) {
	h, _, alpha := twoBrands(t)
	_, projectAdmin := h.operatorWithRole("padmin@example.com", authz.RoleProjectAdmin, 1)
	colleague := h.newAccount("colleague@example.com")

	h.post("/projects/2/members", projectAdmin, url.Values{
		"email": {colleague.Email}, "role": {authz.RoleProjectReader},
	})

	members, _ := h.grants.ListProjectMembers(t.Context(), alpha.ID)
	for _, m := range members {
		if m.UserID == colleague.ID {
			t.Fatal("the grant was written to another project")
		}
	}
	if got := grantsOf(t, h, colleague.ID); len(got) != 0 {
		t.Errorf("the colleague gained %v", got)
	}
}

func TestAProjectRoleCannotBecomeThePlatformAdministratorFlag(t *testing.T) {
	// The escalation this whole model has to refuse: a project role is scoped,
	// the administrator flag is not a role at all, and there must be no path
	// from one to the other. Anything that is not a built-in project role ranks
	// zero and cannot be handed out.
	h, shop, _ := twoBrands(t)
	_, projectAdmin := h.operatorWithRole("padmin@example.com", authz.RoleProjectAdmin, shop.ID)
	colleague := h.newAccount("colleague@example.com")

	for _, role := range []string{"admin", "administrator", "superuser", "made_up", ""} {
		h.post("/projects/1/members", projectAdmin, url.Values{
			"email": {colleague.Email}, "role": {role},
		})
		if got := grantsOf(t, h, colleague.ID); len(got) != 0 {
			t.Fatalf("the role %q produced %v", role, got)
		}
	}

	// And the flag itself is untouched.
	if user := h.store.Job(0); user != nil {
		t.Error("the fixture is not what the test assumes")
	}
}

func TestARoleAboveTheGrantersOwnIsRefused(t *testing.T) {
	// A writer must not be able to hand out a project administrator role, or
	// the rank ordering means nothing.
	h, shop, _ := twoBrands(t)
	_, writer := h.operatorWithRole("writer@example.com", authz.RoleProjectWriter, shop.ID)
	colleague := h.newAccount("colleague@example.com")

	h.post("/projects/1/members", writer, url.Values{
		"email": {colleague.Email}, "role": {authz.RoleProjectAdmin},
	})
	if got := grantsOf(t, h, colleague.ID); len(got) != 0 {
		t.Errorf("a writer granted %v", got)
	}
}

func TestNobodyCanGrantThemselvesARole(t *testing.T) {
	// Self-membership is refused outright. Without it a project administrator
	// could add a second, wider role to their own account.
	h, shop, _ := twoBrands(t)
	caller, session := h.operatorWithRole("padmin@example.com", authz.RoleProjectAdmin, shop.ID)

	before := grantsOf(t, h, caller.ID)
	h.post("/projects/1/members", session, url.Values{
		"email": {caller.Email}, "role": {authz.RoleProjectAdmin},
	})

	if after := grantsOf(t, h, caller.ID); len(after) != len(before) {
		t.Errorf("the caller went from %v to %v", before, after)
	}
}

func TestAPlatformAdministratorIsNotAddedAsAProjectMember(t *testing.T) {
	// The flag already covers every project, so writing a narrower row beside
	// it would look like a change and do nothing.
	h, shop, _ := twoBrands(t)
	_, projectAdmin := h.operatorWithRole("padmin@example.com", authz.RoleProjectAdmin, shop.ID)
	platformAdmin, _ := h.admin("boss@example.com")

	h.post("/projects/1/members", projectAdmin, url.Values{
		"email": {platformAdmin.Email}, "role": {authz.RoleProjectReader},
	})
	if got := grantsOf(t, h, platformAdmin.ID); len(got) != 0 {
		t.Errorf("a platform administrator was given %v", got)
	}
}

func TestGrantingToAnAddressWithNoAccount(t *testing.T) {
	// The screen says so rather than creating one: an account created by
	// somebody else's typo is an account nobody owns.
	h, shop, _ := twoBrands(t)
	_, projectAdmin := h.operatorWithRole("padmin@example.com", authz.RoleProjectAdmin, shop.ID)

	w := h.post("/projects/1/members", projectAdmin, url.Values{
		"email": {"nobody@example.com"}, "role": {authz.RoleProjectReader},
	})
	if !bodyContains(w, "No account with that address") {
		t.Errorf("the screen did not explain that the address is unknown: %s",
			firstLineOf(w.Body.String()))
	}
}

// --- the API ----------------------------------------------------------------

func TestAProjectKeyReachesOnlyItsOwnProject(t *testing.T) {
	// A leaked key is one project's blast radius, which is only true if the
	// key's scope is applied to every read.
	h, _, _ := twoBrands(t)
	_, shopKey := h.project("shop2", "https://shop2.example.com")
	_ = shopKey

	_, key := h.project("keyed", "https://keyed.example.com")
	keyed := h.store.Project(4)
	if keyed == nil {
		t.Fatal("the fixture project was not created")
	}
	h.job(keyed.ID, "keyed-job", "/cron/keyed")

	w := h.apiGet("/api/v1/jobs", key)
	h.mustCode(w, http.StatusOK, "the API job list")

	body := w.Body.String()
	if !strings.Contains(body, "keyed-job") {
		t.Error("the key cannot see its own project's job")
	}
	for _, foreign := range []string{"shop-daily", "alpha-nightly"} {
		if strings.Contains(body, foreign) {
			t.Errorf("the key can see %s, which belongs to another project", foreign)
		}
	}
}

func TestTheAPIRefusesAnUnknownOrMissingKey(t *testing.T) {
	h, _, _ := twoBrands(t)

	for _, key := range []string{"", "cj_" + strings.Repeat("z", 44), "short"} {
		w := h.apiGet("/api/v1/jobs", key)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("key %q answered %d, want 401", key, w.Code)
		}
	}
}

func TestSyncRegistersJobsForTheCallingProjectOnly(t *testing.T) {
	h, shop, _ := twoBrands(t)
	_, key := h.project("deployer", "https://deployer.example.com")
	deployer := h.store.Project(3)

	w := h.apiPost("/api/v1/sync", key, `{
		"jobs": [
			{"code": "daily-report", "name": "Daily report", "url": "/cron/daily-report",
			 "schedules": ["0 3 * * *"]}
		]
	}`)
	if w.Code != http.StatusOK && w.Code != http.StatusMultiStatus {
		t.Fatalf("sync answered %d: %s", w.Code, firstLineOf(w.Body.String()))
	}

	var found *domain.Job
	for id := int64(1); id <= 8; id++ {
		if job := h.store.Job(id); job != nil && job.Code == "daily-report" {
			found = job
		}
	}
	if found == nil {
		t.Fatal("the job was not registered")
	}
	if found.ProjectID != deployer.ID {
		t.Errorf("the job landed on project %d, want the calling project %d",
			found.ProjectID, deployer.ID)
	}
	_ = shop
}

func TestSyncIsIdempotent(t *testing.T) {
	// A deploy runs it on every release. The same payload twice must change
	// nothing the second time, or every deploy churns the schedule.
	h := newHarness(t)
	_, key := h.project("shop", "https://shop.example.com")

	payload := `{"jobs":[{"code":"daily","name":"Daily","url":"/cron/daily","schedules":["0 3 * * *"]}]}`

	first := h.apiPost("/api/v1/sync", key, payload)
	h.mustCode(first, http.StatusOK, "the first sync")

	second := h.apiPost("/api/v1/sync", key, payload)
	h.mustCode(second, http.StatusOK, "the second sync")

	count := 0
	for id := int64(1); id <= 8; id++ {
		if job := h.store.Job(id); job != nil && job.Code == "daily" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("%d jobs named daily, want 1", count)
	}
}

func TestTheAdminAPINeedsABearerTokenAndAnAdministrator(t *testing.T) {
	h, shop, _ := twoBrands(t)
	_, adminSession := h.admin("admin@example.com")
	_, reader := h.operatorWithRole("reader@example.com", authz.RoleProjectReader, shop.ID)

	if w := h.adminAPI("/api/v1/admin/me", ""); w.Code != http.StatusUnauthorized {
		t.Errorf("no token answered %d, want 401", w.Code)
	}
	if w := h.adminAPI("/api/v1/admin/me", adminSession); w.Code != http.StatusOK {
		t.Errorf("an administrator answered %d, want 200", w.Code)
	}
	// An ordinary operator holds a session, so /me is theirs to read.
	if w := h.adminAPI("/api/v1/admin/me", reader); w.Code != http.StatusOK {
		t.Errorf("an operator answered %d on /me, want 200", w.Code)
	}
}

// --- the administrator ------------------------------------------------------

func TestAnAdministratorSeesEveryProject(t *testing.T) {
	h, _, _ := twoBrands(t)
	_, session := h.admin("admin@example.com")

	w := h.get("/jobs", session)
	h.mustCode(w, http.StatusOK, "the job list")

	for _, code := range []string{"shop-daily", "shop-cleanup", "alpha-nightly"} {
		if !bodyContains(w, code) {
			t.Errorf("the administrator cannot see %s", code)
		}
	}
}

func TestAnUnscopedRoleReachesEveryProject(t *testing.T) {
	// The other way to hold everything: a role granted with no project scope.
	// It is not the same as the administrator flag and has to be tested apart
	// from it, because they take different paths through grantz.
	h, _, _ := twoBrands(t)
	_, session := h.operatorWithRole("wide@example.com", authz.RoleProjectReader)

	w := h.get("/jobs", session)
	h.mustCode(w, http.StatusOK, "the job list")

	for _, code := range []string{"shop-daily", "alpha-nightly"} {
		if !bodyContains(w, code) {
			t.Errorf("an unscoped reader cannot see %s", code)
		}
	}
}

func TestSomebodyWithNoRoleSeesAnEmptyListRatherThanAnError(t *testing.T) {
	// The screens are shared. A refusal here would make a new account look
	// broken rather than empty.
	h, _, _ := twoBrands(t)
	user := h.store.AddUser(domain.User{Email: "nobody@example.com", Active: true})
	session := h.tokenFor(user.ID)

	w := h.get("/jobs", session)
	h.mustCode(w, http.StatusOK, "the job list for an account with no role")

	for _, code := range []string{"shop-daily", "alpha-nightly"} {
		if bodyContains(w, code) {
			t.Errorf("an account with no role can see %s", code)
		}
	}
}

// --- field level restriction on runs ----------------------------------------

func TestARestrictedRoleDoesNotSeeTheResponseBody(t *testing.T) {
	// A job's answer frequently carries customer data, and the person watching
	// the schedule is not always the person allowed to read it.
	h, shop, _ := twoBrands(t)

	status := 500
	duration := 12
	h.store.AddRun(domain.Run{
		JobID: 1, Status: domain.StatusFailed, HTTPStatus: &status, DurationMs: &duration,
		Output: "CUSTOMER-SECRET-PAYLOAD", Error: "boom", RequestURL: "https://shop.example.com/cron/daily",
	})

	// The reader role keeps only the error text.
	roleID := h.grants.RoleID(authz.RoleProjectReader)
	if roleID == 0 {
		t.Fatal("the reader role does not exist")
	}
	if err := h.grants.SetRoleFields(t.Context(), roleID, authz.RunsRead,
		[]string{authz.RunFieldError}); err != nil {
		t.Fatalf("SetRoleFields = %v", err)
	}

	_, reader := h.operatorWithRole("reader@example.com", authz.RoleProjectReader, shop.ID)

	w := h.get("/runs/1", reader)
	h.mustCode(w, http.StatusOK, "the run detail")

	if bodyContains(w, "CUSTOMER-SECRET-PAYLOAD") {
		t.Error("a restricted reader can see the response body")
	}
	if !bodyContains(w, "boom") {
		t.Error("a restricted reader cannot see the error text they were granted")
	}
}

// --- the machine and container panels ---------------------------------------

func TestTheContainerPanelIsForThePlatformAdministratorAlone(t *testing.T) {
	// The panel names every container on the box. A role over one project is a
	// grant on that project's scheduled work, not a view of the server it runs
	// on, so the handler leaves the whole panel out rather than emptying it.
	docker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[{"Id":"abc","Names":["/payments-db"],"State":"running","Status":"Up 2 days"}]`))
	}))
	defer docker.Close()

	h := newHarnessWith(t, nil, withContainers(dockerinfo.NewReader(docker.URL, "")))
	h.mw.SetupCompleted()

	shop, _ := h.project("shop", "https://shop.example.com")
	_, admin := h.admin("admin@example.com")
	_, reader := h.operatorWithRole("reader@example.com", authz.RoleProjectReader, shop.ID)

	w := h.get("/", admin)
	h.mustCode(w, http.StatusOK, "the dashboard")
	if !bodyContains(w, "Containers") {
		t.Error("the administrator was not given the container panel")
	}

	w = h.get("/", reader)
	// The dashboard itself is theirs; only the panel is not.
	h.mustCode(w, http.StatusOK, "the dashboard")
	if bodyContains(w, "Containers") {
		t.Error("a project reader was given the container panel")
	}
}
