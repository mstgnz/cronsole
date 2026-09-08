package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/mstgnz/cronsole/v2/internal/applog"
	"github.com/mstgnz/cronsole/v2/internal/domain"
)

// Runner executes one queued run and records what happened.
//
// Two callers reach it: the dispatcher, for scheduled, manual and API
// triggered rows, and the runner itself, when handing over a chain step.
//
// THE CLAIM IS ATOMIC. If ClaimRun does not affect exactly one row, another
// process already has it and this call returns having done nothing. That
// single statement is the only thing preventing a run from executing twice;
// nothing above it repeats the check, and nothing needs to.
type Runner struct {
	repo     domain.RunRepositoryExec
	notify   domain.NotifyRepository
	notifier *Notifier
	observer RunObserver
	client   *http.Client
	log      *applog.Logger

	policy       TargetPolicy
	userAgent    string
	maxBodyBytes int64
	instanceID   string

	timers *chainTimers
}

// RunObserver is told about every finished run.
//
// An interface rather than a direct call into a metrics library: the service
// layer has no business knowing what Prometheus is, and a test that wanted to
// assert on a counter would otherwise have to register one.
//
// It is deliberately narrow. Anything richer belongs to whatever reads the
// run rows, which hold far more than this.
type RunObserver interface {
	// RunFinished reports one execution. at is a Unix timestamp in seconds.
	RunFinished(project, job, status string, durationSeconds, at float64)
}

// RunnerConfig are the runner's settings.
type RunnerConfig struct {
	Policy       TargetPolicy
	UserAgent    string
	MaxBodyBytes int64
	InstanceID   string
	// Observer is optional. Nil means nothing is recorded, which is the right
	// default for a test.
	Observer RunObserver
	// Dispatch hands a chain step over for execution.
	Dispatch DispatchFunc
	// Client is injected by tests. Nothing fills it in production.
	Client *http.Client
	// Routes redirects a hostname to a fixed address at dial time, so a job
	// naming a public host reaches it over the private network. Optional; nil
	// dials exactly what DNS says. Ignored when Client is supplied, which is
	// the test path and brings its own transport.
	Routes *HostResolver
	// Notifier sends the mail for a finished run. Optional; nil means no mail.
	Notifier *Notifier
}

// NewRunner builds the runner.
func NewRunner(repo domain.RunRepositoryExec, notify domain.NotifyRepository,
	log *applog.Logger, cfg RunnerConfig) *Runner {

	client := cfg.Client
	if client == nil {
		client = newOutboundClient(cfg.Routes)
	}
	if cfg.MaxBodyBytes <= 0 {
		cfg.MaxBodyBytes = 64 << 10
	}
	return &Runner{
		repo:         repo,
		notify:       notify,
		client:       client,
		log:          log,
		policy:       cfg.Policy,
		userAgent:    cfg.UserAgent,
		maxBodyBytes: cfg.MaxBodyBytes,
		instanceID:   cfg.InstanceID,
		notifier:     cfg.Notifier,
		observer:     cfg.Observer,
		timers:       &chainTimers{timers: map[int64]*time.Timer{}, dispatch: cfg.Dispatch},
	}
}

