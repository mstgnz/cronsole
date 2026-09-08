package router

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/mstgnz/cronsole/v2/internal/authz"
	"github.com/mstgnz/cronsole/v2/internal/domain"
	"github.com/mstgnz/cronsole/v2/pkg/auth"
)

// The machine surface, and the error classes it has to keep apart. A client
// branches on the status: 400 means fix the request, 401 means fix the
// credentials, 403 means ask somebody, 404 means it is not there, and 5xx means
// retry. Collapsing any two of those makes a pipeline retry something that will
// never work, or give up on something that would.

func TestEveryAPIRouteRefusesAnAnonymousCaller(t *testing.T) {
	// The whole surface, so a route added later without its middleware is
	// caught here rather than in production.
	h, project, _ := console(t)
	h.job(project.ID, "daily", "/cron/daily")

	projectRoutes := []request{
		{method: http.MethodGet, path: "/api/v1/jobs"},
		{method: http.MethodGet, path: "/api/v1/runs"},
		{method: http.MethodPost, path: "/api/v1/sync", body: `{"jobs":[]}`},
		{method: http.MethodPost, path: "/api/v1/jobs/daily/run"},
	}
	for _, req := range projectRoutes {
		w := h.do(req)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s %s answered %d, want 401", req.method, req.path, w.Code)
		}
	}

	operatorRoutes := []request{
		{method: http.MethodGet, path: "/api/v1/admin/me"},
		{method: http.MethodGet, path: "/api/v1/admin/summary"},
		{method: http.MethodGet, path: "/api/v1/admin/projects"},
		{method: http.MethodGet, path: "/api/v1/admin/jobs"},
		{method: http.MethodGet, path: "/api/v1/admin/jobs/1"},
		{method: http.MethodGet, path: "/api/v1/admin/runs"},
		{method: http.MethodGet, path: "/api/v1/admin/schedule-preview?expression=0+3+*+*+*"},
		{method: http.MethodPost, path: "/api/v1/admin/jobs", body: `{}`},
		{method: http.MethodPut, path: "/api/v1/admin/jobs/1", body: `{}`},
		{method: http.MethodDelete, path: "/api/v1/admin/jobs/1"},
		{method: http.MethodPost, path: "/api/v1/admin/jobs/1/run"},
	}
	for _, req := range operatorRoutes {
		w := h.do(req)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s %s answered %d, want 401", req.method, req.path, w.Code)
		}
	}
}

func TestAProjectKeyDoesNotReachTheOperatorAPI(t *testing.T) {
	// The two are different credentials with different scopes. A project key
	// that reached the operator surface would be one project's key reading
	// every project.
	h := newHarness(t)
	_, key := h.project("shop", "https://shop.example.com")

	for _, path := range []string{"/api/v1/admin/me", "/api/v1/admin/jobs", "/api/v1/admin/projects"} {
		w := h.do(request{method: http.MethodGet, path: path, apiKey: key, bearer: key})
		if w.Code != http.StatusUnauthorized {
			t.Errorf("GET %s with a project key answered %d, want 401", path, w.Code)
		}
	}
}

func TestASessionTokenDoesNotWorkAsAProjectKey(t *testing.T) {
	// And the other way round: an operator's session is not scoped to a
	// project, so it cannot stand in for a key.
	h, _, session := console(t)

	for _, path := range []string{"/api/v1/jobs", "/api/v1/runs"} {
		w := h.do(request{method: http.MethodGet, path: path, apiKey: session})
		if w.Code != http.StatusUnauthorized {
			t.Errorf("GET %s with a session token answered %d, want 401", path, w.Code)
		}
	}
}

// --- the error classes ------------------------------------------------------

