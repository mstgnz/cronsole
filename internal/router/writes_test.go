package router

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/mstgnz/cronsole/v2/internal/authz"
	"github.com/mstgnz/cronsole/v2/internal/domain"
)

// Every write, twice: once as a browser navigation and once as an htmx swap.
// The two take different branches on the way out, and the htmx one is the one
// the interface actually uses, so a change that breaks it breaks every button
// on the screen while the browser path stays green.

// writeCase is one state changing route.
type writeCase struct {
	name string
	path string
	form url.Values
	// fullPage marks a route submitted by a plain form rather than by htmx.
	// Those are whole-page navigations on purpose: the job editor and the
	// language switch both have to re-render the document.
	fullPage bool
	// adminOnly marks a route behind the administrator gate, which redirects
	// rather than refusing, so a test cannot read its status code as a verdict.
	adminOnly bool
}

// theWrites is every state changing route on the interface, in an order that
// leaves the fixture usable: nothing here deletes what a later case needs.
func theWrites() []writeCase {
	return []writeCase{
		{name: "create a job", path: "/jobs", fullPage: true, form: url.Values{
			"project_id": {"1"}, "code": {"new-job"}, "name": {"New job"},
			"url": {"/cron/new"}, "method": {"GET"}, "timeout_sec": {"30"},
			"max_duration_sec": {"300"}, "success_min": {"200"}, "success_max": {"299"},
			"active": {"true"},
		}},
		{name: "edit a job", path: "/jobs/1", fullPage: true, form: url.Values{
			"project_id": {"1"}, "code": {"daily"}, "name": {"Renamed"},
			"url": {"/cron/daily"}, "method": {"GET"}, "timeout_sec": {"45"},
			"max_duration_sec": {"300"}, "success_min": {"200"}, "success_max": {"299"},
			"active": {"true"},
		}},
		{name: "switch a job off", path: "/jobs/1/toggle", form: url.Values{"active": {"false"}}},
		{name: "switch a job on", path: "/jobs/1/toggle", form: url.Values{"active": {"true"}}},
		{name: "run a job", path: "/jobs/1/run"},
		{name: "add a schedule", path: "/jobs/1/schedules",
			form: url.Values{"expression": {"*/5 * * * *"}}},
		{name: "add a chain edge", path: "/jobs/1/links", form: url.Values{
			"target_job_id": {"2"}, "condition": {"success"}, "delay_sec": {"15"},
		}},
		{name: "edit a project", path: "/projects/1", form: url.Values{
			"name": {"Shop"}, "slug": {"shop"}, "base_url": {"https://shop.example.com"},
			"active": {"true"},
		}},
		{name: "rotate a project key", path: "/projects/1/rotate-key"},
		{name: "add a member", path: "/projects/1/members", form: url.Values{
			"email": {"colleague@example.com"}, "role": {authz.RoleProjectReader},
		}},
		{name: "create an account", path: "/settings/users", adminOnly: true, form: url.Values{
			"fullname": {"New"}, "email": {"new@example.com"},
			"password": {"a-good-enough-password"}, "active": {"true"},
		}},
		{name: "create a recipient list", path: "/settings/notifications", adminOnly: true,
			form: url.Values{
				"name": {"On call"}, "emails": {"ops@example.com"},
				"on_failure": {"true"}, "active": {"true"},
			}},
		{name: "set run visibility", path: "/settings/roles/run-visibility", adminOnly: true,
			form: url.Values{"role": {authz.RoleProjectReader}, "error": {"true"}}},
		{name: "add a host route", path: "/settings/hosts", adminOnly: true, form: url.Values{
			"hostname": {"shop.example.com"}, "address": {"10.10.0.5"}, "active": {"true"},
		}},
		{name: "switch the language", path: "/lang", fullPage: true,
			form: url.Values{"lang": {"tr"}, "next": {"/jobs"}}},
	}
}

// seedForWrites builds the world every write case assumes.
func seedForWrites(t *testing.T) (*harness, string) {
	t.Helper()

	h := newHarness(t)
	project, _ := h.project("shop", "https://shop.example.com")
	h.job(project.ID, "daily", "/cron/daily")
	h.job(project.ID, "cleanup", "/cron/cleanup")
	h.newAccount("colleague@example.com")
	_, session := h.admin("admin@example.com")
	return h, session
}

