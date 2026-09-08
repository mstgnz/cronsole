package handler

import (
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/mstgnz/cronsole/v2/internal/applog"
	"github.com/mstgnz/cronsole/v2/internal/authz"
	"github.com/mstgnz/cronsole/v2/internal/domain"
	"github.com/mstgnz/cronsole/v2/internal/httpx"
	"github.com/mstgnz/cronsole/v2/internal/service"
)

// JobHandler serves the job list, the job form and the actions on a job.
type JobHandler struct {
	guard
	jobs          *service.JobService
	projects      *service.ProjectService
	runs          *service.RunService
	notifications *service.NotificationService
	stats         *service.StatsService
	render        *Renderer
	log           *applog.Logger
}

// NewJobHandler wires the handler.
func NewJobHandler(a *authz.Service, jobs *service.JobService, projects *service.ProjectService,
	runs *service.RunService, notifications *service.NotificationService,
	stats *service.StatsService, render *Renderer, log *applog.Logger) *JobHandler {
	return &JobHandler{guard: guard{authz: a}, jobs: jobs, projects: projects, runs: runs,
		notifications: notifications, stats: stats, render: render, log: log}
}

const jobsPerPage = 50

// jobTrendRuns is how many runs the job page charts.
//
// Fifty because it is enough to see a trend and short enough that the query is
// an index read rather than a scan of the job's whole history. A job running
// every minute shows the last hour; one running nightly shows seven weeks.
const jobTrendRuns = 50

// List shows the job list.
func (h *JobHandler) List(w http.ResponseWriter, r *http.Request) {
	scope, ok := h.scope(w, r, authz.JobsRead)
	if !ok {
		return
	}
	readScope, _ := h.scope(w, r, authz.ProjectsRead)

	filter := domain.JobFilter{
		Scope:     scope,
		ProjectID: httpx.QueryInt64Ptr(r, "project_id"),
		Tag:       httpx.QueryStringPtr(r, "tag"),
		Active:    httpx.QueryBoolPtr(r, "active"),
		Search:    httpx.QueryStringPtr(r, "q"),
	}
	page := max1(httpx.QueryInt(r, "page", 1))

	rows, total, err := h.jobs.List(r.Context(), filter, (page-1)*jobsPerPage, jobsPerPage)
	if err != nil {
		h.fail(w, r, "The job list could not be loaded.", err)
		return
	}
	projects, err := h.projects.ListNames(r.Context(), readScope)
	if err != nil {
		h.fail(w, r, "The project list could not be loaded.", err)
		return
	}
	tags, err := h.jobs.ListTags(r.Context(), scope)
	if err != nil {
		h.fail(w, r, "The tag list could not be loaded.", err)
		return
	}

	data := map[string]any{
		"jobs":     rows,
		"total":    total,
		"page":     page,
		"pages":    pageCount(total, jobsPerPage),
		"projects": projects,
		"tags":     tags,
		"filter":   r.URL.Query(),
		"query":    r.URL.RawQuery,
		"can":      h.permissions(r),
	}

	// A fragment request swaps only the table, so filtering does not reload
	// the page or lose the scroll position.
	if httpx.IsHTMX(r) {
		h.render.Partial(w, r, "jobs", "job-table", data)
		return
	}
	h.render.Render(w, r, "jobs", PageData{Title: "Jobs", Active: "jobs", Data: data})
}

