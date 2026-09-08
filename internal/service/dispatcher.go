package service

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mstgnz/cronsole/v2/internal/domain"
	"github.com/mstgnz/cronsole/v2/pkg/cronexpr"
)

// Dispatcher turns the minute into queued work.
//
// IT RUNS NOTHING ITSELF. All it does is:
//
//  1. read the minute from the database, the one clock every replica shares
//  2. open a pending row for every job whose expression falls on that minute
//  3. hand pending rows to the runner in priority order, without waiting
//
// Running them itself would mean twelve jobs firing in sequence at 16:15, each
// taking thirty seconds, and the next minute's work sliding behind them. The
// job that must not slide is always the one that would.
//
// Two dispatchers can run side by side. Both protections are in the database
// and neither depends on coordination between processes: the unique index on
// (job_id, planned_minute) gives one row per minute, and the atomic claim in
// the runner gives one execution per row.
type Dispatcher struct {
	repo     domain.DispatchRepository
	dispatch DispatchFunc

	// missScanMin bounds how far back a restarted dispatcher replays.
	missScanMin int
	// maxConcurrent caps runs in flight across the service.
	maxConcurrent int
	// dryRun computes the tick and writes NOTHING: no rows, no dispatch, no
	// heartbeat. For validating a configuration before it goes live.
	dryRun     bool
	instanceID string
	location   *time.Location

	// running keeps ticks from overlapping. A tick that outruns its minute
	// (slow database, wide replay) would otherwise be joined by the next one,
	// and while the rows are safe, the concurrency quota would be computed
	// twice and the cap exceeded.
	running atomic.Bool

	// lastMinute is the minute the previous tick processed. Ticks are
	// serialised by the flag above, so a plain field is enough.
	//
	// Its only job is auditing the chain of minutes. Two ticks landing on the
	// SAME minute means the wall minute between them was never processed; a
	// jump means the skipped minutes never had rows opened. Both happen in
	// silence otherwise, because a repeated minute conflicts on the unique
	// index and does not even raise an error.
	lastMinute time.Time

	// exprCache holds parsed expressions. The same expression is used by
	// several jobs and the tick comes round every minute, so parsing is paid
	// once. The key is the expression text, so an edit produces a new key and
	// no stale entry survives.
	exprCache sync.Map
}

// DispatchFunc hands a run to whatever will execute it. Neither the dispatcher
// nor the runner decides where the work goes; that is the composition root's
// call.
type DispatchFunc func(runID int64, code string)

// Candidate is a job that falls on the minute, reported by a dry run.
type Candidate struct {
	JobID  int64
	Code   string
	Minute time.Time
}

// TickResult summarises one minute.
type TickResult struct {
	Skipped bool // the previous tick was still going
	DryRun  bool // computed, nothing written

	Minute        time.Time
	DriftSec      int
	MissedMinutes int

	// MinuteGap counts wall minutes between this tick and the previous one
	// that were never processed. Above zero means those minutes opened no rows.
	MinuteGap int
	// MinuteRepeat means this tick processed the same minute as the previous
	// one, so the minute between them is lost.
	MinuteRepeat bool

	// Candidates is filled only by a dry run. Carrying hundreds of rows in
	// memory every minute would serve nothing.
	Candidates []Candidate

	// ActiveJobs is how many jobs the tick considered. Reported because a
	// sudden drop is a real signal: somebody deactivated a project, or a
	// deploy pruned more than it meant to.
	ActiveJobs  int
	QueuedRuns  int
	Running     int
	QuotaFull   bool
	Dispatched  int
	SkippedRuns int
	ParseErrors []string
	WriteErrors []string
	Duration    time.Duration
}

// DispatcherConfig are the tick's settings.
type DispatcherConfig struct {
	MissScanMin   int
	MaxConcurrent int
	DryRun        bool
	InstanceID    string
	Location      *time.Location
}

// NewDispatcher builds the dispatcher.
func NewDispatcher(repo domain.DispatchRepository, dispatch DispatchFunc, cfg DispatcherConfig) *Dispatcher {
	if cfg.MissScanMin < 0 {
		cfg.MissScanMin = 0
	}
	if cfg.MaxConcurrent < 1 {
		cfg.MaxConcurrent = 1
	}
	if cfg.Location == nil {
		cfg.Location = time.Local
	}
	return &Dispatcher{
		repo:          repo,
		dispatch:      dispatch,
		missScanMin:   cfg.MissScanMin,
		maxConcurrent: cfg.MaxConcurrent,
		dryRun:        cfg.DryRun,
		instanceID:    cfg.InstanceID,
		location:      cfg.Location,
	}
}

