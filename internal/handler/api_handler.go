package handler

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/mstgnz/cronsole/v2/internal/applog"
	"github.com/mstgnz/cronsole/v2/internal/authz"
	"github.com/mstgnz/cronsole/v2/internal/domain"
	"github.com/mstgnz/cronsole/v2/internal/httpx"
	"github.com/mstgnz/cronsole/v2/internal/service"
)

// APIHandler serves the machine facing endpoints.
//
// Two audiences, and the difference matters. A PROJECT calls in with an API
// key and is scoped to itself and nothing else; an OPERATOR calls in with a
// session token and is scoped to whatever their roles reach. Both arrive at
// the same services through the same argument, a domain.ProjectScope, so the
// two audiences cannot drift apart: what differs is only how the scope is
// resolved, never whether it is applied.
type APIHandler struct {
	guard
	jobs     *service.JobService
	sync     *service.SyncService
	runs     *service.RunService
	projects *service.ProjectService
	stats    *service.StatsService
	location *time.Location
	log      *applog.Logger
}

// NewAPIHandler wires the handler.
func NewAPIHandler(a *authz.Service, jobs *service.JobService, sync *service.SyncService,
	runs *service.RunService, projects *service.ProjectService, stats *service.StatsService,
	loc *time.Location, log *applog.Logger) *APIHandler {
	return &APIHandler{guard: guard{authz: a}, jobs: jobs, sync: sync, runs: runs,
		projects: projects, stats: stats, location: loc, log: log}
}

// projectScope is the calling project's own scope, and the second return is
// false when the request has already been answered.
//
// A project key reaches exactly one project. Returning it as the same scope
// type the operator side uses is what keeps the service layer from needing to
// know which of the two called it.
func (h *APIHandler) projectScope(w http.ResponseWriter, r *http.Request) (*domain.Project, domain.ProjectScope, bool) {
	project := httpx.ProjectFrom(r.Context())
	if project == nil {
		httpx.Fail(w, http.StatusUnauthorized, "an API key is required")
		return nil, domain.NoProjects(), false
	}
	return project, domain.ScopeOf(project.ID), true
}

// --- project scoped ---

// Sync registers a project's whole job set from one declaration.
//
// This is the endpoint that makes a central scheduler workable. A project
// keeps its cron definitions in its own repository, next to the code that
// answers them, and posts them on deploy. Without it the schedule lives here
// and the handler lives there, and the two drift until somebody notices a job
// calling a route that was deleted last quarter.
func (h *APIHandler) Sync(w http.ResponseWriter, r *http.Request) {
	project, _, ok := h.projectScope(w, r)
	if !ok {
		return
	}

	var req service.SyncRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.Fail(w, http.StatusBadRequest, "the request body could not be read: "+err.Error())
		return
	}
	if len(req.Jobs) == 0 && !req.Prune {
		httpx.Fail(w, http.StatusBadRequest, "no jobs were declared")
		return
	}

	result, err := h.sync.Sync(r.Context(), project, req)
	if err != nil {
		h.log.Error("api: sync failed", err.Error(), "project", project.Slug)
		httpx.Fail(w, http.StatusInternalServerError, "the declaration could not be applied")
		return
	}

	h.log.Info("api: jobs synced", "project", project.Slug,
		"created", result.Created, "updated", result.Updated,
		"deactivated", result.Deactivated, "failed", result.Failed)

	// A partial failure answers 207. A 200 would let a deploy pipeline treat
	// "eleven of twelve registered" as a clean run.
	code := http.StatusOK
	if result.Failed > 0 {
		code = http.StatusMultiStatus
	}
	httpx.JSON(w, code, httpx.Envelope{Status: result.Failed == 0, Data: result})
}

// ProjectJobs lists the calling project's jobs.
func (h *APIHandler) ProjectJobs(w http.ResponseWriter, r *http.Request) {
	project, scope, ok := h.projectScope(w, r)
	if !ok {
		return
	}

	rows, total, err := h.jobs.List(r.Context(),
		domain.JobFilter{Scope: scope, ProjectID: &project.ID}, 0, 500)
	if err != nil {
		h.log.Error("api: job list failed", err.Error(), "project", project.Slug)
		httpx.Fail(w, http.StatusInternalServerError, "the job list could not be read")
		return
	}
	httpx.OK(w, map[string]any{"total": total, "jobs": rows})
}