// Form shows the create or edit form.
//
// One form for both, and one form for everything about a job: identity,
// target, schedules, chain, headers. Splitting them was the friction: a new
// job used to need a request record, then a schedule record, then an
// activation, across three screens, and a job that was half created looked
// exactly like one that was finished.
func (h *JobHandler) Form(w http.ResponseWriter, r *http.Request) {
	data := map[string]any{"can": h.permissions(r)}

	// The project picker offers only what the caller may WRITE into: offering a
	// project they can read but not write would produce a form that refuses on
	// save.
	writeScope, ok := h.scope(w, r, authz.JobsCreate)
	if !ok {
		return
	}
	readScope, ok := h.scope(w, r, authz.JobsRead)
	if !ok {
		return
	}

	projects, err := h.projects.ListNames(r.Context(), writeScope)
	if err != nil {
		h.fail(w, r, "The project list could not be loaded.", err)
		return
	}
	notifications, err := h.notifications.List(r.Context())
	if err != nil {
		h.fail(w, r, "The notification list could not be loaded.", err)
		return
	}
	tags, err := h.jobs.ListTags(r.Context(), readScope)
	if err != nil {
		h.fail(w, r, "The tag list could not be loaded.", err)
		return
	}
	data["projects"] = projects
	data["notifications"] = notifications
	data["presets"] = schedulePresets
	data["tags"] = tags

	idParam := chi.URLParam(r, "id")
	if idParam == "" {
		data["job"] = defaultJobForm()
		h.render.Render(w, r, "job_form", PageData{Title: "New job", Active: "jobs", Data: data})
		return
	}

	id, ok := httpx.PathInt64(idParam)
	if !ok {
		http.NotFound(w, r)
		return
	}
	detail, err := h.jobs.Get(r.Context(), readScope, id)
	if errors.Is(err, service.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		h.fail(w, r, "The job could not be loaded.", err)
		return
	}

	options, err := h.jobs.ListOptions(r.Context(), writeScope, id)
	if err != nil {
		h.fail(w, r, "The job list could not be loaded.", err)
		return
	}

	data["job"] = detail.Job
	data["detail"] = detail
	data["options"] = options

	// The trend is a nice-to-have on a page whose job is editing. A failure to
	// read it hides the panel rather than taking the editor down with it.
	if trend, err := h.stats.JobTrendFor(r.Context(), readScope, id, jobTrendRuns); err == nil {
		data["trend"] = trend
	} else {
		h.log.Warn("jobs: trend unreadable", err.Error(), "job_id", id)
	}

	page := PageData{Title: detail.Job.Name, Active: "jobs", Data: data}
	// The save redirects here rather than rendering in place, so a reload does
	// not repeat the write. The marker is what turns that redirect back into
	// visible confirmation.
	if r.URL.Query().Get("saved") == "1" {
		page.Flash = &Flash{Kind: "success", Message: tr(r, "Saved. ") + h.nextRunNote(detail)}
	}
	h.render.Render(w, r, "job_form", page)
}

// nextRunNote says what the job will actually do next, which is the question
// somebody has immediately after saving one.
func (h *JobHandler) nextRunNote(detail *service.JobDetail) string {
	if !detail.Job.Active {
		return "The job is inactive, so nothing is scheduled."
	}
	if len(detail.NextRuns) > 0 {
		return "Next run " + detail.NextRuns[0].Format("2006-01-02 15:04") + "."
	}
	if len(detail.Triggers) > 0 {
		return "No schedule of its own; it runs when the jobs that trigger it finish."
	}
	return "No schedule and nothing chained into it, so it runs only when triggered by hand or through the API."
}

// defaultJobForm supplies the values a new job starts with, so the form is
// filled in with something sensible rather than zeros the operator has to
// correct.
func defaultJobForm() *domain.Job {
	return &domain.Job{
		Method:         "GET",
		TimeoutSec:     30,
		MaxDurationSec: 300,
		MaxDelayMin:    10,
		Priority:       100,
		SuccessMin:     200,
		SuccessMax:     399,
		SingleRun:      true,
	}
}

