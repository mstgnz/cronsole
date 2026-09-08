package repository

import (
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mstgnz/cronsole/v2/internal/domain"
)

// The scheduler's correctness rests on two constraints, both in the database
// and neither expressible in Go:
//
//   - UNIQUE (job_id, planned_minute) gives one run per job per minute
//   - the atomic claim gives one execution per row
//
// Replicas run side by side with no coordination, no leader election and no
// lock table because of those two. This file is where they are checked.

func TestOneRunPerJobPerMinute(t *testing.T) {
	// Every replica queues the same minute. The unique index is what makes that
	// safe, and the repository has to read the conflict as "somebody else got
	// there first" rather than as a failure.
	s := fresh(t)
	repo := NewExecRepo(s)

	projectID := seedProject(t, s, "shop", "https://shop.example.com")
	jobID := seedJob(t, s, projectID, "daily")
	minute := time.Now().Truncate(time.Minute)

	for i := 0; i < 5; i++ {
		if err := repo.QueueRun(ctx(t), jobID, minute); err != nil {
			t.Fatalf("queueing the same minute again returned %v, want it swallowed", err)
		}
	}

	var count int
	err := s.db.QueryRow(
		`SELECT count(*) FROM job_runs WHERE job_id = $1 AND planned_minute = $2`,
		jobID, minute).Scan(&count)
	mustNoErr(t, err, "counting runs")

	if count != 1 {
		t.Errorf("%d rows for one minute, want exactly 1", count)
	}
}

func TestQueueingIsConcurrencySafe(t *testing.T) {
	// The real arrangement: several dispatchers ticking at the same second.
	// Serialised, this test proves nothing; the pool is sized so it is not.
	s := fresh(t)
	repo := NewExecRepo(s)

	projectID := seedProject(t, s, "shop", "https://shop.example.com")
	jobID := seedJob(t, s, projectID, "daily")
	minute := time.Now().Truncate(time.Minute)

	var wg sync.WaitGroup
	var failures atomic.Int32
	start := make(chan struct{})

	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if err := repo.QueueRun(ctx(t), jobID, minute); err != nil {
				failures.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()

	if got := failures.Load(); got != 0 {
		t.Errorf("%d of 10 concurrent queues returned an error", got)
	}

	var count int
	mustNoErr(t, s.db.QueryRow(
		`SELECT count(*) FROM job_runs WHERE job_id = $1`, jobID).Scan(&count), "counting runs")
	if count != 1 {
		t.Errorf("%d rows after 10 concurrent queues, want 1", count)
	}
}

func TestExactlyOneClaimantWins(t *testing.T) {
	// THE guarantee. The claim is one statement:
	//
	//	UPDATE job_runs SET status='running' WHERE id=$1 AND status='pending'
	//
	// If it affects no row, somebody else has it. That is the only thing
	// preventing a run from executing twice, and it has to hold when ten
	// processes reach for the same row at once.
	s := fresh(t)
	repo := NewExecRepo(s)

	projectID := seedProject(t, s, "shop", "https://shop.example.com")
	jobID := seedJob(t, s, projectID, "daily")
	runID := seedRun(t, s, jobID, domain.StatusPending, time.Now())

	var wg sync.WaitGroup
	var claimed, refused atomic.Int32
	start := make(chan struct{})

	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			<-start
			ok, err := repo.ClaimRun(ctx(t), runID, "instance", time.Now())
			if err != nil {
				t.Errorf("ClaimRun = %v", err)
				return
			}
			if ok {
				claimed.Add(1)
			} else {
				refused.Add(1)
			}
		}(i)
	}
	close(start)
	wg.Wait()

	if got := claimed.Load(); got != 1 {
		t.Errorf("%d claimants won, want exactly 1", got)
	}
	if got := refused.Load(); got != 9 {
		t.Errorf("%d claimants were refused, want 9", got)
	}
}

