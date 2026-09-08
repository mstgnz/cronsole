package repository

import (
	"errors"
	"net"
	"testing"
	"time"

	"github.com/mstgnz/cronsole/v2/internal/authz"
	"github.com/mstgnz/cronsole/v2/internal/domain"
)

// --- users ------------------------------------------------------------------

func TestUserLifecycle(t *testing.T) {
	s := fresh(t)
	repo := NewUserRepo(s)

	id, err := repo.Create(ctx(t), &domain.User{
		Fullname: "Mesut", Email: "Mesut@Example.com", Password: "hash",
		IsAdmin: false, Active: true,
	})
	mustNoErr(t, err, "Create")

	user, err := repo.GetByID(ctx(t), id)
	mustNoErr(t, err, "GetByID")
	if user.Fullname != "Mesut" {
		t.Errorf("user = %+v", user)
	}

	// Looked up without regard to case, because that is how somebody types
	// their own address and the index is on lower(email).
	byEmail, err := repo.GetByEmail(ctx(t), "mesut@example.com")
	mustNoErr(t, err, "GetByEmail")
	if byEmail.ID != id {
		t.Errorf("GetByEmail found %d, want %d", byEmail.ID, id)
	}

	user.Fullname = "Mesut Genez"
	user.IsAdmin = true
	mustNoErr(t, repo.UpdateProfile(ctx(t), user), "UpdateProfile")
	back, _ := repo.GetByID(ctx(t), id)
	if back.Fullname != "Mesut Genez" || !back.IsAdmin {
		t.Errorf("the update did not land: %+v", back)
	}

	count, err := repo.Count(ctx(t))
	mustNoErr(t, err, "Count")
	if count != 1 {
		t.Errorf("Count = %d, want 1", count)
	}

	mustNoErr(t, repo.SoftDelete(ctx(t), id, time.Now()), "SoftDelete")
	if _, err := repo.GetByID(ctx(t), id); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetByID after a delete = %v, want ErrNotFound", err)
	}
	// Count feeds the first-run check, so a deleted account must not keep a
	// deployment out of its own bootstrap.
	count, _ = repo.Count(ctx(t))
	if count != 0 {
		t.Errorf("Count = %d after the only account was deleted, want 0", count)
	}
}

func TestAnAddressIsUniqueAmongLiveAccounts(t *testing.T) {
	s := fresh(t)
	repo := NewUserRepo(s)

	first := &domain.User{Email: "a@b.c", Password: "hash", Active: true}
	id, err := repo.Create(ctx(t), first)
	mustNoErr(t, err, "Create")

	if _, err := repo.Create(ctx(t), first); !errors.Is(err, ErrDuplicate) {
		t.Errorf("a duplicate address returned %v, want ErrDuplicate", err)
	}
	upper := &domain.User{Email: "A@B.C", Password: "hash", Active: true}
	if _, err := repo.Create(ctx(t), upper); !errors.Is(err, ErrDuplicate) {
		t.Errorf("a duplicate in another case returned %v, want ErrDuplicate", err)
	}

	// And free again once the account is deleted, because the index is partial.
	mustNoErr(t, repo.SoftDelete(ctx(t), id, time.Now()), "SoftDelete")
	if _, err := repo.Create(ctx(t), first); err != nil {
		t.Errorf("the address of a deleted account could not be reused: %v", err)
	}
}

func TestChangingAPasswordRetiresExistingTokens(t *testing.T) {
	// A JWT cannot be withdrawn once signed. Moving this cut-off is the only
	// thing that makes a password change end other sessions.
	s := fresh(t)
	repo := NewUserRepo(s)

	id := seedUser(t, s, "a@b.c", false)
	before, err := repo.GetByID(ctx(t), id)
	mustNoErr(t, err, "GetByID")
	if before.TokensValidAfter != nil {
		t.Fatal("a new account already has a cut-off")
	}

	at := time.Now()
	mustNoErr(t, repo.UpdatePassword(ctx(t), id, "new-hash", at), "UpdatePassword")

	after, _ := repo.GetByID(ctx(t), id)
	if after.TokensValidAfter == nil {
		t.Fatal("the cut-off was not set, so the change ends no sessions")
	}
	if after.Password == before.Password {
		t.Error("the hash did not change")
	}
	// A token minted a second ago is now retired.
	if !after.TokenRetired(at.Add(-time.Second)) {
		t.Error("a token issued before the change survived it")
	}
}