// ProjectTrigger runs one of the calling project's jobs, addressed by code.
//
// By code, not by id: a code is what the calling project knows about itself,
// and it also means the lookup is naturally scoped to the project rather than
// scoped by a check somebody could forget to write.
func (h *APIHandler) ProjectTrigger(w http.ResponseWriter, r *http.Request) {
	project, scope, ok := h.projectScope(w, r)
	if !ok {
		return
	}

	code := strings.ToLower(strings.TrimSpace(chi.URLParam(r, "code")))
	job, err := h.jobs.GetByCode(r.Context(), project.ID, code)
	if errors.Is(err, service.ErrNotFound) {
		httpx.Fail(w, http.StatusNotFound, "no job with that code in this project")
		return
	}
	if err != nil {
		h.log.Error("api: job lookup failed", err.Error(), "project", project.Slug, "code", code)
		httpx.Fail(w, http.StatusInternalServerError, "the job could not be read")
		return
	}

	runID, err := h.runs.Trigger(r.Context(), scope, job.ID, domain.TriggerAPI, nil)
	if err != nil {
		h.log.Error("api: trigger failed", err.Error(), "job_id", job.ID)
		httpx.Fail(w, http.StatusInternalServerError, "the run could not be queued")
		return
	}
	httpx.JSON(w, http.StatusAccepted, httpx.Envelope{
		Status: true,
		Data:   map[string]any{"run_id": runID, "job_id": job.ID, "code": job.Code},
	})
}

// ProjectRuns lists the calling project's run history.
func (h *APIHandler) ProjectRuns(w http.ResponseWriter, r *http.Request) {
	project, scope, ok := h.projectScope(w, r)
	if !ok {
		return
	}

	filter := domain.RunFilter{
		Scope:     scope,
		ProjectID: &project.ID,
		Status:    httpx.QueryStringPtr(r, "status"),
		Start:     httpx.QueryDatePtr(r, "from", h.location, false),
		End:       httpx.QueryDatePtr(r, "to", h.location, true),
	}
	if filter.Start == nil {
		start := time.Now().Add(-24 * time.Hour)
		filter.Start = &start
	}

	limit := httpx.QueryInt(r, "limit", 100)
	rows, total, err := h.runs.List(r.Context(), filter, httpx.QueryInt(r, "offset", 0), limit)
	if err != nil {
		h.log.Error("api: run list failed", err.Error(), "project", project.Slug)
		httpx.Fail(w, http.StatusInternalServerError, "the run list could not be read")
		return
	}
	httpx.OK(w, map[string]any{"total": total, "runs": rows})
}

// --- operator scoped ---

// Me returns the signed in operator.
func (h *APIHandler) Me(w http.ResponseWriter, r *http.Request) {
	httpx.OK(w, httpx.UserFrom(r.Context()))
}

// Projects lists the projects this operator may see.
func (h *APIHandler) Projects(w http.ResponseWriter, r *http.Request) {
	scope, ok := h.scope(w, r, authz.ProjectsRead)
	if !ok {
		return
	}
	rows, err := h.projects.List(r.Context(), scope, r.URL.Query().Get("q"))
	if err != nil {
		httpx.Fail(w, http.StatusInternalServerError, "the project list could not be read")
		return
	}
	httpx.OK(w, rows)
}

// Jobs lists jobs across the projects this operator may see.
func (h *APIHandler) Jobs(w http.ResponseWriter, r *http.Request) {
	scope, ok := h.scope(w, r, authz.JobsRead)
	if !ok {
		return
	}
	filter := domain.JobFilter{
		Scope:     scope,
		ProjectID: httpx.QueryInt64Ptr(r, "project_id"),
		Tag:       httpx.QueryStringPtr(r, "tag"),
		Active:    httpx.QueryBoolPtr(r, "active"),
		Search:    httpx.QueryStringPtr(r, "q"),
	}
	rows, total, err := h.jobs.List(r.Context(), filter,
		httpx.QueryInt(r, "offset", 0), httpx.QueryInt(r, "limit", 100))
	if err != nil {
		httpx.Fail(w, http.StatusInternalServerError, "the job list could not be read")
		return
	}
	httpx.OK(w, map[string]any{"total": total, "jobs": rows})
}

// Job returns one job with its schedules, headers and chain.
func (h *APIHandler) Job(w http.ResponseWriter, r *http.Request) {
	id, ok := httpx.PathInt64(chi.URLParam(r, "id"))
	if !ok {
		httpx.Fail(w, http.StatusBadRequest, "invalid job id")
		return
	}
	scope, ok := h.scope(w, r, authz.JobsRead)
	if !ok {
		return
	}
	detail, err := h.jobs.Get(r.Context(), scope, id)
	if errors.Is(err, service.ErrNotFound) {
		httpx.Fail(w, http.StatusNotFound, "job not found")
		return
	}
	if err != nil {
		httpx.Fail(w, http.StatusInternalServerError, "the job could not be read")
		return
	}
	httpx.OK(w, detail)
}

// CreateJob adds a job.
func (h *APIHandler) CreateJob(w http.ResponseWriter, r *http.Request) {
	scope, ok := h.require(w, r, authz.JobsCreate)
	if !ok {
		return
	}
	var in service.JobInput
	if err := httpx.DecodeJSON(w, r, &in); err != nil {
		httpx.Fail(w, http.StatusBadRequest, "the request body could not be read: "+err.Error())
		return
	}
	id, err := h.jobs.Create(r.Context(), scope, &in)
	if err != nil {
		h.writeServiceError(w, err, "the job could not be created")
		return
	}
	httpx.JSON(w, http.StatusCreated, httpx.Envelope{Status: true, Data: map[string]any{"id": id}})
}

