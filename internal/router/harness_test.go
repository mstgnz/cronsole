package router

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mstgnz/cronsole/v2/internal/applog"
	"github.com/mstgnz/cronsole/v2/internal/authz"
	"github.com/mstgnz/cronsole/v2/internal/dockerinfo"
	"github.com/mstgnz/cronsole/v2/internal/domain"
	"github.com/mstgnz/cronsole/v2/internal/handler"
	"github.com/mstgnz/cronsole/v2/internal/httpx"
	"github.com/mstgnz/cronsole/v2/internal/middleware"
	"github.com/mstgnz/cronsole/v2/internal/repository/memrepo"
	"github.com/mstgnz/cronsole/v2/internal/service"
	"github.com/mstgnz/cronsole/v2/pkg/auth"
	"github.com/mstgnz/cronsole/v2/pkg/token"
)

// The tests in this package drive the REAL stack. A request goes through the
// real router, the real middleware, the real handlers, the real services and
// the real authorization; only the SQL is replaced by memrepo.
//
// That is deliberate, and it is why they live here rather than beside the
// handlers. A handler test over mocked services proves the handler calls a
// mock. What has to be true is that a reader on one brand cannot reach another
// brand's jobs, and the only way to demonstrate that is to ask the running
// system. Measure the coverage they produce with
//
//	go test ./... -coverpkg=./...
//
// or the reported figure is the router's alone.

const harnessSecret = "a-signing-secret-of-at-least-32-chars"

type harness struct {
	t *testing.T

	store  *memrepo.Store
	grants *memrepo.Authz
	authz  *authz.Service
	issuer *token.Issuer
	mw     *middleware.Set
	mail   *recordingResetMailer
	router http.Handler

	// dispatched records what the runner would have been handed, so a manual
	// run can be observed without executing anything.
	dispatched []int64
}

// newHarness builds the whole application over in-memory storage, on a
// deployment that has already been through its first run.
//
// Latched rather than seeded with an account: the setup gate is what these
// tests need out of the way, and adding a user row for it would show up in
// every test that counts accounts.
func newHarness(t *testing.T) *harness {
	t.Helper()

	h := newFreshHarness(t)
	h.mw.SetupCompleted()
	return h
}

// newFreshHarness is the same stack on a deployment nobody has set up yet:
// no account exists and the setup gate is open. Only the setup tests want it.
func newFreshHarness(t *testing.T) *harness {
	t.Helper()
	return newHarnessWith(t, nil)
}

// repoSet is the storage the services are built over. It exists so a test can
// replace one repository before the stack is assembled, which is the only way
// to make a query fail: memrepo answers everything.
// The field types are the ones the SERVICES ask for, which for jobs and stats
// are wider than the domain repository: the service layer owns those.
type repoSet struct {
	users         domain.UserRepository
	projects      domain.ProjectRepository
	jobs          service.JobStore
	runs          domain.RunRepository
	notifications domain.NotificationRepository
	stats         service.StatsStore
	hosts         domain.HostOverrideRepository
	logs          domain.AppLogRepository
	resets        domain.PasswordResetRepository
}

// recordingResetMailer stands in for the notifier. It keeps the raw token,
// which is the only place a test can get it: the database holds a hash, and
// that is the point.
type recordingResetMailer struct {
	mu    sync.Mutex
	to    string
	token string
	sends int
}

func (m *recordingResetMailer) PasswordReset(to, rawToken string, _ time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.to, m.token = to, rawToken
	m.sends++
}

func (m *recordingResetMailer) last() (to, rawToken string, sends int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.to, m.token, m.sends
}

// harnessOption tunes the stack before it is assembled. Variadic so the call
// sites that want none are unchanged.
type harnessOption func(*harnessConfig)

type harnessConfig struct {
	containers *dockerinfo.Reader
}

