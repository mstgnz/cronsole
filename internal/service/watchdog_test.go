package service

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mstgnz/cronsole/v2/internal/domain"
)

// fakeWatchdogRepo implements domain.WatchdogRepository.
type fakeWatchdogRepo struct {
	mu sync.Mutex

	now       time.Time
	heartbeat *domain.Heartbeat

	stuckClosed  int64
	stalePending int
	failing      []domain.ErrorSummary

	purgedRuns int64
	purgedLogs int64
	marker     string
	markerSet  int

	closeErr error
}

func newWatchdogRepo() *fakeWatchdogRepo {
	return &fakeWatchdogRepo{now: time.Now()}
}

func (f *fakeWatchdogRepo) DBTime(context.Context) (time.Time, error) { return f.now, nil }

func (f *fakeWatchdogRepo) CloseStuckRuns(context.Context) (int64, error) {
	return f.stuckClosed, f.closeErr
}

func (f *fakeWatchdogRepo) CountStalePending(context.Context, time.Duration) (int, error) {
	return f.stalePending, nil
}

func (f *fakeWatchdogRepo) ReadHeartbeat(context.Context) (*domain.Heartbeat, error) {
	return f.heartbeat, nil
}

func (f *fakeWatchdogRepo) ListFailingJobs(context.Context, time.Duration, int) ([]domain.ErrorSummary, error) {
	return f.failing, nil
}

func (f *fakeWatchdogRepo) PurgeRuns(_ context.Context, _, _ int) (int64, error) {
	return f.purgedRuns, nil
}

func (f *fakeWatchdogRepo) PurgeAppLogs(_ context.Context, _ int) (int64, error) {
	return f.purgedLogs, nil
}

func (f *fakeWatchdogRepo) SetWarningMarker(_ context.Context, marker string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.marker = marker
	f.markerSet++
	if f.heartbeat != nil {
		f.heartbeat.Note = marker
	}
	return nil
}

// recordingSender captures what would have been mailed.
type recordingSender struct {
	mu       sync.Mutex
	subjects []string
}

func (r *recordingSender) Send(_ []string, subject, _ string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.subjects = append(r.subjects, subject)
	return nil
}

func (r *recordingSender) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.subjects)
}

func newTestWatchdog(repo domain.WatchdogRepository, sender MailSender) (*Watchdog, *Notifier) {
	notifier := NewNotifier(sender, testLogger(), "")
	cfg := WatchdogConfig{
		HeartbeatLimitMin: 5,
		DriftLimitSec:     30,
		RunRetentionDays:  30,
		LogRetentionDays:  30,
		FailureThreshold:  3,
		AlertTo:           []string{"ops@example.com"},
		AlertRepeat:       time.Hour,
	}
	return NewWatchdog(repo, notifier, testLogger(), cfg), notifier
}

func drain(n *Notifier) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	n.Close(ctx)
}

func TestSweepIsQuietWhenHealthy(t *testing.T) {
	repo := newWatchdogRepo()
	repo.heartbeat = &domain.Heartbeat{LastRun: repo.now.Add(-30 * time.Second)}

	watchdog, notifier := newTestWatchdog(repo, &recordingSender{})
	result, err := watchdog.Sweep(context.Background())
	drain(notifier)

	if err != nil {
		t.Fatalf("sweep failed: %v", err)
	}
	if len(result.Warnings) != 0 {
		t.Errorf("warnings on a healthy system: %v", result.Warnings)
	}
	if result.MailSent {
		t.Error("a mail was sent with nothing wrong")
	}
}

