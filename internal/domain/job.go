package domain

import (
	"context"
	"time"
)

// Job is one scheduled job: the request to fire, and how it behaves when it
// overlaps itself or when the dispatcher was late.
//
// It carries no cron expression. Expressions live in JobSchedule because a
// real job regularly needs several that cannot be folded into one.
type Job struct {
	ID          int64  `json:"id"`
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

	NotificationID *int64 `json:"notification_id,omitempty"`

	Active         bool       `json:"active"`
	LastRunAt      *time.Time `json:"last_run_at,omitempty"`
	LastStatus     string     `json:"last_status"`
	LastDurationMs *int       `json:"last_duration_ms,omitempty"`

	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt *time.Time `json:"updated_at,omitempty"`
}

// Timeout is the per attempt client timeout, clamped to the range the schema
// enforces so a row edited outside the application cannot produce a client
// that waits forever.
func (j Job) Timeout() time.Duration {
	sec := j.TimeoutSec
	if sec < MinTimeoutSec {
		sec = DefaultTimeoutSec
	}
	if sec > MaxTimeoutSec {
		sec = MaxTimeoutSec
	}
	return time.Duration(sec) * time.Second
}

// SuccessfulStatus reports whether an HTTP status counts as success for this
// job. The range is per job because a target that answers 202 or a redirect is
// normal for some jobs and a fault for others.
func (j Job) SuccessfulStatus(code int) bool {
	return code >= j.SuccessMin && code <= j.SuccessMax
}

// JobRow is a job plus what the list screen derives from it. Separate from Job
// so read only aggregates never reach an update statement.
type JobRow struct {
	Job
	ProjectName string     `json:"project_name"`
	ProjectSlug string     `json:"project_slug"`
	Schedules   []string   `json:"schedules"`
	NextRun     *time.Time `json:"next_run,omitempty"`

	DaySuccess int `json:"day_success"`
	// DayFailed counts failed and timeout together, because the summary band
	// is fed from it and the two numbers have to keep adding up.
	DayFailed int `json:"day_failed"`
	// DayTimeout is the timeout part OF DayFailed, not a bucket beside it. It
	// is reported separately because the two need opposite fixes: a timeout
	// means timeout_sec is short for the service being called and the operator
	// can raise it from this screen, while a failure means the target itself
	// broke and raising anything changes nothing.
	DayTimeout int `json:"day_timeout"`
	// LinkCount is how many jobs this one triggers.
	LinkCount int `json:"link_count"`
	// TriggerCount is how many jobs trigger THIS one, and it is what lets the
	// list tell a chain-only job apart from one that will never run at all.
	// Both have no schedule; only one of them is a mistake.
	TriggerCount int `json:"trigger_count"`
}

// NeverRuns reports a job that is switched on and has no way to start: no
// schedule, and nothing chained into it. It can still be triggered by hand or
// through the API, so it is a warning rather than an error, but on a list of
// scheduled work it is almost always an oversight.
func (r JobRow) NeverRuns() bool {
	return r.Active && len(r.Schedules) == 0 && r.TriggerCount == 0
}

// ChainOnly reports a job with no schedule that another job triggers. A normal
// and deliberate arrangement, shown so it is not mistaken for the above.
func (r JobRow) ChainOnly() bool {
	return len(r.Schedules) == 0 && r.TriggerCount > 0
}

// JobHeader is a request header sent with the job.
type JobHeader struct {
	ID       int64  `json:"id"`
	JobID    int64  `json:"job_id"`
	Key      string `json:"key"`
	Value    string `json:"value"`
	IsSecret bool   `json:"is_secret"`
}

// JobSchedule is one cron expression belonging to a job.
type JobSchedule struct {
	ID         int64     `json:"id"`
	JobID      int64     `json:"job_id"`
	Expression string    `json:"expression"`
	Active     bool      `json:"active"`
	CreatedAt  time.Time `json:"created_at"`
	// NextRun is computed by the service, not stored. A mistyped expression is
	// only ever caught by reading this line: "0 16 * * 7" is valid and, meant
	// as Sunday, shows its next run on a Sunday only because the reader can
	// see it before saving rather than six days later.
	NextRun *time.Time `json:"next_run,omitempty"`
	// Description is a short human reading of the expression, for the same
	// reason.
	Description string `json:"description,omitempty"`
}