// withContainers gives the dashboard a container reader, which is what makes
// the panel exist at all. The composition root leaves it nil unless a Docker
// API was configured, so a test that wants to exercise who may see the panel
// has to supply one.
func withContainers(r *dockerinfo.Reader) harnessOption {
	return func(c *harnessConfig) { c.containers = r }
}

// newHarnessWith builds the stack, letting swap replace repositories first.
func newHarnessWith(t *testing.T, swap func(*repoSet), opts ...harnessOption) *harness {
	t.Helper()

	var cfg harnessConfig
	for _, opt := range opts {
		opt(&cfg)
	}

	h := &harness{
		t:      t,
		store:  memrepo.New(),
		issuer: token.NewIssuer(harnessSecret),
	}
	h.grants = memrepo.NewAuthz(h.store)

	authzService, err := authz.New(authz.Config{
		Store:  h.grants,
		Grants: h.grants,
		// No cache: a test that grants a role and immediately asks about it
		// would otherwise be racing the TTL.
		CacheTTL: time.Nanosecond,
	})
	if err != nil {
		t.Fatalf("authz: %v", err)
	}
	// The catalogue and the built-in roles, written the same way boot writes
	// them. Without this the roles a test grants would not exist.
	if _, err := authzService.Sync(context.Background()); err != nil {
		t.Fatalf("authz sync: %v", err)
	}
	h.authz = authzService

	logger := applog.New()
	renderer, err := handler.NewRenderer(time.UTC, logger, "test")
	if err != nil {
		t.Fatalf("templates: %v", err)
	}

	policy := service.TargetPolicy{AllowPrivate: true}
	set := repoSet{
		users:         memrepo.Users{Store: h.store},
		projects:      memrepo.Projects{Store: h.store},
		jobs:          memrepo.Jobs{Store: h.store},
		runs:          memrepo.Runs{Store: h.store},
		notifications: memrepo.Notifications{Store: h.store},
		stats:         memrepo.Stats{Store: h.store},
		hosts:         memrepo.Hosts{Store: h.store},
		logs:          memrepo.Logs{Store: h.store},
		resets:        memrepo.Resets{Store: h.store},
	}
	if swap != nil {
		swap(&set)
	}

	h.mail = &recordingResetMailer{}
	authService := service.NewAuthService(set.users, set.resets, h.mail, h.issuer, h.grants, logger)
	memberService := service.NewMemberService(authzService, set.users)
	notificationService := service.NewNotificationService(set.notifications)
	projectService := service.NewProjectService(set.projects, set.jobs, policy)
	jobService := service.NewJobService(set.jobs, set.projects, set.runs, policy, time.UTC)
	syncService := service.NewSyncService(set.jobs, set.projects, jobService)
	statsService := service.NewStatsService(set.stats, set.jobs, jobService)
	hostService := service.NewHostOverrideService(set.hosts, policy, logger)

	// Nothing executes. The dispatch hook records the run id so a manual
	// trigger can be asserted without a worker pool or an HTTP target.
	dispatch := func(runID int64, _ string) { h.dispatched = append(h.dispatched, runID) }
	runService := service.NewRunService(set.runs, set.jobs, dispatch)

	// secure=false, because httptest speaks plain HTTP and a Secure cookie
	// would be discarded before the next request could present it.
	mw := middleware.New(authService, projectService, authzService, logger, false)
	h.mw = mw

	h.router = New(Handlers{
		Setup: handler.NewSetupHandler(authService, mw, renderer, logger),
		Auth: handler.NewAuthHandler(authService, renderer, mw,
			// A limit high enough that no test trips it by accident. The
			// limiter has its own tests.
			auth.NewLimiter(10000, time.Minute), auth.TrustedProxy{}, logger),
		Lang:      handler.NewLangHandler(false),
		Docs:      handler.NewDocsHandler(),
		Dashboard: handler.NewDashboardHandler(authzService, statsService, nil, cfg.containers, renderer, logger),
		Jobs: handler.NewJobHandler(authzService, jobService, projectService, runService,
			notificationService, statsService, renderer, logger),
		Runs: handler.NewRunHandler(authzService, runService, projectService, renderer, time.UTC, logger),
		Projects: handler.NewProjectHandler(authzService, projectService, memberService,
			renderer, logger, "https://cron.example.com"),
		Settings: handler.NewSettingsHandler(authService, notificationService, memberService,
			hostService, set.logs, renderer, logger),
		API: handler.NewAPIHandler(authzService, jobService, syncService, runService,
			projectService, statsService, time.UTC, logger),
		Health: handler.NewHealthHandler(nil, nil, "test"),
	}, mw, auth.TrustedProxy{}, logger)

	return h
}