func TestEveryWriteWorksAsABrowserNavigation(t *testing.T) {
	// A browser follows a 303. Anything else leaves the operator on a blank
	// page wondering whether the change landed.
	h, session := seedForWrites(t)

	for _, c := range theWrites() {
		t.Run(c.name, func(t *testing.T) {
			w := h.post(c.path, session, c.form)
			if w.Code != http.StatusSeeOther && w.Code != http.StatusOK {
				t.Errorf("POST %s answered %d: %s", c.path, w.Code, firstLineOf(w.Body.String()))
			}
			if w.Code == http.StatusSeeOther && w.Header().Get("Location") == "" {
				t.Errorf("POST %s redirected to nowhere", c.path)
			}
		})
	}
}

func TestEveryWriteWorksOverHTMX(t *testing.T) {
	// htmx does not follow a 303, so a write it makes has to come back either
	// as a fragment or as a 2xx carrying HX-Redirect. A 303 here is a button
	// that silently does nothing.
	h, session := seedForWrites(t)

	for _, c := range theWrites() {
		t.Run(c.name, func(t *testing.T) {
			w := h.htmx(http.MethodPost, c.path, session, c.form)

			switch {
			case w.Code == http.StatusOK:
				// A fragment or a redirect header. Either is fine; a whole
				// document swapped into a table cell is not.
				if strings.Contains(w.Body.String(), "<!doctype html>") {
					t.Errorf("POST %s returned a whole document to htmx", c.path)
				}
			case w.Code == http.StatusSeeOther:
				// A plain form is a whole-page navigation by design, and the
				// browser follows the redirect. Only the routes the interface
				// actually swaps have to answer in htmx's terms.
				if !c.fullPage {
					t.Errorf("POST %s answered 303, which htmx does not follow", c.path)
				}
			default:
				t.Errorf("POST %s answered %d: %s", c.path, w.Code, firstLineOf(w.Body.String()))
			}
		})
	}
}

func TestEveryWriteIsRefusedForAReader(t *testing.T) {
	// The same table from the other side. A route added later inherits the
	// group's middleware but not necessarily the right permission, and this is
	// what notices.
	h, _ := seedForWrites(t)
	_, reader := h.operatorWithRole("reader@example.com", authz.RoleProjectReader, 1)

	// The language switch is not a permission: everybody may read in their own
	// language.
	for _, c := range theWrites() {
		if c.path == "/lang" {
			continue
		}
		t.Run(c.name, func(t *testing.T) {
			w := h.post(c.path, reader, c.form)

			// The administrator gate REDIRECTS rather than refusing, so its
			// status code says nothing. Everything else answers a refusal.
			if !c.adminOnly && w.Code == http.StatusSeeOther {
				t.Errorf("POST %s succeeded for a reader", c.path)
			}
			if w.Code >= 500 {
				t.Errorf("POST %s answered %d; a refusal is not a server fault", c.path, w.Code)
			}
		})
	}

	// And nothing happened. This is the assertion that holds for every route,
	// including the ones that answer with a redirect.
	if job := h.store.Job(1); job == nil || job.Name != "daily" || !job.Active {
		t.Errorf("the job was changed: %+v", job)
	}
	if h.store.Job(3) != nil {
		t.Errorf("a job was created: %+v", h.store.Job(3))
	}
	if h.store.RunCount() != 0 {
		t.Errorf("%d runs were queued", h.store.RunCount())
	}
	if h.store.UserByEmail("new@example.com") != nil {
		t.Error("an account was created")
	}
	if len(h.store.HostOverrides()) != 0 {
		t.Error("a host route was added, which is where credentials get sent")
	}
	if project := h.store.Project(1); project == nil || project.KeyPrefix != "cj_shopk" {
		t.Errorf("the project key was rotated: %+v", project)
	}
}

// --- the job form's summary line --------------------------------------------