func TestInvalidateTokensAndTouchLogin(t *testing.T) {
	s := fresh(t)
	repo := NewUserRepo(s)
	id := seedUser(t, s, "a@b.c", false)

	at := time.Now()
	mustNoErr(t, repo.InvalidateTokens(ctx(t), id, at), "InvalidateTokens")
	user, _ := repo.GetByID(ctx(t), id)
	if user.TokensValidAfter == nil {
		t.Error("signing out did not move the cut-off")
	}

	mustNoErr(t, repo.TouchLogin(ctx(t), id, at), "TouchLogin")
	user, _ = repo.GetByID(ctx(t), id)
	if user.LastLogin == nil {
		t.Error("last_login was not recorded")
	}
}

func TestUserListSearchesAndPages(t *testing.T) {
	s := fresh(t)
	repo := NewUserRepo(s)

	seedUser(t, s, "alice@example.com", false)
	seedUser(t, s, "bob@example.com", false)
	seedUser(t, s, "carol@example.com", true)

	all, total, err := repo.List(ctx(t), "", 0, 10)
	mustNoErr(t, err, "List")
	if len(all) != 3 || total != 3 {
		t.Errorf("%d of %d accounts", len(all), total)
	}

	found, total, err := repo.List(ctx(t), "bob", 0, 10)
	mustNoErr(t, err, "List")
	if len(found) != 1 || total != 1 {
		t.Errorf("the search returned %d accounts", len(found))
	}

	page, total, err := repo.List(ctx(t), "", 0, 2)
	mustNoErr(t, err, "List")
	if len(page) != 2 || total != 3 {
		t.Errorf("%d rows on a page of 2, total %d", len(page), total)
	}
}

// --- projects ---------------------------------------------------------------

func TestProjectLifecycleAndScope(t *testing.T) {
	s := fresh(t)
	repo := NewProjectRepo(s)

	shop, err := repo.Create(ctx(t), &domain.Project{
		Name: "Shop", Slug: "shop", BaseURL: "https://shop.example.com",
		KeyPrefix: "pfx_shop", KeyHash: "hash_shop", Active: true,
	})
	mustNoErr(t, err, "Create")
	alpha, err := repo.Create(ctx(t), &domain.Project{
		Name: "Alpha", Slug: "alpha", KeyPrefix: "pfx_alpha", KeyHash: "hash_alpha", Active: true,
	})
	mustNoErr(t, err, "Create")

	// A slug is unique, because it is the name a person types.
	if _, err := repo.Create(ctx(t), &domain.Project{
		Name: "Shop again", Slug: "shop", KeyPrefix: "p", KeyHash: "h", Active: true,
	}); !errors.Is(err, ErrDuplicate) {
		t.Errorf("a duplicate slug returned %v, want ErrDuplicate", err)
	}

	got, err := repo.GetBySlug(ctx(t), "shop")
	mustNoErr(t, err, "GetBySlug")
	if got.ID != shop {
		t.Errorf("GetBySlug found %d", got.ID)
	}

	// The key prefix is the lookup for an API key. It is not a secret and not
	// sufficient on its own; the caller still compares the hash.
	byPrefix, err := repo.GetByKeyPrefix(ctx(t), "pfx_alpha")
	mustNoErr(t, err, "GetByKeyPrefix")
	if byPrefix.ID != alpha {
		t.Errorf("GetByKeyPrefix found %d", byPrefix.ID)
	}

	// And the scope narrows the list, exactly as it does for jobs.
	rows, err := repo.List(ctx(t), domain.ScopeOf(shop), "")
	mustNoErr(t, err, "List")
	if len(rows) != 1 || rows[0].ID != shop {
		t.Errorf("the scoped list returned %d rows", len(rows))
	}
	if none, _ := repo.List(ctx(t), domain.NoProjects(), ""); len(none) != 0 {
		t.Errorf("the empty scope returned %d projects", len(none))
	}

	names, err := repo.ListNames(ctx(t), domain.ScopeOf(alpha))
	mustNoErr(t, err, "ListNames")
	if len(names) != 1 || names[0].Slug != "alpha" {
		t.Errorf("ListNames returned %+v", names)
	}
}

func TestProjectListCountsItsJobs(t *testing.T) {
	// The counters on the project list. A join that counted deleted jobs would
	// show a number nobody can reconcile with the screen.
	s := fresh(t)
	repo := NewProjectRepo(s)

	shop := seedProject(t, s, "shop", "https://shop.example.com")
	seedJob(t, s, shop, "one")
	off := seedJob(t, s, shop, "two")
	gone := seedJob(t, s, shop, "three")

	_, err := s.db.Exec(`UPDATE jobs SET active = false WHERE id = $1`, off)
	mustNoErr(t, err, "deactivating")
	_, err = s.db.Exec(`UPDATE jobs SET deleted_at = now() WHERE id = $1`, gone)
	mustNoErr(t, err, "deleting")

	rows, err := repo.List(ctx(t), domain.AllProjects(), "")
	mustNoErr(t, err, "List")
	if len(rows) != 1 {
		t.Fatalf("%d projects", len(rows))
	}
	if rows[0].JobTotal != 2 {
		t.Errorf("job total = %d, want 2 (the deleted one does not count)", rows[0].JobTotal)
	}
	if rows[0].JobActive != 1 {
		t.Errorf("active jobs = %d, want 1", rows[0].JobActive)
	}
}

