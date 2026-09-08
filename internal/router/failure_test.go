package router

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/mstgnz/cronsole/v2/internal/domain"
	"github.com/mstgnz/cronsole/v2/internal/service"
)

// What every screen does when a query fails.
//
// It is the one thing the rest of this package cannot ask: memrepo answers
// everything, so the error branch of every handler is unreachable and could be
// a panic, a blank page or a stack trace shown to the operator without any test
// noticing. The database not answering is not an exotic case either. It is a
// failover, a connection limit, a network blip, and it happens to a scheduler
// at the least convenient moment.
//
// Two things are checked for each screen, and the second is the one that
// matters more: the answer is a 500 with a message a person can read, and it
// does NOT carry the driver's own error text, which is where table names,
// column names and connection strings live.

var errNotAnswering = errors.New("pq: connection reset by peer while reading from the jobs table")

// The decorators. Each embeds the working repository and breaks one method, so
// everything else on the screen behaves normally and the failure under test is
// the only one.

type brokenJobList struct{ service.JobStore }

func (b brokenJobList) List(context.Context, domain.JobFilter, int, int) ([]domain.JobRow, int64, error) {
	return nil, 0, errNotAnswering
}

type brokenJobTags struct{ service.JobStore }

func (b brokenJobTags) ListTags(context.Context, domain.ProjectScope) ([]string, error) {
	return nil, errNotAnswering
}

type brokenProjectNames struct{ domain.ProjectRepository }

func (b brokenProjectNames) ListNames(context.Context, domain.ProjectScope) ([]domain.Project, error) {
	return nil, errNotAnswering
}

type brokenProjectList struct{ domain.ProjectRepository }

func (b brokenProjectList) List(context.Context, domain.ProjectScope, string) ([]domain.ProjectRow, error) {
	return nil, errNotAnswering
}

type brokenRunList struct{ domain.RunRepository }

func (b brokenRunList) List(context.Context, domain.RunFilter, int, int) ([]domain.RunRow, int64, error) {
	return nil, 0, errNotAnswering
}

type brokenUserList struct{ domain.UserRepository }

func (b brokenUserList) List(context.Context, string, int, int) ([]domain.User, int64, error) {
	return nil, 0, errNotAnswering
}

type brokenNotificationList struct{ domain.NotificationRepository }

func (b brokenNotificationList) List(context.Context) ([]domain.Notification, error) {
	return nil, errNotAnswering
}

type brokenLogList struct{ domain.AppLogRepository }

func (b brokenLogList) List(context.Context, string, int, int) ([]domain.AppLog, int64, error) {
	return nil, 0, errNotAnswering
}

type brokenHostList struct{ domain.HostOverrideRepository }

func (b brokenHostList) List(context.Context) ([]domain.HostOverride, error) {
	return nil, errNotAnswering
}

type brokenHostCreate struct{ domain.HostOverrideRepository }

func (b brokenHostCreate) Create(context.Context, *domain.HostOverride) (int64, error) {
	return 0, errNotAnswering
}

type brokenSummary struct{ service.StatsStore }

func (b brokenSummary) Summary(context.Context, domain.ProjectScope) (*domain.Summary, error) {
	return nil, errNotAnswering
}

func TestAScreenSaysSoWhenAQueryFails(t *testing.T) {
	cases := []struct {
		name string
		path string
		swap func(*repoSet)
	}{
		{"the dashboard summary", "/", func(s *repoSet) {
			s.stats = brokenSummary{StatsStore: s.stats}
		}},
		{"the job list", "/jobs", func(s *repoSet) {
			s.jobs = brokenJobList{JobStore: s.jobs}
		}},
		{"the project filter on the job list", "/jobs", func(s *repoSet) {
			s.projects = brokenProjectNames{ProjectRepository: s.projects}
		}},
		{"the tag filter on the job list", "/jobs", func(s *repoSet) {
			s.jobs = brokenJobTags{JobStore: s.jobs}
		}},
		{"the run list", "/runs", func(s *repoSet) {
			s.runs = brokenRunList{RunRepository: s.runs}
		}},
		{"the project list", "/projects", func(s *repoSet) {
			s.projects = brokenProjectList{ProjectRepository: s.projects}
		}},
		{"the account list in settings", "/settings", func(s *repoSet) {
			s.users = brokenUserList{UserRepository: s.users}
		}},
		{"the notification list in settings", "/settings", func(s *repoSet) {
			s.notifications = brokenNotificationList{NotificationRepository: s.notifications}
		}},
		{"the application log in settings", "/settings", func(s *repoSet) {
			s.logs = brokenLogList{AppLogRepository: s.logs}
		}},
		{"the host routes in settings", "/settings", func(s *repoSet) {
			s.hosts = brokenHostList{HostOverrideRepository: s.hosts}
		}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarnessWith(t, c.swap)
			h.mw.SetupCompleted()
			session := h.adminSession(t)

			w := h.get(c.path, session)

			if w.Code != http.StatusInternalServerError {
				t.Fatalf("%s answered %d with the query failing, want 500: %s",
					c.path, w.Code, firstLineOf(w.Body.String()))
			}
			body := w.Body.String()
			// The driver's message names the table it was reading. An operator
			// gets the correlation from the log; the browser gets a sentence.
			if strings.Contains(body, "pq:") || strings.Contains(body, errNotAnswering.Error()) {
				t.Error("the page carries the database's own error text")
			}
			// And it is still a page, not an empty body somebody has to guess at.
			if !strings.Contains(body, "could not be loaded") {
				t.Errorf("the page says nothing about what failed: %s", firstLineOf(body))
			}
		})
	}
}