func TestARunThatIsNotPendingCannotBeClaimed(t *testing.T) {
	// A terminal row must not be reopened: doing so executes it a second time,
	// and for anything that writes that is a silent data fault.
	s := fresh(t)
	repo := NewExecRepo(s)

	projectID := seedProject(t, s, "shop", "https://shop.example.com")
	jobID := seedJob(t, s, projectID, "daily")

	for _, status := range []string{
		domain.StatusRunning, domain.StatusSuccess, domain.StatusFailed,
		domain.StatusTimeout, domain.StatusSkipped,
	} {
		runID := seedRun(t, s, jobID, status, time.Now())
		ok, err := repo.ClaimRun(ctx(t), runID, "instance", time.Now())
		mustNoErr(t, err, "ClaimRun")
		if ok {
			t.Errorf("a run in %q was claimed", status)
		}
	}
}

func TestFinishRunWritesTheOutcome(t *testing.T) {
	s := fresh(t)
	repo := NewExecRepo(s)

	projectID := seedProject(t, s, "shop", "https://shop.example.com")
	jobID := seedJob(t, s, projectID, "daily")
	runID := seedRun(t, s, jobID, domain.StatusPending, time.Now())

	_, err := repo.ClaimRun(ctx(t), runID, "instance-a", time.Now())
	mustNoErr(t, err, "ClaimRun")

	result := domain.RunResult{
		Status: domain.StatusFailed, DurationMs: 1234, HTTPStatus: 500,
		RequestURL: "https://shop.example.com/cron/daily",
		Output:     "upstream said no", Error: "HTTP 500 (expected 200-299)",
		Attempt: 2,
	}
	mustNoErr(t, repo.FinishRun(ctx(t), runID, result), "FinishRun")

	var (
		status     string
		duration   *int
		httpStatus *int
		output     string
		errText    string
		attempt    int
		finished   *time.Time
	)
	err = s.db.QueryRow(
		`SELECT status, duration_ms, http_status, output, error, attempt, finished_at
		 FROM job_runs WHERE id = $1`, runID).
		Scan(&status, &duration, &httpStatus, &output, &errText, &attempt, &finished)
	mustNoErr(t, err, "reading the run back")

	if status != domain.StatusFailed {
		t.Errorf("status = %q", status)
	}
	if duration == nil || *duration != 1234 {
		t.Errorf("duration = %v", duration)
	}
	if httpStatus == nil || *httpStatus != 500 {
		t.Errorf("http status = %v", httpStatus)
	}
	if output != "upstream said no" || errText != "HTTP 500 (expected 200-299)" {
		t.Errorf("output = %q, error = %q", output, errText)
	}
	if attempt != 2 {
		t.Errorf("attempt = %d", attempt)
	}
	if finished == nil {
		t.Error("finished_at was not set")
	}
}

func TestAnHTTPStatusOfZeroIsStoredAsNull(t *testing.T) {
	// "No answer" and "the server said 0" are not the same thing. Storing zero
	// would make a connection failure look like a response.
	s := fresh(t)
	repo := NewExecRepo(s)

	projectID := seedProject(t, s, "shop", "https://shop.example.com")
	jobID := seedJob(t, s, projectID, "daily")
	runID := seedRun(t, s, jobID, domain.StatusPending, time.Now())
	_, _ = repo.ClaimRun(ctx(t), runID, "instance", time.Now())

	mustNoErr(t, repo.FinishRun(ctx(t), runID, domain.RunResult{
		Status: domain.StatusFailed, Error: "connection refused",
	}), "FinishRun")

	var httpStatus *int
	mustNoErr(t, s.db.QueryRow(`SELECT http_status FROM job_runs WHERE id = $1`, runID).
		Scan(&httpStatus), "reading it back")

	if httpStatus != nil {
		t.Errorf("http_status = %d, want NULL for a run that got no answer", *httpStatus)
	}
}

func TestTheHeartbeatIsOneRow(t *testing.T) {
	// The dispatcher's pulse. A second row would make "is it running" a
	// question with two answers.
	s := fresh(t)
	repo := NewExecRepo(s)

	appTime := time.Now()
	mustNoErr(t, repo.TouchHeartbeat(ctx(t), appTime, appTime, 3, "instance-a"), "TouchHeartbeat")
	mustNoErr(t, repo.TouchHeartbeat(ctx(t), appTime, appTime, 7, "instance-b"), "TouchHeartbeat")

	var count int
	mustNoErr(t, s.db.QueryRow(`SELECT count(*) FROM heartbeat`).Scan(&count), "counting")
	if count != 1 {
		t.Errorf("%d heartbeat rows, want 1", count)
	}

	beat, err := repo.ReadHeartbeat(ctx(t))
	mustNoErr(t, err, "ReadHeartbeat")
	if beat == nil {
		t.Fatal("no heartbeat was read")
	}
	if beat.DriftSec != 7 || beat.InstanceID != "instance-b" {
		t.Errorf("the last write did not win: %+v", beat)
	}
}

