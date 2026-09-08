package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mstgnz/cronsole/v2/internal/domain"
)

// errBroken is the database refusing, whatever the reason. What the runner does
// then is the subject; which SQLSTATE it was is not.
var errBroken = errors.New("pq: server closed the connection unexpectedly")

func testTarget(url string) *domain.RunTarget {
	return &domain.RunTarget{
		ID:          1,
		JobID:       10,
		Status:      domain.StatusPending,
		Code:        "job",
		Name:        "Job",
		ProjectID:   1,
		ProjectSlug: "demo",
		Method:      http.MethodGet,
		URL:         url,
		TimeoutSec:  5,
		SuccessMin:  200,
		SuccessMax:  299,
	}
}

func newTestRunner(repo domain.RunRepositoryExec, dispatch DispatchFunc) *Runner {
	return NewRunner(repo, &fakeNotifyRepo{}, testLogger(), RunnerConfig{
		Policy:       TargetPolicy{AllowPrivate: true},
		UserAgent:    "cronsole-test",
		MaxBodyBytes: 4096,
		InstanceID:   "test-instance",
		Dispatch:     dispatch,
	})
}

func TestRunSuccess(t *testing.T) {
	var gotHeaders http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeaders = r.Header.Clone()
		_, _ = io.WriteString(w, "all good")
	}))
	defer server.Close()

	target := testTarget(server.URL)
	target.Headers = []domain.JobHeader{{Key: "X-Api-Key", Value: "secret"}}
	repo := newRunRepo(target)

	result, ran := newTestRunner(repo, nil).Run(context.Background(), 1)
	if !ran {
		t.Fatal("Run reported that it did not run")
	}
	if result.Status != domain.StatusSuccess {
		t.Errorf("status = %q, want success", result.Status)
	}
	if result.HTTPStatus != 200 {
		t.Errorf("http status = %d", result.HTTPStatus)
	}
	if result.Output != "all good" {
		t.Errorf("output = %q", result.Output)
	}
	if gotHeaders.Get("X-Api-Key") != "secret" {
		t.Error("the job's header was not sent")
	}
	// These identify the caller to the target, which is what lets a service
	// tell a scheduled call apart from a user request in its own logs.
	if gotHeaders.Get("X-Cron-Job") != "job" {
		t.Error("X-Cron-Job was not sent")
	}
	if gotHeaders.Get("User-Agent") != "cronsole-test" {
		t.Errorf("User-Agent = %q", gotHeaders.Get("User-Agent"))
	}

	// The outcome has to reach the row, not just the return value.
	if written := repo.result(1); written.Status != domain.StatusSuccess {
		t.Errorf("the written result was %q", written.Status)
	}
	if repo.summaryWrites != 1 {
		t.Errorf("summary writes = %d, want 1", repo.summaryWrites)
	}
}

func TestRunSuccessRangeIsPerJob(t *testing.T) {
	// A redirect is normal for one job and a fault for another, which is why
	// the range is a column rather than a constant.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusFound)
	}))
	defer server.Close()

	strict := testTarget(server.URL)
	strict.SuccessMax = 299
	if result, _ := newTestRunner(newRunRepo(strict), nil).Run(context.Background(), 1); result.Status != domain.StatusFailed {
		t.Errorf("with a 200-299 range a 302 gave %q, want failed", result.Status)
	}

	lenient := testTarget(server.URL)
	lenient.SuccessMax = 399
	if result, _ := newTestRunner(newRunRepo(lenient), nil).Run(context.Background(), 1); result.Status != domain.StatusSuccess {
		t.Errorf("with a 200-399 range a 302 gave %q, want success", result.Status)
	}
}

func TestRunDoesNotFollowRedirects(t *testing.T) {
	// Following one would record the wrong outcome, and would also let a
	// target on an allowed host land on a host that is not.
	final := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "followed")
	}))
	defer final.Close()

	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, final.URL, http.StatusFound)
	}))
	defer redirect.Close()

	target := testTarget(redirect.URL)
	target.SuccessMax = 399
	result, _ := newTestRunner(newRunRepo(target), nil).Run(context.Background(), 1)
	if result.HTTPStatus != http.StatusFound {
		t.Errorf("http status = %d, want the 302 itself", result.HTTPStatus)
	}
	if strings.Contains(result.Output, "followed") {
		t.Error("the redirect was followed")
	}
}

