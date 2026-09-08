package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/mstgnz/cronsole/v2/internal/applog"
	"github.com/mstgnz/cronsole/v2/internal/domain"
)

// Watchdog is the only thing that notices the scheduler itself failing.
//
// Everything else in the system reports on jobs. Nothing reports on the
// dispatcher being dead, on runs stuck in running, or on rows queued that
// nothing ever collected, and each of those is silent: the screens simply show
// less activity, which looks like a quiet day.
type Watchdog struct {
	repo domain.WatchdogRepository
	// resets is pruned alongside the runs and the logs. It may be nil, which is
	// what a deployment or a test without the reset flow passes.
	resets   domain.PasswordResetRepository
	notifier *Notifier
	log      *applog.Logger
	cfg      WatchdogConfig
}

// WithPasswordResets attaches the reset table to the sweep, so spent and
// expired links do not accumulate forever. Set separately from the constructor
// because it is the only optional part of the sweep and threading it through
// every caller would make the common case read as though it were.
func (w *Watchdog) WithPasswordResets(resets domain.PasswordResetRepository) *Watchdog {
	w.resets = resets
	return w
}

// WatchdogConfig are the sweep's thresholds.
type WatchdogConfig struct {
	HeartbeatLimitMin int
	DriftLimitSec     int
	RunRetentionDays  int
	LogRetentionDays  int
	FailureThreshold  int
	AlertTo           []string
	AlertRepeat       time.Duration
}

const (
	// stalePendingAfter: a row queued this long ago that is still pending was
	// never handed over by anything.
	stalePendingAfter = 15 * time.Minute
	// failureWindow is the span the repeated failure scan looks at.
	failureWindow = time.Hour
	// purgeBatch is the most rows one sweep deletes, so the delete never holds
	// a long lock on the busiest table.
	purgeBatch = 5000
)

// SweepResult summarises one sweep.
type SweepResult struct {
	StuckClosed     int64
	StalePending    int
	HeartbeatAgeMin int
	DriftSec        int
	PurgedRuns      int64
	PurgedLogs      int64
	PurgedResets    int64
	FailingJobs     int
	Warnings        []string
	MailSent        bool
	// MailSkipReason says why no mail went out. Empty means there was nothing
	// to send. It exists so "why did I not get an alert" is answerable from
	// the log rather than by reasoning about the code.
	MailSkipReason string
}

// NewWatchdog builds the watchdog, filling in sane thresholds for anything
// left at zero.
func NewWatchdog(repo domain.WatchdogRepository, notifier *Notifier, log *applog.Logger, cfg WatchdogConfig) *Watchdog {
	if cfg.HeartbeatLimitMin <= 0 {
		cfg.HeartbeatLimitMin = 5
	}
	if cfg.DriftLimitSec <= 0 {
		cfg.DriftLimitSec = 30
	}
	if cfg.FailureThreshold <= 0 {
		cfg.FailureThreshold = 5
	}
	if cfg.AlertRepeat <= 0 {
		cfg.AlertRepeat = time.Hour
	}
	return &Watchdog{repo: repo, notifier: notifier, log: log, cfg: cfg}
}