func TestDBTimeIsTheDatabasesClock(t *testing.T) {
	// The minute is taken from the database while the tick is fired by the
	// application, so this is the reading the drift is measured against.
	s := fresh(t)
	repo := NewExecRepo(s)

	before := time.Now().Add(-2 * time.Second)
	got, err := repo.DBTime(ctx(t))
	mustNoErr(t, err, "DBTime")
	after := time.Now().Add(2 * time.Second)

	if got.Before(before) || got.After(after) {
		t.Errorf("DBTime = %s, which is not within two seconds of now", got)
	}
	// timestamptz, so it comes back with a zone rather than as a naive time.
	if got.Location() == nil {
		t.Error("the time came back with no location")
	}
}

func TestListScheduledJobsAndSchedules(t *testing.T) {
	// What the dispatcher reads every minute. A job that is switched off, or
	// whose project is deleted, must not be in it.
	s := fresh(t)
	repo := NewExecRepo(s)

	projectID := seedProject(t, s, "shop", "https://shop.example.com")
	active := seedJob(t, s, projectID, "active-job")
	inactive := seedJob(t, s, projectID, "inactive-job")
	_, err := s.db.Exec(`UPDATE jobs SET active = false WHERE id = $1`, inactive)
	mustNoErr(t, err, "deactivating")

	_, err = s.db.Exec(
		`INSERT INTO job_schedules (job_id, expression, active) VALUES ($1, '0 3 * * *', true)`,
		active)
	mustNoErr(t, err, "adding a schedule")
	_, err = s.db.Exec(
		`INSERT INTO job_schedules (job_id, expression, active) VALUES ($1, '0 4 * * *', true)`,
		inactive)
	mustNoErr(t, err, "adding a schedule")

	jobs, err := repo.ListScheduledJobs(ctx(t))
	mustNoErr(t, err, "ListScheduledJobs")

	found := map[int64]bool{}
	for _, j := range jobs {
		found[j.ID] = true
	}
	if !found[active] {
		t.Error("an active job is not in the dispatcher's list")
	}
	if found[inactive] {
		t.Error("a job that is switched off is in the dispatcher's list")
	}

	schedules, err := repo.ListActiveSchedules(ctx(t))
	mustNoErr(t, err, "ListActiveSchedules")
	if len(schedules[active]) != 1 || schedules[active][0] != "0 3 * * *" {
		t.Errorf("the schedules for the active job = %v", schedules[active])
	}
}

func TestCountRunningAndRunningJobIDs(t *testing.T) {
	// The concurrency cap and the single-run check both read these.
	s := fresh(t)
	repo := NewExecRepo(s)

	projectID := seedProject(t, s, "shop", "https://shop.example.com")
	first := seedJob(t, s, projectID, "first")
	second := seedJob(t, s, projectID, "second")

	seedRun(t, s, first, domain.StatusRunning, time.Now())
	seedRun(t, s, first, domain.StatusRunning, time.Now())
	seedRun(t, s, second, domain.StatusPending, time.Now())
	seedRun(t, s, second, domain.StatusSuccess, time.Now())

	running, err := repo.CountRunning(ctx(t))
	mustNoErr(t, err, "CountRunning")
	if running != 2 {
		t.Errorf("CountRunning = %d, want 2", running)
	}

	ids, err := repo.RunningJobIDs(ctx(t))
	mustNoErr(t, err, "RunningJobIDs")
	if !ids[first] {
		t.Error("the job with runs in flight is not reported as running")
	}
	if ids[second] {
		t.Error("a job with only a pending and a finished run is reported as running")
	}
}