func TestRotatingAProjectKey(t *testing.T) {
	s := fresh(t)
	repo := NewProjectRepo(s)

	shop := seedProject(t, s, "shop", "https://shop.example.com")
	mustNoErr(t, repo.SetKey(ctx(t), shop, "pfx_new", "hash_new"), "SetKey")

	project, err := repo.GetByID(ctx(t), shop)
	mustNoErr(t, err, "GetByID")
	if project.KeyPrefix != "pfx_new" || project.KeyHash != "hash_new" {
		t.Errorf("the key was not rotated: %+v", project)
	}
	// The old prefix stops resolving, which is what makes rotation mean
	// something.
	if _, err := repo.GetByKeyPrefix(ctx(t), "pfx_shop"); !errors.Is(err, ErrNotFound) {
		t.Errorf("the old prefix still resolves: %v", err)
	}
}

func TestADeletedProjectDisappearsFromEveryRead(t *testing.T) {
	s := fresh(t)
	repo := NewProjectRepo(s)

	shop := seedProject(t, s, "shop", "https://shop.example.com")
	mustNoErr(t, repo.SoftDelete(ctx(t), shop, time.Now()), "SoftDelete")

	if _, err := repo.GetByID(ctx(t), shop); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetByID = %v, want ErrNotFound", err)
	}
	if _, err := repo.GetBySlug(ctx(t), "shop"); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetBySlug = %v, want ErrNotFound", err)
	}
	// The key stops working the moment the project is deleted, which is the
	// point: a deleted project must not still be able to register jobs.
	if _, err := repo.GetByKeyPrefix(ctx(t), "pfx_shop"); !errors.Is(err, ErrNotFound) {
		t.Errorf("a deleted project's key still resolves: %v", err)
	}
	if rows, _ := repo.List(ctx(t), domain.AllProjects(), ""); len(rows) != 0 {
		t.Errorf("List returned %d rows", len(rows))
	}
}

// --- runs -------------------------------------------------------------------

func TestTheRunListIsScopedThroughItsJob(t *testing.T) {
	// A run has no project of its own; the scope reaches it through the job.
	// A list that forgot would show another brand's output, which is the field
	// most likely to carry customer data.
	s := fresh(t)
	repo := NewRunRepo(s)

	shop := seedProject(t, s, "shop", "https://shop.example.com")
	alpha := seedProject(t, s, "alpha", "https://alpha.example.com")
	shopJob := seedJob(t, s, shop, "shop-daily")
	alphaJob := seedJob(t, s, alpha, "alpha-nightly")

	seedRun(t, s, shopJob, domain.StatusSuccess, time.Now())
	seedRun(t, s, alphaJob, domain.StatusSuccess, time.Now())

	rows, total, err := repo.List(ctx(t), domain.RunFilter{Scope: domain.ScopeOf(shop)}, 0, 50)
	mustNoErr(t, err, "List")
	if len(rows) != 1 || total != 1 {
		t.Fatalf("%d of %d runs for one project", len(rows), total)
	}
	if rows[0].JobCode != "shop-daily" {
		t.Errorf("the run list shows %q", rows[0].JobCode)
	}

	if none, _, _ := repo.List(ctx(t), domain.RunFilter{Scope: domain.NoProjects()}, 0, 50); len(none) != 0 {
		t.Errorf("the empty scope returned %d runs", len(none))
	}
}

func TestRunFilters(t *testing.T) {
	s := fresh(t)
	repo := NewRunRepo(s)

	shop := seedProject(t, s, "shop", "https://shop.example.com")
	jobID := seedJob(t, s, shop, "daily")
	other := seedJob(t, s, shop, "other")

	old := time.Now().Add(-72 * time.Hour)
	seedRun(t, s, jobID, domain.StatusSuccess, time.Now())
	seedRun(t, s, jobID, domain.StatusFailed, time.Now())
	seedRun(t, s, other, domain.StatusSuccess, old)

	status := domain.StatusFailed
	start := time.Now().Add(-time.Hour)
	job := jobID
	project := shop

	cases := []struct {
		name   string
		filter domain.RunFilter
		want   int
	}{
		{"by status", domain.RunFilter{Scope: domain.AllProjects(), Status: &status}, 1},
		{"by job", domain.RunFilter{Scope: domain.AllProjects(), JobID: &job}, 2},
		{"by project", domain.RunFilter{Scope: domain.AllProjects(), ProjectID: &project}, 3},
		{"from a time", domain.RunFilter{Scope: domain.AllProjects(), Start: &start}, 2},
		{"unfiltered", domain.RunFilter{Scope: domain.AllProjects()}, 3},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rows, _, err := repo.List(ctx(t), c.filter, 0, 50)
			mustNoErr(t, err, "List")
			if len(rows) != c.want {
				t.Errorf("%d rows, want %d", len(rows), c.want)
			}
		})
	}
}