func TestSweepDetectsADeadDispatcher(t *testing.T) {
	// Nothing else in the system reports this. The screens simply show less
	// activity, which looks like a quiet day.
	repo := newWatchdogRepo()
	repo.heartbeat = &domain.Heartbeat{LastRun: repo.now.Add(-30 * time.Minute)}

	sender := &recordingSender{}
	watchdog, notifier := newTestWatchdog(repo, sender)
	result, err := watchdog.Sweep(context.Background())
	drain(notifier)

	if err != nil {
		t.Fatal(err)
	}
	if result.HeartbeatAgeMin < 29 {
		t.Errorf("HeartbeatAgeMin = %d, want about 30", result.HeartbeatAgeMin)
	}
	if !containsAny(result.Warnings, "dispatcher has not run") {
		t.Errorf("the stale pulse was not reported: %v", result.Warnings)
	}
	if !result.MailSent || sender.count() != 1 {
		t.Errorf("no alert was sent: MailSent=%v sent=%d", result.MailSent, sender.count())
	}
}

func TestSweepDetectsClockDrift(t *testing.T) {
	repo := newWatchdogRepo()
	repo.heartbeat = &domain.Heartbeat{LastRun: repo.now.Add(-10 * time.Second), DriftSec: 45}

	watchdog, notifier := newTestWatchdog(repo, &recordingSender{})
	result, _ := watchdog.Sweep(context.Background())
	drain(notifier)

	if !containsAny(result.Warnings, "clocks differ") {
		t.Errorf("the drift was not reported: %v", result.Warnings)
	}
}

func TestSweepClosesStuckRunsAndSaysSo(t *testing.T) {
	// Not cosmetic: a job marked single run whose row is stuck in running
	// never executes again.
	repo := newWatchdogRepo()
	repo.heartbeat = &domain.Heartbeat{LastRun: repo.now}
	repo.stuckClosed = 3

	watchdog, notifier := newTestWatchdog(repo, &recordingSender{})
	result, _ := watchdog.Sweep(context.Background())
	drain(notifier)

	if result.StuckClosed != 3 {
		t.Errorf("StuckClosed = %d, want 3", result.StuckClosed)
	}
	if !containsAny(result.Warnings, "maximum duration") {
		t.Errorf("closing stuck runs was not reported: %v", result.Warnings)
	}
}

func TestSweepReportsStalePendingAndFailingJobs(t *testing.T) {
	repo := newWatchdogRepo()
	repo.heartbeat = &domain.Heartbeat{LastRun: repo.now}
	repo.stalePending = 4
	repo.failing = []domain.ErrorSummary{
		{Code: "ak-sonuc", Name: "AK", Count: 9, LastError: "HTTP 500"},
	}

	watchdog, notifier := newTestWatchdog(repo, &recordingSender{})
	result, _ := watchdog.Sweep(context.Background())
	drain(notifier)

	if !containsAny(result.Warnings, "queued for over") {
		t.Errorf("stale pending runs were not reported: %v", result.Warnings)
	}
	if !containsAny(result.Warnings, "ak-sonuc failed 9 times") {
		t.Errorf("the failing job was not reported: %v", result.Warnings)
	}
}

func TestAlertRepeatSuppression(t *testing.T) {
	// The same warnings must not be mailed every sweep.
	repo := newWatchdogRepo()
	repo.heartbeat = &domain.Heartbeat{LastRun: repo.now.Add(-30 * time.Minute)}

	sender := &recordingSender{}
	watchdog, notifier := newTestWatchdog(repo, sender)

	first, _ := watchdog.Sweep(context.Background())
	if !first.MailSent {
		t.Fatal("the first alert was not sent")
	}

	second, _ := watchdog.Sweep(context.Background())
	drain(notifier)

	if second.MailSent {
		t.Error("the same warnings were mailed twice")
	}
	if second.MailSkipReason == "" {
		t.Error(`no reason was recorded, so "why did I not get an alert" is unanswerable`)
	}
	if sender.count() != 1 {
		t.Errorf("%d mails were sent, want 1", sender.count())
	}
}