func TestCountRunningForJobExcludesTheCaller(t *testing.T) {
	// The single-run check asks "is another run of this job going", so the row
	// doing the asking has to be left out or every job would block itself.
	s := fresh(t)
	repo := NewExecRepo(s)

	projectID := seedProject(t, s, "shop", "https://shop.example.com")
	jobID := seedJob(t, s, projectID, "daily")
	mine := seedRun(t, s, jobID, domain.StatusRunning, time.Now())

	n, err := repo.CountRunningForJob(ctx(t), jobID, mine)
	mustNoErr(t, err, "CountRunningForJob")
	if n != 0 {
		t.Errorf("CountRunningForJob = %d with only my own run going, want 0", n)
	}

	seedRun(t, s, jobID, domain.StatusRunning, time.Now())
	n, err = repo.CountRunningForJob(ctx(t), jobID, mine)
	mustNoErr(t, err, "CountRunningForJob")
	if n != 1 {
		t.Errorf("CountRunningForJob = %d with a second run going, want 1", n)
	}
}

func TestListPendingIsOrderedAndBounded(t *testing.T) {
	s := fresh(t)
	repo := NewExecRepo(s)

	projectID := seedProject(t, s, "shop", "https://shop.example.com")
	jobID := seedJob(t, s, projectID, "daily")
	for i := 0; i < 5; i++ {
		seedRun(t, s, jobID, domain.StatusPending, time.Now())
	}
	seedRun(t, s, jobID, domain.StatusSuccess, time.Now())

	pending, err := repo.ListPending(ctx(t), time.Now(), 3)
	mustNoErr(t, err, "ListPending")
	if len(pending) != 3 {
		t.Errorf("%d pending runs, want the limit of 3", len(pending))
	}
	for i := 1; i < len(pending); i++ {
		if pending[i-1].ID > pending[i].ID {
			t.Error("pending runs are not in order")
		}
	}
}

func TestARunScheduledForLaterIsNotPendingYet(t *testing.T) {
	// A chain step with a delay sits in the queue with run_after in the future.
	// Handing it out early would fire the delay it was given.
	s := fresh(t)
	repo := NewExecRepo(s)

	projectID := seedProject(t, s, "shop", "https://shop.example.com")
	jobID := seedJob(t, s, projectID, "daily")

	var runID int64
	err := s.db.QueryRow(
		`INSERT INTO job_runs (job_id, trigger, status, run_after)
		 VALUES ($1, 'chain', 'pending', now() + interval '1 hour') RETURNING id`,
		jobID).Scan(&runID)
	mustNoErr(t, err, "queueing a delayed run")

	pending, err := repo.ListPending(ctx(t), time.Now(), 10)
	mustNoErr(t, err, "ListPending")
	for _, p := range pending {
		if p.ID == runID {
			t.Error("a run scheduled for an hour from now was handed out")
		}
	}

	// And it is handed out once its time arrives.
	later, err := repo.ListPending(ctx(t), time.Now().Add(2*time.Hour), 10)
	mustNoErr(t, err, "ListPending")
	found := false
	for _, p := range later {
		if p.ID == runID {
			found = true
		}
	}
	if !found {
		t.Error("the delayed run never became due")
	}
}

func TestSkipRunClosesTheRowWithAReason(t *testing.T) {
	s := fresh(t)
	repo := NewExecRepo(s)

	projectID := seedProject(t, s, "shop", "https://shop.example.com")
	jobID := seedJob(t, s, projectID, "daily")
	runID := seedRun(t, s, jobID, domain.StatusPending, time.Now())

	mustNoErr(t, repo.SkipRun(ctx(t), runID, "Previous run still going"), "SkipRun")

	var status, note string
	mustNoErr(t, s.db.QueryRow(`SELECT status, error FROM job_runs WHERE id = $1`, runID).
		Scan(&status, &note), "reading it back")

	if status != domain.StatusSkipped {
		t.Errorf("status = %q, want skipped", status)
	}
	if note == "" {
		t.Error("the reason was not recorded, so the row says nothing about why")
	}
}

