package service

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/mstgnz/cronsole/v2/internal/domain"
	"github.com/mstgnz/cronsole/v2/internal/repository"
)

// SyncService registers a project's jobs from a single declaration.
//
// This is the point of the whole service being central. Without it, adding a
// scheduled job to a project means somebody opening this interface and filling
// in a form, which means the schedule lives in one place and the code that
// answers it lives in another, and the two drift. With it, a project ships its
// cron definitions next to the code they belong to and posts them on deploy.
//
// The operation is DECLARATIVE and idempotent: the caller sends the state it
// wants and the same payload posted twice changes nothing the second time.
type SyncService struct {
	jobs     JobStore
	projects domain.ProjectRepository
	service  *JobService
}

// NewSyncService wires the service.
func NewSyncService(jobs JobStore, projects domain.ProjectRepository, jobService *JobService) *SyncService {
	return &SyncService{jobs: jobs, projects: projects, service: jobService}
}

// SyncRequest is a project's whole declaration.
type SyncRequest struct {
	// Prune deactivates jobs that exist here but are absent from the payload.
	//
	// It DEACTIVATES rather than deletes, and that is deliberate: a deployment
	// that posts a partial list by accident should cost a switch being flipped
	// back, not a job definition and its history.
	Prune bool      `json:"prune"`
	Jobs  []SyncJob `json:"jobs"`
}