func TestRunsAreNewestFirst(t *testing.T) {
	// Every screen reads them that way, and a run log in insertion order is a
	// run log nobody scrolls to the bottom of.
	s := fresh(t)
	repo := NewRunRepo(s)

	shop := seedProject(t, s, "shop", "https://shop.example.com")
	jobID := seedJob(t, s, shop, "daily")
	for i := 0; i < 5; i++ {
		seedRun(t, s, jobID, domain.StatusSuccess, time.Now().Add(time.Duration(i)*time.Second))
	}

	rows, _, err := repo.List(ctx(t), domain.RunFilter{Scope: domain.AllProjects()}, 0, 50)
	mustNoErr(t, err, "List")
	for i := 1; i < len(rows); i++ {
		if rows[i-1].CreatedAt.Before(rows[i].CreatedAt) {
			t.Fatal("the run list is not newest first")
		}
	}
}

func TestEnqueueAndChildren(t *testing.T) {
	s := fresh(t)
	repo := NewRunRepo(s)

	shop := seedProject(t, s, "shop", "https://shop.example.com")
	jobID := seedJob(t, s, shop, "daily")
	userID := seedUser(t, s, "a@b.c", true)

	runID, err := repo.Enqueue(ctx(t), jobID, domain.TriggerManual, &userID)
	mustNoErr(t, err, "Enqueue")

	row, err := repo.Get(ctx(t), runID)
	mustNoErr(t, err, "Get")
	if row.Status != domain.StatusPending {
		t.Errorf("a queued run starts as %q, want pending", row.Status)
	}
	if row.Trigger != domain.TriggerManual {
		t.Errorf("trigger = %q", row.Trigger)
	}
	// Who pressed the button, which is the whole reason a manual run is worth
	// telling apart from a scheduled one.
	if row.UserID == nil || *row.UserID != userID {
		t.Errorf("user = %v, want %d", row.UserID, userID)
	}

	_, err = s.db.Exec(
		`INSERT INTO job_runs (job_id, trigger, status, parent_run_id)
		 VALUES ($1, 'chain', 'success', $2)`, jobID, runID)
	mustNoErr(t, err, "adding a chain step")

	children, err := repo.Children(ctx(t), runID)
	mustNoErr(t, err, "Children")
	if len(children) != 1 {
		t.Errorf("%d children, want 1", len(children))
	}

	recent, err := repo.Recent(ctx(t), jobID, 10)
	mustNoErr(t, err, "Recent")
	if len(recent) != 2 {
		t.Errorf("%d recent runs, want 2", len(recent))
	}
}

// --- statistics -------------------------------------------------------------

func TestTheDashboardIsScoped(t *testing.T) {
	// Every figure on it. A summary that counted every brand would tell a
	// reader on one project how much work the others are doing.
	s := fresh(t)
	repo := NewStatsRepo(s)

	shop := seedProject(t, s, "shop", "https://shop.example.com")
	alpha := seedProject(t, s, "alpha", "https://alpha.example.com")
	shopJob := seedJob(t, s, shop, "shop-daily")
	alphaJob := seedJob(t, s, alpha, "alpha-nightly")

	seedRun(t, s, shopJob, domain.StatusSuccess, time.Now())
	seedRun(t, s, alphaJob, domain.StatusSuccess, time.Now())
	seedRun(t, s, alphaJob, domain.StatusFailed, time.Now())

	summary, err := repo.Summary(ctx(t), domain.ScopeOf(shop))
	mustNoErr(t, err, "Summary")
	if summary.ProjectTotal != 1 {
		t.Errorf("project total = %d, want 1", summary.ProjectTotal)
	}
	if summary.JobTotal != 1 {
		t.Errorf("job total = %d, want 1", summary.JobTotal)
	}
	if summary.DayFailed != 0 {
		t.Errorf("the other brand's failure was counted: %d", summary.DayFailed)
	}

	failures, err := repo.RecentFailures(ctx(t), domain.ScopeOf(shop), 10)
	mustNoErr(t, err, "RecentFailures")
	if len(failures) != 0 {
		t.Errorf("another brand's failure is on this dashboard: %+v", failures)
	}

	history, err := repo.JobHistory(ctx(t), domain.ScopeOf(shop), alphaJob, 10)
	mustNoErr(t, err, "JobHistory")
	if len(history) != 0 {
		t.Errorf("another brand's job history is readable: %+v", history)
	}

	slowest, err := repo.SlowestJobs(ctx(t), domain.ScopeOf(shop), 24, 10)
	mustNoErr(t, err, "SlowestJobs")
	for _, j := range slowest {
		if j.Code == "alpha-nightly" {
			t.Error("another brand's job is on the slowest list")
		}
	}

	activity, err := repo.Activity(ctx(t), domain.ScopeOf(shop), 24)
	mustNoErr(t, err, "Activity")
	total := 0
	for _, bucket := range activity {
		total += bucket.Success + bucket.Failed + bucket.Timeout + bucket.Skipped
	}
	if total != 1 {
		t.Errorf("the activity chart counted %d runs, want only this brand's 1", total)
	}
}