func TestCloseStuckRuns(t *testing.T) {
	// A process killed mid-run leaves a row in running that nothing closes. For
	// a job marked single run that means it never executes again, so the
	// watchdog is what breaks the deadlock.
	s := fresh(t)
	repo := NewExecRepo(s)

	projectID := seedProject(t, s, "shop", "https://shop.example.com")
	jobID := seedJob(t, s, projectID, "daily")

	var stuck int64
	err := s.db.QueryRow(
		`INSERT INTO job_runs (job_id, trigger, status, started_at)
		 VALUES ($1, 'schedule', 'running', now() - interval '3 hours') RETURNING id`,
		jobID).Scan(&stuck)
	mustNoErr(t, err, "seeding a stuck run")

	var recent int64
	err = s.db.QueryRow(
		`INSERT INTO job_runs (job_id, trigger, status, started_at)
		 VALUES ($1, 'schedule', 'running', now()) RETURNING id`,
		jobID).Scan(&recent)
	mustNoErr(t, err, "seeding a running run")

	closed, err := repo.CloseStuckRuns(ctx(t))
	mustNoErr(t, err, "CloseStuckRuns")
	if closed != 1 {
		t.Errorf("%d runs were closed, want 1", closed)
	}

	var stuckStatus, recentStatus string
	mustNoErr(t, s.db.QueryRow(`SELECT status FROM job_runs WHERE id = $1`, stuck).
		Scan(&stuckStatus), "reading the stuck run")
	mustNoErr(t, s.db.QueryRow(`SELECT status FROM job_runs WHERE id = $1`, recent).
		Scan(&recentStatus), "reading the recent run")

	if stuckStatus == domain.StatusRunning {
		t.Error("the stuck run is still running")
	}
	if recentStatus != domain.StatusRunning {
		t.Error("a run that started moments ago was closed as stuck")
	}
}

func TestPurgeRunsKeepsTheRecentOnes(t *testing.T) {
	s := fresh(t)
	repo := NewExecRepo(s)

	projectID := seedProject(t, s, "shop", "https://shop.example.com")
	jobID := seedJob(t, s, projectID, "daily")

	old := seedRun(t, s, jobID, domain.StatusSuccess, time.Now().AddDate(0, 0, -120))
	recent := seedRun(t, s, jobID, domain.StatusSuccess, time.Now().AddDate(0, 0, -3))

	removed, err := repo.PurgeRuns(ctx(t), 90, 1000)
	mustNoErr(t, err, "PurgeRuns")
	if removed != 1 {
		t.Errorf("%d rows were purged, want 1", removed)
	}

	var count int
	mustNoErr(t, s.db.QueryRow(`SELECT count(*) FROM job_runs WHERE id = $1`, old).
		Scan(&count), "checking the old run")
	if count != 0 {
		t.Error("the run past the retention window is still there")
	}
	mustNoErr(t, s.db.QueryRow(`SELECT count(*) FROM job_runs WHERE id = $1`, recent).
		Scan(&count), "checking the recent run")
	if count != 1 {
		t.Error("a recent run was purged")
	}
}

func TestPurgeAppLogs(t *testing.T) {
	s := fresh(t)
	repo := NewExecRepo(s)

	_, err := s.db.Exec(
		`INSERT INTO app_logs (level, message, created_at) VALUES
			('error', 'old', now() - interval '200 days'),
			('error', 'recent', now())`)
	mustNoErr(t, err, "seeding logs")

	removed, err := repo.PurgeAppLogs(ctx(t), 90)
	mustNoErr(t, err, "PurgeAppLogs")
	if removed != 1 {
		t.Errorf("%d log rows were purged, want 1", removed)
	}
}

func TestListFailingJobs(t *testing.T) {
	// What the watchdog alerts on: a job failing repeatedly inside a window.
	s := fresh(t)
	repo := NewExecRepo(s)

	projectID := seedProject(t, s, "shop", "https://shop.example.com")
	noisy := seedJob(t, s, projectID, "noisy")
	quiet := seedJob(t, s, projectID, "quiet")

	for i := 0; i < 6; i++ {
		seedRun(t, s, noisy, domain.StatusFailed, time.Now().Add(-10*time.Minute))
	}
	seedRun(t, s, quiet, domain.StatusFailed, time.Now().Add(-10*time.Minute))
	// Old failures are outside the window and must not count.
	for i := 0; i < 20; i++ {
		seedRun(t, s, quiet, domain.StatusFailed, time.Now().Add(-48*time.Hour))
	}

	failing, err := repo.ListFailingJobs(ctx(t), time.Hour, 5)
	mustNoErr(t, err, "ListFailingJobs")

	found := map[string]int{}
	for _, f := range failing {
		found[f.Code] = f.Count
	}
	if found["noisy"] < 5 {
		t.Errorf("the failing job was reported with %d failures", found["noisy"])
	}
	if _, ok := found["quiet"]; ok {
		t.Error("a job with one recent failure was reported as failing")
	}
}

