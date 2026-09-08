package service

import (
	"context"
	"errors"
	"testing"

	"github.com/mstgnz/cronsole/v2/internal/domain"
)

// twoProjects sets up projects 1 and 2 with one job each, and returns the job
// service, the store behind it and the two job ids.
func twoProjects(t *testing.T) (*JobService, *memStore, int64, int64) {
	t.Helper()

	store := newMemStore()
	store.addProject(1, "alpha", "")
	store.addProject(2, "beta", "")
	svc := newTestJobService(store)
	ctx := context.Background()

	alpha, err := svc.Create(ctx, allProjects, &JobInput{
		ProjectID: 1, Code: "alpha-job", Name: "Alpha", URL: "https://alpha.example.com/x",
		Schedules: []string{"* * * * *"}, Active: true,
	})
	if err != nil {
		t.Fatalf("creating the alpha job failed: %v", err)
	}
	beta, err := svc.Create(ctx, allProjects, &JobInput{
		ProjectID: 2, Code: "beta-job", Name: "Beta", URL: "https://beta.example.com/x",
		Schedules: []string{"* * * * *"}, Active: true,
	})
	if err != nil {
		t.Fatalf("creating the beta job failed: %v", err)
	}
	return svc, store, alpha, beta
}

func TestOutOfScopeJobAnswersNotFound(t *testing.T) {
	// ErrNotFound, never ErrForbidden. Telling somebody that a job exists but is
	// not theirs confirms an id, and an id is the only thing needed to walk the
	// rest of the table.
	svc, store, _, beta := twoProjects(t)
	ctx := context.Background()
	onlyAlpha := domain.ScopeOf(1)

	if _, err := svc.Get(ctx, onlyAlpha, beta); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get gave %v, want ErrNotFound", err)
	}
	if err := svc.Delete(ctx, onlyAlpha, beta); !errors.Is(err, ErrNotFound) {
		t.Errorf("Delete gave %v, want ErrNotFound", err)
	}
	if err := svc.SetActive(ctx, onlyAlpha, beta, false); !errors.Is(err, ErrNotFound) {
		t.Errorf("SetActive gave %v, want ErrNotFound", err)
	}
	if _, err := svc.AddSchedule(ctx, onlyAlpha, beta, "0 4 * * *"); !errors.Is(err, ErrNotFound) {
		t.Errorf("AddSchedule gave %v, want ErrNotFound", err)
	}
	if _, err := svc.Clone(ctx, onlyAlpha, beta, "beta-copy"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Clone gave %v, want ErrNotFound", err)
	}
	if err := svc.RemoveLink(ctx, onlyAlpha, beta, 1); !errors.Is(err, ErrNotFound) {
		t.Errorf("RemoveLink gave %v, want ErrNotFound", err)
	}

	// The job was not touched by any of it.
	if job := store.job(beta); job == nil || !job.Active {
		t.Error("a job outside the scope was modified")
	}
}

func TestEmptyScopeReachesNoJob(t *testing.T) {
	// The zero value again, this time through the service. Somebody with no
	// grants sees nothing rather than everything.
	svc, _, alpha, _ := twoProjects(t)
	ctx := context.Background()

	if _, err := svc.Get(ctx, domain.NoProjects(), alpha); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get with an empty scope gave %v, want ErrNotFound", err)
	}
	var unresolved domain.ProjectScope
	if _, err := svc.Get(ctx, unresolved, alpha); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get with an unresolved scope gave %v, want ErrNotFound", err)
	}
}

