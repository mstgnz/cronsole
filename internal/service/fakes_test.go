package service

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/mstgnz/cronsole/v2/internal/applog"
	"github.com/mstgnz/cronsole/v2/internal/domain"
)

// The fakes here are in-memory implementations of the narrow execution
// interfaces, not mocks that record calls. They behave like the real thing for
// the parts under test, so a test that passes against them is testing
// behaviour rather than a call sequence: a fake QueueRun really does reject a
// duplicate minute, and a fake ClaimRun really is exclusive.

func testLogger() *applog.Logger { return applog.New() }

type queuedRun struct {
	jobID  int64
	minute time.Time
}

// fakeDispatchRepo implements domain.DispatchRepository.
type fakeDispatchRepo struct {
	mu sync.Mutex

	now       time.Time
	heartbeat *domain.Heartbeat
	jobs      []domain.ScheduledJob
	schedules map[int64][]string

	queued      []queuedRun
	pending     []domain.PendingRun
	running     int
	runningJobs map[int64]bool
	skipped     []int64

	touched  int
	queueErr error
	timeErr  error
}

func newDispatchRepo(now time.Time) *fakeDispatchRepo {
	return &fakeDispatchRepo{
		now:         now,
		schedules:   map[int64][]string{},
		runningJobs: map[int64]bool{},
	}
}

func (f *fakeDispatchRepo) DBTime(context.Context) (time.Time, error) {
	if f.timeErr != nil {
		return time.Time{}, f.timeErr
	}
	return f.now, nil
}

func (f *fakeDispatchRepo) ReadHeartbeat(context.Context) (*domain.Heartbeat, error) {
	return f.heartbeat, nil
}

func (f *fakeDispatchRepo) TouchHeartbeat(_ context.Context, dbTime, appTime time.Time, drift int, instance string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.touched++
	f.heartbeat = &domain.Heartbeat{LastRun: dbTime, AppTime: &appTime, DriftSec: drift, InstanceID: instance}
	return nil
}

func (f *fakeDispatchRepo) ListScheduledJobs(context.Context) ([]domain.ScheduledJob, error) {
	return f.jobs, nil
}

func (f *fakeDispatchRepo) ListActiveSchedules(context.Context) (map[int64][]string, error) {
	return f.schedules, nil
}

// QueueRun refuses a second row for the same job and minute, which is what the
// unique index does in the database. A fake that accepted duplicates would let
// a test pass while production deduplicated.
func (f *fakeDispatchRepo) QueueRun(_ context.Context, jobID int64, minute time.Time) error {
	if f.queueErr != nil {
		return f.queueErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, q := range f.queued {
		if q.jobID == jobID && q.minute.Equal(minute) {
			return nil
		}
	}
	f.queued = append(f.queued, queuedRun{jobID: jobID, minute: minute})
	return nil
}

func (f *fakeDispatchRepo) CountRunning(context.Context) (int, error) { return f.running, nil }

func (f *fakeDispatchRepo) RunningJobIDs(context.Context) (map[int64]bool, error) {
	out := map[int64]bool{}
	for k, v := range f.runningJobs {
		out[k] = v
	}
	return out, nil
}

func (f *fakeDispatchRepo) ListPending(_ context.Context, _ time.Time, limit int) ([]domain.PendingRun, error) {
	if limit > len(f.pending) {
		limit = len(f.pending)
	}
	return f.pending[:limit], nil
}

func (f *fakeDispatchRepo) SkipRun(_ context.Context, runID int64, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.skipped = append(f.skipped, runID)
	return nil
}

func (f *fakeDispatchRepo) queuedFor(jobID int64) []time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []time.Time
	for _, q := range f.queued {
		if q.jobID == jobID {
			out = append(out, q.minute)
		}
	}
	return out
}

