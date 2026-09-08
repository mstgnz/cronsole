package service

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/mstgnz/cronsole/v2/internal/domain"
)

func minute(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.ParseInLocation("2006-01-02 15:04", value, time.UTC)
	if err != nil {
		t.Fatalf("bad test time %q: %v", value, err)
	}
	return parsed
}

// collector records what the dispatcher handed over.
type collector struct {
	mu  sync.Mutex
	ids []int64
}

func (c *collector) dispatch(runID int64, _ string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ids = append(c.ids, runID)
}

func (c *collector) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.ids)
}

func newTestDispatcher(repo domain.DispatchRepository, dispatch DispatchFunc, missScan int) *Dispatcher {
	return NewDispatcher(repo, dispatch, DispatcherConfig{
		MissScanMin:   missScan,
		MaxConcurrent: 5,
		InstanceID:    "test",
		Location:      time.UTC,
	})
}

func TestTickQueuesDueJobs(t *testing.T) {
	now := minute(t, "2026-03-04 10:10").Add(5 * time.Second)
	repo := newDispatchRepo(now)
	repo.jobs = []domain.ScheduledJob{
		{ID: 1, Code: "five", Priority: 10},
		{ID: 2, Code: "daily", Priority: 20},
	}
	repo.schedules = map[int64][]string{
		1: {"*/5 * * * *"},
		2: {"0 3 * * *"},
	}

	result, err := newTestDispatcher(repo, func(int64, string) {}, 60).Tick(context.Background())
	if err != nil {
		t.Fatalf("tick failed: %v", err)
	}
	if result.QueuedRuns != 1 {
		t.Errorf("queued %d runs, want 1", result.QueuedRuns)
	}
	if got := repo.queuedFor(1); len(got) != 1 || !got[0].Equal(minute(t, "2026-03-04 10:10")) {
		t.Errorf("job 1 queued %v, want the 10:10 minute", got)
	}
	if got := repo.queuedFor(2); len(got) != 0 {
		t.Errorf("job 2 should not have been queued, got %v", got)
	}
}

func TestTickIsIdempotentWithinAMinute(t *testing.T) {
	// Two dispatchers reaching the same minute must produce one row. The
	// database's unique index is what guarantees it, so the test asserts on
	// the rows rather than on how many times QueueRun was called.
	now := minute(t, "2026-03-04 10:10").Add(5 * time.Second)
	repo := newDispatchRepo(now)
	repo.jobs = []domain.ScheduledJob{{ID: 1, Code: "every"}}
	repo.schedules = map[int64][]string{1: {"* * * * *"}}

	for i := 0; i < 3; i++ {
		// A fresh dispatcher each time, standing in for a separate replica.
		if _, err := newTestDispatcher(repo, func(int64, string) {}, 60).Tick(context.Background()); err != nil {
			t.Fatalf("tick %d failed: %v", i, err)
		}
	}
	if got := repo.queuedFor(1); len(got) != 1 {
		t.Errorf("three ticks on the same minute produced %d rows, want 1", len(got))
	}
}

func TestTickReplaysMissedMinutesOnlyWhenAsked(t *testing.T) {
	now := minute(t, "2026-03-04 10:10").Add(5 * time.Second)
	repo := newDispatchRepo(now)
	// The pulse is five minutes stale, so 10:06 to 10:09 were missed.
	repo.heartbeat = &domain.Heartbeat{LastRun: minute(t, "2026-03-04 10:05")}
	repo.jobs = []domain.ScheduledJob{
		{ID: 1, Code: "replays", RunMissed: true, MaxDelayMin: 10},
		{ID: 2, Code: "does-not", RunMissed: false, MaxDelayMin: 10},
	}
	repo.schedules = map[int64][]string{
		1: {"* * * * *"},
		2: {"* * * * *"},
	}

	result, err := newTestDispatcher(repo, func(int64, string) {}, 60).Tick(context.Background())
	if err != nil {
		t.Fatalf("tick failed: %v", err)
	}
	if result.MissedMinutes != 4 {
		t.Errorf("MissedMinutes = %d, want 4", result.MissedMinutes)
	}
	// 10:06, 10:07, 10:08, 10:09 and the current 10:10.
	if got := repo.queuedFor(1); len(got) != 5 {
		t.Errorf("the replaying job queued %d minutes, want 5: %v", len(got), got)
	}
	// The other job only ever gets the current minute.
	if got := repo.queuedFor(2); len(got) != 1 {
		t.Errorf("the non-replaying job queued %d minutes, want 1: %v", len(got), got)
	}
}