func TestUpdateChecksBothTheStoredAndTheTargetProject(t *testing.T) {
	// The form carries a project field, so an update can move a job between
	// projects. Checking only one end leaves a hole in whichever direction was
	// not checked.
	svc, _, alpha, beta := twoProjects(t)
	ctx := context.Background()

	// Pushing a job INTO a project the caller cannot reach.
	err := svc.Update(ctx, domain.ScopeOf(1), alpha, &JobInput{
		ProjectID: 2, Name: "Moved", URL: "https://beta.example.com/x",
		Schedules: []string{"* * * * *"},
	})
	if !errors.Is(err, ErrForbidden) {
		t.Errorf("moving a job into an unreachable project gave %v, want ErrForbidden", err)
	}

	// Pulling a job OUT of a project the caller cannot reach.
	err = svc.Update(ctx, domain.ScopeOf(1), beta, &JobInput{
		ProjectID: 1, Name: "Stolen", URL: "https://alpha.example.com/x",
		Schedules: []string{"* * * * *"},
	})
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("taking a job out of an unreachable project gave %v, want ErrNotFound", err)
	}

	// Somebody who holds both may move it, which is the point of allowing the
	// field at all.
	if err := svc.Update(ctx, domain.ScopeOf(1, 2), alpha, &JobInput{
		ProjectID: 2, Name: "Moved", URL: "https://beta.example.com/x",
		Schedules: []string{"* * * * *"},
	}); err != nil {
		t.Errorf("a caller holding both projects could not move the job: %v", err)
	}
}

func TestCreateRefusesAProjectOutsideTheScope(t *testing.T) {
	svc, _, _, _ := twoProjects(t)
	ctx := context.Background()

	_, err := svc.Create(ctx, domain.ScopeOf(1), &JobInput{
		ProjectID: 2, Code: "smuggled", Name: "Smuggled",
		URL: "https://beta.example.com/y", Schedules: []string{"* * * * *"},
	})
	if err == nil {
		t.Fatal("a job was created in a project outside the caller's scope")
	}
}

func TestChainLinkAcrossProjectsNeedsBothEnds(t *testing.T) {
	// Cross-project chains are allowed on purpose: one brand's export finishing
	// is a real reason for another's import to start. What a link may not do is
	// cross OUT of the caller's reach, so both ends are checked.
	//
	// Both, not just the source, because a link is a change to the target as
	// much as to the source: the target starts running because of something the
	// source did.
	svc, _, alpha, beta := twoProjects(t)
	ctx := context.Background()

	if err := svc.AddLink(ctx, domain.ScopeOf(1), alpha, beta, "success", 5); !errors.Is(err, ErrNotFound) {
		t.Errorf("linking to a job outside the scope gave %v, want ErrNotFound", err)
	}
	if err := svc.AddLink(ctx, domain.ScopeOf(2), alpha, beta, "success", 5); !errors.Is(err, ErrNotFound) {
		t.Errorf("linking from a job outside the scope gave %v, want ErrNotFound", err)
	}

	if err := svc.AddLink(ctx, domain.ScopeOf(1, 2), alpha, beta, "success", 5); err != nil {
		t.Fatalf("a caller holding both projects could not link across them: %v", err)
	}

	detail, err := svc.Get(ctx, allProjects, alpha)
	if err != nil {
		t.Fatal(err)
	}
	if len(detail.Links) != 1 {
		t.Fatalf("links = %d, want the cross-project link to have been created", len(detail.Links))
	}
}

func TestListsAreNarrowedByTheScope(t *testing.T) {
	svc, _, _, _ := twoProjects(t)
	ctx := context.Background()

	rows, total, err := svc.List(ctx, domain.JobFilter{Scope: domain.ScopeOf(1)}, 0, 20)
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 || len(rows) != 1 || rows[0].ProjectID != 1 {
		t.Fatalf("list = %+v (total %d), want only the alpha job", rows, total)
	}

	// An empty scope returns an empty list, not everything. This is the failure
	// that would be invisible in the interface: the page renders, it just shows
	// somebody else's jobs.
	rows, total, err = svc.List(ctx, domain.JobFilter{Scope: domain.NoProjects()}, 0, 20)
	if err != nil {
		t.Fatal(err)
	}
	if total != 0 || len(rows) != 0 {
		t.Fatalf("an empty scope returned %+v (total %d), want nothing", rows, total)
	}
}

func TestRunScopeRefusesAnotherProjectsRun(t *testing.T) {
	_, store, _, beta := twoProjects(t)
	ctx := context.Background()

	runs := NewRunService(&memRunRepo{}, memJobRepo{memStore: store}, nil)
	if _, err := runs.Trigger(ctx, domain.ScopeOf(1), beta, domain.TriggerManual, nil); !errors.Is(err, ErrNotFound) {
		t.Errorf("triggering another project's job gave %v, want ErrNotFound", err)
	}
}