func TestSetWarningMarker(t *testing.T) {
	// The repeat guard: the same alarm is not mailed every sweep.
	s := fresh(t)
	repo := NewExecRepo(s)

	mustNoErr(t, repo.SetWarningMarker(ctx(t), "dispatcher-down"), "SetWarningMarker")

	beat, err := repo.ReadHeartbeat(ctx(t))
	mustNoErr(t, err, "ReadHeartbeat")
	if beat.Note != "dispatcher-down" {
		t.Errorf("note = %q", beat.Note)
	}
}

func TestGetRunReadsTheTargetWithItsHeaders(t *testing.T) {
	// What the runner executes from. It reads the target rather than the job,
	// so the two must carry the same rules.
	s := fresh(t)
	repo := NewExecRepo(s)

	projectID := seedProject(t, s, "shop", "https://shop.example.com")
	jobID := seedJob(t, s, projectID, "daily")
	runID := seedRun(t, s, jobID, domain.StatusPending, time.Now())

	// is_secret masks the value on screen; it does not stop the header being
	// sent, because a job whose token is hidden still needs it on the wire.
	_, err := s.db.Exec(
		`INSERT INTO job_headers (job_id, key, value, is_secret)
		 VALUES ($1, 'X-Token', 'secret', true), ($1, 'X-Trace', 'on', false)`, jobID)
	mustNoErr(t, err, "adding headers")

	target, err := repo.GetRun(ctx(t), runID)
	mustNoErr(t, err, "GetRun")
	if target == nil {
		t.Fatal("no target was read")
	}
	if target.Code != "daily" || target.BaseURL != "https://shop.example.com" {
		t.Errorf("target = %+v", target)
	}

	sent := map[string]string{}
	for _, h := range target.Headers {
		sent[h.Key] = h.Value
	}
	if sent["X-Token"] != "secret" {
		t.Errorf("a secret header was not sent: %v", sent)
	}
	if sent["X-Trace"] != "on" {
		t.Errorf("an ordinary header was not sent: %v", sent)
	}
}

// A chain edge's CONDITION and a run's STATUS are two different vocabularies,
// and they nearly overlap, which is the trap. A run ends "failed"; an edge fires
// on "failure". The runner maps between them, and this is the value the column
// actually holds.
const (
	conditionSuccess = "success"
	conditionFailure = "failure"
	conditionAlways  = "always"
)

func TestChainTargetsFollowTheCondition(t *testing.T) {
	// A cleanup that runs after a failure must not also run after a success.
	s := fresh(t)
	repo := NewExecRepo(s)

	projectID := seedProject(t, s, "shop", "https://shop.example.com")
	source := seedJob(t, s, projectID, "source")
	onSuccess := seedJob(t, s, projectID, "on-success")
	onFailure := seedJob(t, s, projectID, "on-failure")
	always := seedJob(t, s, projectID, "always")

	_, err := s.db.Exec(
		`INSERT INTO job_links (job_id, target_job_id, condition, delay_sec, active)
		 VALUES ($1, $2, 'success', 0, true),
		        ($1, $3, 'failure', 30, true),
		        ($1, $4, 'always',  0, true)`,
		source, onSuccess, onFailure, always)
	mustNoErr(t, err, "adding chain edges")

	targetsFor := func(condition string) map[int64]domain.ChainTarget {
		t.Helper()
		rows, err := repo.ListChainTargets(ctx(t), source, condition)
		mustNoErr(t, err, "ListChainTargets")
		out := map[int64]domain.ChainTarget{}
		for _, r := range rows {
			out[r.JobID] = r
		}
		return out
	}

	onOK := targetsFor(conditionSuccess)
	if _, ok := onOK[onSuccess]; !ok {
		t.Error("the success edge did not fire on a success")
	}
	if _, ok := onOK[onFailure]; ok {
		t.Error("the failure edge fired on a success")
	}
	// An "always" edge fires either way, which is what makes it useful for a
	// cleanup that has to happen regardless.
	if _, ok := onOK[always]; !ok {
		t.Error("the always edge did not fire on a success")
	}

	onFail := targetsFor(conditionFailure)
	if _, ok := onFail[onFailure]; !ok {
		t.Error("the failure edge did not fire on a failure")
	}
	if _, ok := onFail[onSuccess]; ok {
		t.Error("the success edge fired on a failure")
	}
	if _, ok := onFail[always]; !ok {
		t.Error("the always edge did not fire on a failure")
	}
	if got := onFail[onFailure].DelaySec; got != 30 {
		t.Errorf("delay = %d, want 30", got)
	}

	// And the run status vocabulary matches nothing, which is exactly why the
	// runner translates rather than passing the status through.
	if got := targetsFor(domain.StatusFailed); len(got) != 1 {
		t.Errorf("the run status %q matched %d edges beyond 'always'; the two "+
			"vocabularies are not interchangeable", domain.StatusFailed, len(got)-1)
	}
}

