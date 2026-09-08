package domain

import (
	"context"
	"time"
)

// Summary is the dashboard's top band. Not one query: the pulse is a single
// row, the job counts come from jobs, the run counts from job_runs.
type Summary struct {
	// LastRun is the dispatcher's last pulse. nil means it has never run.
	LastRun *time.Time `json:"last_run,omitempty"`
	// ElapsedSec is how long ago that was; nil when LastRun is nil.
	ElapsedSec *int   `json:"elapsed_sec,omitempty"`
	DriftSec   int    `json:"drift_sec"`
	InstanceID string `json:"instance_id"`

	ProjectTotal int `json:"project_total"`
	JobTotal     int `json:"job_total"`
	JobActive    int `json:"job_active"`

	DayTotal   int `json:"day_total"`
	DaySuccess int `json:"day_success"`
	DayFailed  int `json:"day_failed"`
	DayTimeout int `json:"day_timeout"`
	DaySkipped int `json:"day_skipped"`
	Running    int `json:"running"`
	Pending    int `json:"pending"`
	// AvgDurationMs over successful runs in the last 24 hours.
	AvgDurationMs int `json:"avg_duration_ms"`
}

// Health classifies the dispatcher pulse for the dashboard badge. The
// thresholds are generous on purpose: the tick is once a minute, so a single
// slow tick must not read as an outage.
func (s Summary) Health() string {
	if s.ElapsedSec == nil {
		return "unknown"
	}
	switch {
	case *s.ElapsedSec <= 120:
		return "healthy"
	case *s.ElapsedSec <= 600:
		return "late"
	default:
		return "down"
	}
}

// HourBucket is one hour of the activity chart.
type HourBucket struct {
	Hour    time.Time `json:"hour"`
	Success int       `json:"success"`
	Failed  int       `json:"failed"`
	Timeout int       `json:"timeout"`
	Skipped int       `json:"skipped"`
}

// StatsRepository feeds the dashboard.
//
// Every method takes a scope. The dashboard is where a missing filter hides
// best, because it reports numbers rather than rows.
type StatsRepository interface {
	Summary(ctx context.Context, scope ProjectScope) (*Summary, error)
	// Activity returns per hour counts over the window, oldest first, with
	// empty hours filled in so the chart has no gaps.
	Activity(ctx context.Context, scope ProjectScope, hours int) ([]HourBucket, error)
	// SlowestJobs ranks jobs by average duration over the window. It answers
	// the question the operator actually has when a minute is congested.
	SlowestJobs(ctx context.Context, scope ProjectScope, hours, limit int) ([]JobDuration, error)
	// JobHistory returns one job's recent runs, oldest first, for its trend.
	JobHistory(ctx context.Context, scope ProjectScope, jobID int64, limit int) ([]RunPoint, error)
}

// RunPoint is one run reduced to what a trend line needs.
//
// Deliberately not a RunRow: plotting fifty runs must not drag fifty response
// bodies out of the database, and a chart that carries run output is a chart
// that leaks it to whoever may see the shape but not the content.
type RunPoint struct {
	RunID      int64     `json:"run_id"`
	At         time.Time `json:"at"`
	Status     string    `json:"status"`
	DurationMs int       `json:"duration_ms"`
	HTTPStatus int       `json:"http_status,omitempty"`
}

// JobDuration is one row of the slowest jobs list.
type JobDuration struct {
	JobID       int64  `json:"job_id"`
	Code        string `json:"code"`
	Name        string `json:"name"`
	ProjectSlug string `json:"project_slug"`
	Runs        int    `json:"runs"`
	AvgMs       int    `json:"avg_ms"`
	MaxMs       int    `json:"max_ms"`
}