func TestRunningNowIsWhatIsExecuting(t *testing.T) {
	s := fresh(t)
	repo := NewStatsRepo(s)

	shop := seedProject(t, s, "shop", "https://shop.example.com")
	jobID := seedJob(t, s, shop, "daily")

	_, err := s.db.Exec(
		`INSERT INTO job_runs (job_id, trigger, status, started_at)
		 VALUES ($1, 'schedule', 'running', now())`, jobID)
	mustNoErr(t, err, "seeding a running run")
	seedRun(t, s, jobID, domain.StatusPending, time.Now())
	seedRun(t, s, jobID, domain.StatusSuccess, time.Now())

	rows, err := repo.RunningNow(ctx(t), domain.AllProjects(), 10)
	mustNoErr(t, err, "RunningNow")
	if len(rows) != 1 {
		t.Errorf("%d runs in flight, want 1", len(rows))
	}
	if len(rows) > 0 && rows[0].JobCode != "daily" {
		t.Errorf("the in-flight panel shows %q", rows[0].JobCode)
	}
}

func TestTheActivityChartCoversTheWholeWindow(t *testing.T) {
	// One bucket per hour, present even where nothing ran: a chart with gaps
	// where the quiet hours were reads as missing data rather than as quiet.
	s := fresh(t)
	repo := NewStatsRepo(s)

	buckets, err := repo.Activity(ctx(t), domain.AllProjects(), 24)
	mustNoErr(t, err, "Activity")
	if len(buckets) < 24 {
		t.Errorf("%d buckets for a 24 hour window", len(buckets))
	}
	for i := 1; i < len(buckets); i++ {
		if !buckets[i].Hour.After(buckets[i-1].Hour) {
			t.Fatal("the buckets are not in order")
		}
	}
}

// --- notifications and the application log ----------------------------------

func TestNotificationLifecycle(t *testing.T) {
	s := fresh(t)
	repo := NewNotificationRepo(s)

	id, err := repo.Create(ctx(t), &domain.Notification{
		Name: "On call", OnFailure: true, Active: true,
	})
	mustNoErr(t, err, "Create")

	mustNoErr(t, repo.ReplaceEmails(ctx(t), id,
		[]string{"ops@example.com", "oncall@example.com"}), "ReplaceEmails")

	got, err := repo.Get(ctx(t), id)
	mustNoErr(t, err, "Get")
	if len(got.Emails) != 2 {
		t.Errorf("emails = %v", got.Emails)
	}
	if !got.OnFailure || got.OnSuccess {
		t.Errorf("notification = %+v", got)
	}

	// Replacing is a replacement.
	mustNoErr(t, repo.ReplaceEmails(ctx(t), id, []string{"only@example.com"}), "ReplaceEmails")
	got, _ = repo.Get(ctx(t), id)
	if len(got.Emails) != 1 || got.Emails[0] != "only@example.com" {
		t.Errorf("emails after a replace = %v", got.Emails)
	}

	got.Name = "Renamed"
	got.OnSuccess = true
	mustNoErr(t, repo.Update(ctx(t), got), "Update")
	got, _ = repo.Get(ctx(t), id)
	if got.Name != "Renamed" || !got.OnSuccess {
		t.Errorf("the update did not land: %+v", got)
	}

	mustNoErr(t, repo.SoftDelete(ctx(t), id, time.Now()), "SoftDelete")
	if _, err := repo.Get(ctx(t), id); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get after a delete = %v, want ErrNotFound", err)
	}
	if rows, _ := repo.List(ctx(t)); len(rows) != 0 {
		t.Errorf("List returned %d rows", len(rows))
	}
}