func TestReplayIsBoundedByMaxDelay(t *testing.T) {
	// A job that says it will tolerate two minutes of lateness must not be
	// replayed for an hour of it.
	now := minute(t, "2026-03-04 10:30").Add(5 * time.Second)
	repo := newDispatchRepo(now)
	repo.heartbeat = &domain.Heartbeat{LastRun: minute(t, "2026-03-04 10:00")}
	repo.jobs = []domain.ScheduledJob{{ID: 1, Code: "narrow", RunMissed: true, MaxDelayMin: 2}}
	repo.schedules = map[int64][]string{1: {"* * * * *"}}

	if _, err := newTestDispatcher(repo, func(int64, string) {}, 60).Tick(context.Background()); err != nil {
		t.Fatalf("tick failed: %v", err)
	}
	// 10:28, 10:29 and 10:30. Anything older is past max_delay_min.
	if got := repo.queuedFor(1); len(got) != 3 {
		t.Errorf("queued %d minutes, want 3: %v", len(got), got)
	}
}

func TestReplayIsBoundedByScanWindow(t *testing.T) {
	// A dispatcher that was down for a week must not queue a week of work the
	// instant it returns, however tolerant the job is.
	now := minute(t, "2026-03-04 10:30").Add(5 * time.Second)
	repo := newDispatchRepo(now)
	repo.heartbeat = &domain.Heartbeat{LastRun: minute(t, "2026-02-25 10:00")}
	repo.jobs = []domain.ScheduledJob{{ID: 1, Code: "tolerant", RunMissed: true, MaxDelayMin: 1440}}
	repo.schedules = map[int64][]string{1: {"* * * * *"}}

	result, err := newTestDispatcher(repo, func(int64, string) {}, 10).Tick(context.Background())
	if err != nil {
		t.Fatalf("tick failed: %v", err)
	}
	if result.MissedMinutes != 10 {
		t.Errorf("MissedMinutes = %d, want the 10 minute scan window", result.MissedMinutes)
	}
}

func TestBadExpressionSkipsOnlyItself(t *testing.T) {
	// One bad line typed into one form must not stop every scheduled job in
	// the system.
	now := minute(t, "2026-03-04 10:10").Add(5 * time.Second)
	repo := newDispatchRepo(now)
	repo.jobs = []domain.ScheduledJob{
		{ID: 1, Code: "broken"},
		{ID: 2, Code: "fine"},
	}
	repo.schedules = map[int64][]string{
		1: {"not a cron expression"},
		2: {"* * * * *"},
	}

	result, err := newTestDispatcher(repo, func(int64, string) {}, 60).Tick(context.Background())
	if err != nil {
		t.Fatalf("tick failed: %v", err)
	}
	if len(result.ParseErrors) != 1 {
		t.Errorf("ParseErrors = %v, want one", result.ParseErrors)
	}
	if got := repo.queuedFor(2); len(got) != 1 {
		t.Error("the healthy job was not queued alongside the broken one")
	}
}

func TestQuotaLeavesWorkPending(t *testing.T) {
	// Over the cap the rest stay pending and are picked up next minute. They
	// are delayed, never lost; that is the whole reason the queue is a table.
	now := minute(t, "2026-03-04 10:10").Add(5 * time.Second)
	repo := newDispatchRepo(now)
	repo.running = 5 // equals MaxConcurrent
	repo.pending = []domain.PendingRun{{ID: 1, JobID: 1, Code: "a"}}

	c := &collector{}
	result, err := newTestDispatcher(repo, c.dispatch, 60).Tick(context.Background())
	if err != nil {
		t.Fatalf("tick failed: %v", err)
	}
	if !result.QuotaFull {
		t.Error("QuotaFull was not reported")
	}
	if c.count() != 0 {
		t.Errorf("dispatched %d runs while the quota was full", c.count())
	}
	if len(repo.skipped) != 0 {
		t.Error("a run was skipped rather than left pending")
	}
}