// JobLink is a chain edge: when a job finishes with the given outcome, the
// target runs without waiting for its own clock.
type JobLink struct {
	ID          int64     `json:"id"`
	JobID       int64     `json:"job_id"`
	TargetJobID int64     `json:"target_job_id"`
	Condition   string    `json:"condition"`
	DelaySec    int       `json:"delay_sec"`
	Active      bool      `json:"active"`
	CreatedAt   time.Time `json:"created_at"`
}

// JobLinkRow is a chain edge plus the identity of the job on the other end.
//
// The list has to show a name rather than an id, and it has to say when the
// target is inactive: a link into an inactive job silently does nothing, which
// is the most expensive mistake available when building a chain.
type JobLinkRow struct {
	JobLink
	TargetCode   string `json:"target_code"`
	TargetName   string `json:"target_name"`
	TargetActive bool   `json:"target_active"`
	SourceCode   string `json:"source_code,omitempty"`
	SourceName   string `json:"source_name,omitempty"`
}

// JobOption is one row of the job picker used when building a chain. Not the
// full JobRow: the picker shows four fields and JobRow costs two subqueries
// per row.
type JobOption struct {
	ID          int64  `json:"id"`
	Code        string `json:"code"`
	Name        string `json:"name"`
	ProjectSlug string `json:"project_slug"`
	Active      bool   `json:"active"`
}

// JobFilter narrows the job list. A nil field is not applied.
//
// Scope is different: it is NOT optional and NOT nil-able. The zero value
// reaches nothing, so a query built without resolving the caller's scope
// returns an empty list rather than everything. That is the whole reason it
// sits on the filter instead of being an extra argument somebody can forget.
type JobFilter struct {
	Scope     ProjectScope
	ProjectID *int64
	Tag       *string
	Active    *bool
	Search    *string
}

// JobRepository is data access for job definitions.
type JobRepository interface {
	Get(ctx context.Context, id int64) (*Job, error)
	GetByCode(ctx context.Context, projectID int64, code string) (*Job, error)
	List(ctx context.Context, f JobFilter, offset, limit int) ([]JobRow, int64, error)
	ListOptions(ctx context.Context, scope ProjectScope, excludeID int64) ([]JobOption, error)
	ListTags(ctx context.Context, scope ProjectScope) ([]string, error)
	Create(ctx context.Context, j *Job) (int64, error)
	Update(ctx context.Context, j *Job) error
	SetActive(ctx context.Context, id int64, active bool) error
	SoftDelete(ctx context.Context, id int64, at time.Time) error

	ListHeaders(ctx context.Context, jobID int64) ([]JobHeader, error)
	ReplaceHeaders(ctx context.Context, jobID int64, headers []JobHeader) error

	ListSchedules(ctx context.Context, jobID int64) ([]JobSchedule, error)
	CreateSchedule(ctx context.Context, s *JobSchedule) (int64, error)
	DeleteSchedule(ctx context.Context, id, jobID int64) error
	CountSchedules(ctx context.Context, jobID int64) (int, error)
	ReplaceSchedules(ctx context.Context, jobID int64, expressions []string) error

	// ListLinks returns the chain edges leaving this job.
	ListLinks(ctx context.Context, jobID int64) ([]JobLinkRow, error)
	// ListTriggers is the chain in reverse: the jobs that trigger this one.
	// It cannot be edited from here, because a link is a record of its source
	// job. It is the only visible answer to "why did this run".
	ListTriggers(ctx context.Context, jobID int64) ([]JobLinkRow, error)
	CreateLink(ctx context.Context, l *JobLink) (int64, error)
	DeleteLink(ctx context.Context, id, jobID int64) error
}