// Tick processes one minute.
//
// The result is a NAMED return, and that is load bearing: the deferred timing
// write runs after the return value has been copied. With an unnamed result it
// wrote into a local nobody could see, and every tick logged a duration of
// zero, which is the only measurement that says whether a tick outran its
// minute.
func (d *Dispatcher) Tick(ctx context.Context) (result TickResult, err error) {
	if !d.running.CompareAndSwap(false, true) {
		return TickResult{Skipped: true}, nil
	}
	defer d.running.Store(false)

	start := time.Now()
	defer func() { result.Duration = time.Since(start) }()

	// 1. The database is the clock. The application clock is used only to
	//    measure how far the two have drifted.
	dbTime, err := d.repo.DBTime(ctx)
	if err != nil {
		return result, err
	}
	appTime := time.Now()
	minute := minuteStart(dbTime.In(d.location))
	result.Minute = minute
	result.DriftSec = int(appTime.Sub(dbTime).Round(time.Second).Seconds())

	// Do not read drift_sec as larger than it is. now() truncates to the
	// second, so an application clock a hair ahead of the database reads the
	// previous second and the gap looks like a whole second when the real
	// divergence is milliseconds. Two machines always differ by a few
	// milliseconds and NTP cannot remove that. The fix is not to correct the
	// clocks, it is to sample away from the boundary; see
	// domain.DispatcherSpec.

	// Audit the chain of minutes. Nothing else notices this failure: the drift
	// figure is rounded to seconds and the watchdog's threshold is thirty of
	// them, while the fault that breaks the boundary is a few hundred
	// milliseconds wide.
	if !d.lastMinute.IsZero() {
		switch {
		case minute.Equal(d.lastMinute):
			result.MinuteRepeat = true
		case minute.After(d.lastMinute):
			result.MinuteGap = int(minute.Sub(d.lastMinute)/time.Minute) - 1
		}
	}
	d.lastMinute = minute

	// 2. The pulse. Read first, to work out which minutes were missed, then
	//    written.
	var lastRun time.Time
	heartbeat, err := d.repo.ReadHeartbeat(ctx)
	if err != nil {
		return result, err
	}
	if heartbeat != nil {
		lastRun = heartbeat.LastRun.In(d.location)
	}
	// A dry run does not touch the pulse. Refreshing it without doing the work
	// would make a real dispatcher coming back later skip its catch-up scan
	// and lose those minutes in silence.
	if !d.dryRun {
		if err := d.repo.TouchHeartbeat(ctx, dbTime, appTime, result.DriftSec, d.instanceID); err != nil {
			return result, err
		}
	}

	// 3. Which past minutes to replay.
	past := d.missedMinutes(minute, lastRun)
	result.MissedMinutes = len(past)

	// 4. Active jobs and their expressions.
	jobs, err := d.repo.ListScheduledJobs(ctx)
	if err != nil {
		return result, err
	}
	result.ActiveJobs = len(jobs)

	// 5. Open a pending row for everything that is due.
	//
	// With no active job there is nothing to queue, and the schedule query is
	// skipped. The tick does NOT stop here, though: rows can already be
	// pending from an earlier minute or from a chain step waiting on its
	// delay, and returning early would strand them until the watchdog
	// eventually reported them as stale.
	if len(jobs) > 0 {
		schedules, err := d.repo.ListActiveSchedules(ctx)
		if err != nil {
			return result, err
		}
		d.queueDue(ctx, jobs, schedules, minute, past, &result)
	}

	// 6. Hand over what is pending. A dry run stops here: handing over is
	//    running.
	if d.dryRun {
		result.DryRun = true
		return result, nil
	}
	if err := d.dispatchPending(ctx, dbTime, &result); err != nil {
		return result, err
	}
	return result, nil
}

// missedMinutes lists the minutes skipped while the dispatcher was not
// running.
//
// The current minute is always evaluated separately; this is only the gap. The
// scan reaches back at most missScanMin, so a dispatcher that was down for a
// week does not queue a week of work the instant it returns.
func (d *Dispatcher) missedMinutes(minute, lastRun time.Time) []time.Time {
	if lastRun.IsZero() {
		return nil
	}
	oldest := minute.Add(-time.Duration(d.missScanMin) * time.Minute)
	start := minuteStart(lastRun).Add(time.Minute)
	if start.Before(oldest) {
		start = oldest
	}
	var out []time.Time
	for t := start; t.Before(minute); t = t.Add(time.Minute) {
		out = append(out, t)
	}
	return out
}