func TestSingleRunSkipsWhilePreviousIsGoing(t *testing.T) {
	now := minute(t, "2026-03-04 10:10").Add(5 * time.Second)
	repo := newDispatchRepo(now)
	repo.runningJobs = map[int64]bool{7: true}
	repo.pending = []domain.PendingRun{
		{ID: 100, JobID: 7, Code: "busy", SingleRun: true},
		{ID: 101, JobID: 8, Code: "free", SingleRun: true},
	}

	c := &collector{}
	result, err := newTestDispatcher(repo, c.dispatch, 60).Tick(context.Background())
	if err != nil {
		t.Fatalf("tick failed: %v", err)
	}
	if result.SkippedRuns != 1 {
		t.Errorf("SkippedRuns = %d, want 1", result.SkippedRuns)
	}
	if len(repo.skipped) != 1 || repo.skipped[0] != 100 {
		t.Errorf("skipped %v, want run 100", repo.skipped)
	}
	if result.Dispatched != 1 {
		t.Errorf("Dispatched = %d, want 1", result.Dispatched)
	}
}

func TestOneJobIsNotDispatchedTwiceInATick(t *testing.T) {
	// Two pending rows for the same single-run job: the first is handed over,
	// the second has to be skipped, because the first has not started yet and
	// would not show up in RunningJobIDs.
	now := minute(t, "2026-03-04 10:10").Add(5 * time.Second)
	repo := newDispatchRepo(now)
	repo.pending = []domain.PendingRun{
		{ID: 200, JobID: 9, Code: "same", SingleRun: true},
		{ID: 201, JobID: 9, Code: "same", SingleRun: true},
	}

	c := &collector{}
	result, err := newTestDispatcher(repo, c.dispatch, 60).Tick(context.Background())
	if err != nil {
		t.Fatalf("tick failed: %v", err)
	}
	if result.Dispatched != 1 {
		t.Errorf("Dispatched = %d, want 1", result.Dispatched)
	}
	if result.SkippedRuns != 1 {
		t.Errorf("SkippedRuns = %d, want 1", result.SkippedRuns)
	}
}

func TestOverlappingTickIsSkipped(t *testing.T) {
	// A tick that outruns its minute must not be joined by the next one: the
	// rows are safe either way, but the concurrency quota would be computed
	// twice and the cap exceeded.
	now := minute(t, "2026-03-04 10:10").Add(5 * time.Second)
	repo := newDispatchRepo(now)
	repo.jobs = []domain.ScheduledJob{{ID: 1, Code: "slow"}}
	repo.schedules = map[int64][]string{1: {"* * * * *"}}
	repo.pending = []domain.PendingRun{{ID: 1, JobID: 1, Code: "slow"}}

	// entered signals that a tick has reached dispatch; release lets it leave.
	// Both are needed: waiting on a timer instead would let the second tick
	// start first, and then BOTH would block here with nothing to release
	// them.
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once

	d := newTestDispatcher(repo, func(int64, string) {
		once.Do(func() { close(entered) })
		<-release
	}, 60)

	var first TickResult
	done := make(chan struct{})
	go func() {
		first, _ = d.Tick(context.Background())
		close(done)
	}()

	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("the first tick never reached dispatch")
	}

	second, err := d.Tick(context.Background())
	if err != nil {
		t.Fatalf("second tick failed: %v", err)
	}

	close(release)
	<-done

	if !second.Skipped {
		t.Error("an overlapping tick was not skipped")
	}
	if first.Skipped {
		t.Error("the first tick reported itself as skipped")
	}
}