func TestTheAPIKeepsItsErrorClassesApart(t *testing.T) {
	h, project, session := console(t)
	h.job(project.ID, "daily", "/cron/daily")
	_, reader := h.operatorWithRole("reader@example.com", authz.RoleProjectReader, project.ID)

	cases := []struct {
		name string
		req  request
		want int
		why  string
	}{
		{
			name: "a body that is not JSON",
			req: request{method: http.MethodPost, path: "/api/v1/admin/jobs", bearer: session,
				body: `not json`},
			want: http.StatusBadRequest,
			why:  "retrying will not help; the request is wrong",
		},
		{
			name: "a job that fails validation",
			req: request{method: http.MethodPost, path: "/api/v1/admin/jobs", bearer: session,
				body: `{"project_id":1,"code":"","name":"","url":"","method":"GET"}`},
			want: http.StatusUnprocessableEntity,
			why:  "the body parsed and the values are wrong, which is not the same as a body that did not parse",
		},
		{
			name: "a job on a project that is not there",
			req: request{method: http.MethodPost, path: "/api/v1/admin/jobs", bearer: session,
				body: `{"project_id":999,"code":"elsewhere","name":"Elsewhere","url":"/cron/x","method":"GET"}`},
			want: http.StatusUnprocessableEntity,
			why: "the project is a FIELD of the payload, so the answer names the field rather " +
				"than saying the endpoint is missing. A project out of scope answers the same " +
				"way as one that does not exist, which is what stops the id being probed.",
		},
		{
			name: "a write a reader may not make",
			req:  request{method: http.MethodDelete, path: "/api/v1/admin/jobs/1", bearer: reader},
			want: http.StatusForbidden,
			why:  "the caller is known and not allowed, which is a person to ask rather than a retry",
		},
		{
			name: "an id that is not one",
			req:  request{method: http.MethodDelete, path: "/api/v1/admin/jobs/abc", bearer: session},
			want: http.StatusBadRequest,
			why: "the API separates a malformed id from a missing row, which tells a client " +
				"whether to fix the request or the assumption. The screens answer 404 for both, " +
				"because a person following a stale link is not debugging a request.",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := h.do(c.req)
			if w.Code != c.want {
				t.Errorf("answered %d, want %d (%s): %s",
					w.Code, c.want, c.why, firstLineOf(w.Body.String()))
			}
		})
	}
}

func TestASyncThatFailsValidationSaysWhich(t *testing.T) {
	// A pipeline reads this to print the value it got wrong. "Invalid" alone
	// sends somebody to read the whole file.
	h := newHarness(t)
	_, key := h.project("shop", "https://shop.example.com")

	w := h.apiPost("/api/v1/sync", key, `{
		"jobs": [
			{"code": "good", "name": "Good", "url": "/cron/good"},
			{"code": "bad code with spaces", "name": "Bad", "url": "/cron/bad"},
			{"code": "elsewhere", "name": "Elsewhere", "url": "https://evil.com/collect"}
		]
	}`)
	if w.Code != http.StatusMultiStatus {
		t.Fatalf("answered %d, want 207", w.Code)
	}

	body := w.Body.String()
	for _, want := range []string{"bad code with spaces", "elsewhere", "good"} {
		if !strings.Contains(body, want) {
			t.Errorf("the answer does not mention %q: %s", want, firstLineOf(body))
		}
	}

	// The one that was fine is registered: a bad neighbour does not refuse the
	// whole payload.
	found := false
	for id := int64(1); id <= 4; id++ {
		if job := h.store.Job(id); job != nil && job.Code == "good" {
			found = true
		}
	}
	if !found {
		t.Error("the valid job was not registered alongside the invalid ones")
	}
}

// --- signing in through the API ---------------------------------------------

func TestTheAPILoginRefusals(t *testing.T) {
	h := newHarness(t)
	h.store.AddUser(domain.User{
		Email: "admin@example.com", Active: true, IsAdmin: true,
		Password: auth.HashAndSalt("a-good-enough-password"),
	})
	h.store.AddUser(domain.User{
		Email: "gone@example.com", Active: false,
		Password: auth.HashAndSalt("a-good-enough-password"),
	})

	cases := []struct {
		name string
		body string
		want int
	}{
		{"a wrong password", `{"email":"admin@example.com","password":"wrong"}`, http.StatusUnauthorized},
		{"an unknown address", `{"email":"nobody@example.com","password":"x"}`, http.StatusUnauthorized},
		// A deactivated account answers the same thing as a wrong password.
		// Telling them apart says which addresses are registered.
		{"a deactivated account", `{"email":"gone@example.com","password":"a-good-enough-password"}`,
			http.StatusUnauthorized},
		{"an empty body", `{}`, http.StatusUnauthorized},
		{"a malformed body", `{`, http.StatusBadRequest},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := h.do(request{method: http.MethodPost, path: "/api/v1/login", body: c.body})
			if w.Code != c.want {
				t.Errorf("answered %d, want %d: %s", w.Code, c.want, firstLineOf(w.Body.String()))
			}
		})
	}
}