// SyncJob is one declared job.
type SyncJob struct {
	Code        string `json:"code"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Tag         string `json:"tag"`

	URL    string `json:"url"`
	Method string `json:"method"`
	Body   string `json:"body"`

	Schedules []string          `json:"schedules"`
	Headers   map[string]string `json:"headers"`

	TimeoutSec     int   `json:"timeout_sec"`
	MaxDurationSec int   `json:"max_duration_sec"`
	Retries        int   `json:"retries"`
	SingleRun      *bool `json:"single_run"`
	RunMissed      *bool `json:"run_missed"`
	MaxDelayMin    int   `json:"max_delay_min"`
	Priority       int   `json:"priority"`
	SuccessMin     int   `json:"success_min"`
	SuccessMax     int   `json:"success_max"`
	Active         *bool `json:"active"`

	// Links are expressed by target CODE, not id. Ids belong to this service;
	// codes are what the declaring project actually knows about itself.
	Links []ChainIn `json:"links"`
}

// SyncOutcome is what happened to one declared job.
type SyncOutcome struct {
	Code   string   `json:"code"`
	Action string   `json:"action"` // created, updated, deactivated, failed
	JobID  int64    `json:"job_id,omitempty"`
	Errors []string `json:"errors,omitempty"`
}

// SyncResult is the whole outcome.
type SyncResult struct {
	Project     string        `json:"project"`
	Created     int           `json:"created"`
	Updated     int           `json:"updated"`
	Deactivated int           `json:"deactivated"`
	Failed      int           `json:"failed"`
	Jobs        []SyncOutcome `json:"jobs"`
}

// Sync applies a declaration to a project.
//
// One job failing does not stop the rest. A deployment that declares twelve
// jobs and gets one expression wrong should register eleven and be told
// precisely which one it got wrong, not be refused wholesale.
func (s *SyncService) Sync(ctx context.Context, project *domain.Project, req SyncRequest) (*SyncResult, error) {
	// Pruning against an empty declaration switches off every job the project
	// has, and answers success. That is indistinguishable from the mistake it
	// usually is: a template that rendered nothing, a file that parsed into no
	// jobs, a variable that was never set. Somebody who genuinely means to stop
	// everything can send the jobs with active false, or use the screen, and
	// either way it is a thing they did on purpose.
	if req.Prune && len(req.Jobs) == 0 {
		return nil, (&ValidationError{}).Add("jobs",
			"cannot prune against an empty declaration; it would deactivate every job in the project")
	}

	result := &SyncResult{Project: project.Slug}
	// The caller is a project API key, so its scope IS that project and
	// nothing else. Building it here rather than accepting one from the handler
	// means no route can widen it.
	scope := domain.ScopeOf(project.ID)
	declared := make(map[string]bool, len(req.Jobs))

	// First pass: the jobs themselves. Links are left to a second pass,
	// because a link may point at a job declared later in the same payload.
	for _, decl := range req.Jobs {
		outcome := s.applyJob(ctx, scope, project, decl)
		if outcome.Code != "" {
			declared[strings.ToLower(outcome.Code)] = true
		}
		switch outcome.Action {
		case "created":
			result.Created++
		case "updated":
			result.Updated++
		case "failed":
			result.Failed++
		}
		result.Jobs = append(result.Jobs, outcome)
	}

	// Second pass: links, now that every code in the payload exists.
	for i, decl := range req.Jobs {
		if len(decl.Links) == 0 || result.Jobs[i].Action == "failed" {
			continue
		}
		if errs := s.applyLinks(ctx, scope, project, result.Jobs[i].JobID, decl.Links); len(errs) > 0 {
			result.Jobs[i].Errors = append(result.Jobs[i].Errors, errs...)
		}
	}

	if req.Prune {
		deactivated, err := s.prune(ctx, scope, project.ID, declared)
		if err != nil {
			return nil, err
		}
		for _, code := range deactivated {
			result.Jobs = append(result.Jobs, SyncOutcome{Code: code, Action: "deactivated"})
		}
		result.Deactivated = len(deactivated)
	}

	sort.Slice(result.Jobs, func(i, j int) bool { return result.Jobs[i].Code < result.Jobs[j].Code })
	return result, nil
}

func (s *SyncService) applyJob(ctx context.Context, scope domain.ProjectScope, project *domain.Project, decl SyncJob) SyncOutcome {
	code := strings.ToLower(strings.TrimSpace(decl.Code))
	outcome := SyncOutcome{Code: code}

	if code == "" {
		outcome.Action = "failed"
		outcome.Errors = []string{"code is required"}
		return outcome
	}

	existing, err := s.jobs.GetByCode(ctx, project.ID, code)
	if err != nil && !errors.Is(err, repository.ErrNotFound) {
		outcome.Action = "failed"
		outcome.Errors = []string{err.Error()}
		return outcome
	}

	in := decl.toInput(project.ID, existing)

	if existing == nil {
		id, err := s.service.Create(ctx, scope, in)
		if err != nil {
			outcome.Action = "failed"
			outcome.Errors = messagesFor(err)
			return outcome
		}
		outcome.Action = "created"
		outcome.JobID = id
		return outcome
	}

	if err := s.service.Update(ctx, scope, existing.ID, in); err != nil {
		outcome.Action = "failed"
		outcome.Errors = messagesFor(err)
		return outcome
	}
	outcome.Action = "updated"
	outcome.JobID = existing.ID
	return outcome
}

// toInput turns a declaration into the shape the job service validates.
//
// The tri-state pointers matter here. A payload that omits single_run means
// "leave it as it is", while false means "turn it off"; collapsing the two
// would let every sync silently reset flags the operator had changed on the
// screen.
func (d SyncJob) toInput(projectID int64, existing *domain.Job) *JobInput {
	in := &JobInput{
		ProjectID:      projectID,
		Code:           d.Code,
		Name:           d.Name,
		Description:    d.Description,
		Tag:            d.Tag,
		Method:         d.Method,
		URL:            d.URL,
		Body:           d.Body,
		TimeoutSec:     d.TimeoutSec,
		MaxDurationSec: d.MaxDurationSec,
		Retries:        d.Retries,
		MaxDelayMin:    d.MaxDelayMin,
		Priority:       d.Priority,
		SuccessMin:     d.SuccessMin,
		SuccessMax:     d.SuccessMax,
		Schedules:      d.Schedules,
	}

	// single_run defaults ON for a declared job. A project registering a job
	// from a deploy script has not thought about overlap, and overlapping is
	// the outcome that corrupts data rather than merely wasting a minute.
	has := existing != nil
	in.SingleRun = resolveBool(d.SingleRun, has, has && existing.SingleRun, true)
	in.RunMissed = resolveBool(d.RunMissed, has, has && existing.RunMissed, false)
	in.Active = resolveBool(d.Active, has, has && existing.Active, len(d.Schedules) > 0)

	if existing != nil {
		in.NotificationID = existing.NotificationID
		if d.Name == "" {
			in.Name = existing.Name
		}
		// An omitted field keeps the stored value rather than snapping back to
		// a default, so tuning a timeout on the screen survives the next
		// deploy.
		if d.TimeoutSec == 0 {
			in.TimeoutSec = existing.TimeoutSec
		}
		if d.MaxDurationSec == 0 {
			in.MaxDurationSec = existing.MaxDurationSec
		}
		if d.Priority == 0 {
			in.Priority = existing.Priority
		}
		if d.Tag == "" {
			in.Tag = existing.Tag
		}
	}

	if d.Headers != nil {
		keys := make([]string, 0, len(d.Headers))
		for k := range d.Headers {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			in.Headers = append(in.Headers, HeaderIn{Key: k, Value: d.Headers[k]})
		}
		if in.Headers == nil {
			// An empty map is an instruction to clear the headers, so it has
			// to reach the repository as an empty slice rather than nil.
			in.Headers = []HeaderIn{}
		}
	}
	if d.Schedules == nil && existing != nil {
		// nil means "not declared". Leaving it nil keeps the stored schedules;
		// an empty array would clear them.
		in.Schedules = nil
	}
	return in
}

// resolveBool answers a tri-state flag: the value the payload stated, else the
// value already stored, else the default for a brand new job.
//
// The middle case is the one worth having. Without it, every sync would reset
// flags an operator had changed on the screen back to whatever the deploy
// script happens not to mention.
func resolveBool(explicit *bool, hasExisting, stored, fallback bool) bool {
	if explicit != nil {
		return *explicit
	}
	if hasExisting {
		return stored
	}
	return fallback
}

func (s *SyncService) applyLinks(ctx context.Context, scope domain.ProjectScope, project *domain.Project, jobID int64, links []ChainIn) []string {
	var errs []string

	existing, err := s.jobs.ListLinks(ctx, jobID)
	if err != nil {
		return []string{err.Error()}
	}
	have := make(map[string]bool, len(existing))
	for _, l := range existing {
		have[strings.ToLower(l.TargetCode)+"/"+l.Condition] = true
	}

	for _, link := range links {
		code := strings.ToLower(strings.TrimSpace(link.TargetCode))
		if code == "" {
			errs = append(errs, "link target is empty")
			continue
		}
		condition := link.Condition
		if condition == "" {
			condition = "success"
		}
		if have[code+"/"+condition] {
			continue
		}

		target, err := s.jobs.GetByCode(ctx, project.ID, code)
		if errors.Is(err, repository.ErrNotFound) {
			errs = append(errs, fmt.Sprintf("link target %q does not exist in this project", code))
			continue
		}
		if err != nil {
			errs = append(errs, err.Error())
			continue
		}

		delay := link.DelaySec
		if delay == 0 {
			delay = 15
		}
		if err := s.service.AddLink(ctx, scope, jobID, target.ID, condition, delay); err != nil {
			errs = append(errs, fmt.Sprintf("link to %q: %s", code, err.Error()))
		}
	}
	return errs
}

// prune deactivates jobs that exist but were not declared.
//
// The scope is not redundant beside ProjectID: an unresolved scope matches
// nothing, so a list built without it silently prunes nothing at all.
func (s *SyncService) prune(ctx context.Context, scope domain.ProjectScope, projectID int64, declared map[string]bool) ([]string, error) {
	rows, _, err := s.jobs.List(ctx, domain.JobFilter{Scope: scope, ProjectID: &projectID}, 0, 500)
	if err != nil {
		return nil, err
	}

	var deactivated []string
	for _, row := range rows {
		if declared[strings.ToLower(row.Code)] || !row.Active {
			continue
		}
		if err := s.jobs.SetActive(ctx, row.ID, false); err != nil {
			return nil, err
		}
		deactivated = append(deactivated, row.Code)
	}
	return deactivated, nil
}

// messagesFor flattens a validation error into readable lines.
func messagesFor(err error) []string {
	var v *ValidationError
	if errors.As(err, &v) {
		out := make([]string, 0, len(v.Errors))
		for _, fe := range v.Errors {
			out = append(out, fe.Field+": "+fe.Message)
		}
		return out
	}
	return []string{err.Error()}
}