// Run executes one queued row. It reports the outcome and whether this call is
// the one that ran it.
func (r *Runner) Run(ctx context.Context, runID int64) (domain.RunResult, bool) {
	row, err := r.repo.GetRun(ctx, runID)
	if err != nil {
		r.log.Error("runner: run row unreadable", err.Error(), "run_id", runID)
		return domain.RunResult{}, false
	}
	if row == nil {
		r.log.Warn("runner: run row missing", "", "run_id", runID)
		return domain.RunResult{}, false
	}

	// The single run check exists in the dispatcher too. It has to be here as
	// well because a chain step never passes through the dispatcher, and
	// without it a chained run can execute alongside the same job's scheduled
	// run, which for anything that writes is a silent data fault.
	if row.SingleRun {
		running, err := r.repo.CountRunningForJob(ctx, row.JobID, runID)
		if err != nil {
			r.log.Error("runner: single run check failed", err.Error(), "run_id", runID, "job", row.Code)
			return domain.RunResult{}, false
		}
		if running > 0 {
			if err := r.repo.SkipRun(ctx, runID, "Previous run still going (single run)"); err != nil {
				r.log.Error("runner: could not record skip", err.Error(), "run_id", runID, "job", row.Code)
			}
			return domain.RunResult{Status: domain.StatusSkipped}, false
		}
	}

	claimed, err := r.repo.ClaimRun(ctx, runID, r.instanceID, time.Now())
	if err != nil {
		r.log.Error("runner: claim failed", err.Error(), "run_id", runID, "job", row.Code)
		return domain.RunResult{}, false
	}
	if !claimed {
		// Another process has it. Not an error: this is exactly what the
		// guard is for.
		return domain.RunResult{}, false
	}

	// From here the row is marked running and MUST be closed, whichever way
	// this function leaves: early return, error, panic. This defer is the only
	// exit.
	//
	// The starting value is deliberately failed. If the closer runs without a
	// result having been assigned, the process ended unexpectedly and the row
	// must not read as success.
	result := domain.RunResult{
		Status: domain.StatusFailed,
		Error:  "Run ended unexpectedly.",
	}
	defer func() {
		if rec := recover(); rec != nil {
			result = domain.RunResult{
				Status: domain.StatusFailed,
				Error:  fmt.Sprintf("panic: %v", rec),
			}
			r.log.Error("runner: panic during run", fmt.Sprintf("%v", rec), "run_id", runID, "job", row.Code)
		}
		// The closing work gets its OWN context. The caller's may already be
		// cancelled by the job's timeout, and writing through a cancelled
		// context is how a row stays stuck in running forever.
		closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		defer cancel()
		r.finish(closeCtx, row, result)
	}()

	result = r.execute(ctx, row)
	return result, true
}

// execute calls the target and classifies the answer.
func (r *Runner) execute(ctx context.Context, row *domain.RunTarget) domain.RunResult {
	targetURL, err := r.policy.Resolve(row.BaseURL, row.URL)
	if err != nil {
		return domain.RunResult{Status: domain.StatusFailed, Error: err.Error()}
	}

	timeout := row.Timeout()
	attempts := row.Retries + 1
	var last domain.RunResult

	for attempt := 1; attempt <= attempts; attempt++ {
		last = r.attempt(ctx, row, targetURL, timeout)
		last.Attempt = attempt
		last.RequestURL = targetURL

		if last.Status == domain.StatusSuccess {
			return last
		}
		// A timeout is not retried. The client gave up but the target may well
		// still be working, and firing the same job again while the first call
		// is possibly still running is how a job that writes runs twice.
		if last.Status == domain.StatusTimeout {
			return last
		}
		if attempt < attempts {
			// A short fixed pause. Backing off further would push the run past
			// its max duration and get it closed as stuck by the watchdog.
			select {
			case <-ctx.Done():
				return last
			case <-time.After(time.Second):
			}
		}
	}
	return last
}

// attempt performs one request.
func (r *Runner) attempt(ctx context.Context, row *domain.RunTarget, targetURL string, timeout time.Duration) domain.RunResult {
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// The body reader is rebuilt per attempt: a failed attempt has already
	// consumed it, and reusing it would send an empty body on the retry.
	var body io.Reader
	if row.Body != "" {
		body = strings.NewReader(row.Body)
	}

	req, err := http.NewRequestWithContext(reqCtx, row.Method, targetURL, body)
	if err != nil {
		return domain.RunResult{Status: domain.StatusFailed, Error: "request could not be built: " + err.Error()}
	}
	req.Header.Set("User-Agent", r.userAgent)
	req.Header.Set("X-Cron-Job", row.Code)
	req.Header.Set("X-Cron-Run-Id", fmt.Sprint(row.ID))
	if row.Body != "" && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for _, h := range row.Headers {
		if ValidHeaderName(h.Key) && ValidHeaderValue(h.Value) {
			req.Header.Set(h.Key, h.Value)
		}
	}

	start := time.Now()
	resp, err := r.client.Do(req)
	durMs := int(time.Since(start).Milliseconds())

	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || isTimeout(err) {
			return domain.RunResult{
				Status:     domain.StatusTimeout,
				DurationMs: durMs,
				Error: fmt.Sprintf("Client gave up after %d seconds. The target may still be running.",
					int(timeout.Seconds())),
			}
		}
		if errors.Is(err, context.Canceled) {
			return domain.RunResult{
				Status:     domain.StatusFailed,
				DurationMs: durMs,
				Error:      "Run cancelled (the service is shutting down).",
			}
		}
		return domain.RunResult{Status: domain.StatusFailed, DurationMs: durMs, Error: err.Error()}
	}
	defer resp.Body.Close()

	// The body is kept for diagnosis only and stored truncated. The limit
	// applies at read time: a target answering with a large page must not be
	// able to grow this process.
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, r.maxBodyBytes))

	out := domain.RunResult{
		DurationMs: durMs,
		HTTPStatus: resp.StatusCode,
		Output:     string(raw),
	}
	if row.SuccessfulStatus(resp.StatusCode) {
		out.Status = domain.StatusSuccess
		return out
	}
	out.Status = domain.StatusFailed
	out.Error = fmt.Sprintf("HTTP %d (expected %d-%d)", resp.StatusCode, row.SuccessMin, row.SuccessMax)
	return out
}