func TestRunTimeoutIsNotAFailure(t *testing.T) {
	// A timeout means the client gave up, not that the target failed. It gets
	// its own status because the two need opposite fixes, and because a
	// dependent job must not run on data that may be half written.
	block := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-block
	}))
	defer func() { close(block); server.Close() }()

	target := testTarget(server.URL)
	target.TimeoutSec = 1
	repo := newRunRepo(target)
	repo.chainTargets["success"] = []domain.ChainTarget{{JobID: 20, Code: "next", Active: true}}

	result, ran := newTestRunner(repo, nil).Run(context.Background(), 1)
	if !ran {
		t.Fatal("Run reported that it did not run")
	}
	if result.Status != domain.StatusTimeout {
		t.Errorf("status = %q, want timeout", result.Status)
	}
	if len(repo.chainCreated) != 0 {
		t.Error("a timeout triggered the chain; whether the target finished is unknown")
	}
}

func TestRunRetriesFailuresButNotTimeouts(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) < 3 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = io.WriteString(w, "recovered")
	}))
	defer server.Close()

	target := testTarget(server.URL)
	target.Retries = 2
	result, _ := newTestRunner(newRunRepo(target), nil).Run(context.Background(), 1)

	if result.Status != domain.StatusSuccess {
		t.Errorf("status = %q, want success on the third attempt", result.Status)
	}
	if result.Attempt != 3 {
		t.Errorf("attempt = %d, want 3", result.Attempt)
	}
	if got := attempts.Load(); got != 3 {
		t.Errorf("the target was called %d times, want 3", got)
	}
}

func TestRunGivesUpAfterTheRetries(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer server.Close()

	target := testTarget(server.URL)
	target.Retries = 1
	result, _ := newTestRunner(newRunRepo(target), nil).Run(context.Background(), 1)

	if result.Status != domain.StatusFailed {
		t.Errorf("status = %q, want failed", result.Status)
	}
	if got := attempts.Load(); got != 2 {
		t.Errorf("the target was called %d times, want 2 (one attempt plus one retry)", got)
	}
}

func TestClaimIsExclusive(t *testing.T) {
	// This is the only thing preventing a run from executing twice. Nothing
	// above it in the stack repeats the check.
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = io.WriteString(w, "ok")
	}))
	defer server.Close()

	repo := newRunRepo(testTarget(server.URL))
	runner := newTestRunner(repo, nil)

	var wg sync.WaitGroup
	ran := make([]bool, 8)
	for i := range ran {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, ok := runner.Run(context.Background(), 1)
			ran[i] = ok
		}(i)
	}
	wg.Wait()

	winners := 0
	for _, ok := range ran {
		if ok {
			winners++
		}
	}
	if winners != 1 {
		t.Errorf("%d callers ran the same job, want exactly 1", winners)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("the target was called %d times, want 1", got)
	}
}

func TestSingleRunSkipsInTheRunner(t *testing.T) {
	// The dispatcher checks this too, but a chain step never passes through
	// the dispatcher. Without the check here, a chained run can execute
	// alongside the same job's scheduled run.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("the target was called while a previous run was going")
	}))
	defer server.Close()

	target := testTarget(server.URL)
	target.SingleRun = true
	repo := newRunRepo(target)
	repo.runningForJob = 1

	result, ran := newTestRunner(repo, nil).Run(context.Background(), 1)
	if ran {
		t.Error("Run reported that it ran")
	}
	if result.Status != domain.StatusSkipped {
		t.Errorf("status = %q, want skipped", result.Status)
	}
	if _, ok := repo.skips[1]; !ok {
		t.Error("the skip was not recorded on the row")
	}
}

func TestARunIsNotExecutedWhenTheDatabaseCannotBeTrusted(t *testing.T) {
	// Every one of these leaves the runner unable to prove something it is
	// about to depend on: which job this is, that nothing else holds it, that
	// nobody else is already running it. Executing anyway is how a job runs
	// twice, and for anything that writes, twice is the expensive direction.
	cases := []struct {
		name   string
		break_ func(*fakeRunRepo)
	}{
		{"the run row cannot be read", func(f *fakeRunRepo) { f.getErr = errBroken }},
		{"the claim cannot be made", func(f *fakeRunRepo) { f.claimErr = errBroken }},
		{"the single run check fails", func(f *fakeRunRepo) { f.runningErr = errBroken }},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				calls.Add(1)
			}))
			defer server.Close()

			target := testTarget(server.URL)
			target.SingleRun = true // so the running check is reached at all
			repo := newRunRepo(target)
			c.break_(repo)

			_, ran := newTestRunner(repo, nil).Run(context.Background(), 1)
			if ran {
				t.Error("Run reported that it ran")
			}
			if got := calls.Load(); got != 0 {
				t.Errorf("the target was called %d times, want none", got)
			}
		})
	}
}