// fakeRunRepo implements domain.RunRepositoryExec.
type fakeRunRepo struct {
	mu sync.Mutex

	target  *domain.RunTarget
	claimed map[int64]bool
	results map[int64]domain.RunResult
	notes   map[int64][]string
	skips   map[int64]string

	runningForJob int
	runningTotal  int

	chainTargets  map[string][]domain.ChainTarget
	chainStep     int
	chainCreated  []int64
	chainSkipped  []int64
	summaryWrites int

	nextChainID int64
	getErr      error
	summaryErr  error
}

func newRunRepo(target *domain.RunTarget) *fakeRunRepo {
	return &fakeRunRepo{
		target:       target,
		claimed:      map[int64]bool{},
		results:      map[int64]domain.RunResult{},
		notes:        map[int64][]string{},
		skips:        map[int64]string{},
		chainTargets: map[string][]domain.ChainTarget{},
		nextChainID:  1000,
	}
}

func (f *fakeRunRepo) GetRun(_ context.Context, runID int64) (*domain.RunTarget, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	if f.target == nil || f.target.ID != runID {
		return nil, nil
	}
	copied := *f.target
	return &copied, nil
}

func (f *fakeRunRepo) CountRunningForJob(context.Context, int64, int64) (int, error) {
	return f.runningForJob, nil
}

// ClaimRun is exclusive, exactly as the single UPDATE ... WHERE status =
// 'pending' is. The second caller for the same run gets false.
func (f *fakeRunRepo) ClaimRun(_ context.Context, runID int64, _ string, _ time.Time) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.claimed[runID] {
		return false, nil
	}
	f.claimed[runID] = true
	return true, nil
}

func (f *fakeRunRepo) FinishRun(_ context.Context, runID int64, result domain.RunResult) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.results[runID] = result
	return nil
}

func (f *fakeRunRepo) WriteJobSummary(context.Context, int64, string, time.Time, int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.summaryWrites++
	return f.summaryErr
}

func (f *fakeRunRepo) AppendRunNote(_ context.Context, runID int64, message string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.notes[runID] = append(f.notes[runID], message)
	return nil
}

func (f *fakeRunRepo) SkipRun(_ context.Context, runID int64, reason string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.skips[runID] = reason
	return nil
}

func (f *fakeRunRepo) CountRunning(context.Context) (int, error) { return f.runningTotal, nil }

func (f *fakeRunRepo) ListChainTargets(_ context.Context, _ int64, outcome string) ([]domain.ChainTarget, error) {
	return f.chainTargets[outcome], nil
}

func (f *fakeRunRepo) ChainStepCount(context.Context, int64) (int, error) { return f.chainStep, nil }

func (f *fakeRunRepo) CreateChainRun(_ context.Context, jobID int64, _ int, _ int64) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextChainID++
	f.chainCreated = append(f.chainCreated, jobID)
	return f.nextChainID, nil
}

func (f *fakeRunRepo) CreateChainSkipped(_ context.Context, jobID int64, _ int64, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.chainSkipped = append(f.chainSkipped, jobID)
	return nil
}

func (f *fakeRunRepo) result(runID int64) domain.RunResult {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.results[runID]
}

// fakeNotifyRepo implements domain.NotifyRepository.
type fakeNotifyRepo struct{ target *domain.NotifyTarget }

func (f *fakeNotifyRepo) TargetForJob(context.Context, int64) (*domain.NotifyTarget, error) {
	return f.target, nil
}

// fakeGrants implements GrantRevoker and records who was stripped of access.
type fakeGrants struct {
	mu      sync.Mutex
	revoked []int64
}

func (f *fakeGrants) RevokeUser(_ context.Context, userID int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.revoked = append(f.revoked, userID)
	return nil
}

func (f *fakeGrants) was(userID int64) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, id := range f.revoked {
		if id == userID {
			return true
		}
	}
	return false
}

// allProjects is the scope a platform administrator carries. Most tests here
// are about the rule under test rather than about who may see it, so they use
// this; the scope rules have their own tests.
var allProjects = domain.AllProjects()

// errFake marks an injected failure in a test.
var errFake = errors.New("injected failure")
