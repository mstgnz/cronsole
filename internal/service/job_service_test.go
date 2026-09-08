package service

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mstgnz/cronsole/v2/internal/domain"
)

func fieldErrors(t *testing.T, err error) map[string]string {
	t.Helper()
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("expected a validation error, got %v", err)
	}
	out := map[string]string{}
	for _, fe := range ve.Errors {
		out[fe.Field] = fe.Message
	}
	return out
}

func TestCreateJobInOneSubmission(t *testing.T) {
	// The point of the form: one save produces a job that is ready to run.
	// It used to take a request record, then a schedule, then an activation.
	store := newMemStore()
	store.addProject(1, "demo", "https://service.example.com")
	svc := newTestJobService(store)

	id, err := svc.Create(context.Background(), allProjects, &JobInput{
		ProjectID: 1,
		Code:      "daily-report",
		Name:      "Daily report",
		URL:       "/cron/report",
		Schedules: []string{"0 3 * * *"},
		Headers:   []HeaderIn{{Key: "X-Token", Value: "abc"}},
		Active:    true,
	})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}

	detail, err := svc.Get(context.Background(), allProjects, id)
	if err != nil {
		t.Fatalf("get failed: %v", err)
	}
	if !detail.Job.Active {
		t.Error("the job was not active")
	}
	if len(detail.Schedules) != 1 {
		t.Errorf("schedules = %d, want 1", len(detail.Schedules))
	}
	if len(detail.Headers) != 1 {
		t.Errorf("headers = %d, want 1", len(detail.Headers))
	}
	if len(detail.NextRuns) == 0 {
		t.Error("no next run was computed")
	}
	if detail.ResolvedURL != "https://service.example.com/cron/report" {
		t.Errorf("resolved URL = %q", detail.ResolvedURL)
	}
}

func TestCreateReportsEveryFailureAtOnce(t *testing.T) {
	// A form that reports one problem per submission takes as many round trips
	// as it has mistakes.
	store := newMemStore()
	store.addProject(1, "demo", "")
	svc := newTestJobService(store)

	_, err := svc.Create(context.Background(), allProjects, &JobInput{
		ProjectID:  1,
		Code:       "Bad Code",
		Name:       "",
		Method:     "FETCH",
		URL:        "/relative-with-no-base",
		TimeoutSec: 9999,
		Retries:    99,
		Schedules:  []string{"not a cron"},
	})
	got := fieldErrors(t, err)

	for _, field := range []string{"code", "method", "url", "timeout_sec", "retries", "schedules.0"} {
		if _, ok := got[field]; !ok {
			t.Errorf("no failure reported for %q; got %v", field, got)
		}
	}
}

func TestCreateRejectsADuplicateCode(t *testing.T) {
	store := newMemStore()
	store.addProject(1, "demo", "")
	svc := newTestJobService(store)

	in := func() *JobInput {
		return &JobInput{ProjectID: 1, Code: "twice", Name: "Twice",
			URL: "https://example.com/x", Schedules: []string{"* * * * *"}}
	}
	if _, err := svc.Create(context.Background(), allProjects, in()); err != nil {
		t.Fatalf("first create failed: %v", err)
	}
	_, err := svc.Create(context.Background(), allProjects, in())
	if _, ok := fieldErrors(t, err)["code"]; !ok {
		t.Errorf("the duplicate was not reported on the code field: %v", err)
	}
}

func TestJobWithoutAScheduleIsAllowed(t *testing.T) {
	// Two entirely normal jobs have no schedule of their own: one that runs
	// only as a chain step, and one a project triggers through the API.
	// Refusing them made the chain feature unreachable from the endpoint meant
	// to declare it.
	store := newMemStore()
	store.addProject(1, "demo", "")
	svc := newTestJobService(store)

	id, err := svc.Create(context.Background(), allProjects, &JobInput{
		ProjectID: 1, Code: "chain-only", Name: "Chain only",
		URL: "https://example.com/x", Active: true,
	})
	if err != nil {
		t.Fatalf("a chain-only job was refused: %v", err)
	}

	if job := store.job(id); !job.Active {
		t.Error("the job was not left active")
	}
}

