package domain

import (
	"context"
	"time"
)

// Run status values. They are stored as text and constrained by the schema,
// so these constants and the CHECK in cronsole.sql have to move together.
const (
	StatusPending = "pending"
	StatusRunning = "running"
	StatusSuccess = "success"
	StatusFailed  = "failed"
	StatusTimeout = "timeout"
	StatusSkipped = "skipped"
)

// How a run came to exist.
const (
	TriggerSchedule = "schedule"
	TriggerManual   = "manual"
	TriggerChain    = "chain"
	TriggerAPI      = "api"
)

// Terminal reports whether a status is final.
func Terminal(status string) bool {
	switch status {
	case StatusSuccess, StatusFailed, StatusTimeout, StatusSkipped:
		return true
	}
	return false
}

// Run is one execution record, and the queue entry at the same time. A row is
// created pending, claimed into running, and closed into a terminal state.
type Run struct {
	ID            int64      `json:"id"`
	JobID         int64      `json:"job_id"`
	PlannedMinute *time.Time `json:"planned_minute,omitempty"`
	RunAfter      *time.Time `json:"run_after,omitempty"`
	Trigger       string     `json:"trigger"`
	UserID        *int64     `json:"user_id,omitempty"`
	ParentRunID   *int64     `json:"parent_run_id,omitempty"`

	Status     string `json:"status"`
	Attempt    int    `json:"attempt"`
	InstanceID string `json:"instance_id"`

	StartedAt  *time.Time `json:"started_at,omitempty"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	DurationMs *int       `json:"duration_ms,omitempty"`
	HTTPStatus *int       `json:"http_status,omitempty"`
	RequestURL string     `json:"request_url"`
	Output     string     `json:"output"`
	Error      string     `json:"error"`

	CreatedAt time.Time `json:"created_at"`
}

// RunRow is a run plus the identity of its job. The run list can span every
// job, so the row itself has to say which one it belongs to.
type RunRow struct {
	Run
	JobCode     string `json:"job_code"`
	JobName     string `json:"job_name"`
	ProjectSlug string `json:"project_slug"`
	ProjectName string `json:"project_name"`
}

// RunFilter narrows the run list. A nil field is not applied.
//
// Scope is not optional: see JobFilter for why it lives here rather than
// beside the call.
type RunFilter struct {
	Scope ProjectScope

	JobID     *int64
	ProjectID *int64
	Status    *string
	Trigger   *string
	Start     *time.Time
	End       *time.Time
	Search    *string
}

// RunResult is how an execution ended. HTTPStatus is zero when no response was
// received at all; "no answer" and "the server said 0" are not the same thing,
// so zero is written as NULL.
type RunResult struct {
	Status     string
	DurationMs int
	HTTPStatus int
	RequestURL string
	Output     string
	Error      string
	Attempt    int
}

// RunRepository is data access for execution records, from the management
// side. The execution path uses the narrower interfaces in exec.go.
type RunRepository interface {
	Get(ctx context.Context, id int64) (*RunRow, error)
	List(ctx context.Context, f RunFilter, offset, limit int) ([]RunRow, int64, error)
	Recent(ctx context.Context, jobID int64, n int) ([]Run, error)
	// Children are the runs this one triggered, so a chain can be walked
	// forward from any point.
	Children(ctx context.Context, runID int64) ([]RunRow, error)
	// Enqueue opens a pending row outside the dispatcher: the "run now" button
	// and the API trigger both land here, so a manual run goes through exactly
	// the same execution path as a scheduled one.
	Enqueue(ctx context.Context, jobID int64, trigger string, userID *int64) (int64, error)
}
