package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/mstgnz/cronsole/v2/internal/domain"
	"github.com/mstgnz/cronsole/v2/internal/repository"
	"github.com/mstgnz/cronsole/v2/pkg/cronexpr"
)

// JobService owns everything about defining a job: validating it, storing it,
// and the rules that decide whether it may be switched on.
type JobService struct {
	jobs     JobStore
	projects domain.ProjectRepository
	runs     domain.RunRepository
	policy   TargetPolicy
	location *time.Location
}

// JobStore is the job repository plus the reachability query the chain form
// needs. Declared here rather than in domain because it is this service's
// requirement and no other caller has it.
type JobStore interface {
	domain.JobRepository
	ReachesJob(ctx context.Context, fromID, targetID int64, maxDepth int) (bool, error)
}

// NewJobService wires the service.
func NewJobService(jobs JobStore, projects domain.ProjectRepository, runs domain.RunRepository,
	policy TargetPolicy, loc *time.Location) *JobService {
	return &JobService{jobs: jobs, projects: projects, runs: runs, policy: policy, location: loc}
}

// JobInput is everything a caller may set on a job.
//
// It is a separate type from domain.Job on purpose. Binding a request straight
// onto the stored struct is how last_status, last_run_at or a foreign id get
// written from a request body; here the fields that must never arrive from
// outside simply do not exist.
type JobInput struct {
	ProjectID   int64  `json:"project_id"`
	Code        string `json:"code"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Tag         string `json:"tag"`

	Method string `json:"method"`
	URL    string `json:"url"`
	Body   string `json:"body"`

	TimeoutSec     int `json:"timeout_sec"`
	MaxDurationSec int `json:"max_duration_sec"`
	Retries        int `json:"retries"`

	SingleRun   bool `json:"single_run"`
	RunMissed   bool `json:"run_missed"`
	MaxDelayMin int  `json:"max_delay_min"`
	Priority    int  `json:"priority"`

	SuccessMin int `json:"success_min"`
	SuccessMax int `json:"success_max"`

	NotificationID *int64 `json:"notification_id"`
	Active         bool   `json:"active"`

	// Schedules and Headers are part of the same form. Creating a job and
	// giving it a time used to be two screens and two saves, which is the
	// friction this collapses: one submission produces a job that is ready to
	// run.
	Schedules []string   `json:"schedules"`
	Headers   []HeaderIn `json:"headers"`
	Links     []ChainIn  `json:"links,omitempty"`
}

// HeaderIn is one request header from a form or an API payload.
type HeaderIn struct {
	Key      string `json:"key"`
	Value    string `json:"value"`
	IsSecret bool   `json:"is_secret"`
}

// ChainIn is one chain link expressed by target code, which is what a project
// declaring its jobs can actually know: ids are ours, codes are theirs.
type ChainIn struct {
	TargetCode string `json:"target"`
	Condition  string `json:"condition"`
	DelaySec   int    `json:"delay_sec"`
}

// JobDetail is a job with everything the detail screen shows.
type JobDetail struct {
	Job         *domain.Job          `json:"job"`
	Project     *domain.Project      `json:"project"`
	Schedules   []domain.JobSchedule `json:"schedules"`
	Headers     []domain.JobHeader   `json:"headers"`
	Links       []domain.JobLinkRow  `json:"links"`
	Triggers    []domain.JobLinkRow  `json:"triggers"`
	Recent      []domain.Run         `json:"recent"`
	NextRuns    []time.Time          `json:"next_runs"`
	ResolvedURL string               `json:"resolved_url"`
}

// applyDefaults fills the values a caller may reasonably leave out. It runs
// before validation so a partial payload from the sync API is judged on what
// it will actually become.
func (in *JobInput) applyDefaults() {
	in.Code = strings.ToLower(strings.TrimSpace(in.Code))
	in.Name = strings.TrimSpace(in.Name)
	in.Tag = strings.TrimSpace(in.Tag)
	in.Method = strings.ToUpper(strings.TrimSpace(in.Method))
	in.URL = strings.TrimSpace(in.URL)

	if in.Name == "" {
		in.Name = in.Code
	}
	if in.Method == "" {
		in.Method = "GET"
	}
	if in.TimeoutSec == 0 {
		in.TimeoutSec = domain.DefaultTimeoutSec
	}
	if in.MaxDurationSec == 0 {
		// Ten times the timeout, floored at five minutes. The watchdog has to
		// wait long enough that a slow but healthy run is not declared stuck,
		// and a multiple of the job's own patience is the only figure that
		// scales with the job.
		in.MaxDurationSec = in.TimeoutSec * 10
		if in.MaxDurationSec < 300 {
			in.MaxDurationSec = 300
		}
		if in.MaxDurationSec > 86400 {
			in.MaxDurationSec = 86400
		}
	}
	if in.MaxDelayMin == 0 {
		in.MaxDelayMin = 10
	}
	if in.Priority == 0 {
		in.Priority = 100
	}
	if in.SuccessMin == 0 {
		in.SuccessMin = 200
	}
	if in.SuccessMax == 0 {
		in.SuccessMax = 399
	}

	cleaned := make([]string, 0, len(in.Schedules))
	for _, s := range in.Schedules {
		if normalized := strings.Join(strings.Fields(s), " "); normalized != "" {
			cleaned = append(cleaned, normalized)
		}
	}
	in.Schedules = cleaned
}

var allowedMethods = map[string]bool{
	"GET": true, "POST": true, "PUT": true, "PATCH": true, "DELETE": true, "HEAD": true,
}

// Validate checks an input against every rule, reporting all failures at once.
func (s *JobService) Validate(ctx context.Context, in *JobInput, existing *domain.Job) error {
	in.applyDefaults()
	v := &ValidationError{}

	if existing == nil {
		if !ValidSlug(in.Code) {
			v.Add("code", "use lower case letters, digits, dash and underscore, 2 to 64 characters")
		}
	}
	if in.Name == "" {
		v.Add("name", "required")
	}
	if len(in.Name) > 200 {
		v.Add("name", "at most 200 characters")
	}
	if !allowedMethods[in.Method] {
		v.Add("method", "must be one of GET, POST, PUT, PATCH, DELETE, HEAD")
	}

	project, err := s.projects.GetByID(ctx, in.ProjectID)
	switch {
	case errors.Is(err, repository.ErrNotFound):
		v.Add("project_id", "unknown project")
	case err != nil:
		return err
	default:
		if _, err := s.policy.Resolve(project.BaseURL, in.URL); err != nil {
			v.Add("url", err.Error())
		}
	}

	if in.TimeoutSec < domain.MinTimeoutSec || in.TimeoutSec > domain.MaxTimeoutSec {
		v.Add("timeout_sec", fmt.Sprintf("must be between %d and %d seconds", domain.MinTimeoutSec, domain.MaxTimeoutSec))
	}
	if in.MaxDurationSec < in.TimeoutSec {
		v.Add("max_duration_sec", "must be at least the timeout, otherwise the watchdog closes a run that is still healthy")
	}
	if in.Retries < 0 || in.Retries > 5 {
		v.Add("retries", "must be between 0 and 5")
	}
	if in.MaxDelayMin < 0 || in.MaxDelayMin > 1440 {
		v.Add("max_delay_min", "must be between 0 and 1440 minutes")
	}
	if in.SuccessMin < 100 || in.SuccessMin > 599 || in.SuccessMax < 100 || in.SuccessMax > 599 {
		v.Add("success_min", "status range must be between 100 and 599")
	}
	if in.SuccessMax < in.SuccessMin {
		v.Add("success_max", "must not be below the lower bound")
	}

	for i, expr := range in.Schedules {
		if err := cronexpr.Validate(expr); err != nil {
			v.Add(fmt.Sprintf("schedules.%d", i), err.Error())
		}
	}
	for i, h := range in.Headers {
		key := strings.TrimSpace(h.Key)
		if key == "" && strings.TrimSpace(h.Value) == "" {
			continue
		}
		if !ValidHeaderName(key) {
			v.Add(fmt.Sprintf("headers.%d.key", i), "not a valid header name")
		}
		if !ValidHeaderValue(h.Value) {
			v.Add(fmt.Sprintf("headers.%d.value", i), "value is too long or contains a line break")
		}
	}

	// A job with no schedule is NOT refused, and that took a correction to get
	// right. Two entirely normal jobs have no schedule of their own: one that
	// only ever runs as a chain step after another job, and one a project
	// triggers itself through the API. Refusing those made the chain feature
	// unreachable from the very endpoint meant to declare it.
	//
	// The risk the old rule was guarding against is real, though: a job that
	// sits active and never fires looks like a broken scheduler. That warning
	// now lives on the list, where it can distinguish "chain only" from "never
	// runs", which a validation error never could.

	return v.ErrOrNil()
}

func (in *JobInput) toDomain(existing *domain.Job) *domain.Job {
	job := &domain.Job{
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
	if existing != nil {
		job.ID = existing.ID
		// The code is never taken from the input on update. History, chain
		// links and alert subjects are keyed by it; renaming it would orphan
		// the trail that explains what the job did.
		job.Code = existing.Code
	}
	return job
}

// Create stores a new job together with its schedules and headers.
//
// The scope is checked against the project the job is being created IN, not
// against anything already stored: this is the one write where the target
// project comes entirely from the request body.
func (s *JobService) Create(ctx context.Context, scope domain.ProjectScope, in *JobInput) (int64, error) {
	if err := s.Validate(ctx, in, nil); err != nil {
		return 0, err
	}
	if !scope.Allows(in.ProjectID) {
		return 0, ErrForbidden
	}

	id, err := s.jobs.Create(ctx, in.toDomain(nil))
	if errors.Is(err, repository.ErrDuplicate) {
		return 0, (&ValidationError{}).Add("code", "a job with this code already exists in the project")
	}
	if err != nil {
		return 0, err
	}

	if err := s.jobs.ReplaceSchedules(ctx, id, in.Schedules); err != nil {
		return id, err
	}
	if err := s.jobs.ReplaceHeaders(ctx, id, headersToDomain(id, in.Headers)); err != nil {
		return id, err
	}
	return id, nil
}

// Update writes an existing job.
func (s *JobService) Update(ctx context.Context, scope domain.ProjectScope, id int64, in *JobInput) error {
	existing, err := s.jobs.Get(ctx, id)
	if errors.Is(err, repository.ErrNotFound) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	// BOTH ends. The form carries a project field, so an update can move a job
	// between projects; checking only the stored one would let somebody push a
	// job into a project they cannot reach, and checking only the new one would
	// let them take a job out of a project they cannot reach.
	if !scope.Allows(existing.ProjectID) {
		return ErrNotFound
	}
	if !scope.Allows(in.ProjectID) {
		return ErrForbidden
	}
	if err := s.Validate(ctx, in, existing); err != nil {
		return err
	}

	if err := s.jobs.Update(ctx, in.toDomain(existing)); err != nil {
		return err
	}
	if in.Schedules != nil {
		if err := s.jobs.ReplaceSchedules(ctx, id, in.Schedules); err != nil {
			return err
		}
	}
	if in.Headers != nil {
		if err := s.jobs.ReplaceHeaders(ctx, id, headersToDomain(id, in.Headers)); err != nil {
			return err
		}
	}
	return nil
}

func headersToDomain(jobID int64, in []HeaderIn) []domain.JobHeader {
	out := make([]domain.JobHeader, 0, len(in))
	for _, h := range in {
		key := strings.TrimSpace(h.Key)
		if key == "" {
			continue
		}
		out = append(out, domain.JobHeader{JobID: jobID, Key: key, Value: h.Value, IsSecret: h.IsSecret})
	}
	return out
}

// SetActive switches a job on or off.
func (s *JobService) SetActive(ctx context.Context, scope domain.ProjectScope, id int64, active bool) error {
	if _, err := s.owned(ctx, scope, id); err != nil {
		return err
	}
	return s.jobs.SetActive(ctx, id, active)
}

// owned reads a job and refuses it when it is outside the scope.
//
// It answers ErrNotFound rather than ErrForbidden on purpose: telling somebody
// that a job exists but is not theirs confirms an id, and an id is the only
// thing needed to probe the rest.
//
// Every single-row method goes through here. That is the whole IDOR defence,
// and it lives in the service rather than a handler because a handler can be
// bypassed by the next route somebody adds.
func (s *JobService) owned(ctx context.Context, scope domain.ProjectScope, id int64) (*domain.Job, error) {
	job, err := s.jobs.Get(ctx, id)
	if errors.Is(err, repository.ErrNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if !scope.Allows(job.ProjectID) {
		return nil, ErrNotFound
	}
	return job, nil
}

// Delete removes a job definition. Its run history stays: the rows are the
// record of what the system did, and they outlive the definition.
func (s *JobService) Delete(ctx context.Context, scope domain.ProjectScope, id int64) error {
	if _, err := s.owned(ctx, scope, id); err != nil {
		return err
	}
	return s.jobs.SoftDelete(ctx, id, time.Now())
}

// Get assembles the detail view.
func (s *JobService) Get(ctx context.Context, scope domain.ProjectScope, id int64) (*JobDetail, error) {
	job, err := s.owned(ctx, scope, id)
	if err != nil {
		return nil, err
	}

	project, err := s.projects.GetByID(ctx, job.ProjectID)
	if err != nil && !errors.Is(err, repository.ErrNotFound) {
		return nil, err
	}

	schedules, err := s.jobs.ListSchedules(ctx, id)
	if err != nil {
		return nil, err
	}
	headers, err := s.jobs.ListHeaders(ctx, id)
	if err != nil {
		return nil, err
	}
	links, err := s.jobs.ListLinks(ctx, id)
	if err != nil {
		return nil, err
	}
	triggers, err := s.jobs.ListTriggers(ctx, id)
	if err != nil {
		return nil, err
	}
	recent, err := s.runs.Recent(ctx, id, 20)
	if err != nil {
		return nil, err
	}

	detail := &JobDetail{
		Job:       job,
		Project:   project,
		Schedules: s.annotate(schedules),
		Headers:   headers,
		Links:     links,
		Triggers:  triggers,
		Recent:    recent,
	}
	detail.NextRuns = s.nextRuns(schedules, 5)
	if project != nil {
		if resolved, err := s.policy.Resolve(project.BaseURL, job.URL); err == nil {
			detail.ResolvedURL = resolved
		}
	}
	return detail, nil
}

// annotate fills in the next run and the human reading of each expression.
//
// It is the only place a mistyped expression becomes visible before it costs a
// day: "0 16 * * 7" is perfectly valid, and only the date printed next to it
// tells the reader whether it means what they thought.
func (s *JobService) annotate(schedules []domain.JobSchedule) []domain.JobSchedule {
	now := time.Now().In(s.location)
	for i := range schedules {
		expr, err := cronexpr.Parse(schedules[i].Expression)
		if err != nil {
			schedules[i].Description = "invalid: " + err.Error()
			continue
		}
		schedules[i].Description = cronexpr.Describe(schedules[i].Expression)
		if next, ok := expr.NextRun(now, cronexpr.DefaultScanDays); ok {
			schedules[i].NextRun = &next
		}
	}
	return schedules
}

// nextRuns merges every schedule of a job into one ordered list of upcoming
// firings, which is what the operator actually wants to see: not "when does
// each expression fire" but "when does this job run next".
func (s *JobService) nextRuns(schedules []domain.JobSchedule, n int) []time.Time {
	now := time.Now().In(s.location)
	var all []time.Time
	for _, sc := range schedules {
		if !sc.Active {
			continue
		}
		expr, err := cronexpr.Parse(sc.Expression)
		if err != nil {
			continue
		}
		all = append(all, expr.NextRuns(now, n)...)
	}
	sortTimes(all)
	return dedupeTimes(all, n)
}

// NextRunFor is the same calculation for a list row, where only the first
// firing is shown.
func (s *JobService) NextRunFor(expressions []string) *time.Time {
	now := time.Now().In(s.location)
	var best *time.Time
	for _, e := range expressions {
		expr, err := cronexpr.Parse(e)
		if err != nil {
			continue
		}
		next, ok := expr.NextRun(now, cronexpr.DefaultScanDays)
		if !ok {
			continue
		}
		if best == nil || next.Before(*best) {
			n := next
			best = &n
		}
	}
	return best
}

// List returns the job list with the next run filled in per row.
func (s *JobService) List(ctx context.Context, f domain.JobFilter, offset, limit int) ([]domain.JobRow, int64, error) {
	// The scope rides on the filter, so there is nothing to forget here: a
	// caller that built the filter without one gets the zero value, which
	// reaches no project.
	rows, total, err := s.jobs.List(ctx, f, offset, limit)
	if err != nil {
		return nil, 0, err
	}
	for i := range rows {
		rows[i].NextRun = s.NextRunFor(rows[i].Schedules)
	}
	return rows, total, nil
}

// SchedulePreview is what the form shows while an expression is being typed.
type SchedulePreview struct {
	Expression  string      `json:"expression"`
	Valid       bool        `json:"valid"`
	Error       string      `json:"error,omitempty"`
	Description string      `json:"description,omitempty"`
	NextRuns    []time.Time `json:"next_runs,omitempty"`
}

// PreviewSchedule validates one expression and shows what it would do.
//
// This is the cheapest correction in the system. A wrong expression saved
// without this is discovered by the job not running, days later, by somebody
// looking for a different problem.
func (s *JobService) PreviewSchedule(expression string) SchedulePreview {
	expression = strings.Join(strings.Fields(expression), " ")
	out := SchedulePreview{Expression: expression}

	expr, err := cronexpr.Parse(expression)
	if err != nil {
		out.Error = err.Error()
		return out
	}
	out.Valid = true
	out.Description = cronexpr.Describe(expression)
	out.NextRuns = expr.NextRuns(time.Now().In(s.location), 5)
	return out
}

// --- schedules ---

// AddSchedule appends one expression to a job.
func (s *JobService) AddSchedule(ctx context.Context, scope domain.ProjectScope, jobID int64, expression string) (*domain.JobSchedule, error) {
	if _, err := s.owned(ctx, scope, jobID); err != nil {
		return nil, err
	}
	expression = strings.Join(strings.Fields(expression), " ")
	if err := cronexpr.Validate(expression); err != nil {
		return nil, (&ValidationError{}).Add("expression", err.Error())
	}

	schedule := &domain.JobSchedule{JobID: jobID, Expression: expression, Active: true}
	id, err := s.jobs.CreateSchedule(ctx, schedule)
	if errors.Is(err, repository.ErrDuplicate) {
		return nil, (&ValidationError{}).Add("expression", "this job already has that schedule")
	}
	if err != nil {
		return nil, err
	}
	schedule.ID = id
	annotated := s.annotate([]domain.JobSchedule{*schedule})
	return &annotated[0], nil
}

// RemoveSchedule deletes one expression.
//
// Removing the last schedule switches the job off, but ONLY when nothing else
// can start it. A job that is the target of a chain link still runs, and
// deactivating it there would quietly break the chain that depends on it.
func (s *JobService) RemoveSchedule(ctx context.Context, scope domain.ProjectScope, jobID, scheduleID int64) error {
	if _, err := s.owned(ctx, scope, jobID); err != nil {
		return err
	}
	if err := s.jobs.DeleteSchedule(ctx, scheduleID, jobID); err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return ErrNotFound
		}
		return err
	}

	count, err := s.jobs.CountSchedules(ctx, jobID)
	if err != nil {
		return err
	}
	if count > 0 {
		return nil
	}

	triggers, err := s.jobs.ListTriggers(ctx, jobID)
	if err != nil {
		return err
	}
	if len(triggers) > 0 {
		return nil
	}
	return s.jobs.SetActive(ctx, jobID, false)
}

// --- chain ---

// AddLink creates a chain edge, refusing self links and loops.
//
// A link MAY cross projects: a job finishing in one brand triggering work in
// another is a real arrangement and the reason chains exist at all. What it may
// not do is cross out of the caller's reach, so the scope is checked on BOTH
// ends.
//
// Both, not just the source, because a link is a change to the target as much
// as to the source: the target starts running because of something the source
// did. Checking only the source would be a way to trigger any job in the system
// from one you happen to own.
func (s *JobService) AddLink(ctx context.Context, scope domain.ProjectScope, jobID, targetID int64, condition string, delaySec int) error {
	if jobID == targetID {
		return ErrSelfLink
	}
	if _, err := s.owned(ctx, scope, jobID); err != nil {
		return err
	}
	if _, err := s.owned(ctx, scope, targetID); err != nil {
		// ErrNotFound, from owned. The caller learns the target is unavailable,
		// not whether it exists in a project they cannot see.
		return err
	}
	switch condition {
	case "success", "failure", "always":
	default:
		return (&ValidationError{}).Add("condition", "must be success, failure or always")
	}
	if delaySec < 0 || delaySec > domain.ChainMaxDelaySec {
		return (&ValidationError{}).Add("delay_sec",
			fmt.Sprintf("must be between 0 and %d seconds", domain.ChainMaxDelaySec))
	}

	// Walking from the target back to this job: if the target can already
	// reach us, adding this edge closes a cycle. The runner's depth cap would
	// stop the loop anyway, but it stops it silently and days later.
	loops, err := s.jobs.ReachesJob(ctx, targetID, jobID, domain.ChainMaxDepth)
	if err != nil {
		return err
	}
	if loops {
		return ErrChainLoop
	}

	_, err = s.jobs.CreateLink(ctx, &domain.JobLink{
		JobID: jobID, TargetJobID: targetID, Condition: condition, DelaySec: delaySec, Active: true,
	})
	if errors.Is(err, repository.ErrDuplicate) {
		return (&ValidationError{}).Add("target", "this link already exists")
	}
	return err
}

// RemoveLink deletes a chain edge.
func (s *JobService) RemoveLink(ctx context.Context, scope domain.ProjectScope, jobID, linkID int64) error {
	if _, err := s.owned(ctx, scope, jobID); err != nil {
		return err
	}
	err := s.jobs.DeleteLink(ctx, linkID, jobID)
	if errors.Is(err, repository.ErrNotFound) {
		return ErrNotFound
	}
	return err
}

// GetByCode reads a job by its machine name inside a project.
//
// The project id is part of the lookup rather than a check afterwards, which
// is what makes the API's project scoping structural: a caller holding one
// project's key cannot address another project's job at all, because there is
// no query that would find it.
func (s *JobService) GetByCode(ctx context.Context, projectID int64, code string) (*domain.Job, error) {
	job, err := s.jobs.GetByCode(ctx, projectID, code)
	if errors.Is(err, repository.ErrNotFound) {
		return nil, ErrNotFound
	}
	return job, err
}

// ListOptions feeds the chain picker, over the caller's WRITE scope.
func (s *JobService) ListOptions(ctx context.Context, scope domain.ProjectScope, excludeID int64) ([]domain.JobOption, error) {
	return s.jobs.ListOptions(ctx, scope, excludeID)
}

// ListTags feeds the filter dropdown.
func (s *JobService) ListTags(ctx context.Context, scope domain.ProjectScope) ([]string, error) {
	return s.jobs.ListTags(ctx, scope)
}

// Clone copies a job under a new code. Most new jobs are a variation of an
// existing one, and retyping eleven fields to change one is how the wrong
// value gets carried over.
func (s *JobService) Clone(ctx context.Context, scope domain.ProjectScope, id int64, newCode string) (int64, error) {
	detail, err := s.Get(ctx, scope, id)
	if err != nil {
		return 0, err
	}

	in := &JobInput{
		ProjectID:      detail.Job.ProjectID,
		Code:           newCode,
		Name:           detail.Job.Name + " (copy)",
		Description:    detail.Job.Description,
		Tag:            detail.Job.Tag,
		Method:         detail.Job.Method,
		URL:            detail.Job.URL,
		Body:           detail.Job.Body,
		TimeoutSec:     detail.Job.TimeoutSec,
		MaxDurationSec: detail.Job.MaxDurationSec,
		Retries:        detail.Job.Retries,
		SingleRun:      detail.Job.SingleRun,
		RunMissed:      detail.Job.RunMissed,
		MaxDelayMin:    detail.Job.MaxDelayMin,
		Priority:       detail.Job.Priority,
		SuccessMin:     detail.Job.SuccessMin,
		SuccessMax:     detail.Job.SuccessMax,
		NotificationID: detail.Job.NotificationID,
		// A clone is created switched off. Copying a job and having it start
		// firing on the same schedule as the original, immediately, is never
		// what was meant.
		Active: false,
	}
	for _, sc := range detail.Schedules {
		in.Schedules = append(in.Schedules, sc.Expression)
	}
	for _, h := range detail.Headers {
		in.Headers = append(in.Headers, HeaderIn{Key: h.Key, Value: h.Value, IsSecret: h.IsSecret})
	}
	return s.Create(ctx, scope, in)
}

func sortTimes(times []time.Time) {
	for i := 1; i < len(times); i++ {
		for j := i; j > 0 && times[j].Before(times[j-1]); j-- {
			times[j], times[j-1] = times[j-1], times[j]
		}
	}
}

func dedupeTimes(times []time.Time, n int) []time.Time {
	out := make([]time.Time, 0, n)
	for _, t := range times {
		if len(out) > 0 && out[len(out)-1].Equal(t) {
			continue
		}
		out = append(out, t)
		if len(out) == n {
			break
		}
	}
	return out
}
