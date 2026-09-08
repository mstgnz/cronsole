package domain

import (
	"context"
	"time"
)

// This file holds what the EXECUTION path needs. The management interfaces in
// job.go and run.go stay separate on purpose: the dispatcher has no business
// depending on a twenty method surface, and if it did, every method added for
// a screen would grow the fake repository in the dispatcher's tests.

const (
	// DispatcherSpec fires the minute tick. Six fields: the leading 5 is
	// SECONDS, so the tick runs at the fifth second of every minute.
	//
	// THE SECONDS FIELD MUST NOT BE ZERO, and that is the only reason this is
	// a named constant. The tick is fired by the application clock while the
	// minute it processes is read from the database. Let the two clocks differ
	// by a few hundred milliseconds and a tick on the exact boundary falls on
	// the wrong side of it:
	//
	//	app 10:11:00.000 -> database 10:10:59.7 -> minute processed: 10:10
	//
	// 10:10 was already queued on the previous tick, so nothing is written,
	// and 10:11 is never processed at all. Jobs without run_missed lose that
	// minute for the day.
	//
	// Sampling at the fifth second makes it impossible for ordinary clock
	// skew to land on the boundary; a five second drift either way still falls
	// inside the same minute. Do not move it back to zero.
	DispatcherSpec = "5 * * * * *"

	// ChainMaxDepth bounds A -> B -> C -> D -> E.
	ChainMaxDepth = 5

	// ChainDirectMaxSec is the longest chain delay the runner hands over
	// itself. Anything longer is left pending for the dispatcher to pick up
	// when run_after arrives, because holding a timer in process for minutes
	// buys nothing and is lost on restart.
	ChainDirectMaxSec = 120

	// ChainMaxDelaySec caps a chain delay.
	ChainMaxDelaySec = 3600

	// Per job timeout limits. Mirrored by a CHECK constraint in cronsole.sql.
	MinTimeoutSec     = 1
	MaxTimeoutSec     = 600
	DefaultTimeoutSec = 30

	// OutputMaxLen is how much of a response body is kept on the run row.
	// Diagnostics, not an archive.
	OutputMaxLen = 16000

	// ErrorMaxLen leaves room for a note to be appended afterwards.
	ErrorMaxLen = 2000
)

// ScheduledJob is what the per minute scan needs. Not the whole Job: the scan
// reads every active job every minute and carrying unused columns is waste.
type ScheduledJob struct {
	ID          int64
	Code        string
	Priority    int
	RunMissed   bool
	MaxDelayMin int
}

// PendingRun is a queued row waiting to be handed to the runner.
type PendingRun struct {
	ID        int64
	JobID     int64
	Code      string
	SingleRun bool
}

// RunTarget is everything needed to execute one run: the run row joined with
// its job and the project's base address.
type RunTarget struct {
	ID      int64
	JobID   int64
	Status  string
	Attempt int

	Code        string
	Name        string
	ProjectID   int64
	ProjectSlug string
	BaseURL     string

	Method  string
	URL     string
	Body    string
	Headers []JobHeader

	TimeoutSec int
	Retries    int
	SingleRun  bool
	SuccessMin int
	SuccessMax int

	NotificationID *int64
}

// Timeout is the per attempt client timeout, clamped the same way Job does it.
func (t RunTarget) Timeout() time.Duration {
	sec := t.TimeoutSec
	if sec < MinTimeoutSec {
		sec = DefaultTimeoutSec
	}
	if sec > MaxTimeoutSec {
		sec = MaxTimeoutSec
	}
	return time.Duration(sec) * time.Second
}

// SuccessfulStatus reports whether an HTTP status counts as success.
func (t RunTarget) SuccessfulStatus(code int) bool {
	return code >= t.SuccessMin && code <= t.SuccessMax
}

// ChainTarget is a job to run once the source job has finished.
type ChainTarget struct {
	JobID     int64
	Code      string
	Active    bool
	SingleRun bool
	DelaySec  int
	Condition string
}

// Delay clamps a chain delay into the allowed range.
func (c ChainTarget) Delay() time.Duration {
	sec := c.DelaySec
	if sec < 0 {
		sec = 0
	}
	if sec > ChainMaxDelaySec {
		sec = ChainMaxDelaySec
	}
	return time.Duration(sec) * time.Second
}

// Heartbeat is the dispatcher's pulse. If nothing else in the system is
// broken, a stale pulse is the only evidence that the dispatcher itself died.
type Heartbeat struct {
	LastRun    time.Time
	AppTime    *time.Time
	DriftSec   int
	InstanceID string
	Note       string
}

// Clock is the time authority.
//
// The minute comes from the DATABASE, never from the application clock.
// Replicas run on separate machines and every one of them has to produce the
// same minute value, otherwise UNIQUE (job_id, planned_minute) does not line
// up and the same job runs twice in the same minute.
type Clock interface {
	DBTime(ctx context.Context) (time.Time, error)
}