func TestAResultThatCannotBeWrittenStopsTheChain(t *testing.T) {
	// A chain built on a result nobody recorded is a silent data fault: the
	// next job runs, its own row points at a parent that says nothing, and the
	// screen cannot explain why any of it happened.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer server.Close()

	repo := newRunRepo(testTarget(server.URL))
	repo.chainTargets["success"] = []domain.ChainTarget{{JobID: 20, Code: "next", Active: true}}
	repo.finishErr = errBroken

	if _, ran := newTestRunner(repo, nil).Run(context.Background(), 1); !ran {
		t.Fatal("Run reported that it did not run: the request itself succeeded")
	}
	if len(repo.chainCreated) != 0 {
		t.Errorf("%d chain step(s) were queued on an unwritten result", len(repo.chainCreated))
	}
	if repo.summaryWrites != 0 {
		t.Error("the job summary was refreshed from a result that was never written")
	}
}

func TestATransportTimeoutIsRecognisedAsOne(t *testing.T) {
	// Not every timeout arrives as a context deadline. A dial or a TLS
	// handshake that gives up produces a net.Error instead, and it matters
	// which one it is read as: a timeout is recorded as a timeout and is not
	// retried, while a failure is retried and would double the load on a target
	// that is already struggling.
	timedOut := &net.DNSError{Err: "i/o timeout", IsTimeout: true}
	if !isTimeout(timedOut) {
		t.Error("a net timeout was not recognised")
	}
	if !isTimeout(fmt.Errorf("get %q: %w", "http://example.invalid", timedOut)) {
		t.Error("a wrapped net timeout was not recognised")
	}
	if isTimeout(errors.New("connection refused")) {
		t.Error("an ordinary failure was read as a timeout")
	}
}

func TestRunRecordsAnUnreachableTarget(t *testing.T) {
	// A target that cannot be resolved is a failure with an explanation, not a
	// silent no-op: the row still has to close.
	target := testTarget("not-a-url")
	repo := newRunRepo(target)

	result, ran := newTestRunner(repo, nil).Run(context.Background(), 1)
	if !ran {
		t.Fatal("Run reported that it did not run")
	}
	if result.Status != domain.StatusFailed {
		t.Errorf("status = %q, want failed", result.Status)
	}
	if written := repo.result(1); written.Status != domain.StatusFailed || written.Error == "" {
		t.Errorf("the row was closed as %q with error %q", written.Status, written.Error)
	}
}

func TestOutputIsBounded(t *testing.T) {
	// A target answering with a large page must not be able to grow this
	// process. The limit applies at read time, not after.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, strings.Repeat("x", 100000))
	}))
	defer server.Close()

	result, _ := newTestRunner(newRunRepo(testTarget(server.URL)), nil).Run(context.Background(), 1)
	if len(result.Output) > 4096 {
		t.Errorf("output was %d bytes, want at most the 4096 byte read limit", len(result.Output))
	}
}

func TestChainTriggersOnSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	defer server.Close()

	repo := newRunRepo(testTarget(server.URL))
	repo.chainTargets["success"] = []domain.ChainTarget{
		{JobID: 20, Code: "next", Active: true, DelaySec: 0},
	}

	handed := make(chan int64, 1)
	runner := newTestRunner(repo, func(runID int64, _ string) { handed <- runID })

	if _, ran := runner.Run(context.Background(), 1); !ran {
		t.Fatal("Run reported that it did not run")
	}
	if len(repo.chainCreated) != 1 || repo.chainCreated[0] != 20 {
		t.Errorf("chain rows created for %v, want job 20", repo.chainCreated)
	}

	select {
	case <-handed:
	case <-time.After(3 * time.Second):
		t.Error("the chain step was never handed over")
	}
	runner.Close()
}

