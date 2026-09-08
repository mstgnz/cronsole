package service

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mstgnz/cronsole/v2/internal/domain"
)

// captureSender records the whole message, not just the subject.
type captureSender struct {
	mu   sync.Mutex
	sent []struct {
		To            []string
		Subject, Body string
	}
}

func (c *captureSender) Send(to []string, subject, body string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sent = append(c.sent, struct {
		To            []string
		Subject, Body string
	}{To: to, Subject: subject, Body: body})
	return nil
}

func (c *captureSender) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.sent)
}

func runnerWithNotifier(repo domain.RunRepositoryExec, notify domain.NotifyRepository,
	notifier *Notifier) *Runner {
	return NewRunner(repo, notify, testLogger(), RunnerConfig{
		Policy:       TargetPolicy{AllowPrivate: true},
		UserAgent:    "cronsole-test",
		MaxBodyBytes: 4096,
		InstanceID:   "test",
		Notifier:     notifier,
	})
}

func TestNotificationRespectsTheChosenOutcomes(t *testing.T) {
	// A list that only wants failures must not be told about successes. A job
	// running every five minutes would otherwise send 288 messages a day.
	success := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	defer success.Close()

	notificationID := int64(1)
	target := testTarget(success.URL)
	target.NotificationID = &notificationID

	sender := &captureSender{}
	notifier := NewNotifier(sender, testLogger(), "https://cron.example.com")

	notify := &fakeNotifyRepo{target: &domain.NotifyTarget{
		OnSuccess: false, OnFailure: true, Emails: []string{"ops@example.com"},
	}}
	runnerWithNotifier(newRunRepo(target), notify, notifier).Run(context.Background(), 1)
	drain(notifier)

	if sender.count() != 0 {
		t.Errorf("a success was mailed to a failures-only list: %+v", sender.sent)
	}
}

func TestNotificationOnFailureCarriesTheDetail(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, "upstream exploded")
	}))
	defer server.Close()

	notificationID := int64(1)
	target := testTarget(server.URL)
	target.NotificationID = &notificationID

	sender := &captureSender{}
	notifier := NewNotifier(sender, testLogger(), "https://cron.example.com")
	notify := &fakeNotifyRepo{target: &domain.NotifyTarget{
		OnFailure: true, Emails: []string{"ops@example.com"},
	}}

	runnerWithNotifier(newRunRepo(target), notify, notifier).Run(context.Background(), 1)
	drain(notifier)

	if sender.count() != 1 {
		t.Fatalf("%d messages were sent, want 1", sender.count())
	}
	message := sender.sent[0]
	if !strings.Contains(message.Subject, "demo") || !strings.Contains(message.Subject, "job") {
		t.Errorf("subject = %q, want it to name the project and job", message.Subject)
	}
	// The message has to be enough to act on without opening the screen.
	for _, want := range []string{"HTTP", "upstream exploded", "https://cron.example.com"} {
		if !strings.Contains(message.Body, want) {
			t.Errorf("the body does not mention %q:\n%s", want, message.Body)
		}
	}
}

func TestNotificationIsSkippedWithNoRecipients(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	notificationID := int64(1)
	target := testTarget(server.URL)
	target.NotificationID = &notificationID

	sender := &captureSender{}
	notifier := NewNotifier(sender, testLogger(), "")
	notify := &fakeNotifyRepo{target: &domain.NotifyTarget{OnFailure: true}}

	runnerWithNotifier(newRunRepo(target), notify, notifier).Run(context.Background(), 1)
	drain(notifier)

	if sender.count() != 0 {
		t.Error("a message was sent with no recipients")
	}
}

func TestChainHandOffIsBlockedByASingleRunTarget(t *testing.T) {
	// A blocker means "do not hand it over", never "cancel it": the row stays
	// pending and the dispatcher takes it when the target is free.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	defer server.Close()

	repo := newRunRepo(testTarget(server.URL))
	repo.runningForJob = 1 // the chain target is already running
	repo.chainTargets["success"] = []domain.ChainTarget{
		{JobID: 20, Code: "busy", Active: true, SingleRun: true, DelaySec: 1},
	}

	handed := make(chan int64, 1)
	runner := newTestRunner(repo, func(runID int64, _ string) { handed <- runID })
	runner.Run(context.Background(), 1)

	if len(repo.chainCreated) != 1 {
		t.Fatalf("chain rows created = %v, want the row to exist anyway", repo.chainCreated)
	}
	select {
	case id := <-handed:
		t.Errorf("run %d was handed over while its job was already running", id)
	case <-time.After(300 * time.Millisecond):
	}
	if notes := repo.notes[1001]; len(notes) == 0 {
		t.Error("no note explains why the hand-off was skipped")
	}
	runner.Close()
}

// statsStore is an in-memory StatsStore.
type statsStore struct {
	summary  *domain.Summary
	activity []domain.HourBucket
	slowest  []domain.JobDuration
	running  []domain.RunRow
	failures []domain.RunRow
	history  []domain.RunPoint
}

func (s *statsStore) Summary(context.Context, domain.ProjectScope) (*domain.Summary, error) {
	return s.summary, nil
}

func (s *statsStore) Activity(context.Context, domain.ProjectScope, int) ([]domain.HourBucket, error) {
	return s.activity, nil
}

func (s *statsStore) SlowestJobs(context.Context, domain.ProjectScope, int, int) ([]domain.JobDuration, error) {
	return s.slowest, nil
}

func (s *statsStore) RunningNow(context.Context, domain.ProjectScope, int) ([]domain.RunRow, error) {
	return s.running, nil
}