// Save creates or updates a job.
func (h *JobHandler) Save(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		h.fail(w, r, "The form could not be read.", err)
		return
	}
	in := jobInputFromForm(r)

	idParam := chi.URLParam(r, "id")
	if idParam == "" {
		scope, ok := h.require(w, r, authz.JobsCreate)
		if !ok {
			return
		}
		id, err := h.jobs.Create(r.Context(), scope, in)
		if err != nil {
			h.formError(w, r, in, err)
			return
		}
		h.log.Info("job created", "job_id", id, "code", in.Code)
		http.Redirect(w, r, "/jobs/"+itoa64(id)+"?saved=1", http.StatusSeeOther)
		return
	}

	id, ok := httpx.PathInt64(idParam)
	if !ok {
		http.NotFound(w, r)
		return
	}
	scope, ok := h.require(w, r, authz.JobsUpdate)
	if !ok {
		return
	}
	if err := h.jobs.Update(r.Context(), scope, id, in); err != nil {
		if denied(w, r, err) {
			return
		}
		h.formError(w, r, in, err)
		return
	}
	h.log.Info("job updated", "job_id", id)
	http.Redirect(w, r, "/jobs/"+itoa64(id)+"?saved=1", http.StatusSeeOther)
}

// formError re-renders the form with the failures attached, keeping what was
// typed. Losing a filled in form to one bad field is how a validation message
// becomes a reason not to use the screen.
func (h *JobHandler) formError(w http.ResponseWriter, r *http.Request, in *service.JobInput, err error) {
	var ve *service.ValidationError
	if !errors.As(err, &ve) {
		h.fail(w, r, "The job could not be saved.", err)
		return
	}

	writeScope, _ := h.authz.Scope(r.Context(), h.user(r), authz.JobsCreate)
	readScope, _ := h.authz.Scope(r.Context(), h.user(r), authz.JobsRead)
	projects, _ := h.projects.ListNames(r.Context(), writeScope)
	notifications, _ := h.notifications.List(r.Context())
	tags, _ := h.jobs.ListTags(r.Context(), readScope)

	data := map[string]any{
		"projects":      projects,
		"notifications": notifications,
		"presets":       schedulePresets,
		"tags":          tags,
		"job":           inputToJob(in),
		"input":         in,
		"errors":        ve.Errors,
		"can":           h.permissions(r),
	}
	if id, ok := httpx.PathInt64(chi.URLParam(r, "id")); ok {
		if detail, derr := h.jobs.Get(r.Context(), readScope, id); derr == nil {
			data["detail"] = detail
			options, _ := h.jobs.ListOptions(r.Context(), writeScope, id)
			data["options"] = options
		}
	}

	w.WriteHeader(http.StatusUnprocessableEntity)
	h.render.Render(w, r, "job_form", PageData{
		Title:  "Job",
		Active: "jobs",
		Flash:  &Flash{Kind: "danger", Message: ve.Error()},
		Data:   data,
	})
}