func TestMinuteChainIsAudited(t *testing.T) {
	// A repeated or skipped minute is silent everywhere else: a repeated one
	// conflicts on the unique index without raising an error, and a skipped
	// one leaves no trace at all. This report is the only detection.
	repo := newDispatchRepo(minute(t, "2026-03-04 10:10").Add(5 * time.Second))
	repo.jobs = []domain.ScheduledJob{{ID: 1, Code: "j"}}
	repo.schedules = map[int64][]string{1: {"* * * * *"}}
	d := newTestDispatcher(repo, func(int64, string) {}, 60)

	if _, err := d.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}

	// The same minute again.
	result, err := d.Tick(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !result.MinuteRepeat {
		t.Error("a repeated minute was not reported")
	}

	// Now jump three minutes: two of them were never processed.
	repo.now = minute(t, "2026-03-04 10:13").Add(5 * time.Second)
	result, err = d.Tick(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.MinuteGap != 2 {
		t.Errorf("MinuteGap = %d, want 2", result.MinuteGap)
	}
}

func TestDryRunWritesNothing(t *testing.T) {
	// A dry run must not touch the pulse either. Refreshing it without doing
	// the work would make a real dispatcher coming back later skip its
	// catch-up scan and lose those minutes in silence.
	now := minute(t, "2026-03-04 10:10").Add(5 * time.Second)
	repo := newDispatchRepo(now)
	repo.jobs = []domain.ScheduledJob{{ID: 1, Code: "j"}}
	repo.schedules = map[int64][]string{1: {"* * * * *"}}
	repo.pending = []domain.PendingRun{{ID: 1, JobID: 1, Code: "j"}}

	c := &collector{}
	d := NewDispatcher(repo, c.dispatch, DispatcherConfig{
		MaxConcurrent: 5, DryRun: true, InstanceID: "test", Location: time.UTC,
	})

	result, err := d.Tick(context.Background())
	if err != nil {
		t.Fatalf("tick failed: %v", err)
	}
	if !result.DryRun {
		t.Error("DryRun was not reported")
	}
	if len(repo.queued) != 0 {
		t.Errorf("a dry run wrote %d queued rows", len(repo.queued))
	}
	if repo.touched != 0 {
		t.Error("a dry run touched the heartbeat")
	}
	if c.count() != 0 {
		t.Error("a dry run dispatched work")
	}
	if len(result.Candidates) != 1 {
		t.Errorf("Candidates = %v, want the one due job", result.Candidates)
	}
}

func TestTickFailsWhenTheClockIsUnreadable(t *testing.T) {
	// Without the database clock there is no minute, and guessing one from the
	// application clock is what makes two replicas disagree.
	repo := newDispatchRepo(time.Now())
	repo.timeErr = errFake

	if _, err := newTestDispatcher(repo, func(int64, string) {}, 60).Tick(context.Background()); err == nil {
		t.Error("tick succeeded with an unreadable clock")
	}
}

func TestDispatcherSpecDoesNotFireOnTheBoundary(t *testing.T) {
	// The seconds field must not be zero. The tick is fired by the application
	// clock while the minute it processes comes from the database, and a tick
	// on the exact boundary falls on the wrong side of it when the two clocks
	// differ by milliseconds, losing a whole minute of work.
	if domain.DispatcherSpec == "" {
		t.Fatal("DispatcherSpec is empty")
	}
	fields := len(splitFields(domain.DispatcherSpec))
	if fields != 6 {
		t.Fatalf("DispatcherSpec has %d fields, want 6 so it can name a second", fields)
	}
	if seconds := splitFields(domain.DispatcherSpec)[0]; seconds == "0" || seconds == "*" {
		t.Errorf("DispatcherSpec seconds field is %q; it must be a non-zero second", seconds)
	}
}

func splitFields(s string) []string {
	var out []string
	current := ""
	for _, r := range s {
		if r == ' ' {
			if current != "" {
				out = append(out, current)
				current = ""
			}
			continue
		}
		current += string(r)
	}
	if current != "" {
		out = append(out, current)
	}
	return out
}