func TestTheAPIAnswersJSONWhenAQueryFails(t *testing.T) {
	// The API's callers are deploy pipelines. A failure has to arrive as the
	// same JSON envelope every other answer uses, with a 5xx so an at-least-
	// once caller retries rather than recording a successful sync that never
	// happened.
	cases := []struct {
		name    string
		request func(h *harness) *httptest.ResponseRecorder
		swap    func(*repoSet)
	}{
		{"the project job list", func(h *harness) *httptest.ResponseRecorder {
			_, key := h.project("shop", "https://shop.example.com")
			return h.apiGet("/api/v1/jobs", key)
		}, func(s *repoSet) { s.jobs = brokenJobList{JobStore: s.jobs} }},

		{"the project run list", func(h *harness) *httptest.ResponseRecorder {
			_, key := h.project("shop", "https://shop.example.com")
			return h.apiGet("/api/v1/runs", key)
		}, func(s *repoSet) { s.runs = brokenRunList{RunRepository: s.runs} }},

		{"the operator job list", func(h *harness) *httptest.ResponseRecorder {
			return h.adminAPI("/api/v1/admin/jobs", h.adminSession(t))
		}, func(s *repoSet) { s.jobs = brokenJobList{JobStore: s.jobs} }},

		{"the operator project list", func(h *harness) *httptest.ResponseRecorder {
			return h.adminAPI("/api/v1/admin/projects", h.adminSession(t))
		}, func(s *repoSet) { s.projects = brokenProjectList{ProjectRepository: s.projects} }},

		{"the summary", func(h *harness) *httptest.ResponseRecorder {
			return h.adminAPI("/api/v1/admin/summary", h.adminSession(t))
		}, func(s *repoSet) { s.stats = brokenSummary{StatsStore: s.stats} }},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarnessWith(t, c.swap)
			h.mw.SetupCompleted()

			w := c.request(h)

			if w.Code < 500 {
				t.Fatalf("answered %d with the query failing, want a 5xx: %s",
					w.Code, firstLineOf(w.Body.String()))
			}
			body := h.envelope(w)
			if status, _ := body["status"].(bool); status {
				t.Errorf("the envelope reports success: %s", w.Body.String())
			}
			message, _ := body["message"].(string)
			if message == "" {
				t.Errorf("no message for the caller to log: %s", w.Body.String())
			}
			if strings.Contains(message, "pq:") {
				t.Errorf("the driver's error reached the caller: %q", message)
			}
		})
	}
}

func TestAFailedQueryOnAWriteDoesNotClaimSuccess(t *testing.T) {
	// The dangerous shape is the opposite of a 500: an insert that fails and
	// redirects anyway, so the operator watches their change disappear on the
	// next screen and has no reason to think anything went wrong.
	h := newHarnessWith(t, func(s *repoSet) {
		s.hosts = brokenHostCreate{HostOverrideRepository: s.hosts}
	})
	h.mw.SetupCompleted()
	session := h.adminSession(t)

	w := h.post("/settings/hosts", session, url.Values{
		"hostname": {"internal.example.com"},
		"address":  {"10.0.0.9"},
		"active":   {"on"},
	})
	if w.Code >= 200 && w.Code < 400 {
		t.Errorf("a host route saved against a failing database answered %d, want a failure", w.Code)
	}
}