func isTimeout(err error) bool {
	var timeouter interface{ Timeout() bool }
	return errors.As(err, &timeouter) && timeouter.Timeout()
}

// finish writes the outcome, refreshes the summary, notifies and triggers the
// chain.
func (r *Runner) finish(ctx context.Context, row *domain.RunTarget, result domain.RunResult) {
	if err := r.repo.FinishRun(ctx, row.ID, result); err != nil {
		// With the result unwritten, the summary and the chain are meaningless:
		// nobody knows what this run did, and building a chain on top of that
		// is a silent data fault.
		r.log.Error("runner: result could not be written", err.Error(),
			"run_id", row.ID, "job", row.Code, "status", result.Status)
		return
	}

	// The summary is informational; the real result was written above. A
	// failure to write it is not swallowed: the reason is APPENDED to the
	// run's error text, so it shows up next to the run on screen.
	if err := r.repo.WriteJobSummary(ctx, row.JobID, result.Status, time.Now(), result.DurationMs); err != nil {
		message := "job summary could not be written: " + err.Error()
		r.log.Warn("runner: job summary write failed", message, "run_id", row.ID, "job", row.Code)
		if err := r.repo.AppendRunNote(ctx, row.ID, message); err != nil {
			r.log.Error("runner: summary note could not be appended", err.Error(), "run_id", row.ID)
		}
	}

	// Observed after the row is written, so a metric can never claim an
	// outcome the history does not have.
	if r.observer != nil {
		r.observer.RunFinished(row.ProjectSlug, row.Code, result.Status,
			float64(result.DurationMs)/1000, float64(time.Now().Unix()))
	}

	r.sendNotification(ctx, row, result)

	switch result.Status {
	case domain.StatusSuccess:
		r.triggerChain(ctx, row, "success")
	case domain.StatusFailed:
		r.triggerChain(ctx, row, "failure")
		// A timeout deliberately triggers nothing. Whether the target finished
		// is unknown, and running a dependent job on half written data is
		// worse than not running it.
	}
}

func (r *Runner) sendNotification(ctx context.Context, row *domain.RunTarget, result domain.RunResult) {
	if r.notifier == nil || row.NotificationID == nil {
		return
	}
	target, err := r.notify.TargetForJob(ctx, *row.NotificationID)
	if err != nil {
		r.log.Warn("runner: notification target unreadable", err.Error(), "run_id", row.ID)
		return
	}
	if target == nil || len(target.Emails) == 0 {
		return
	}

	success := result.Status == domain.StatusSuccess
	if success && !target.OnSuccess {
		return
	}
	if !success && !target.OnFailure {
		return
	}
	r.notifier.RunFinished(target.Emails, row, result)
}