func TestCodeIsNotChangedOnUpdate(t *testing.T) {
	// History, chain links and alert subjects are keyed by the code. Renaming
	// it would orphan the trail that explains what the job did.
	store := newMemStore()
	store.addProject(1, "demo", "")
	svc := newTestJobService(store)

	id, err := svc.Create(context.Background(), allProjects, &JobInput{
		ProjectID: 1, Code: "original", Name: "Original",
		URL: "https://example.com/x", Schedules: []string{"* * * * *"},
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := svc.Update(context.Background(), allProjects, id, &JobInput{
		ProjectID: 1, Code: "renamed", Name: "Renamed",
		URL: "https://example.com/y", Schedules: []string{"* * * * *"},
	}); err != nil {
		t.Fatalf("update failed: %v", err)
	}

	job := store.job(id)
	if job.Code != "original" {
		t.Errorf("code = %q, want it unchanged", job.Code)
	}
	if job.Name != "Renamed" {
		t.Errorf("name = %q, want the update to have applied", job.Name)
	}
}

func TestMaxDurationDefaultsAboveTheTimeout(t *testing.T) {
	// The watchdog closes a run that outlives max_duration_sec. Left equal to
	// the timeout it would close healthy runs, so the default scales with how
	// patient the job already is.
	store := newMemStore()
	store.addProject(1, "demo", "")
	svc := newTestJobService(store)

	id, err := svc.Create(context.Background(), allProjects, &JobInput{
		ProjectID: 1, Code: "slow", Name: "Slow", URL: "https://example.com/x",
		TimeoutSec: 120, Schedules: []string{"* * * * *"},
	})
	if err != nil {
		t.Fatal(err)
	}
	job := store.job(id)
	if job.MaxDurationSec <= job.TimeoutSec {
		t.Errorf("max duration %d is not above the timeout %d", job.MaxDurationSec, job.TimeoutSec)
	}
}

func TestRemovingTheLastScheduleDeactivatesOnlyWhenNothingElseStartsIt(t *testing.T) {
	store := newMemStore()
	store.addProject(1, "demo", "")
	svc := newTestJobService(store)
	ctx := context.Background()

	lonely, _ := svc.Create(ctx, allProjects, &JobInput{ProjectID: 1, Code: "lonely", Name: "Lonely",
		URL: "https://example.com/a", Schedules: []string{"* * * * *"}, Active: true})
	chained, _ := svc.Create(ctx, allProjects, &JobInput{ProjectID: 1, Code: "chained", Name: "Chained",
		URL: "https://example.com/b", Schedules: []string{"* * * * *"}, Active: true})
	source, _ := svc.Create(ctx, allProjects, &JobInput{ProjectID: 1, Code: "source", Name: "Source",
		URL: "https://example.com/c", Schedules: []string{"* * * * *"}, Active: true})

	if err := svc.AddLink(ctx, allProjects, source, chained, "success", 5); err != nil {
		t.Fatalf("link failed: %v", err)
	}

	for _, id := range []int64{lonely, chained} {
		schedules, _ := memJobRepo{store}.ListSchedules(ctx, id)
		if len(schedules) != 1 {
			t.Fatalf("job %d has %d schedules", id, len(schedules))
		}
		if err := svc.RemoveSchedule(ctx, allProjects, id, schedules[0].ID); err != nil {
			t.Fatalf("remove failed: %v", err)
		}
	}

	if job := store.job(lonely); job.Active {
		t.Error("a job with nothing left to start it stayed active")
	}
	if job := store.job(chained); !job.Active {
		t.Error("a chain target was deactivated, which quietly breaks the chain that depends on it")
	}
}

func TestChainLinkRules(t *testing.T) {
	store := newMemStore()
	store.addProject(1, "demo", "")
	svc := newTestJobService(store)
	ctx := context.Background()

	// Codes are at least two characters, which ValidSlug enforces. Ignoring
	// the error here would have every id come back as zero and turn the first
	// assertion into a self link, which is exactly what happened once.
	create := func(code string) int64 {
		id, err := svc.Create(ctx, allProjects, &JobInput{
			ProjectID: 1, Code: code, Name: strings.ToUpper(code),
			URL: "https://example.com/" + code,
		})
		if err != nil {
			t.Fatalf("create %q failed: %v", code, err)
		}
		return id
	}
	a, b, c := create("job-a"), create("job-b"), create("job-c")

	if err := svc.AddLink(ctx, allProjects, a, a, "success", 5); !errors.Is(err, ErrSelfLink) {
		t.Errorf("a self link gave %v, want ErrSelfLink", err)
	}

	if err := svc.AddLink(ctx, allProjects, a, b, "success", 5); err != nil {
		t.Fatalf("a -> b failed: %v", err)
	}
	if err := svc.AddLink(ctx, allProjects, b, c, "success", 5); err != nil {
		t.Fatalf("b -> c failed: %v", err)
	}

	// c -> a would close the cycle. The runner's depth cap would stop the loop
	// anyway, but it stops it silently and days later.
	if err := svc.AddLink(ctx, allProjects, c, a, "success", 5); !errors.Is(err, ErrChainLoop) {
		t.Errorf("a cycle gave %v, want ErrChainLoop", err)
	}

	if err := svc.AddLink(ctx, allProjects, a, b, "success", 5); err == nil {
		t.Error("a duplicate link was accepted")
	}
	if err := svc.AddLink(ctx, allProjects, a, c, "whenever", 5); err == nil {
		t.Error("an unknown condition was accepted")
	}
	if err := svc.AddLink(ctx, allProjects, a, c, "success", domain.ChainMaxDelaySec+1); err == nil {
		t.Error("a delay past the cap was accepted")
	}
}

func TestCloneIsInactiveAndCopiesEverything(t *testing.T) {
	// Copying a job and having it start firing on the same schedule as the
	// original, immediately, is never what was meant.
	store := newMemStore()
	store.addProject(1, "demo", "")
	svc := newTestJobService(store)
	ctx := context.Background()

	id, err := svc.Create(ctx, allProjects, &JobInput{
		ProjectID: 1, Code: "original", Name: "Original", URL: "https://example.com/x",
		Schedules: []string{"0 3 * * *", "0 15 * * *"},
		Headers:   []HeaderIn{{Key: "X-Token", Value: "abc"}},
		Active:    true, TimeoutSec: 90,
	})
	if err != nil {
		t.Fatal(err)
	}

	cloneID, err := svc.Clone(ctx, allProjects, id, "original-2")
	if err != nil {
		t.Fatalf("clone failed: %v", err)
	}

	clone, err := svc.Get(ctx, allProjects, cloneID)
	if err != nil {
		t.Fatal(err)
	}
	if clone.Job.Active {
		t.Error("the clone was created active")
	}
	if len(clone.Schedules) != 2 {
		t.Errorf("the clone has %d schedules, want 2", len(clone.Schedules))
	}
	if len(clone.Headers) != 1 {
		t.Errorf("the clone has %d headers, want 1", len(clone.Headers))
	}
	if clone.Job.TimeoutSec != 90 {
		t.Errorf("the clone's timeout is %d, want 90", clone.Job.TimeoutSec)
	}
}

func TestPreviewSchedule(t *testing.T) {
	svc := newTestJobService(newMemStore())

	good := svc.PreviewSchedule(" */5  * * * * ")
	if !good.Valid {
		t.Fatalf("a valid expression was rejected: %s", good.Error)
	}
	if good.Expression != "*/5 * * * *" {
		t.Errorf("expression = %q, want it normalised", good.Expression)
	}
	if good.Description == "" || len(good.NextRuns) == 0 {
		t.Error("the preview carried no description or next runs")
	}

	bad := svc.PreviewSchedule("0 3 * *")
	if bad.Valid {
		t.Error("a four field expression was accepted")
	}
	if bad.Error == "" {
		t.Error("no reason was given for the rejection")
	}
}

func TestNextRunForPicksTheEarliest(t *testing.T) {
	// A job with several expressions runs at whichever fires first; showing
	// the first expression's answer instead would be wrong most of the time.
	svc := newTestJobService(newMemStore())
	next := svc.NextRunFor([]string{"0 3 * * *", "* * * * *"})
	if next == nil {
		t.Fatal("no next run")
	}
	if until := time.Until(*next); until > time.Minute+time.Second {
		t.Errorf("the next run is %s away; the every-minute schedule should win", until)
	}
}