// queueDue opens pending rows for the jobs that are due.
//
// A broken expression skips that expression only. Stopping the tick would let
// one bad line typed into one form stop every scheduled job in the system.
func (d *Dispatcher) queueDue(
	ctx context.Context,
	jobs []domain.ScheduledJob,
	schedules map[int64][]string,
	minute time.Time,
	past []time.Time,
	result *TickResult,
) {
	for _, job := range jobs {
		exprs := schedules[job.ID]
		if len(exprs) == 0 {
			continue
		}
		candidates := d.candidateMinutes(job, minute, past)

		for _, text := range exprs {
			expr, err := d.parse(text)
			if err != nil {
				result.ParseErrors = append(result.ParseErrors,
					fmt.Sprintf("%s [%s] %v", job.Code, text, err))
				continue
			}
			for _, candidate := range candidates {
				if !expr.Matches(candidate) {
					continue
				}
				if d.dryRun {
					result.Candidates = append(result.Candidates,
						Candidate{JobID: job.ID, Code: job.Code, Minute: candidate})
					result.QueuedRuns++
					continue
				}
				if err := d.repo.QueueRun(ctx, job.ID, candidate); err != nil {
					result.WriteErrors = append(result.WriteErrors, job.Code+": "+err.Error())
					continue
				}
				result.QueuedRuns++
			}
		}
	}
}

// candidateMinutes lists the minutes to evaluate for one job.
func (d *Dispatcher) candidateMinutes(job domain.ScheduledJob, minute time.Time, past []time.Time) []time.Time {
	candidates := []time.Time{minute}
	if !job.RunMissed || len(past) == 0 {
		return candidates
	}
	limit := time.Duration(job.MaxDelayMin) * time.Minute
	for _, t := range past {
		if minute.Sub(t) <= limit {
			candidates = append(candidates, t)
		}
	}
	return candidates
}

// parse returns a cached expression, parsing it on first use.
func (d *Dispatcher) parse(text string) (*cronexpr.Expr, error) {
	if v, ok := d.exprCache.Load(text); ok {
		switch cached := v.(type) {
		case *cronexpr.Expr:
			return cached, nil
		case error:
			// A broken expression is cached too. Re-parsing it every minute to
			// produce the same error serves nothing.
			return nil, cached
		}
	}
	expr, err := cronexpr.Parse(text)
	if err != nil {
		d.exprCache.Store(text, err)
		return nil, err
	}
	d.exprCache.Store(text, expr)
	return expr, nil
}

// dispatchPending hands rows over, respecting the concurrency cap and the
// single_run rule.
//
// Over the cap, the rest stay pending and are picked up next minute. They are
// delayed, never lost, which is the whole reason the queue is a table.
func (d *Dispatcher) dispatchPending(ctx context.Context, now time.Time, result *TickResult) error {
	running, err := d.repo.CountRunning(ctx)
	if err != nil {
		return err
	}
	result.Running = running

	quota := d.maxConcurrent - running
	if quota <= 0 {
		result.QuotaFull = true
		return nil
	}

	runningJobs, err := d.repo.RunningJobIDs(ctx)
	if err != nil {
		return err
	}
	pending, err := d.repo.ListPending(ctx, now, quota)
	if err != nil {
		return err
	}

	for _, row := range pending {
		if row.SingleRun && runningJobs[row.JobID] {
			if err := d.repo.SkipRun(ctx, row.ID, "Previous run still going (single run)"); err != nil {
				result.WriteErrors = append(result.WriteErrors, row.Code+": "+err.Error())
				continue
			}
			result.SkippedRuns++
			continue
		}

		d.dispatch(row.ID, row.Code)
		// Do not hand the same job over twice inside one tick.
		runningJobs[row.JobID] = true
		result.Dispatched++
	}
	return nil
}

// minuteStart zeroes the seconds.
//
// Built explicitly rather than with Truncate, which works on absolute time and
// produces a surprise across a zone offset that is not a whole number of
// minutes. Cron expressions are read against the wall clock and this has to
// match.
func minuteStart(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), 0, 0, t.Location())
}
