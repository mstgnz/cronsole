package handler

import (
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/mstgnz/cronsole/v2/internal/applog"
	"github.com/mstgnz/cronsole/v2/internal/authz"
	"github.com/mstgnz/cronsole/v2/internal/domain"
	"github.com/mstgnz/cronsole/v2/internal/httpx"
	"github.com/mstgnz/cronsole/v2/internal/service"
)

// RunHandler serves the execution history.
type RunHandler struct {
	guard
	runs     *service.RunService
	projects *service.ProjectService
	render   *Renderer
	location *time.Location
	log      *applog.Logger
}

// NewRunHandler wires the handler.
func NewRunHandler(a *authz.Service, runs *service.RunService, projects *service.ProjectService,
	render *Renderer, loc *time.Location, log *applog.Logger) *RunHandler {
	return &RunHandler{guard: guard{authz: a}, runs: runs, projects: projects,
		render: render, location: loc, log: log}
}

const runsPerPage = 50

// List shows the run log.
func (h *RunHandler) List(w http.ResponseWriter, r *http.Request) {
	scope, ok := h.scope(w, r, authz.RunsRead)
	if !ok {
		return
	}
	// What of a run this caller may see. A reader who may know that a job
	// failed does not necessarily get the response body it failed with, and
	// whoever granted the access decides which.
	view, err := h.authz.RunView(r.Context(), h.user(r))
	if err != nil {
		view = authz.RunView{}
	}
	projectScope, _ := h.scope(w, r, authz.ProjectsRead)

	filter := domain.RunFilter{
		Scope:     scope,
		JobID:     httpx.QueryInt64Ptr(r, "job_id"),
		ProjectID: httpx.QueryInt64Ptr(r, "project_id"),
		Status:    httpx.QueryStringPtr(r, "status"),
		Trigger:   httpx.QueryStringPtr(r, "trigger"),
		Search:    httpx.QueryStringPtr(r, "q"),
		Start:     httpx.QueryDatePtr(r, "from", h.location, false),
		End:       httpx.QueryDatePtr(r, "to", h.location, true),
	}
	// Without a window the list scans a table that grows with every
	// execution. A default of one day keeps the common case cheap; a wider
	// range is a choice the operator makes.
	if filter.Start == nil && filter.End == nil && filter.JobID == nil {
		start := time.Now().Add(-24 * time.Hour)
		filter.Start = &start
	}

	page := max1(httpx.QueryInt(r, "page", 1))
	rows, total, err := h.runs.List(r.Context(), filter, (page-1)*runsPerPage, runsPerPage)
	if err != nil {
		h.log.Error("runs: list failed", err.Error())
		w.WriteHeader(http.StatusInternalServerError)
		h.render.Render(w, r, "error", PageData{
			Title: "Error",
			Flash: &Flash{Kind: "danger", Message: tr(r, "The run list could not be loaded.")},
		})
		return
	}

	projects, _ := h.projects.ListNames(r.Context(), projectScope)
	data := map[string]any{
		"runs":     maskRuns(rows, view),
		"view":     view,
		"can":      h.permissions(r),
		"total":    total,
		"page":     page,
		"pages":    pageCount(total, runsPerPage),
		"projects": projects,
		"filter":   r.URL.Query(),
		"query":    r.URL.RawQuery,
		"statuses": []string{
			domain.StatusPending, domain.StatusRunning, domain.StatusSuccess,
			domain.StatusFailed, domain.StatusTimeout, domain.StatusSkipped,
		},
		"triggers": []string{
			domain.TriggerSchedule, domain.TriggerManual, domain.TriggerChain, domain.TriggerAPI,
		},
	}

	if httpx.IsHTMX(r) {
		h.render.Partial(w, r, "runs", "run-table", data)
		return
	}
	h.render.Render(w, r, "runs", PageData{Title: "Runs", Active: "runs", Data: data})
}

// Detail shows one run, its output and what it triggered.
func (h *RunHandler) Detail(w http.ResponseWriter, r *http.Request) {
	id, ok := httpx.PathInt64(chi.URLParam(r, "id"))
	if !ok {
		http.NotFound(w, r)
		return
	}

	scope, ok := h.scope(w, r, authz.RunsRead)
	if !ok {
		return
	}
	view, err := h.authz.RunView(r.Context(), h.user(r))
	if err != nil {
		view = authz.RunView{}
	}

	run, err := h.runs.Get(r.Context(), scope, id)
	if errors.Is(err, service.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		h.log.Error("runs: detail failed", err.Error(), "run_id", id)
		httpx.Fail(w, http.StatusInternalServerError, tr(r, "The run could not be loaded."))
		return
	}

	children, err := h.runs.Children(r.Context(), scope, id)
	if err != nil {
		h.log.Warn("runs: chain children unreadable", err.Error(), "run_id", id)
	}

	masked := maskRuns([]domain.RunRow{*run}, view)
	data := map[string]any{
		"run":      &masked[0],
		"children": maskRuns(children, view),
		"view":     view,
	}
	if httpx.IsHTMX(r) {
		h.render.Partial(w, r, "runs", "run-detail", data)
		return
	}
	h.render.Render(w, r, "run_detail", PageData{
		Title:  "Run " + itoa64(id),
		Active: "runs",
		Data:   data,
	})
}

// maskRuns removes the parts of a run the caller may not see.
//
// Applied here rather than in the query, because the same rows feed several
// screens and a restriction that lives in one SELECT is a restriction the next
// screen does not have. The row is read in full and narrowed once, on the way
// out.
func maskRuns(rows []domain.RunRow, view authz.RunView) []domain.RunRow {
	if view.Output && view.Error && view.RequestURL {
		return rows
	}
	out := make([]domain.RunRow, len(rows))
	copy(out, rows)
	for i := range out {
		if !view.Output {
			out[i].Output = ""
		}
		if !view.Error {
			out[i].Error = ""
		}
		if !view.RequestURL {
			out[i].RequestURL = ""
		}
	}
	return out
}