func TestChainSkipsAnInactiveTargetButRecordsIt(t *testing.T) {
	// A step that vanishes without a trace is far harder to notice than one
	// that failed, so an inactive target still produces a row.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	defer server.Close()

	repo := newRunRepo(testTarget(server.URL))
	repo.chainTargets["success"] = []domain.ChainTarget{{JobID: 20, Code: "off", Active: false}}

	newTestRunner(repo, nil).Run(context.Background(), 1)

	if len(repo.chainCreated) != 0 {
		t.Error("an inactive target was queued")
	}
	if len(repo.chainSkipped) != 1 || repo.chainSkipped[0] != 20 {
		t.Errorf("chain skips = %v, want a recorded skip for job 20", repo.chainSkipped)
	}
}

func TestChainStopsAtTheDepthCap(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	defer server.Close()

	repo := newRunRepo(testTarget(server.URL))
	repo.chainStep = domain.ChainMaxDepth
	repo.chainTargets["success"] = []domain.ChainTarget{{JobID: 20, Code: "deep", Active: true}}

	newTestRunner(repo, nil).Run(context.Background(), 1)

	if len(repo.chainCreated) != 0 {
		t.Error("the chain continued past the depth cap")
	}
	if len(repo.chainSkipped) != 1 {
		t.Error("stopping at the cap left no record")
	}
}

func TestChainOnFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	repo := newRunRepo(testTarget(server.URL))
	repo.chainTargets["failure"] = []domain.ChainTarget{{JobID: 30, Code: "cleanup", Active: true}}
	repo.chainTargets["success"] = []domain.ChainTarget{{JobID: 20, Code: "next", Active: true}}

	newTestRunner(repo, nil).Run(context.Background(), 1)

	if len(repo.chainCreated) != 1 || repo.chainCreated[0] != 30 {
		t.Errorf("chain rows created for %v, want only the failure branch (job 30)", repo.chainCreated)
	}
}

func TestLongChainDelayIsLeftToTheDispatcher(t *testing.T) {
	// Holding a timer in process for minutes buys nothing and is lost on
	// restart. The row is already durable with run_after set.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	defer server.Close()

	repo := newRunRepo(testTarget(server.URL))
	repo.chainTargets["success"] = []domain.ChainTarget{
		{JobID: 20, Code: "later", Active: true, DelaySec: domain.ChainDirectMaxSec + 60},
	}

	handed := make(chan int64, 1)
	runner := newTestRunner(repo, func(runID int64, _ string) { handed <- runID })
	runner.Run(context.Background(), 1)

	if len(repo.chainCreated) != 1 {
		t.Fatalf("chain rows created = %v, want one", repo.chainCreated)
	}
	select {
	case id := <-handed:
		t.Errorf("run %d was handed over directly despite a long delay", id)
	case <-time.After(300 * time.Millisecond):
	}
	// The reason is written on the row, so the operator can see why it is
	// waiting rather than assuming it was lost.
	if notes := repo.notes[1001]; len(notes) == 0 {
		t.Error("no note explains why the hand-off was left to the dispatcher")
	}
	runner.Close()
}

func TestSummaryFailureIsAppendedNotSwallowed(t *testing.T) {
	// The summary is informational, so its failure must not fail the run. It
	// must not vanish either: the reason is appended to the run's error text,
	// where it shows up next to the run on screen.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	defer server.Close()

	repo := newRunRepo(testTarget(server.URL))
	repo.summaryErr = errFake

	result, ran := newTestRunner(repo, nil).Run(context.Background(), 1)
	if !ran || result.Status != domain.StatusSuccess {
		t.Errorf("the run should still have succeeded, got ran=%v status=%q", ran, result.Status)
	}
	if notes := repo.notes[1]; len(notes) == 0 {
		t.Error("the summary failure was swallowed")
	}
}

func TestClosedTimersDoNotFire(t *testing.T) {
	// Starting work while the process is closing is worse than not starting
	// it. The row stays pending and the dispatcher takes it on the next tick.
	fired := make(chan int64, 1)
	timers := &chainTimers{
		timers:   map[int64]*time.Timer{},
		dispatch: func(runID int64, _ string) { fired <- runID },
	}

	if !timers.schedule(50*time.Millisecond, 7, "job") {
		t.Fatal("schedule refused while open")
	}
	if stopped := timers.close(); stopped != 1 {
		t.Errorf("close stopped %d timers, want 1", stopped)
	}
	select {
	case id := <-fired:
		t.Errorf("run %d fired after close", id)
	case <-time.After(200 * time.Millisecond):
	}

	if timers.schedule(time.Millisecond, 8, "job") {
		t.Error("schedule accepted work after close")
	}
}