func TestTheApplicationLogFiltersByLevel(t *testing.T) {
	s := fresh(t)
	repo := NewAppLogRepo(s)

	_, err := s.db.Exec(
		`INSERT INTO app_logs (level, message) VALUES
			('info', 'routine'), ('warn', 'attention'), ('error', 'broken'), ('error', 'also broken')`)
	mustNoErr(t, err, "seeding the log")

	all, total, err := repo.List(ctx(t), "", 0, 50)
	mustNoErr(t, err, "List")
	if len(all) != 4 || total != 4 {
		t.Errorf("%d of %d entries", len(all), total)
	}

	errorsOnly, total, err := repo.List(ctx(t), "error", 0, 50)
	mustNoErr(t, err, "List")
	if len(errorsOnly) != 2 || total != 2 {
		t.Errorf("%d of %d error entries", len(errorsOnly), total)
	}

	// Newest first, like the runs.
	for i := 1; i < len(all); i++ {
		if all[i-1].CreatedAt.Before(all[i].CreatedAt) {
			t.Fatal("the log is not newest first")
		}
	}
}

// --- host overrides ---------------------------------------------------------

func TestHostOverrideLifecycle(t *testing.T) {
	s := fresh(t)
	repo := NewHostOverrideRepo(s)

	userID := seedUser(t, s, "admin@example.com", true)
	port := 8443

	id, err := repo.Create(ctx(t), &domain.HostOverride{
		Hostname: "shop.example.com", Address: "10.10.0.5", Port: &port,
		Note: "through the private network", Active: true, CreatedBy: &userID,
	})
	mustNoErr(t, err, "Create")

	got, err := repo.Get(ctx(t), id)
	mustNoErr(t, err, "Get")
	// host(address) strips the mask inet carries, so the value that comes back
	// is a dial address rather than 10.10.0.5/32.
	if got.Address != "10.10.0.5" {
		t.Errorf("address = %q, want a bare address", got.Address)
	}
	if got.Port == nil || *got.Port != 8443 {
		t.Errorf("port = %v", got.Port)
	}
	if got.CreatedBy == nil || *got.CreatedBy != userID {
		t.Errorf("created_by = %v; a route must not be anonymous", got.CreatedBy)
	}

	// One route per hostname and port, with the "every port" row folded into
	// the same rule by coalesce.
	if _, err := repo.Create(ctx(t), &domain.HostOverride{
		Hostname: "shop.example.com", Address: "10.10.0.6", Port: &port, Active: true,
	}); !errors.Is(err, ErrDuplicate) {
		t.Errorf("a duplicate route returned %v, want ErrDuplicate", err)
	}
	// The same host on every port is a different route.
	anyPort, err := repo.Create(ctx(t), &domain.HostOverride{
		Hostname: "shop.example.com", Address: "10.10.0.7", Active: true,
	})
	mustNoErr(t, err, "Create for every port")
	// And that one cannot be entered twice either.
	if _, err := repo.Create(ctx(t), &domain.HostOverride{
		Hostname: "shop.example.com", Address: "10.10.0.8", Active: true,
	}); !errors.Is(err, ErrDuplicate) {
		t.Errorf("a duplicate every-port route returned %v, want ErrDuplicate", err)
	}

	got.Active = false
	mustNoErr(t, repo.Update(ctx(t), got), "Update")

	all, err := repo.List(ctx(t))
	mustNoErr(t, err, "List")
	if len(all) != 2 {
		t.Errorf("%d routes, want 2 including the disabled one", len(all))
	}
	// A disabled route stays visible on the screen and out of the dialer.
	active, err := repo.ListActive(ctx(t))
	mustNoErr(t, err, "ListActive")
	if len(active) != 1 || active[0].ID != anyPort {
		t.Errorf("%d active routes: %+v", len(active), active)
	}

	mustNoErr(t, repo.Delete(ctx(t), id), "Delete")
	if _, err := repo.Get(ctx(t), id); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get after a delete = %v, want ErrNotFound", err)
	}
	if err := repo.Delete(ctx(t), id); !errors.Is(err, ErrNotFound) {
		t.Errorf("deleting it twice = %v, want ErrNotFound", err)
	}
}

func TestAHostOverrideStoresAnIPv6Address(t *testing.T) {
	// inet holds both families, and JoinHostPort brackets the v6 form. A route
	// that came back unbracketed would produce a dial address that is not one.
	s := fresh(t)
	repo := NewHostOverrideRepo(s)

	_, err := repo.Create(ctx(t), &domain.HostOverride{
		Hostname: "shop.example.com", Address: "fd00::5", Active: true,
	})
	mustNoErr(t, err, "Create")

	rows, err := repo.ListActive(ctx(t))
	mustNoErr(t, err, "ListActive")
	if len(rows) != 1 {
		t.Fatalf("%d routes", len(rows))
	}
	if net.ParseIP(rows[0].Address) == nil {
		t.Errorf("address = %q, which is not an address", rows[0].Address)
	}
	if _, _, err := net.SplitHostPort(net.JoinHostPort(rows[0].Address, "443")); err != nil {
		t.Errorf("the stored address does not make a dial address: %v", err)
	}
}

