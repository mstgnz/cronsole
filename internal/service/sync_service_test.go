package service

import (
	"context"
	"testing"

	"github.com/mstgnz/cronsole/v2/internal/domain"
)

func boolPtr(v bool) *bool { return &v }

func outcomeFor(result *SyncResult, code string) SyncOutcome {
	for _, o := range result.Jobs {
		if o.Code == code {
			return o
		}
	}
	return SyncOutcome{}
}

func TestSyncRegistersAWholeProject(t *testing.T) {
	// The point of a central scheduler: a project ships its cron definitions
	// next to the code that answers them and posts them on deploy.
	store := newMemStore()
	project := store.addProject(1, "demo", "https://service.example.com")
	sync, jobs := newTestSyncService(store)

	req := SyncRequest{Jobs: []SyncJob{
		{
			Code: "daily-report", Name: "Daily report", URL: "/cron/report",
			Schedules: []string{"0 3 * * *"}, TimeoutSec: 120,
			Links: []ChainIn{{TargetCode: "cleanup", DelaySec: 30}},
		},
		// Declared AFTER the job that links to it, which is the ordinary case
		// and the reason links are applied in a second pass.
		{Code: "cleanup", Name: "Cleanup", URL: "/cron/cleanup"},
	}}

	result, err := sync.Sync(context.Background(), project, req)
	if err != nil {
		t.Fatalf("sync failed: %v", err)
	}
	if result.Created != 2 || result.Failed != 0 {
		t.Fatalf("created=%d failed=%d, want 2 and 0: %+v", result.Created, result.Failed, result.Jobs)
	}
	if errs := outcomeFor(result, "daily-report").Errors; len(errs) != 0 {
		t.Errorf("the link was not applied: %v", errs)
	}

	report, err := jobs.GetByCode(context.Background(), 1, "daily-report")
	if err != nil {
		t.Fatal(err)
	}
	detail, err := jobs.Get(context.Background(), allProjects, report.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(detail.Links) != 1 || detail.Links[0].TargetCode != "cleanup" {
		t.Errorf("links = %+v, want one to cleanup", detail.Links)
	}
	if detail.Job.TimeoutSec != 120 {
		t.Errorf("timeout = %d, want 120", detail.Job.TimeoutSec)
	}
	// A job with a schedule defaults to active; one without does not, because
	// nothing would start it.
	if !detail.Job.Active {
		t.Error("the scheduled job was not activated")
	}
	if cleanup, _ := jobs.GetByCode(context.Background(), 1, "cleanup"); cleanup.Active {
		t.Error("a job with no schedule was activated by default")
	}
}

func TestSyncIsIdempotent(t *testing.T) {
	// The same payload posted twice must change nothing the second time,
	// because a deploy pipeline posts it on every deploy.
	store := newMemStore()
	project := store.addProject(1, "demo", "")
	sync, _ := newTestSyncService(store)

	req := SyncRequest{Jobs: []SyncJob{{
		Code: "one", Name: "One", URL: "https://example.com/one",
		Schedules: []string{"*/5 * * * *"},
		Links:     []ChainIn{{TargetCode: "two"}},
	}, {
		Code: "two", Name: "Two", URL: "https://example.com/two",
	}}}

	first, err := sync.Sync(context.Background(), project, req)
	if err != nil {
		t.Fatal(err)
	}
	if first.Created != 2 {
		t.Fatalf("first run created %d, want 2", first.Created)
	}

	second, err := sync.Sync(context.Background(), project, req)
	if err != nil {
		t.Fatal(err)
	}
	if second.Created != 0 || second.Updated != 2 || second.Failed != 0 {
		t.Errorf("second run: created=%d updated=%d failed=%d, want 0/2/0",
			second.Created, second.Updated, second.Failed)
	}
	for _, o := range second.Jobs {
		if len(o.Errors) != 0 {
			t.Errorf("%s reported %v on the second run", o.Code, o.Errors)
		}
	}
	if len(store.links) != 1 {
		t.Errorf("links = %d after two syncs, want 1", len(store.links))
	}
}

func TestSyncDoesNotUndoScreenChanges(t *testing.T) {
	// An omitted field keeps the stored value rather than snapping back to a
	// default. Without that, every deploy would silently reset a timeout an
	// operator had raised on the screen.
	store := newMemStore()
	project := store.addProject(1, "demo", "")
	sync, jobs := newTestSyncService(store)
	ctx := context.Background()

	declared := SyncRequest{Jobs: []SyncJob{{
		Code: "tuned", Name: "Tuned", URL: "https://example.com/x",
		Schedules: []string{"*/5 * * * *"}, TimeoutSec: 30,
	}}}
	if _, err := sync.Sync(ctx, project, declared); err != nil {
		t.Fatal(err)
	}

	job, _ := jobs.GetByCode(ctx, 1, "tuned")
	// The operator raises the timeout and the priority on the screen.
	if err := jobs.Update(ctx, allProjects, job.ID, &JobInput{
		ProjectID: 1, Name: "Tuned", URL: "https://example.com/x",
		TimeoutSec: 300, Priority: 10, Schedules: []string{"*/5 * * * *"},
	}); err != nil {
		t.Fatal(err)
	}

	// The next deploy posts the same payload, which does not mention priority
	// and states the old timeout.
	quiet := SyncRequest{Jobs: []SyncJob{{
		Code: "tuned", Name: "Tuned", URL: "https://example.com/x",
		Schedules: []string{"*/5 * * * *"},
	}}}
	if _, err := sync.Sync(ctx, project, quiet); err != nil {
		t.Fatal(err)
	}

	after, _ := jobs.GetByCode(ctx, 1, "tuned")
	if after.TimeoutSec != 300 {
		t.Errorf("timeout = %d, want the screen's 300 to survive", after.TimeoutSec)
	}
	if after.Priority != 10 {
		t.Errorf("priority = %d, want the screen's 10 to survive", after.Priority)
	}
}

func TestSyncTriStateFlags(t *testing.T) {
	// An explicit false must turn a flag off; an omitted field must leave it
	// alone. Collapsing the two is how a deploy quietly disables single run on
	// a job that writes.
	store := newMemStore()
	project := store.addProject(1, "demo", "")
	sync, jobs := newTestSyncService(store)
	ctx := context.Background()

	// Declared without single_run: it defaults ON, because a deploy script has
	// not thought about overlap and overlapping is what corrupts data.
	if _, err := sync.Sync(ctx, project, SyncRequest{Jobs: []SyncJob{{
		Code: "flags", Name: "Flags", URL: "https://example.com/x",
		Schedules: []string{"* * * * *"},
	}}}); err != nil {
		t.Fatal(err)
	}
	job, _ := jobs.GetByCode(ctx, 1, "flags")
	if !job.SingleRun {
		t.Error("single_run did not default on for a declared job")
	}

	// Explicit false turns it off.
	if _, err := sync.Sync(ctx, project, SyncRequest{Jobs: []SyncJob{{
		Code: "flags", Name: "Flags", URL: "https://example.com/x",
		Schedules: []string{"* * * * *"}, SingleRun: boolPtr(false),
	}}}); err != nil {
		t.Fatal(err)
	}
	job, _ = jobs.GetByCode(ctx, 1, "flags")
	if job.SingleRun {
		t.Error("an explicit false did not turn single_run off")
	}

	// Omitting it again leaves it off.
	if _, err := sync.Sync(ctx, project, SyncRequest{Jobs: []SyncJob{{
		Code: "flags", Name: "Flags", URL: "https://example.com/x",
		Schedules: []string{"* * * * *"},
	}}}); err != nil {
		t.Fatal(err)
	}
	job, _ = jobs.GetByCode(ctx, 1, "flags")
	if job.SingleRun {
		t.Error("omitting single_run reset it instead of leaving it alone")
	}
}

func TestSyncPartialFailure(t *testing.T) {
	// Twelve declared jobs and one bad expression should register eleven and
	// name the one that failed, not refuse the lot.
	store := newMemStore()
	project := store.addProject(1, "demo", "")
	sync, _ := newTestSyncService(store)

	result, err := sync.Sync(context.Background(), project, SyncRequest{Jobs: []SyncJob{
		{Code: "good", Name: "Good", URL: "https://example.com/a", Schedules: []string{"* * * * *"}},
		{Code: "bad-cron", Name: "Bad", URL: "https://example.com/b", Schedules: []string{"nope"}},
		{Code: "", Name: "No code", URL: "https://example.com/c"},
	}})
	if err != nil {
		t.Fatalf("sync returned an error instead of per job outcomes: %v", err)
	}
	if result.Created != 1 {
		t.Errorf("created = %d, want 1", result.Created)
	}
	if result.Failed != 2 {
		t.Errorf("failed = %d, want 2", result.Failed)
	}
	if o := outcomeFor(result, "bad-cron"); o.Action != "failed" || len(o.Errors) == 0 {
		t.Errorf("the bad expression was not reported: %+v", o)
	}
}

func TestSyncLinkToAnUnknownTargetIsReported(t *testing.T) {
	store := newMemStore()
	project := store.addProject(1, "demo", "")
	sync, _ := newTestSyncService(store)

	result, err := sync.Sync(context.Background(), project, SyncRequest{Jobs: []SyncJob{{
		Code: "source", Name: "Source", URL: "https://example.com/a",
		Schedules: []string{"* * * * *"},
		Links:     []ChainIn{{TargetCode: "does-not-exist"}},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	// The job itself is registered; only the link failed, and it says so.
	if result.Created != 1 {
		t.Errorf("created = %d, want the job to have registered anyway", result.Created)
	}
	if errs := outcomeFor(result, "source").Errors; len(errs) == 0 {
		t.Error("the missing link target was not reported")
	}
}

func TestSyncPruneDeactivatesRatherThanDeletes(t *testing.T) {
	// A deployment that posts a partial list by accident should cost a switch
	// being flipped back, not a job definition and its history.
	store := newMemStore()
	project := store.addProject(1, "demo", "")
	sync, jobs := newTestSyncService(store)
	ctx := context.Background()

	full := SyncRequest{Jobs: []SyncJob{
		{Code: "keep", Name: "Keep", URL: "https://example.com/a", Schedules: []string{"* * * * *"}},
		{Code: "drop", Name: "Drop", URL: "https://example.com/b", Schedules: []string{"* * * * *"}},
	}}
	if _, err := sync.Sync(ctx, project, full); err != nil {
		t.Fatal(err)
	}

	partial := SyncRequest{Prune: true, Jobs: []SyncJob{full.Jobs[0]}}
	result, err := sync.Sync(ctx, project, partial)
	if err != nil {
		t.Fatal(err)
	}
	if result.Deactivated != 1 {
		t.Errorf("deactivated = %d, want 1", result.Deactivated)
	}

	dropped, err := jobs.GetByCode(ctx, 1, "drop")
	if err != nil {
		t.Fatal("the pruned job was deleted rather than deactivated")
	}
	if dropped.Active {
		t.Error("the pruned job is still active")
	}
	if kept, _ := jobs.GetByCode(ctx, 1, "keep"); !kept.Active {
		t.Error("the declared job was deactivated")
	}
}

func TestSyncCannotEscapeTheProjectBaseAddress(t *testing.T) {
	// A project key reaches the sync endpoint. It must not be able to point a
	// job at a host the project does not own.
	store := newMemStore()
	project := store.addProject(1, "demo", "https://service.example.com")
	sync, _ := newTestSyncService(store)

	result, err := sync.Sync(context.Background(), project, SyncRequest{Jobs: []SyncJob{{
		Code: "escape", Name: "Escape", URL: "https://evil.example.com/steal",
		Schedules: []string{"* * * * *"},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	if result.Failed != 1 {
		t.Errorf("an absolute address was accepted against a project base address: %+v", result.Jobs)
	}
}

func TestSyncClearsHeadersWhenAnEmptyMapIsSent(t *testing.T) {
	// A declared empty map means "no headers", while omitting the field means
	// "leave them". A header the project removed from its declaration must
	// stop being sent.
	store := newMemStore()
	project := store.addProject(1, "demo", "")
	sync, jobs := newTestSyncService(store)
	ctx := context.Background()

	if _, err := sync.Sync(ctx, project, SyncRequest{Jobs: []SyncJob{{
		Code: "with-headers", Name: "H", URL: "https://example.com/x",
		Schedules: []string{"* * * * *"},
		Headers:   map[string]string{"X-Token": "abc"},
	}}}); err != nil {
		t.Fatal(err)
	}
	job, _ := jobs.GetByCode(ctx, 1, "with-headers")
	headers, _ := memJobRepo{memStore: store}.ListHeaders(ctx, job.ID)
	if len(headers) != 1 {
		t.Fatalf("headers = %d, want 1", len(headers))
	}

	if _, err := sync.Sync(ctx, project, SyncRequest{Jobs: []SyncJob{{
		Code: "with-headers", Name: "H", URL: "https://example.com/x",
		Schedules: []string{"* * * * *"},
		Headers:   map[string]string{},
	}}}); err != nil {
		t.Fatal(err)
	}
	headers, _ = memJobRepo{memStore: store}.ListHeaders(ctx, job.ID)
	if len(headers) != 0 {
		t.Errorf("headers = %d after an empty map, want 0", len(headers))
	}
}

func TestResolveBool(t *testing.T) {
	if !resolveBool(boolPtr(true), true, false, false) {
		t.Error("an explicit true was not honoured")
	}
	if resolveBool(boolPtr(false), true, true, true) {
		t.Error("an explicit false was not honoured")
	}
	if !resolveBool(nil, true, true, false) {
		t.Error("an omitted flag did not keep the stored value")
	}
	if resolveBool(nil, true, false, true) {
		t.Error("an omitted flag fell back to the default instead of the stored value")
	}
	if !resolveBool(nil, false, false, true) {
		t.Error("a new job did not take the default")
	}
}

func TestChainMaxDepthIsRespectedByTheFormGuard(t *testing.T) {
	// The runner's cap stops a loop silently and days later; this one tells
	// the operator at the moment they build it.
	if domain.ChainMaxDepth < 2 {
		t.Fatalf("ChainMaxDepth = %d, which makes chains useless", domain.ChainMaxDepth)
	}
}