// UpdateJob writes a job.
func (h *APIHandler) UpdateJob(w http.ResponseWriter, r *http.Request) {
	id, ok := httpx.PathInt64(chi.URLParam(r, "id"))
	if !ok {
		httpx.Fail(w, http.StatusBadRequest, "invalid job id")
		return
	}
	scope, ok := h.require(w, r, authz.JobsUpdate)
	if !ok {
		return
	}
	var in service.JobInput
	if err := httpx.DecodeJSON(w, r, &in); err != nil {
		httpx.Fail(w, http.StatusBadRequest, "the request body could not be read: "+err.Error())
		return
	}
	if err := h.jobs.Update(r.Context(), scope, id, &in); err != nil {
		h.writeServiceError(w, err, "the job could not be saved")
		return
	}
	httpx.OK(w, map[string]any{"id": id})
}

// DeleteJob removes a job definition.
func (h *APIHandler) DeleteJob(w http.ResponseWriter, r *http.Request) {
	id, ok := httpx.PathInt64(chi.URLParam(r, "id"))
	if !ok {
		httpx.Fail(w, http.StatusBadRequest, "invalid job id")
		return
	}
	scope, ok := h.require(w, r, authz.JobsDelete)
	if !ok {
		return
	}
	if err := h.jobs.Delete(r.Context(), scope, id); err != nil {
		if denied(w, r, err) {
			return
		}
		httpx.Fail(w, http.StatusInternalServerError, "the job could not be deleted")
		return
	}
	httpx.OK(w, map[string]any{"id": id})
}

// TriggerJob runs a job now.
func (h *APIHandler) TriggerJob(w http.ResponseWriter, r *http.Request) {
	id, ok := httpx.PathInt64(chi.URLParam(r, "id"))
	if !ok {
		httpx.Fail(w, http.StatusBadRequest, "invalid job id")
		return
	}
	scope, ok := h.require(w, r, authz.JobsRun)
	if !ok {
		return
	}
	user := h.user(r)
	var userID *int64
	if user != nil {
		userID = &user.ID
	}

	runID, err := h.runs.Trigger(r.Context(), scope, id, domain.TriggerManual, userID)
	if errors.Is(err, service.ErrNotFound) {
		httpx.Fail(w, http.StatusNotFound, "job not found")
		return
	}
	if err != nil {
		httpx.Fail(w, http.StatusInternalServerError, "the run could not be queued")
		return
	}
	httpx.JSON(w, http.StatusAccepted, httpx.Envelope{Status: true, Data: map[string]any{"run_id": runID}})
}

// Runs lists runs across the jobs this operator may see.
func (h *APIHandler) Runs(w http.ResponseWriter, r *http.Request) {
	scope, ok := h.scope(w, r, authz.RunsRead)
	if !ok {
		return
	}
	view, err := h.authz.RunView(r.Context(), h.user(r))
	if err != nil {
		view = authz.RunView{}
	}

	filter := domain.RunFilter{
		Scope:     scope,
		JobID:     httpx.QueryInt64Ptr(r, "job_id"),
		ProjectID: httpx.QueryInt64Ptr(r, "project_id"),
		Status:    httpx.QueryStringPtr(r, "status"),
		Start:     httpx.QueryDatePtr(r, "from", h.location, false),
		End:       httpx.QueryDatePtr(r, "to", h.location, true),
	}
	rows, total, err := h.runs.List(r.Context(), filter,
		httpx.QueryInt(r, "offset", 0), httpx.QueryInt(r, "limit", 100))
	if err != nil {
		httpx.Fail(w, http.StatusInternalServerError, "the run list could not be read")
		return
	}
	httpx.OK(w, map[string]any{"total": total, "runs": maskRuns(rows, view)})
}

// Summary returns the dashboard figures, for an external monitor.
func (h *APIHandler) Summary(w http.ResponseWriter, r *http.Request) {
	scope, ok := h.scope(w, r, authz.JobsRead)
	if !ok {
		return
	}
	board, err := h.stats.Build(r.Context(), scope, httpx.QueryInt(r, "hours", 24))
	if err != nil {
		httpx.Fail(w, http.StatusInternalServerError, "the summary could not be read")
		return
	}
	httpx.OK(w, board)
}

// PreviewSchedule validates a cron expression and returns its next firings.
func (h *APIHandler) PreviewSchedule(w http.ResponseWriter, r *http.Request) {
	httpx.OK(w, h.jobs.PreviewSchedule(r.URL.Query().Get("expression")))
}

func (h *APIHandler) writeServiceError(w http.ResponseWriter, err error, fallback string) {
	var ve *service.ValidationError
	switch {
	case errors.As(err, &ve):
		httpx.FailWith(w, http.StatusUnprocessableEntity, ve.Error(), ve.Errors)
	case errors.Is(err, service.ErrNotFound):
		httpx.Fail(w, http.StatusNotFound, "not found")
	case errors.Is(err, service.ErrConflict):
		httpx.Fail(w, http.StatusConflict, "already exists")
	default:
		h.log.Error("api: request failed", err.Error())
		httpx.Fail(w, http.StatusInternalServerError, fallback)
	}
}