func TestARouteKeepsTheAccountThatWroteIt(t *testing.T) {
	// ON DELETE RESTRICT: deleting the account that added a route must not
	// silently change where traffic goes.
	s := fresh(t)
	repo := NewHostOverrideRepo(s)

	userID := seedUser(t, s, "admin@example.com", true)
	_, err := repo.Create(ctx(t), &domain.HostOverride{
		Hostname: "shop.example.com", Address: "10.10.0.5", Active: true, CreatedBy: &userID,
	})
	mustNoErr(t, err, "Create")

	if _, err := s.db.Exec(`DELETE FROM users WHERE id = $1`, userID); err == nil {
		t.Error("the account was deleted while a route still points at it")
	}
}

// --- authorization ----------------------------------------------------------

func TestRolesAndGrants(t *testing.T) {
	s := fresh(t)
	repo := NewAuthzRepo(s)

	// The catalogue has to exist first: role permissions are a foreign key into
	// it, which is what stops a role naming a permission nobody defined.
	_, err := s.db.Exec(
		`INSERT INTO grantz_permissions (key, resource, action) VALUES
			('jobs.read', 'jobs', 'read'), ('jobs.update', 'jobs', 'update'),
			('runs.read', 'runs', 'read')`)
	mustNoErr(t, err, "seeding the catalogue")

	roleID, err := repo.EnsureRole(ctx(t), authz.Role{
		Key: "project_reader", Name: "Reader", Description: "Sees things",
	})
	mustNoErr(t, err, "EnsureRole")

	// Idempotent: it runs at every boot.
	again, err := repo.EnsureRole(ctx(t), authz.Role{
		Key: "project_reader", Name: "Reader", Description: "Sees things",
	})
	mustNoErr(t, err, "EnsureRole")
	if again != roleID {
		t.Errorf("EnsureRole made a second role: %d then %d", roleID, again)
	}

	mustNoErr(t, repo.ReplaceRolePermissions(ctx(t), roleID,
		[]string{"jobs.read", "runs.read"}), "ReplaceRolePermissions")

	byKey, err := repo.RoleIDByKey(ctx(t), "project_reader")
	mustNoErr(t, err, "RoleIDByKey")
	if byKey != roleID {
		t.Errorf("RoleIDByKey = %d, want %d", byKey, roleID)
	}
	if _, err := repo.RoleIDByKey(ctx(t), "made-up"); err == nil {
		t.Error("an unknown role key resolved")
	}

	roles, err := repo.ListRoles(ctx(t))
	mustNoErr(t, err, "ListRoles")
	if len(roles) != 1 || len(roles[0].Permissions) != 2 {
		t.Errorf("roles = %+v", roles)
	}

	// Replacing is a replacement, which is what makes upgrading the binary
	// upgrade what a role can do.
	mustNoErr(t, repo.ReplaceRolePermissions(ctx(t), roleID, []string{"jobs.read"}),
		"ReplaceRolePermissions")
	roles, _ = repo.ListRoles(ctx(t))
	if len(roles[0].Permissions) != 1 {
		t.Errorf("permissions after a replace = %v", roles[0].Permissions)
	}

	// Grants.
	userID := seedUser(t, s, "reader@example.com", false)
	shop := seedProject(t, s, "shop", "https://shop.example.com")
	alpha := seedProject(t, s, "alpha", "https://alpha.example.com")

	mustNoErr(t, repo.GrantProject(ctx(t), userID, roleID, shop), "GrantProject")
	mustNoErr(t, repo.GrantProject(ctx(t), userID, roleID, alpha), "GrantProject")

	// One assignment carrying both projects, not two rows: the table is keyed
	// on (user_id, role_id).
	assignments, err := repo.ListUserRoles(ctx(t), userID)
	mustNoErr(t, err, "ListUserRoles")
	if len(assignments) != 1 {
		t.Fatalf("%d assignments, want 1 carrying both projects", len(assignments))
	}
	if len(assignments[0].ProjectIDs) != 2 {
		t.Errorf("projects = %v, want both", assignments[0].ProjectIDs)
	}

	members, err := repo.ListProjectMembers(ctx(t), shop)
	mustNoErr(t, err, "ListProjectMembers")
	if len(members) != 1 || members[0].UserID != userID {
		t.Errorf("members = %+v", members)
	}
	if members[0].Email != "reader@example.com" {
		t.Errorf("the member list does not carry the account's identity: %+v", members[0])
	}

	mustNoErr(t, repo.RevokeProject(ctx(t), userID, roleID, alpha), "RevokeProject")
	assignments, _ = repo.ListUserRoles(ctx(t), userID)
	if len(assignments) != 1 || len(assignments[0].ProjectIDs) != 1 {
		t.Errorf("assignments after a revoke = %+v", assignments)
	}

	// Revoking the last project deletes the assignment rather than leaving an
	// empty scope, which grantz would read as unscoped and therefore as every
	// project.
	mustNoErr(t, repo.RevokeProject(ctx(t), userID, roleID, shop), "RevokeProject")
	assignments, _ = repo.ListUserRoles(ctx(t), userID)
	for _, a := range assignments {
		if a.Unscoped {
			t.Fatal("revoking the last project left an unscoped grant, which reaches everything")
		}
	}

	// And revoking a user clears the lot.
	mustNoErr(t, repo.GrantProject(ctx(t), userID, roleID, shop), "GrantProject")
	mustNoErr(t, repo.RevokeUser(ctx(t), userID), "RevokeUser")
	assignments, _ = repo.ListUserRoles(ctx(t), userID)
	if len(assignments) != 0 {
		t.Errorf("assignments after RevokeUser = %+v", assignments)
	}
}