// Toggle switches a job on or off.
func (h *JobHandler) Toggle(w http.ResponseWriter, r *http.Request) {
	id, ok := httpx.PathInt64(chi.URLParam(r, "id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	active := httpx.FormBool(r, "active")

	scope, ok := h.require(w, r, authz.JobsActivate)
	if !ok {
		return
	}
	if err := h.jobs.SetActive(r.Context(), scope, id, active); err != nil {
		if denied(w, r, err) {
			return
		}
		h.log.Error("job: activation failed", err.Error(), "job_id", id)
		httpx.Fail(w, http.StatusInternalServerError, tr(r, "The job could not be updated."))
		return
	}
	h.reloadOrRedirect(w, r, "/jobs")
}

// Run queues an immediate run.
func (h *JobHandler) Run(w http.ResponseWriter, r *http.Request) {
	id, ok := httpx.PathInt64(chi.URLParam(r, "id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	user := httpx.UserFrom(r.Context())
	var userID *int64
	if user != nil {
		userID = &user.ID
	}

	scope, ok := h.require(w, r, authz.JobsRun)
	if !ok {
		return
	}
	runID, err := h.runs.Trigger(r.Context(), scope, id, domain.TriggerManual, userID)
	if err != nil {
		if denied(w, r, err) {
			return
		}
		h.log.Error("job: manual run failed", err.Error(), "job_id", id)
		httpx.Fail(w, http.StatusInternalServerError, tr(r, "The run could not be queued."))
		return
	}

	h.log.Info("job triggered manually", "job_id", id, "run_id", runID)
	if httpx.IsHTMX(r) {
		w.Header().Set("HX-Redirect", "/runs?job_id="+itoa64(id))
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, "/runs?job_id="+itoa64(id), http.StatusSeeOther)
}

// Clone copies a job under a new code.
func (h *JobHandler) Clone(w http.ResponseWriter, r *http.Request) {
	id, ok := httpx.PathInt64(chi.URLParam(r, "id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	code := strings.TrimSpace(r.PostFormValue("code"))

	scope, ok := h.require(w, r, authz.JobsCreate)
	if !ok {
		return
	}
	newID, err := h.jobs.Clone(r.Context(), scope, id, code)
	if err != nil {
		if denied(w, r, err) {
			return
		}
		var ve *service.ValidationError
		if errors.As(err, &ve) {
			httpx.Fail(w, http.StatusBadRequest, ve.Error())
			return
		}
		h.log.Error("job: clone failed", err.Error(), "job_id", id)
		httpx.Fail(w, http.StatusInternalServerError, tr(r, "The job could not be copied."))
		return
	}

	if httpx.IsHTMX(r) {
		w.Header().Set("HX-Redirect", "/jobs/"+itoa64(newID))
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, "/jobs/"+itoa64(newID), http.StatusSeeOther)
}

// Delete removes a job definition. Its runs stay: they are the record of what
// the system did.
func (h *JobHandler) Delete(w http.ResponseWriter, r *http.Request) {
	id, ok := httpx.PathInt64(chi.URLParam(r, "id"))
	if !ok {
		http.NotFound(w, r)
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
		h.log.Error("job: delete failed", err.Error(), "job_id", id)
		httpx.Fail(w, http.StatusInternalServerError, tr(r, "The job could not be deleted."))
		return
	}
	h.log.Info("job deleted", "job_id", id)
	h.reloadOrRedirect(w, r, "/jobs")
}

// AddSchedule appends one expression.
func (h *JobHandler) AddSchedule(w http.ResponseWriter, r *http.Request) {
	id, ok := httpx.PathInt64(chi.URLParam(r, "id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	scope, ok := h.require(w, r, authz.JobsUpdate)
	if !ok {
		return
	}
	if _, err := h.jobs.AddSchedule(r.Context(), scope, id, r.PostFormValue("expression")); err != nil {
		if denied(w, r, err) {
			return
		}
		var ve *service.ValidationError
		if errors.As(err, &ve) {
			httpx.Fail(w, http.StatusBadRequest, ve.Errors[0].Message)
			return
		}
		h.log.Error("job: schedule could not be added", err.Error(), "job_id", id)
		httpx.Fail(w, http.StatusInternalServerError, tr(r, "The schedule could not be added."))
		return
	}
	h.reloadOrRedirect(w, r, "/jobs/"+itoa64(id))
}

// RemoveSchedule deletes one expression.
func (h *JobHandler) RemoveSchedule(w http.ResponseWriter, r *http.Request) {
	id, ok := httpx.PathInt64(chi.URLParam(r, "id"))
	scheduleID, ok2 := httpx.PathInt64(chi.URLParam(r, "scheduleID"))
	if !ok || !ok2 {
		http.NotFound(w, r)
		return
	}
	scope, ok := h.require(w, r, authz.JobsUpdate)
	if !ok {
		return
	}
	if err := h.jobs.RemoveSchedule(r.Context(), scope, id, scheduleID); err != nil {
		if denied(w, r, err) {
			return
		}
		h.log.Error("job: schedule could not be removed", err.Error(), "job_id", id)
		httpx.Fail(w, http.StatusInternalServerError, tr(r, "The schedule could not be removed."))
		return
	}
	h.reloadOrRedirect(w, r, "/jobs/"+itoa64(id))
}

// AddLink creates a chain edge.
func (h *JobHandler) AddLink(w http.ResponseWriter, r *http.Request) {
	id, ok := httpx.PathInt64(chi.URLParam(r, "id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	targetID, ok := httpx.PathInt64(r.PostFormValue("target_job_id"))
	if !ok {
		httpx.Fail(w, http.StatusBadRequest, tr(r, "Choose the job to trigger."))
		return
	}
	condition := r.PostFormValue("condition")
	if condition == "" {
		condition = "success"
	}

	scope, ok := h.require(w, r, authz.JobsUpdate)
	if !ok {
		return
	}
	err := h.jobs.AddLink(r.Context(), scope, id, targetID, condition, httpx.FormInt(r, "delay_sec", 15))
	switch {
	case err == nil:
	case errors.Is(err, service.ErrSelfLink):
		httpx.Fail(w, http.StatusBadRequest, tr(r, "A job cannot trigger itself."))
		return
	case errors.Is(err, service.ErrChainLoop):
		httpx.Fail(w, http.StatusBadRequest, tr(r, "That link would make the chain loop back on itself."))
		return
	default:
		var ve *service.ValidationError
		if errors.As(err, &ve) {
			httpx.Fail(w, http.StatusBadRequest, ve.Error())
			return
		}
		h.log.Error("job: link could not be added", err.Error(), "job_id", id)
		httpx.Fail(w, http.StatusInternalServerError, tr(r, "The link could not be added."))
		return
	}
	h.reloadOrRedirect(w, r, "/jobs/"+itoa64(id))
}

// RemoveLink deletes a chain edge.
func (h *JobHandler) RemoveLink(w http.ResponseWriter, r *http.Request) {
	id, ok := httpx.PathInt64(chi.URLParam(r, "id"))
	linkID, ok2 := httpx.PathInt64(chi.URLParam(r, "linkID"))
	if !ok || !ok2 {
		http.NotFound(w, r)
		return
	}
	scope, ok := h.require(w, r, authz.JobsUpdate)
	if !ok {
		return
	}
	if err := h.jobs.RemoveLink(r.Context(), scope, id, linkID); err != nil {
		if denied(w, r, err) {
			return
		}
		h.log.Error("job: link could not be removed", err.Error(), "job_id", id)
		httpx.Fail(w, http.StatusInternalServerError, tr(r, "The link could not be removed."))
		return
	}
	h.reloadOrRedirect(w, r, "/jobs/"+itoa64(id))
}

// PreviewSchedule validates an expression and shows when it would run next.
//
// It answers while the field is being typed. This is the cheapest correction
// available anywhere in the system: the alternative is discovering the mistake
// from a job that did not run, days later, while looking for something else.
func (h *JobHandler) PreviewSchedule(w http.ResponseWriter, r *http.Request) {
	expression := r.URL.Query().Get("expression")
	if expression == "" {
		expression = r.PostFormValue("expression")
	}
	preview := h.jobs.PreviewSchedule(expression)

	if httpx.IsHTMX(r) {
		h.render.Partial(w, r, "job_form", "schedule-preview", preview)
		return
	}
	httpx.OK(w, preview)
}

// schedulePresets are the expressions people actually want, offered as one
// click so the common case never requires knowing cron syntax at all.
var schedulePresets = []struct {
	Label      string
	Expression string
}{
	{"Every minute", "* * * * *"},
	{"Every 5 minutes", "*/5 * * * *"},
	{"Every 15 minutes", "*/15 * * * *"},
	{"Every 30 minutes", "*/30 * * * *"},
	{"Hourly, on the hour", "0 * * * *"},
	{"Every 6 hours", "0 */6 * * *"},
	{"Daily at 03:00", "0 3 * * *"},
	{"Daily at 08:30", "30 8 * * *"},
	{"Weekdays at 09:00", "0 9 * * 1-5"},
	{"Mondays at 07:00", "0 7 * * 1"},
	{"First of the month, 02:00", "0 2 1 * *"},
}

// jobInputFromForm reads the whole job form.
//
// Every field is read explicitly. Binding the form onto the stored struct
// would be shorter and would also let last_status, last_run_at or another
// project's id arrive from a request body.
func jobInputFromForm(r *http.Request) *service.JobInput {
	in := &service.JobInput{
		ProjectID:      formInt64(r, "project_id"),
		Code:           r.PostFormValue("code"),
		Name:           r.PostFormValue("name"),
		Description:    r.PostFormValue("description"),
		Tag:            r.PostFormValue("tag"),
		Method:         r.PostFormValue("method"),
		URL:            r.PostFormValue("url"),
		Body:           r.PostFormValue("body"),
		TimeoutSec:     httpx.FormInt(r, "timeout_sec", 30),
		MaxDurationSec: httpx.FormInt(r, "max_duration_sec", 0),
		Retries:        httpx.FormInt(r, "retries", 0),
		SingleRun:      httpx.FormBool(r, "single_run"),
		RunMissed:      httpx.FormBool(r, "run_missed"),
		MaxDelayMin:    httpx.FormInt(r, "max_delay_min", 10),
		Priority:       httpx.FormInt(r, "priority", 100),
		SuccessMin:     httpx.FormInt(r, "success_min", 200),
		SuccessMax:     httpx.FormInt(r, "success_max", 399),
		NotificationID: httpx.FormInt64Ptr(r, "notification_id"),
		Active:         httpx.FormBool(r, "active"),
		Schedules:      []string{},
		Headers:        []service.HeaderIn{},
	}

	for _, expr := range r.PostForm["schedules[]"] {
		if trimmed := strings.TrimSpace(expr); trimmed != "" {
			in.Schedules = append(in.Schedules, trimmed)
		}
	}

	keys := r.PostForm["header_key[]"]
	values := r.PostForm["header_value[]"]
	for i, key := range keys {
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		value := ""
		if i < len(values) {
			value = values[i]
		}
		in.Headers = append(in.Headers, service.HeaderIn{Key: key, Value: value})
	}
	return in
}

// inputToJob turns a rejected input back into the shape the form template
// reads, so a failed save keeps what was typed.
func inputToJob(in *service.JobInput) *domain.Job {
	return &domain.Job{
		ProjectID:      in.ProjectID,
		Code:           in.Code,
		Name:           in.Name,
		Description:    in.Description,
		Tag:            in.Tag,
		Method:         in.Method,
		URL:            in.URL,
		Body:           in.Body,
		TimeoutSec:     in.TimeoutSec,
		MaxDurationSec: in.MaxDurationSec,
		Retries:        in.Retries,
		SingleRun:      in.SingleRun,
		RunMissed:      in.RunMissed,
		MaxDelayMin:    in.MaxDelayMin,
		Priority:       in.Priority,
		SuccessMin:     in.SuccessMin,
		SuccessMax:     in.SuccessMax,
		NotificationID: in.NotificationID,
		Active:         in.Active,
	}
}

func (h *JobHandler) fail(w http.ResponseWriter, r *http.Request, message string, err error) {
	h.log.Error("job handler: "+message, err.Error(), "path", r.URL.Path)
	w.WriteHeader(http.StatusInternalServerError)
	h.render.Render(w, r, "error", PageData{
		Title: "Error",
		Flash: &Flash{Kind: "danger", Message: message},
	})
}

// reloadOrRedirect answers an action: a fragment request is told to refresh,
// an ordinary form post is redirected so a reload does not repeat the action.
func (h *JobHandler) reloadOrRedirect(w http.ResponseWriter, r *http.Request, target string) {
	if httpx.IsHTMX(r) {
		w.Header().Set("HX-Refresh", "true")
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

func formInt64(r *http.Request, name string) int64 {
	if v := httpx.FormInt64Ptr(r, name); v != nil {
		return *v
	}
	return 0
}

func itoa64(n int64) string {
	if n == 0 {
		return "0"
	}
	negative := n < 0
	if negative {
		n = -n
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	if negative {
		return "-" + string(digits)
	}
	return string(digits)
}

func max1(n int) int {
	if n < 1 {
		return 1
	}
	return n
}

func pageCount(total int64, perPage int) int {
	if total <= 0 {
		return 1
	}
	pages := int((total + int64(perPage) - 1) / int64(perPage))
	if pages < 1 {
		return 1
	}
	return pages
}