// Sweep runs one pass.
//
// No step stops another. Skipping log cleanup because the pulse could not be
// read would make no sense, so failures join the warning list and the sweep
// carries on.
func (w *Watchdog) Sweep(ctx context.Context) (SweepResult, error) {
	result := SweepResult{}

	// 1. Close stuck runs.
	//
	// If the runner crashed or the process was killed, the row stays in
	// running. Left there, a job marked single_run never runs again: every
	// tick decides the previous execution is still going and skips.
	closed, err := w.repo.CloseStuckRuns(ctx)
	if err != nil {
		result.Warnings = append(result.Warnings, "stuck runs could not be closed: "+err.Error())
	} else {
		result.StuckClosed = closed
		if closed > 0 {
			result.Warnings = append(result.Warnings,
				fmt.Sprintf("%d run(s) exceeded their maximum duration and were closed as timeout", closed))
		}
	}

	// 2. Rows nothing ever collected.
	stale, err := w.repo.CountStalePending(ctx, stalePendingAfter)
	if err != nil {
		result.Warnings = append(result.Warnings, "pending runs could not be counted: "+err.Error())
	} else {
		result.StalePending = stale
		if stale > 0 {
			result.Warnings = append(result.Warnings,
				fmt.Sprintf("%d run(s) have been queued for over %s without being started",
					stale, stalePendingAfter))
		}
	}

	// 3. The pulse, and the gap between the two clocks.
	dbTime, err := w.repo.DBTime(ctx)
	if err != nil {
		result.Warnings = append(result.Warnings, "database time could not be read: "+err.Error())
		dbTime = time.Now()
	}
	heartbeat, err := w.repo.ReadHeartbeat(ctx)
	switch {
	case err != nil:
		result.Warnings = append(result.Warnings, "heartbeat could not be read: "+err.Error())
	case heartbeat == nil:
		result.Warnings = append(result.Warnings, "the dispatcher has never run")
	default:
		age := int(dbTime.Sub(heartbeat.LastRun).Minutes())
		result.HeartbeatAgeMin = age
		result.DriftSec = heartbeat.DriftSec
		if age >= w.cfg.HeartbeatLimitMin {
			result.Warnings = append(result.Warnings,
				fmt.Sprintf("the dispatcher has not run for %d minutes; scheduled jobs are not being queued", age))
		}
		if abs(heartbeat.DriftSec) >= w.cfg.DriftLimitSec {
			result.Warnings = append(result.Warnings,
				fmt.Sprintf("application and database clocks differ by %d seconds", heartbeat.DriftSec))
		}
	}

	// 4. Jobs failing repeatedly.
	failing, err := w.repo.ListFailingJobs(ctx, failureWindow, w.cfg.FailureThreshold)
	if err != nil {
		result.Warnings = append(result.Warnings, "failing jobs could not be listed: "+err.Error())
	} else {
		result.FailingJobs = len(failing)
		for _, f := range failing {
			line := fmt.Sprintf("%s failed %d times in the last hour", f.Code, f.Count)
			if f.LastError != "" {
				line += ": " + firstRunes(f.LastError, 200)
			}
			result.Warnings = append(result.Warnings, line)
		}
	}

	// 5. Retention. Both tables grow with every execution and nothing else
	//    ever deletes from them.
	if w.cfg.RunRetentionDays > 0 {
		if n, err := w.repo.PurgeRuns(ctx, w.cfg.RunRetentionDays, purgeBatch); err != nil {
			result.Warnings = append(result.Warnings, "run history could not be pruned: "+err.Error())
		} else {
			result.PurgedRuns = n
		}
	}
	if w.cfg.LogRetentionDays > 0 {
		if n, err := w.repo.PurgeAppLogs(ctx, w.cfg.LogRetentionDays); err != nil {
			result.Warnings = append(result.Warnings, "application logs could not be pruned: "+err.Error())
		} else {
			result.PurgedLogs = n
		}
	}
	// Reset links are pruned on their own clock rather than a retention
	// setting: a spent or expired one is already useless, and it is a
	// credential hash tied to a person, so there is no reason to keep it.
	if w.resets != nil {
		if n, err := w.resets.DeleteExpired(ctx, time.Now()); err != nil {
			result.Warnings = append(result.Warnings, "reset links could not be pruned: "+err.Error())
		} else {
			result.PurgedResets = n
		}
	}

	// 6. Alert, with repeat suppression.
	w.alert(ctx, heartbeat, &result)
	return result, nil
}

// alert mails the warning list, at most once per AlertRepeat for the same set.
//
// The suppression key is a fingerprint of the warnings themselves, not a
// timestamp: a NEW problem appearing during a quiet period has to get through
// immediately, even if an unrelated alert went out two minutes ago.
func (w *Watchdog) alert(ctx context.Context, heartbeat *domain.Heartbeat, result *SweepResult) {
	if len(result.Warnings) == 0 {
		if heartbeat != nil && heartbeat.Note != "" {
			// The system recovered. Clearing the marker means the next
			// occurrence is reported at once rather than being suppressed by a
			// fingerprint from a problem that is over.
			if err := w.repo.SetWarningMarker(ctx, ""); err != nil {
				w.log.Warn("watchdog: alert marker could not be cleared", err.Error())
			}
		}
		return
	}

	w.log.Warn("watchdog: warnings raised", strings.Join(result.Warnings, "; "))

	if w.notifier == nil || len(w.cfg.AlertTo) == 0 {
		result.MailSkipReason = "no alert recipient is configured"
		return
	}

	fingerprint := warningFingerprint(result.Warnings)
	if heartbeat != nil && heartbeat.Note != "" {
		mark, at, ok := parseMarker(heartbeat.Note)
		if ok && mark == fingerprint && time.Since(at) < w.cfg.AlertRepeat {
			result.MailSkipReason = fmt.Sprintf("the same warnings were mailed %s ago", time.Since(at).Round(time.Minute))
			return
		}
	}

	subject := fmt.Sprintf("[cron] %d warning(s) from the scheduler", len(result.Warnings))
	var b strings.Builder
	b.WriteString("The watchdog raised the following:\n\n")
	for _, warning := range result.Warnings {
		b.WriteString("  - " + warning + "\n")
	}
	w.notifier.Alert(w.cfg.AlertTo, subject, b.String())
	result.MailSent = true

	if err := w.repo.SetWarningMarker(ctx, formatMarker(fingerprint, time.Now())); err != nil {
		w.log.Warn("watchdog: alert marker could not be written", err.Error())
	}
}

func warningFingerprint(warnings []string) string {
	sum := sha256.Sum256([]byte(strings.Join(warnings, "\n")))
	return hex.EncodeToString(sum[:8])
}

func formatMarker(fingerprint string, at time.Time) string {
	return fingerprint + "@" + at.UTC().Format(time.RFC3339)
}

func parseMarker(note string) (string, time.Time, bool) {
	parts := strings.SplitN(note, "@", 2)
	if len(parts) != 2 {
		return "", time.Time{}, false
	}
	at, err := time.Parse(time.RFC3339, parts[1])
	if err != nil {
		return "", time.Time{}, false
	}
	return parts[0], at, true
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}