func TestADeletedOrInactiveChainTargetIsNotFired(t *testing.T) {
	// The edge survives the target being switched off, so it comes back when
	// the target does. Firing it meanwhile would run something switched off.
	s := fresh(t)
	repo := NewExecRepo(s)

	projectID := seedProject(t, s, "shop", "https://shop.example.com")
	source := seedJob(t, s, projectID, "source")
	deleted := seedJob(t, s, projectID, "deleted")

	_, err := s.db.Exec(
		`INSERT INTO job_links (job_id, target_job_id, condition, active)
		 VALUES ($1, $2, 'always', true)`, source, deleted)
	mustNoErr(t, err, "adding a chain edge")

	_, err = s.db.Exec(`UPDATE jobs SET deleted_at = now() WHERE id = $1`, deleted)
	mustNoErr(t, err, "deleting the target")

	targets, err := repo.ListChainTargets(ctx(t), source, conditionSuccess)
	mustNoErr(t, err, "ListChainTargets")
	if len(targets) != 0 {
		t.Errorf("a deleted target is still chained: %+v", targets)
	}
}

func TestAnInactiveChainEdgeDoesNotFire(t *testing.T) {
	s := fresh(t)
	repo := NewExecRepo(s)

	projectID := seedProject(t, s, "shop", "https://shop.example.com")
	source := seedJob(t, s, projectID, "source")
	target := seedJob(t, s, projectID, "target")

	_, err := s.db.Exec(
		`INSERT INTO job_links (job_id, target_job_id, condition, active)
		 VALUES ($1, $2, 'always', false)`, source, target)
	mustNoErr(t, err, "adding a chain edge")

	targets, err := repo.ListChainTargets(ctx(t), source, conditionSuccess)
	mustNoErr(t, err, "ListChainTargets")
	if len(targets) != 0 {
		t.Errorf("an inactive edge fired: %+v", targets)
	}
}

func TestCreateChainRunRecordsItsParent(t *testing.T) {
	// Every chain step is itself a run, so it appears in the history like
	// anything else, and the parent is what makes the chain readable.
	s := fresh(t)
	repo := NewExecRepo(s)

	projectID := seedProject(t, s, "shop", "https://shop.example.com")
	source := seedJob(t, s, projectID, "source")
	target := seedJob(t, s, projectID, "target")
	parent := seedRun(t, s, source, domain.StatusSuccess, time.Now())

	childID, err := repo.CreateChainRun(ctx(t), target, 30, parent)
	mustNoErr(t, err, "CreateChainRun")

	var (
		trigger    string
		parentRun  *int64
		runAfter   *time.Time
		status     string
		plannedMin *time.Time
	)
	err = s.db.QueryRow(
		`SELECT trigger, parent_run_id, run_after, status, planned_minute
		 FROM job_runs WHERE id = $1`, childID).
		Scan(&trigger, &parentRun, &runAfter, &status, &plannedMin)
	mustNoErr(t, err, "reading the chain step")

	if trigger != domain.TriggerChain {
		t.Errorf("trigger = %q, want chain", trigger)
	}
	if parentRun == nil || *parentRun != parent {
		t.Errorf("parent = %v, want %d", parentRun, parent)
	}
	if runAfter == nil || !runAfter.After(time.Now().Add(20*time.Second)) {
		t.Errorf("run_after = %v, want it about thirty seconds out", runAfter)
	}
	// No planned minute: a chain step is not a scheduled minute, and giving it
	// one would collide with the unique index the dispatcher relies on.
	if plannedMin != nil {
		t.Errorf("planned_minute = %v, want NULL for a chain step", plannedMin)
	}
}