func TestSigningOutWithoutASession(t *testing.T) {
	// A logout button on a page whose session has already expired. It must not
	// be an error: the caller wanted to be signed out, and they are.
	h := newHarness(t)

	w := h.post("/logout", "", nil)
	if w.Code >= 500 {
		t.Errorf("signing out with no session answered %d", w.Code)
	}
}

// --- the job routes' error branches -----------------------------------------

func TestCloningIntoACodeThatIsTaken(t *testing.T) {
	h, project, session := console(t)
	h.job(project.ID, "daily", "/cron/daily")
	h.job(project.ID, "taken", "/cron/taken")

	w := h.post("/jobs/1/clone", session, url.Values{"code": {"taken"}})
	if w.Code == http.StatusSeeOther {
		t.Error("the clone took a code that is already in use")
	}
	if h.store.Job(3) != nil {
		t.Errorf("a third job was created: %+v", h.store.Job(3))
	}
}

func TestAScheduleThatIsAlreadyThereIsRefused(t *testing.T) {
	// Two identical expressions would queue the same minute twice, and the
	// second insert conflicts on the unique index.
	h, project, session := console(t)
	job := h.job(project.ID, "daily", "/cron/daily")

	h.post("/jobs/"+itoa(job.ID)+"/schedules", session, url.Values{"expression": {"0 3 * * *"}})
	h.post("/jobs/"+itoa(job.ID)+"/schedules", session, url.Values{"expression": {"0 3 * * *"}})

	rows, err := memrepoJobs(h).ListSchedules(t.Context(), job.ID)
	if err != nil {
		t.Fatalf("ListSchedules = %v", err)
	}
	if len(rows) != 1 {
		t.Errorf("%d schedules, want 1", len(rows))
	}
}

func TestAChainEdgeToAJobThatIsNotThere(t *testing.T) {
	h, project, session := console(t)
	job := h.job(project.ID, "daily", "/cron/daily")

	w := h.post("/jobs/"+itoa(job.ID)+"/links", session, url.Values{
		"target_job_id": {"999"}, "condition": {"success"},
	})
	if w.Code == http.StatusSeeOther {
		t.Error("a chain edge to a job that is not there was accepted")
	}

	links, err := memrepoJobs(h).ListLinks(t.Context(), job.ID)
	if err != nil {
		t.Fatalf("ListLinks = %v", err)
	}
	if len(links) != 0 {
		t.Errorf("the edge was stored: %+v", links)
	}
}

func TestOnlySomebodyWhoSeesBothProjectsCanChainAcrossThem(t *testing.T) {
	// A chain edge is created within the caller's SCOPE, so who may make one
	// across brands follows from what they can see rather than from a rule of
	// its own.
	//
	// A platform administrator sees every project, so they can. That is a
	// deliberate consequence worth knowing: the run appears in the other
	// project's history with trigger "chain", and its owners cannot see the job
	// that caused it. If cross-project chaining should be refused outright,
	// this is the test that says so.
	h, shop, alpha := twoBrands(t)

	// A writer on one brand cannot: job 3 is out of their scope, so the target
	// simply does not exist as far as they are concerned.
	_, writer := h.operatorWithRole("writer@example.com", authz.RoleProjectWriter, shop.ID)
	h.post("/jobs/1/links", writer, url.Values{
		"target_job_id": {"3"}, "condition": {"success"},
	})
	links, err := memrepoJobs(h).ListLinks(t.Context(), 1)
	if err != nil {
		t.Fatalf("ListLinks = %v", err)
	}
	if len(links) != 0 {
		t.Fatalf("a writer chained into another brand: %+v", links)
	}

	// A platform administrator can, because both jobs are within their scope.
	h.post("/jobs/1/links", h.adminSession(t), url.Values{
		"target_job_id": {"3"}, "condition": {"success"},
	})
	links, _ = memrepoJobs(h).ListLinks(t.Context(), 1)
	if len(links) != 1 {
		t.Fatalf("%d edges after an administrator chained across brands", len(links))
	}
	if links[0].TargetJobID != 3 {
		t.Errorf("the edge points at %d, want the other brand's job", links[0].TargetJobID)
	}
	_ = alpha
}