// DispatchRepository is what the per minute scan needs.
type DispatchRepository interface {
	Clock

	// ReadHeartbeat returns the pulse row, or (nil, nil) when there is none.
	ReadHeartbeat(ctx context.Context) (*Heartbeat, error)
	// TouchHeartbeat updates the pulse. driftSec is app clock minus DB clock.
	TouchHeartbeat(ctx context.Context, dbTime, appTime time.Time, driftSec int, instanceID string) error

	// ListScheduledJobs returns active jobs in priority order.
	ListScheduledJobs(ctx context.Context) ([]ScheduledJob, error)
	// ListActiveSchedules returns active expressions grouped by job id.
	ListActiveSchedules(ctx context.Context) (map[int64][]string, error)

	// QueueRun opens a pending row for a job and minute. A second call for the
	// same pair does nothing: the unique index is what makes it idempotent, so
	// two dispatchers reaching the same minute is harmless.
	QueueRun(ctx context.Context, jobID int64, plannedMinute time.Time) error

	// CountRunning is the total number of runs currently in flight.
	CountRunning(ctx context.Context) (int, error)
	// RunningJobIDs is the set of jobs with a run in flight.
	RunningJobIDs(ctx context.Context) (map[int64]bool, error)
	// ListPending returns queued rows whose run_after has arrived, in
	// priority order.
	ListPending(ctx context.Context, now time.Time, limit int) ([]PendingRun, error)
	// SkipRun closes a row as skipped. It only affects a pending row, so it
	// can never overwrite a result.
	SkipRun(ctx context.Context, runID int64, reason string) error
}

// RunRepositoryExec is what executing one run needs.
type RunRepositoryExec interface {
	// GetRun reads the run together with its job definition.
	GetRun(ctx context.Context, runID int64) (*RunTarget, error)
	// CountRunningForJob counts a job's in flight runs. excludeRunID is left
	// out of the count, which is how a run excludes itself.
	CountRunningForJob(ctx context.Context, jobID, excludeRunID int64) (int, error)
	// ClaimRun takes the row atomically: UPDATE ... WHERE status = 'pending'.
	// If the affected row count is not 1 another process already has it and
	// this call does nothing. This is what makes a run impossible to execute
	// twice, and it is the only mechanism that does.
	ClaimRun(ctx context.Context, runID int64, instanceID string, at time.Time) (bool, error)
	// FinishRun writes the outcome.
	FinishRun(ctx context.Context, runID int64, result RunResult) error
	// WriteJobSummary refreshes the denormalised columns on jobs. Read only
	// convenience for the list screen; the run row is the source of truth.
	WriteJobSummary(ctx context.Context, jobID int64, status string, at time.Time, durationMs int) error
	// AppendRunNote adds to the error text WITHOUT overwriting it.
	AppendRunNote(ctx context.Context, runID int64, message string) error
	// SkipRun closes a pending row as skipped. The runner needs it too: a
	// chained run never passes through the dispatcher, so the single_run
	// check has to exist on this side as well.
	SkipRun(ctx context.Context, runID int64, reason string) error
	CountRunning(ctx context.Context) (int, error)

	// ListChainTargets returns the active links leaving this job that match
	// the outcome.
	ListChainTargets(ctx context.Context, jobID int64, outcome string) ([]ChainTarget, error)
	// ChainStepCount reports how deep in a chain this run sits.
	ChainStepCount(ctx context.Context, runID int64) (int, error)
	// CreateChainRun opens a pending row for a chain step.
	CreateChainRun(ctx context.Context, jobID int64, delaySec int, parentRunID int64) (int64, error)
	// CreateChainSkipped records a chain step that was not run. A step that
	// vanishes without a trace is far harder to notice than one that failed.
	CreateChainSkipped(ctx context.Context, jobID int64, parentRunID int64, reason string) error
}

// NotifyTarget is who to tell about a finished run.
type NotifyTarget struct {
	OnSuccess bool
	OnFailure bool
	Emails    []string
}

// NotifyRepository reads the recipient list attached to a job.
type NotifyRepository interface {
	TargetForJob(ctx context.Context, notificationID int64) (*NotifyTarget, error)
}

// ErrorSummary is a per job failure count produced by the watchdog.
type ErrorSummary struct {
	Code      string
	Name      string
	Count     int
	LastError string
}

// WatchdogRepository is what the sweep needs.
type WatchdogRepository interface {
	Clock

	// CloseStuckRuns closes runs that have been running past their job's
	// max_duration_sec. Leaving them open is not cosmetic: a job marked
	// single_run would never run again, because every tick would decide the
	// previous execution is still going.
	CloseStuckRuns(ctx context.Context) (int64, error)
	// CountStalePending counts rows that have been pending too long, meaning
	// nothing ever handed them over.
	CountStalePending(ctx context.Context, threshold time.Duration) (int, error)
	ReadHeartbeat(ctx context.Context) (*Heartbeat, error)
	// ListFailingJobs returns jobs at or over the failure threshold in the
	// window.
	ListFailingJobs(ctx context.Context, window time.Duration, threshold int) ([]ErrorSummary, error)
	// PurgeRuns deletes old run rows, bounded per call so the delete never
	// holds a long lock.
	PurgeRuns(ctx context.Context, days, limit int) (int64, error)
	// PurgeAppLogs deletes old application log rows.
	PurgeAppLogs(ctx context.Context, days int) (int64, error)
	// SetWarningMarker stores the repeat suppression fingerprint. Empty
	// clears it. It lives in heartbeat.note and is read back through
	// ReadHeartbeat, so there is no separate getter.
	SetWarningMarker(ctx context.Context, marker string) error
}