// triggerChain queues the jobs linked to this one and hands them over.
//
// Hand-off has two routes:
//
//  1. Short delays are handed over by the runner itself. Leaving them to a
//     dispatcher that comes round once a minute meant a link written as two
//     seconds actually ran at the fifth second of the next minute.
//  2. Long delays, and anything blocked by the quota or the single run rule,
//     stay pending with run_after set, and the dispatcher takes over.
//
// The second route is also the safety net: if the direct hand-off is lost for
// any reason, a restart or a closed timer, the ROW STILL EXISTS. That is why
// it is written before the timer is set, not after.
func (r *Runner) triggerChain(ctx context.Context, row *domain.RunTarget, outcome string) {
	step, err := r.repo.ChainStepCount(ctx, row.ID)
	if err != nil {
		r.log.Error("chain: step count unreadable", err.Error(), "run_id", row.ID, "job", row.Code)
		return
	}

	targets, err := r.repo.ListChainTargets(ctx, row.JobID, outcome)
	if err != nil {
		r.log.Error("chain: targets unreadable", err.Error(), "run_id", row.ID, "job", row.Code)
		return
	}

	for _, target := range targets {
		// An inactive job does not run through a chain either. Inactive has to
		// close every route, or it means nothing.
		if !target.Active {
			r.recordChainSkipped(ctx, target, row.ID, "Linked job is inactive; the chain step did not run.")
			continue
		}

		if step >= domain.ChainMaxDepth {
			r.recordChainSkipped(ctx, target, row.ID,
				fmt.Sprintf("Chain depth passed %d steps and was stopped.", domain.ChainMaxDepth))
			continue
		}

		delay := target.Delay()
		newID, err := r.repo.CreateChainRun(ctx, target.JobID, int(delay.Seconds()), row.ID)
		if err != nil {
			// A chain step that cannot be queued must not change the result of
			// the job that triggered it.
			r.log.Error("chain: run could not be queued", err.Error(),
				"run_id", row.ID, "target", target.Code)
			continue
		}

		if delay > domain.ChainDirectMaxSec*time.Second {
			r.appendNote(ctx, newID, fmt.Sprintf(
				"Delay is %d seconds, over the %d second direct hand-off cap. The dispatcher will take it.",
				int(delay.Seconds()), domain.ChainDirectMaxSec))
			continue
		}

		blocker, err := r.dispatchBlocker(ctx, target)
		if err != nil {
			r.appendNote(ctx, newID, "Hand-off check failed. The dispatcher will take it.")
			continue
		}
		if blocker != "" {
			r.appendNote(ctx, newID, "Not handed over directly: "+blocker+". The dispatcher will take it.")
			continue
		}

		if !r.timers.schedule(delay, newID, target.Code) {
			r.appendNote(ctx, newID, "Timer closed. The dispatcher will take it.")
		}
	}
}

// dispatchBlocker reports whether something stops a direct hand-off.
//
// The dispatcher's quota and single run rules apply to chain steps too. A
// blocker means "do not hand it over", never "cancel it": the row stays
// pending either way.
func (r *Runner) dispatchBlocker(ctx context.Context, target domain.ChainTarget) (string, error) {
	if target.SingleRun {
		// Exclude nothing: every running row for that job counts.
		n, err := r.repo.CountRunningForJob(ctx, target.JobID, 0)
		if err != nil {
			return "", err
		}
		if n > 0 {
			return "the target job is already running (single run)", nil
		}
	}
	return "", nil
}

func (r *Runner) recordChainSkipped(ctx context.Context, target domain.ChainTarget, parentRunID int64, reason string) {
	if err := r.repo.CreateChainSkipped(ctx, target.JobID, parentRunID, reason); err != nil {
		r.log.Error("chain: skipped row could not be written", err.Error(),
			"target", target.Code, "parent_run_id", parentRunID)
	}
}

func (r *Runner) appendNote(ctx context.Context, runID int64, message string) {
	if err := r.repo.AppendRunNote(ctx, runID, message); err != nil {
		r.log.Error("chain: note could not be written", err.Error(), "run_id", runID)
	}
}

// Close stops pending chain timers and reports how many were stopped.
func (r *Runner) Close() int { return r.timers.close() }

// --- chain timers ---

// chainTimers holds short chain delays in process.
//
// One time.Timer per pending step: no shell, no external process, a few
// hundred bytes each.
//
// THEY DO NOT FIRE DURING SHUTDOWN. The row is already in the database with
// run_after set and the dispatcher picks it up on the next tick. Starting work
// while the process is closing is worse than not starting it.
type chainTimers struct {
	mu       sync.Mutex
	timers   map[int64]*time.Timer
	closed   bool
	dispatch DispatchFunc
}

func (c *chainTimers) schedule(delay time.Duration, runID int64, code string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed || c.dispatch == nil {
		return false
	}

	c.timers[runID] = time.AfterFunc(delay, func() {
		c.mu.Lock()
		delete(c.timers, runID)
		closed := c.closed
		dispatch := c.dispatch
		c.mu.Unlock()

		if closed {
			return
		}
		dispatch(runID, code)
	})
	return true
}

func (c *chainTimers) close() int {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.closed = true
	stopped := 0
	for id, t := range c.timers {
		if t.Stop() {
			stopped++
		}
		delete(c.timers, id)
	}
	return stopped
}