func TestANewWarningGetsThroughSuppression(t *testing.T) {
	// The suppression key is a fingerprint of the warnings themselves, not a
	// timestamp, so a NEW problem is reported at once even during a quiet
	// period after an unrelated alert.
	repo := newWatchdogRepo()
	repo.heartbeat = &domain.Heartbeat{LastRun: repo.now.Add(-30 * time.Minute)}

	sender := &recordingSender{}
	watchdog, notifier := newTestWatchdog(repo, sender)

	if first, _ := watchdog.Sweep(context.Background()); !first.MailSent {
		t.Fatal("the first alert was not sent")
	}

	// A different fault appears alongside the first.
	repo.failing = []domain.ErrorSummary{{Code: "new-fault", Count: 5}}
	second, _ := watchdog.Sweep(context.Background())
	drain(notifier)

	if !second.MailSent {
		t.Error("a new warning was suppressed by the previous fingerprint")
	}
	if sender.count() != 2 {
		t.Errorf("%d mails were sent, want 2", sender.count())
	}
}

func TestRecoveryClearsTheMarker(t *testing.T) {
	// Otherwise the next occurrence is suppressed by a fingerprint left over
	// from a problem that is over.
	repo := newWatchdogRepo()
	repo.heartbeat = &domain.Heartbeat{LastRun: repo.now.Add(-30 * time.Minute)}

	watchdog, notifier := newTestWatchdog(repo, &recordingSender{})
	watchdog.Sweep(context.Background())
	if repo.marker == "" {
		t.Fatal("no marker was written for the first alert")
	}

	// The dispatcher comes back.
	repo.heartbeat.LastRun = repo.now
	watchdog.Sweep(context.Background())
	drain(notifier)

	if repo.marker != "" {
		t.Errorf("the marker survived recovery: %q", repo.marker)
	}
}

func TestSweepContinuesPastAFailedStep(t *testing.T) {
	// Skipping log cleanup because the pulse could not be read would make no
	// sense, so a failure joins the warning list and the sweep carries on.
	repo := newWatchdogRepo()
	repo.heartbeat = &domain.Heartbeat{LastRun: repo.now}
	repo.closeErr = errFake
	repo.purgedRuns = 12

	watchdog, notifier := newTestWatchdog(repo, &recordingSender{})
	result, err := watchdog.Sweep(context.Background())
	drain(notifier)

	if err != nil {
		t.Fatalf("the sweep aborted: %v", err)
	}
	if !containsAny(result.Warnings, "stuck runs could not be closed") {
		t.Errorf("the failed step was not reported: %v", result.Warnings)
	}
	if result.PurgedRuns != 12 {
		t.Errorf("PurgedRuns = %d; retention did not run after the earlier failure", result.PurgedRuns)
	}
}

func TestNoRecipientIsReportedRatherThanIgnored(t *testing.T) {
	repo := newWatchdogRepo()
	repo.heartbeat = &domain.Heartbeat{LastRun: repo.now.Add(-30 * time.Minute)}

	notifier := NewNotifier(&recordingSender{}, testLogger(), "")
	watchdog := NewWatchdog(repo, notifier, testLogger(), WatchdogConfig{})

	result, _ := watchdog.Sweep(context.Background())
	drain(notifier)

	if result.MailSent {
		t.Error("a mail was sent with no configured recipient")
	}
	if !strings.Contains(result.MailSkipReason, "recipient") {
		t.Errorf("MailSkipReason = %q, want it to name the missing recipient", result.MailSkipReason)
	}
}

func TestNeverRunDispatcherIsReported(t *testing.T) {
	repo := newWatchdogRepo()
	repo.heartbeat = nil

	watchdog, notifier := newTestWatchdog(repo, &recordingSender{})
	result, _ := watchdog.Sweep(context.Background())
	drain(notifier)

	if !containsAny(result.Warnings, "never run") {
		t.Errorf("a missing pulse was not reported: %v", result.Warnings)
	}
}

func containsAny(warnings []string, needle string) bool {
	for _, w := range warnings {
		if strings.Contains(w, needle) {
			return true
		}
	}
	return false
}