func TestSavingAJobSaysWhatItWillDoNext(t *testing.T) {
	// The question somebody has immediately after saving one, answered on the
	// screen they land on. The save redirects rather than rendering in place,
	// so a reload does not repeat the write, and ?saved=1 is what turns that
	// redirect back into visible confirmation.
	h, project, session := console(t)

	cases := []struct {
		name  string
		setUp func(t *testing.T) int64
		want  string
	}{
		{
			name: "with a schedule",
			setUp: func(t *testing.T) int64 {
				job := h.job(project.ID, "scheduled", "/cron/scheduled")
				h.store.AddSchedule(job.ID, "*/5 * * * *")
				return job.ID
			},
			want: "Next run",
		},
		{
			name: "switched off",
			setUp: func(t *testing.T) int64 {
				job := h.store.AddJob(domain.Job{
					ProjectID: project.ID, Code: "off", Name: "off", URL: "/cron/off",
					Method: "GET", Active: false,
				})
				h.store.AddSchedule(job.ID, "*/5 * * * *")
				return job.ID
			},
			want: "inactive",
		},
		{
			name: "chained only",
			setUp: func(t *testing.T) int64 {
				source := h.job(project.ID, "source", "/cron/source")
				target := h.job(project.ID, "chained", "/cron/chained")
				h.post("/jobs/"+itoa(source.ID)+"/links", session, url.Values{
					"target_job_id": {itoa(target.ID)}, "condition": {"success"},
				})
				return target.ID
			},
			want: "trigger it",
		},
		{
			name: "nothing runs it",
			setUp: func(t *testing.T) int64 {
				return h.job(project.ID, "orphan", "/cron/orphan").ID
			},
			want: "triggered by hand",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			id := c.setUp(t)
			w := h.get("/jobs/"+itoa(id)+"?saved=1", session)
			h.mustCode(w, http.StatusOK, "the job form after a save")
			if !bodyContains(w, c.want) {
				t.Errorf("the confirmation does not mention %q", c.want)
			}
		})
	}

	// And without the marker there is no confirmation, or every reload would
	// look like a fresh save.
	job := h.job(project.ID, "quiet", "/cron/quiet")
	if bodyContains(h.get("/jobs/"+itoa(job.ID), session), "Saved.") {
		t.Error("an ordinary page load reported a save")
	}
}

// --- the API's write paths --------------------------------------------------

func TestTheAdminAPIRefusesWritesForAReader(t *testing.T) {
	// The API and the screens enforce the same rules, because both go through
	// the service. Checked separately because they map errors differently.
	h, project, _ := console(t)
	h.job(project.ID, "daily", "/cron/daily")
	_, reader := h.operatorWithRole("reader@example.com", authz.RoleProjectReader, project.ID)

	writes := []request{
		{method: http.MethodPost, path: "/api/v1/admin/jobs", bearer: reader,
			body: `{"project_id":1,"code":"new","name":"New","url":"/cron/new","method":"GET"}`},
		{method: http.MethodPut, path: "/api/v1/admin/jobs/1", bearer: reader,
			body: `{"project_id":1,"code":"daily","name":"Renamed","url":"/cron/daily","method":"GET"}`},
		{method: http.MethodDelete, path: "/api/v1/admin/jobs/1", bearer: reader},
		{method: http.MethodPost, path: "/api/v1/admin/jobs/1/run", bearer: reader},
	}

	for _, req := range writes {
		w := h.do(req)
		if w.Code < 400 {
			t.Errorf("%s %s succeeded for a reader (%d)", req.method, req.path, w.Code)
		}
		if w.Code >= 500 {
			t.Errorf("%s %s answered %d; a refusal is not a server fault", req.method, req.path, w.Code)
		}
	}

	if job := h.store.Job(1); job == nil || job.Name == "Renamed" {
		t.Errorf("the job was changed: %+v", job)
	}
	if h.store.RunCount() != 0 {
		t.Errorf("%d runs were queued by a reader", h.store.RunCount())
	}
}

func TestTheAdminAPIRefusesAJobOnAnotherProject(t *testing.T) {
	h, shop, _ := twoBrands(t)
	_, writer := h.operatorWithRole("writer@example.com", authz.RoleProjectWriter, shop.ID)

	// Job 3 belongs to the other brand.
	for _, req := range []request{
		{method: http.MethodGet, path: "/api/v1/admin/jobs/3", bearer: writer},
		{method: http.MethodDelete, path: "/api/v1/admin/jobs/3", bearer: writer},
		{method: http.MethodPost, path: "/api/v1/admin/jobs/3/run", bearer: writer},
	} {
		w := h.do(req)
		if w.Code != http.StatusNotFound {
			t.Errorf("%s %s answered %d, want 404", req.method, req.path, w.Code)
		}
	}
	if h.store.Job(3) == nil {
		t.Error("the other brand's job was deleted")
	}
}