func (s *statsStore) RecentFailures(context.Context, domain.ProjectScope, int) ([]domain.RunRow, error) {
	return s.failures, nil
}

func (s *statsStore) JobHistory(context.Context, domain.ProjectScope, int64, int) ([]domain.RunPoint, error) {
	return s.history, nil
}

func TestJobTrendSummarisesTheSameWindowItCharts(t *testing.T) {
	// The figures printed above the chart have to describe the chart. A number
	// computed over a different period is worse than no number: the reader takes
	// it as the chart's own summary.
	now := time.Now()
	points := []domain.RunPoint{
		{RunID: 1, At: now, Status: domain.StatusSuccess, DurationMs: 10},
		{RunID: 2, At: now, Status: domain.StatusSuccess, DurationMs: 20},
		{RunID: 3, At: now, Status: domain.StatusSuccess, DurationMs: 30},
		{RunID: 4, At: now, Status: domain.StatusFailed, DurationMs: 40},
		// A skipped run never executed, so it carries no duration.
		{RunID: 5, At: now, Status: domain.StatusSkipped},
	}

	store := newMemStore()
	stats := NewStatsService(&statsStore{summary: &domain.Summary{}, history: points},
		memJobRepo{memStore: store}, newTestJobService(store))

	trend, err := stats.JobTrendFor(context.Background(), allProjects, 1, 50)
	if err != nil {
		t.Fatal(err)
	}

	if trend.Total != 5 || trend.Success != 3 || trend.Failed != 1 || trend.Skipped != 1 {
		t.Errorf("counts = %+v", trend)
	}
	if trend.SuccessRate != 60 {
		t.Errorf("success rate = %d, want 60", trend.SuccessRate)
	}
	// The average covers only the runs that finished. Folding the skipped run's
	// zero in would make a job look faster the more often it fails to start.
	if trend.AvgMs != 25 {
		t.Errorf("average = %d, want 25 over the four runs that executed", trend.AvgMs)
	}
	if trend.MaxMs != 40 {
		t.Errorf("slowest = %d, want 40", trend.MaxMs)
	}
	// Nearest-rank p95 over four values lands on the slowest, which is the
	// honest answer on a short window rather than an interpolation.
	if trend.P95Ms != 40 {
		t.Errorf("p95 = %d, want 40", trend.P95Ms)
	}
}

func TestJobTrendWithNoRuns(t *testing.T) {
	// A job that has never run must not report a 0% success rate: zero reads as
	// "everything failed", and the page would accuse a job nobody has started.
	store := newMemStore()
	stats := NewStatsService(&statsStore{summary: &domain.Summary{}},
		memJobRepo{memStore: store}, newTestJobService(store))

	trend, err := stats.JobTrendFor(context.Background(), allProjects, 1, 50)
	if err != nil {
		t.Fatal(err)
	}
	if trend.Total != 0 {
		t.Fatalf("total = %d, want 0", trend.Total)
	}
	if trend.SuccessRate != -1 {
		t.Errorf("success rate = %d, want -1 for nothing to divide by", trend.SuccessRate)
	}
	if trend.AvgMs != 0 || trend.P95Ms != 0 {
		t.Errorf("durations = %+v, want zero", trend)
	}
}

func TestDashboardUpcomingIsOrderedAndActiveOnly(t *testing.T) {
	// Nothing schedules ahead: the dispatcher decides a minute at a time, so
	// "what runs next" exists only as a calculation over the expressions.
	store := newMemStore()
	store.addProject(1, "demo", "")
	jobs := newTestJobService(store)
	ctx := context.Background()

	if _, err := jobs.Create(ctx, allProjects, &JobInput{
		ProjectID: 1, Code: "soon", Name: "Soon", URL: "https://example.com/a",
		Schedules: []string{"* * * * *"}, Active: true,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := jobs.Create(ctx, allProjects, &JobInput{
		ProjectID: 1, Code: "later", Name: "Later", URL: "https://example.com/b",
		Schedules: []string{"0 3 * * *"}, Active: true,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := jobs.Create(ctx, allProjects, &JobInput{
		ProjectID: 1, Code: "off", Name: "Off", URL: "https://example.com/c",
		Schedules: []string{"* * * * *"}, Active: false,
	}); err != nil {
		t.Fatal(err)
	}

	stats := NewStatsService(&statsStore{summary: &domain.Summary{}}, memJobRepo{memStore: store}, jobs)
	board, err := stats.Build(ctx, allProjects, 24)
	if err != nil {
		t.Fatalf("build failed: %v", err)
	}

	if len(board.Upcoming) != 2 {
		t.Fatalf("upcoming = %+v, want only the two active jobs", board.Upcoming)
	}
	if board.Upcoming[0].Code != "soon" {
		t.Errorf("upcoming[0] = %q, want the every-minute job first", board.Upcoming[0].Code)
	}
	if !board.Upcoming[0].At.Before(board.Upcoming[1].At) {
		t.Error("upcoming is not ordered by time")
	}
}

func TestSummaryHealth(t *testing.T) {
	// The thresholds are generous on purpose: the tick is once a minute, so a
	// single slow tick must not read as an outage.
	cases := []struct {
		name    string
		elapsed *int
		want    string
	}{
		{"never run", nil, "unknown"},
		{"fresh", intPtr(10), "healthy"},
		{"at the healthy edge", intPtr(120), "healthy"},
		{"late", intPtr(300), "late"},
		{"down", intPtr(3600), "down"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := domain.Summary{ElapsedSec: c.elapsed}
			if got := s.Health(); got != c.want {
				t.Errorf("Health() = %q, want %q", got, c.want)
			}
		})
	}
}

func intPtr(v int) *int { return &v }