func TestChainStepCountBoundsTheDepth(t *testing.T) {
	s := fresh(t)
	repo := NewExecRepo(s)

	projectID := seedProject(t, s, "shop", "https://shop.example.com")
	jobID := seedJob(t, s, projectID, "daily")

	first := seedRun(t, s, jobID, domain.StatusSuccess, time.Now())
	second, err := repo.CreateChainRun(ctx(t), jobID, 0, first)
	mustNoErr(t, err, "CreateChainRun")
	third, err := repo.CreateChainRun(ctx(t), jobID, 0, second)
	mustNoErr(t, err, "CreateChainRun")

	depth, err := repo.ChainStepCount(ctx(t), third)
	mustNoErr(t, err, "ChainStepCount")
	if depth < 2 {
		t.Errorf("depth = %d, want at least 2", depth)
	}
}

func TestAppendRunNote(t *testing.T) {
	// Retries and warnings are appended rather than replacing, so the row says
	// what happened across every attempt.
	s := fresh(t)
	repo := NewExecRepo(s)

	projectID := seedProject(t, s, "shop", "https://shop.example.com")
	jobID := seedJob(t, s, projectID, "daily")
	runID := seedRun(t, s, jobID, domain.StatusPending, time.Now())

	mustNoErr(t, repo.AppendRunNote(ctx(t), runID, "attempt 1 failed"), "AppendRunNote")
	mustNoErr(t, repo.AppendRunNote(ctx(t), runID, "attempt 2 failed"), "AppendRunNote")

	var note string
	mustNoErr(t, s.db.QueryRow(`SELECT error FROM job_runs WHERE id = $1`, runID).
		Scan(&note), "reading it back")

	for _, want := range []string{"attempt 1 failed", "attempt 2 failed"} {
		if !contains(note, want) {
			t.Errorf("the note is missing %q: %q", want, note)
		}
	}
}

func TestWriteJobSummaryUpdatesTheJobsLastRun(t *testing.T) {
	// The list screen reads these off the job rather than joining every run.
	s := fresh(t)
	repo := NewExecRepo(s)

	projectID := seedProject(t, s, "shop", "https://shop.example.com")
	jobID := seedJob(t, s, projectID, "daily")

	at := time.Now().Truncate(time.Second)
	mustNoErr(t, repo.WriteJobSummary(ctx(t), jobID, domain.StatusSuccess, at, 250), "WriteJobSummary")

	var (
		status   string
		lastRun  *time.Time
		duration *int
	)
	err := s.db.QueryRow(
		`SELECT last_status, last_run_at, last_duration_ms FROM jobs WHERE id = $1`, jobID).
		Scan(&status, &lastRun, &duration)
	mustNoErr(t, err, "reading the job back")

	if status != domain.StatusSuccess {
		t.Errorf("last_status = %q", status)
	}
	if lastRun == nil {
		t.Error("last_run_at was not set")
	}
	if duration == nil || *duration != 250 {
		t.Errorf("last_duration_ms = %v", duration)
	}
}

func TestTargetForJobReadsTheRecipients(t *testing.T) {
	s := fresh(t)
	repo := NewExecRepo(s)

	var notificationID int64
	err := s.db.QueryRow(
		`INSERT INTO notifications (name, on_success, on_failure, active)
		 VALUES ('On call', false, true, true) RETURNING id`).Scan(&notificationID)
	mustNoErr(t, err, "seeding a notification")

	_, err = s.db.Exec(
		`INSERT INTO notify_emails (notification_id, email, active)
		 VALUES ($1, 'ops@example.com', true), ($1, 'off@example.com', false)`, notificationID)
	mustNoErr(t, err, "seeding recipients")

	target, err := repo.TargetForJob(ctx(t), notificationID)
	mustNoErr(t, err, "TargetForJob")
	if target == nil {
		t.Fatal("no target was read")
	}
	if !target.OnFailure || target.OnSuccess {
		t.Errorf("target = %+v", target)
	}
	if len(target.Emails) != 1 || target.Emails[0] != "ops@example.com" {
		t.Errorf("emails = %v, want only the active one", target.Emails)
	}
}

func contains(haystack, needle string) bool {
	return strings.Contains(haystack, needle)
}