// --- seeding ----------------------------------------------------------------

// admin creates a platform administrator and returns their session token.
func (h *harness) admin(email string) (*domain.User, string) {
	h.t.Helper()
	user := h.store.AddUser(domain.User{
		Fullname: "Administrator", Email: email, Active: true, IsAdmin: true,
	})
	return user, h.tokenFor(user.ID)
}

// adminWithPassword creates an administrator who can actually sign in. admin
// leaves the password empty, which is fine for a test that carries a session
// and useless for one that has to go through the login form.
func (h *harness) adminWithPassword(email, password string) (*domain.User, string) {
	h.t.Helper()

	user := h.store.AddUser(domain.User{
		Fullname: "Administrator", Email: email, Active: true, IsAdmin: true,
		Password: auth.HashAndSalt(password),
	})
	return user, h.tokenFor(user.ID)
}

// operatorWithRole creates an ordinary account holding one role over the named
// projects. No projects means the role reaches every project.
func (h *harness) operatorWithRole(email, roleKey string, projectIDs ...int64) (*domain.User, string) {
	h.t.Helper()
	user := h.store.AddUser(domain.User{Fullname: email, Email: email, Active: true})
	h.grants.GrantRole(user.ID, roleKey, projectIDs...)
	return user, h.tokenFor(user.ID)
}

func (h *harness) tokenFor(userID int64) string {
	h.t.Helper()
	signed, err := h.issuer.Issue(userID)
	if err != nil {
		h.t.Fatalf("Issue = %v", err)
	}
	return signed
}

// project creates a project and returns it with the raw API key.
func (h *harness) project(slug, baseURL string) (*domain.Project, string) {
	h.t.Helper()

	raw := "cj_" + slug + strings.Repeat("k", 44-len(slug))
	sum := sha256.Sum256([]byte(raw))
	project := h.store.AddProject(domain.Project{
		Slug: slug, Name: slug, BaseURL: baseURL, Active: true,
		KeyPrefix: raw[:8], KeyHash: hex.EncodeToString(sum[:]),
	})
	return project, raw
}

// job creates a job under a project.
func (h *harness) job(projectID int64, code, target string) *domain.Job {
	h.t.Helper()
	return h.store.AddJob(domain.Job{
		ProjectID: projectID, Code: code, Name: code, URL: target,
		Method: "GET", Active: true,
	})
}

// --- driving the router -----------------------------------------------------

type request struct {
	method  string
	path    string
	token   string
	apiKey  string
	bearer  string
	lang    string
	form    url.Values
	body    string
	headers map[string]string
}