func TestRoleFieldRestrictions(t *testing.T) {
	s := fresh(t)
	repo := NewAuthzRepo(s)

	_, err := s.db.Exec(
		`INSERT INTO grantz_permissions (key, resource, action, has_fields)
		 VALUES ('runs.read', 'runs', 'read', true)`)
	mustNoErr(t, err, "seeding the catalogue")

	roleID, err := repo.EnsureRole(ctx(t), authz.Role{Key: "project_reader", Name: "Reader"})
	mustNoErr(t, err, "EnsureRole")
	mustNoErr(t, repo.ReplaceRolePermissions(ctx(t), roleID, []string{"runs.read"}),
		"ReplaceRolePermissions")

	// No restriction is not the same as an empty one: unrestricted means every
	// field, and an empty list means none.
	fields, restricted, err := repo.RoleFields(ctx(t), roleID, "runs.read")
	mustNoErr(t, err, "RoleFields")
	if restricted {
		t.Errorf("a fresh role is already restricted to %v", fields)
	}

	mustNoErr(t, repo.SetRoleFields(ctx(t), roleID, "runs.read", []string{"error"}),
		"SetRoleFields")
	fields, restricted, err = repo.RoleFields(ctx(t), roleID, "runs.read")
	mustNoErr(t, err, "RoleFields")
	if !restricted || len(fields) != 1 || fields[0] != "error" {
		t.Errorf("fields = %v, restricted = %v", fields, restricted)
	}

	// An empty list is a real restriction: the role sees none of them.
	mustNoErr(t, repo.SetRoleFields(ctx(t), roleID, "runs.read", []string{}), "SetRoleFields")
	fields, restricted, err = repo.RoleFields(ctx(t), roleID, "runs.read")
	mustNoErr(t, err, "RoleFields")
	if !restricted || len(fields) != 0 {
		t.Errorf("an empty allow-list came back as fields = %v, restricted = %v", fields, restricted)
	}

	// And nil clears it.
	mustNoErr(t, repo.SetRoleFields(ctx(t), roleID, "runs.read", nil), "SetRoleFields")
	_, restricted, _ = repo.RoleFields(ctx(t), roleID, "runs.read")
	if restricted {
		t.Error("nil did not clear the restriction")
	}
}

func TestDeletingAnAccountTakesItsGrantsWithIt(t *testing.T) {
	// ON DELETE CASCADE on grantz_user_roles. An orphaned grant is a grant
	// nobody can see to revoke.
	s := fresh(t)
	repo := NewAuthzRepo(s)

	_, err := s.db.Exec(
		`INSERT INTO grantz_permissions (key, resource, action) VALUES ('jobs.read', 'jobs', 'read')`)
	mustNoErr(t, err, "seeding the catalogue")

	roleID, err := repo.EnsureRole(ctx(t), authz.Role{Key: "project_reader", Name: "Reader"})
	mustNoErr(t, err, "EnsureRole")

	userID := seedUser(t, s, "gone@example.com", false)
	shop := seedProject(t, s, "shop", "https://shop.example.com")
	mustNoErr(t, repo.GrantProject(ctx(t), userID, roleID, shop), "GrantProject")

	_, err = s.db.Exec(`DELETE FROM users WHERE id = $1`, userID)
	mustNoErr(t, err, "deleting the account")

	var count int
	mustNoErr(t, s.db.QueryRow(
		`SELECT count(*) FROM grantz_user_roles WHERE user_id = $1`, userID).Scan(&count),
		"counting grants")
	if count != 0 {
		t.Errorf("%d grants outlived the account", count)
	}
}