// do issues a request against the router.
func (h *harness) do(r request) *httptest.ResponseRecorder {
	h.t.Helper()

	var req *http.Request
	switch {
	case r.form != nil:
		req = httptest.NewRequest(r.method, r.path, strings.NewReader(r.form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	case r.body != "":
		req = httptest.NewRequest(r.method, r.path, strings.NewReader(r.body))
		req.Header.Set("Content-Type", "application/json")
	default:
		req = httptest.NewRequest(r.method, r.path, nil)
	}

	if r.token != "" {
		req.AddCookie(&http.Cookie{Name: middleware.CookieName, Value: r.token})
	}
	if r.lang != "" {
		req.AddCookie(&http.Cookie{Name: httpx.LangCookie, Value: r.lang})
	}
	if r.apiKey != "" {
		req.Header.Set("X-API-Key", r.apiKey)
	}
	if r.bearer != "" {
		req.Header.Set("Authorization", "Bearer "+r.bearer)
	}
	for k, v := range r.headers {
		req.Header.Set(k, v)
	}

	w := httptest.NewRecorder()
	h.router.ServeHTTP(w, req)
	return w
}

// get is the common case: a screen, signed in.
func (h *harness) get(path, token string) *httptest.ResponseRecorder {
	h.t.Helper()
	return h.do(request{method: http.MethodGet, path: path, token: token})
}

// post is a state changing form submission, carrying the Origin header a
// browser would send so it passes the same origin check.
func (h *harness) post(path, token string, form url.Values) *httptest.ResponseRecorder {
	h.t.Helper()
	if form == nil {
		form = url.Values{}
	}
	return h.do(request{
		method: http.MethodPost, path: path, token: token, form: form,
		headers: map[string]string{"Origin": "http://example.com"},
	})
}

// apiGet calls a project-scoped API route with a key.
func (h *harness) apiGet(path, key string) *httptest.ResponseRecorder {
	h.t.Helper()
	return h.do(request{method: http.MethodGet, path: path, apiKey: key})
}

// apiPost posts JSON to a project-scoped API route.
func (h *harness) apiPost(path, key, body string) *httptest.ResponseRecorder {
	h.t.Helper()
	return h.do(request{method: http.MethodPost, path: path, apiKey: key, body: body})
}

// adminAPI calls an operator-scoped API route with a bearer token.
func (h *harness) adminAPI(path, bearer string) *httptest.ResponseRecorder {
	h.t.Helper()
	return h.do(request{method: http.MethodGet, path: path, bearer: bearer})
}

// --- assertions -------------------------------------------------------------

func (h *harness) mustCode(w *httptest.ResponseRecorder, want int, what string) {
	h.t.Helper()
	if w.Code != want {
		h.t.Errorf("%s answered %d, want %d: %s", what, w.Code, want, firstLineOf(w.Body.String()))
	}
}

// envelope decodes the JSON body every API route answers with.
func (h *harness) envelope(w *httptest.ResponseRecorder) map[string]any {
	h.t.Helper()

	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		h.t.Fatalf("the response is not JSON: %v: %s", err, w.Body.String())
	}
	return out
}

// dataList reads the array a list endpoint answers with.
func (h *harness) dataList(w *httptest.ResponseRecorder) []any {
	h.t.Helper()

	body := h.envelope(w)
	data, ok := body["data"]
	if !ok || data == nil {
		return nil
	}
	switch typed := data.(type) {
	case []any:
		return typed
	case map[string]any:
		// Some endpoints wrap the rows with a total.
		for _, key := range []string{"jobs", "runs", "items", "rows"} {
			if rows, ok := typed[key].([]any); ok {
				return rows
			}
		}
	}
	h.t.Fatalf("data is not a list: %#v", data)
	return nil
}

func firstLineOf(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	if len(line) > 200 {
		line = line[:200]
	}
	return line
}

// bodyContains reports whether the rendered page mentions something.
func bodyContains(w *httptest.ResponseRecorder, want string) bool {
	return strings.Contains(w.Body.String(), want)
}

// memrepoJobs reads the job store back, for assertions that must not depend on
// what a screen happens to render.
func memrepoJobs(h *harness) memrepo.Jobs { return memrepo.Jobs{Store: h.store} }

// adminSession creates a platform administrator and returns their token, for a
// test that already has its fixture and only needs a way in.
func (h *harness) adminSession(t *testing.T) string {
	t.Helper()
	_, session := h.admin("admin@example.com")
	return session
}
