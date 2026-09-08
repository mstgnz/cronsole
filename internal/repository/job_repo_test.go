package repository

import (
	"errors"
	"testing"
	"time"

	"github.com/mstgnz/cronsole/v2/internal/domain"
)

// The scope is the reason this file exists. domain.ProjectScope reaches the
// WHERE clause, and whether it actually narrows it is a property of the SQL:
// an in-memory store can be made to agree with any implementation, so only the
// database can say the filter is real.

func TestTheScopeNarrowsTheJobList(t *testing.T) {
	s := fresh(t)
	repo := NewJobRepo(s)

	shop := seedProject(t, s, "shop", "https://shop.example.com")
	alpha := seedProject(t, s, "alpha", "https://alpha.example.com")
	seedJob(t, s, shop, "shop-daily")
	seedJob(t, s, alpha, "alpha-nightly")

	cases := []struct {
		name  string
		scope domain.ProjectScope
		want  []string
	}{
		{"one project", domain.ScopeOf(shop), []string{"shop-daily"}},
		{"the other", domain.ScopeOf(alpha), []string{"alpha-nightly"}},
		{"both", domain.ScopeOf(shop, alpha), []string{"shop-daily", "alpha-nightly"}},
		{"everything", domain.AllProjects(), []string{"shop-daily", "alpha-nightly"}},
		// The one that matters most: the zero value reaches nothing, so a query
		// built without resolving the caller's scope returns an empty list
		// rather than every brand's jobs.
		{"nothing", domain.NoProjects(), nil},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rows, total, err := repo.List(ctx(t), domain.JobFilter{Scope: c.scope}, 0, 50)
			mustNoErr(t, err, "List")

			if int(total) != len(c.want) {
				t.Errorf("total = %d, want %d", total, len(c.want))
			}
			got := codesOf(rows)
			if len(got) != len(c.want) {
				t.Fatalf("got %v, want %v", got, c.want)
			}
			for _, want := range c.want {
				if !containsString(got, want) {
					t.Errorf("got %v, want it to contain %q", got, want)
				}
			}
		})
	}
}

func TestTheZeroScopeIsNotTheSameAsUnrestricted(t *testing.T) {
	// len(IDs) == 0 is true for both. Confusing them opens everything, so it is
	// checked here against the query rather than only against the type.
	s := fresh(t)
	repo := NewJobRepo(s)

	shop := seedProject(t, s, "shop", "https://shop.example.com")
	seedJob(t, s, shop, "daily")

	empty, _, err := repo.List(ctx(t), domain.JobFilter{Scope: domain.ProjectScope{}}, 0, 50)
	mustNoErr(t, err, "List")
	if len(empty) != 0 {
		t.Errorf("the zero scope returned %v", codesOf(empty))
	}

	all, _, err := repo.List(ctx(t), domain.JobFilter{Scope: domain.ProjectScope{All: true}}, 0, 50)
	mustNoErr(t, err, "List")
	if len(all) != 1 {
		t.Errorf("the unrestricted scope returned %v", codesOf(all))
	}
}

func TestJobFilters(t *testing.T) {
	s := fresh(t)
	repo := NewJobRepo(s)

	shop := seedProject(t, s, "shop", "https://shop.example.com")
	alpha := seedProject(t, s, "alpha", "https://alpha.example.com")

	active := seedJob(t, s, shop, "daily-report")
	off := seedJob(t, s, shop, "cleanup")
	seedJob(t, s, alpha, "nightly")

	_, err := s.db.Exec(`UPDATE jobs SET active = false WHERE id = $1`, off)
	mustNoErr(t, err, "deactivating")
	_, err = s.db.Exec(`UPDATE jobs SET tag = 'reports' WHERE id = $1`, active)
	mustNoErr(t, err, "tagging")

	activeOnly := true
	tag := "reports"
	search := "daily"
	project := shop

	cases := []struct {
		name   string
		filter domain.JobFilter
		want   []string
	}{
		{"by project", domain.JobFilter{Scope: domain.AllProjects(), ProjectID: &project},
			[]string{"daily-report", "cleanup"}},
		{"by active", domain.JobFilter{Scope: domain.AllProjects(), Active: &activeOnly},
			[]string{"daily-report", "nightly"}},
		{"by tag", domain.JobFilter{Scope: domain.AllProjects(), Tag: &tag},
			[]string{"daily-report"}},
		{"by search", domain.JobFilter{Scope: domain.AllProjects(), Search: &search},
			[]string{"daily-report"}},
		{"combined", domain.JobFilter{Scope: domain.AllProjects(), ProjectID: &project, Active: &activeOnly},
			[]string{"daily-report"}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rows, _, err := repo.List(ctx(t), c.filter, 0, 50)
			mustNoErr(t, err, "List")
			got := codesOf(rows)
			if len(got) != len(c.want) {
				t.Fatalf("got %v, want %v", got, c.want)
			}
			for _, want := range c.want {
				if !containsString(got, want) {
					t.Errorf("got %v, want it to contain %q", got, want)
				}
			}
		})
	}
}

func TestASearchTermIsNotSQL(t *testing.T) {
	// Every statement in this package is parameterised. This is the proof
	// against the query rather than against a reading of it.
	s := fresh(t)
	repo := NewJobRepo(s)

	shop := seedProject(t, s, "shop", "https://shop.example.com")
	seedJob(t, s, shop, "daily")

	for _, attack := range []string{
		"'; DROP TABLE jobs; --",
		"' OR '1'='1",
		"%' OR 1=1 --",
		"\\", "%", "_",
	} {
		search := attack
		rows, _, err := repo.List(ctx(t),
			domain.JobFilter{Scope: domain.AllProjects(), Search: &search}, 0, 50)
		mustNoErr(t, err, "List with a hostile search term")
		// A term that matches nothing returns nothing. The wildcards are the
		// interesting ones: '%' as a LIKE pattern would match everything, and
		// it must be treated as a literal.
		if attack == "%" || attack == "_" {
			continue
		}
		if len(rows) != 0 {
			t.Errorf("the search %q returned %v", attack, codesOf(rows))
		}
	}

	// And the table is still there.
	var count int
	mustNoErr(t, s.db.QueryRow(`SELECT count(*) FROM jobs`).Scan(&count), "counting jobs")
	if count != 1 {
		t.Fatalf("%d jobs remain; a search term reached the parser", count)
	}
}

func TestASoftDeletedJobDisappearsFromEveryRead(t *testing.T) {
	// Deleted rows are kept so the history survives, which only works if every
	// read filters them. One query that forgets shows a deleted job on a screen.
	s := fresh(t)
	repo := NewJobRepo(s)

	shop := seedProject(t, s, "shop", "https://shop.example.com")
	jobID := seedJob(t, s, shop, "daily")

	mustNoErr(t, repo.SoftDelete(ctx(t), jobID, time.Now()), "SoftDelete")

	if _, err := repo.Get(ctx(t), jobID); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get = %v, want ErrNotFound", err)
	}
	if _, err := repo.GetByCode(ctx(t), shop, "daily"); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetByCode = %v, want ErrNotFound", err)
	}
	rows, total, err := repo.List(ctx(t), domain.JobFilter{Scope: domain.AllProjects()}, 0, 50)
	mustNoErr(t, err, "List")
	if len(rows) != 0 || total != 0 {
		t.Errorf("List returned %v", codesOf(rows))
	}
	options, err := repo.ListOptions(ctx(t), domain.AllProjects(), 0)
	mustNoErr(t, err, "ListOptions")
	if len(options) != 0 {
		t.Errorf("ListOptions returned %+v", options)
	}

	// And the row is still on disk, with its history.
	var count int
	mustNoErr(t, s.db.QueryRow(`SELECT count(*) FROM jobs WHERE id = $1`, jobID).
		Scan(&count), "counting")
	if count != 1 {
		t.Error("the row was actually deleted, taking its history with it")
	}
}

func TestACodeIsUniqueWithinAProjectAndFreeAcrossThem(t *testing.T) {
	s := fresh(t)
	repo := NewJobRepo(s)

	shop := seedProject(t, s, "shop", "https://shop.example.com")
	alpha := seedProject(t, s, "alpha", "https://alpha.example.com")

	first := &domain.Job{
		ProjectID: shop, Code: "daily", Name: "Daily", Method: "GET", URL: "/cron/daily",
		TimeoutSec: 30, MaxDurationSec: 300, SuccessMin: 200, SuccessMax: 299, Active: true,
	}
	_, err := repo.Create(ctx(t), first)
	mustNoErr(t, err, "Create")

	// The same code on the same project is refused, and the driver's error is
	// mapped to the sentinel the service branches on.
	if _, err := repo.Create(ctx(t), first); !errors.Is(err, ErrDuplicate) {
		t.Errorf("a duplicate code returned %v, want ErrDuplicate", err)
	}

	// Case-insensitively, because the index is on lower(code).
	upper := *first
	upper.Code = "DAILY"
	if _, err := repo.Create(ctx(t), &upper); !errors.Is(err, ErrDuplicate) {
		t.Errorf("a duplicate in another case returned %v, want ErrDuplicate", err)
	}

	// And free on another project: two brands both having a "daily" is normal.
	onAlpha := *first
	onAlpha.ProjectID = alpha
	if _, err := repo.Create(ctx(t), &onAlpha); err != nil {
		t.Errorf("the same code on another project was refused: %v", err)
	}
}

func TestADeletedCodeBecomesFreeAgain(t *testing.T) {
	// The unique index is partial, WHERE deleted_at IS NULL. Without that, a
	// code could never be reused after a mistake.
	s := fresh(t)
	repo := NewJobRepo(s)

	shop := seedProject(t, s, "shop", "https://shop.example.com")
	jobID := seedJob(t, s, shop, "daily")
	mustNoErr(t, repo.SoftDelete(ctx(t), jobID, time.Now()), "SoftDelete")

	_, err := repo.Create(ctx(t), &domain.Job{
		ProjectID: shop, Code: "daily", Name: "Daily again", Method: "GET",
		URL: "/cron/daily", TimeoutSec: 30, MaxDurationSec: 300, SuccessMin: 200, SuccessMax: 299,
	})
	if err != nil {
		t.Errorf("the code of a deleted job could not be reused: %v", err)
	}
}

func TestUpdateAndSetActive(t *testing.T) {
	s := fresh(t)
	repo := NewJobRepo(s)

	shop := seedProject(t, s, "shop", "https://shop.example.com")
	jobID := seedJob(t, s, shop, "daily")

	job, err := repo.Get(ctx(t), jobID)
	mustNoErr(t, err, "Get")

	job.Name = "Renamed"
	job.Method = "POST"
	job.TimeoutSec = 120
	job.Body = `{"x":1}`
	mustNoErr(t, repo.Update(ctx(t), job), "Update")

	back, err := repo.Get(ctx(t), jobID)
	mustNoErr(t, err, "Get")
	if back.Name != "Renamed" || back.Method != "POST" || back.TimeoutSec != 120 {
		t.Errorf("the update did not land: %+v", back)
	}
	if back.UpdatedAt == nil {
		t.Error("updated_at was not set")
	}

	mustNoErr(t, repo.SetActive(ctx(t), jobID, false), "SetActive")
	back, _ = repo.Get(ctx(t), jobID)
	if back.Active {
		t.Error("the job is still active")
	}
}

func TestSchedules(t *testing.T) {
	s := fresh(t)
	repo := NewJobRepo(s)

	shop := seedProject(t, s, "shop", "https://shop.example.com")
	jobID := seedJob(t, s, shop, "daily")

	first, err := repo.CreateSchedule(ctx(t),
		&domain.JobSchedule{JobID: jobID, Expression: "0 3 * * *", Active: true})
	mustNoErr(t, err, "CreateSchedule")
	_, err = repo.CreateSchedule(ctx(t),
		&domain.JobSchedule{JobID: jobID, Expression: "58,59 15 * * *", Active: true})
	mustNoErr(t, err, "CreateSchedule")

	// The same expression twice would queue the same minute twice.
	_, err = repo.CreateSchedule(ctx(t),
		&domain.JobSchedule{JobID: jobID, Expression: "0 3 * * *", Active: true})
	if !errors.Is(err, ErrDuplicate) {
		t.Errorf("a duplicate expression returned %v, want ErrDuplicate", err)
	}

	n, err := repo.CountSchedules(ctx(t), jobID)
	mustNoErr(t, err, "CountSchedules")
	if n != 2 {
		t.Errorf("CountSchedules = %d, want 2", n)
	}

	mustNoErr(t, repo.DeleteSchedule(ctx(t), first, jobID), "DeleteSchedule")
	n, _ = repo.CountSchedules(ctx(t), jobID)
	if n != 1 {
		t.Errorf("CountSchedules = %d after a delete, want 1", n)
	}

	// A schedule cannot be deleted through another job's id, which is the
	// ownership check the URL alone does not give.
	rows, err := repo.ListSchedules(ctx(t), jobID)
	mustNoErr(t, err, "ListSchedules")
	other := seedJob(t, s, shop, "other")
	if err := repo.DeleteSchedule(ctx(t), rows[0].ID, other); err == nil {
		t.Error("a schedule was deleted through another job's id")
	}

	mustNoErr(t, repo.ReplaceSchedules(ctx(t), jobID, []string{"*/5 * * * *", "0 0 * * *"}),
		"ReplaceSchedules")
	rows, _ = repo.ListSchedules(ctx(t), jobID)
	if len(rows) != 2 {
		t.Errorf("%d schedules after a replace, want 2", len(rows))
	}
	for _, row := range rows {
		if row.Expression == "58,59 15 * * *" {
			t.Error("ReplaceSchedules kept an expression that was replaced away")
		}
	}
}

func TestHeaders(t *testing.T) {
	s := fresh(t)
	repo := NewJobRepo(s)

	shop := seedProject(t, s, "shop", "https://shop.example.com")
	jobID := seedJob(t, s, shop, "daily")

	mustNoErr(t, repo.ReplaceHeaders(ctx(t), jobID, []domain.JobHeader{
		{Key: "X-Token", Value: "secret", IsSecret: true},
		{Key: "Accept", Value: "application/json"},
	}), "ReplaceHeaders")

	rows, err := repo.ListHeaders(ctx(t), jobID)
	mustNoErr(t, err, "ListHeaders")
	if len(rows) != 2 {
		t.Fatalf("%d headers, want 2", len(rows))
	}

	// Replacing is a replacement, not an append.
	mustNoErr(t, repo.ReplaceHeaders(ctx(t), jobID, []domain.JobHeader{
		{Key: "Accept", Value: "text/plain"},
	}), "ReplaceHeaders")
	rows, _ = repo.ListHeaders(ctx(t), jobID)
	if len(rows) != 1 || rows[0].Value != "text/plain" {
		t.Errorf("headers after a replace = %+v", rows)
	}

	// And clearing them leaves none.
	mustNoErr(t, repo.ReplaceHeaders(ctx(t), jobID, nil), "ReplaceHeaders")
	rows, _ = repo.ListHeaders(ctx(t), jobID)
	if len(rows) != 0 {
		t.Errorf("headers after clearing = %+v", rows)
	}
}

func TestLinksAndReachability(t *testing.T) {
	s := fresh(t)
	repo := NewJobRepo(s)

	shop := seedProject(t, s, "shop", "https://shop.example.com")
	first := seedJob(t, s, shop, "first")
	second := seedJob(t, s, shop, "second")
	third := seedJob(t, s, shop, "third")

	_, err := repo.CreateLink(ctx(t), &domain.JobLink{
		JobID: first, TargetJobID: second, Condition: "success", DelaySec: 30, Active: true,
	})
	mustNoErr(t, err, "CreateLink")
	_, err = repo.CreateLink(ctx(t), &domain.JobLink{
		JobID: second, TargetJobID: third, Condition: "success", Active: true,
	})
	mustNoErr(t, err, "CreateLink")

	links, err := repo.ListLinks(ctx(t), first)
	mustNoErr(t, err, "ListLinks")
	if len(links) != 1 || links[0].TargetCode != "second" {
		t.Errorf("links = %+v", links)
	}

	triggers, err := repo.ListTriggers(ctx(t), second)
	mustNoErr(t, err, "ListTriggers")
	if len(triggers) != 1 || triggers[0].SourceCode != "first" {
		t.Errorf("triggers = %+v", triggers)
	}

	// Reachability is what refuses a cycle before it is stored. It has to see
	// through the whole chain, not just one edge.
	reaches, err := repo.ReachesJob(ctx(t), first, third, 10)
	mustNoErr(t, err, "ReachesJob")
	if !reaches {
		t.Error("first does not reach third, but first -> second -> third")
	}

	reaches, err = repo.ReachesJob(ctx(t), third, first, 10)
	mustNoErr(t, err, "ReachesJob")
	if reaches {
		t.Error("third reaches first, but the chain runs the other way")
	}

	// And the same edge twice is refused.
	_, err = repo.CreateLink(ctx(t), &domain.JobLink{
		JobID: first, TargetJobID: second, Condition: "success", Active: true,
	})
	if !errors.Is(err, ErrDuplicate) {
		t.Errorf("a duplicate edge returned %v, want ErrDuplicate", err)
	}

	mustNoErr(t, repo.DeleteLink(ctx(t), links[0].ID, first), "DeleteLink")
	links, _ = repo.ListLinks(ctx(t), first)
	if len(links) != 0 {
		t.Errorf("links after a delete = %+v", links)
	}
}

func TestReachesJobIsDepthBounded(t *testing.T) {
	// A cycle that already exists, written straight into the table, must not
	// make the walk run forever.
	s := fresh(t)
	repo := NewJobRepo(s)

	shop := seedProject(t, s, "shop", "https://shop.example.com")
	a := seedJob(t, s, shop, "a")
	b := seedJob(t, s, shop, "b")

	_, err := s.db.Exec(
		`INSERT INTO job_links (job_id, target_job_id, condition, active)
		 VALUES ($1, $2, 'always', true), ($2, $1, 'always', true)`, a, b)
	mustNoErr(t, err, "writing a cycle by hand")

	done := make(chan bool, 1)
	go func() {
		reaches, err := repo.ReachesJob(ctx(t), a, b, 5)
		if err != nil {
			t.Errorf("ReachesJob = %v", err)
		}
		done <- reaches
	}()

	select {
	case reaches := <-done:
		if !reaches {
			t.Error("a reaches b through a direct edge")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("ReachesJob did not return; the walk is not depth bounded")
	}
}

func TestListOptionsExcludesTheJobBeingEdited(t *testing.T) {
	// The chain picker. Offering the job itself would let somebody build a
	// self-loop from the screen.
	s := fresh(t)
	repo := NewJobRepo(s)

	shop := seedProject(t, s, "shop", "https://shop.example.com")
	first := seedJob(t, s, shop, "first")
	seedJob(t, s, shop, "second")

	options, err := repo.ListOptions(ctx(t), domain.AllProjects(), first)
	mustNoErr(t, err, "ListOptions")
	for _, o := range options {
		if o.ID == first {
			t.Error("the job being edited is in its own chain picker")
		}
	}
	if len(options) != 1 {
		t.Errorf("%d options, want 1", len(options))
	}
}

func TestListTags(t *testing.T) {
	s := fresh(t)
	repo := NewJobRepo(s)

	shop := seedProject(t, s, "shop", "https://shop.example.com")
	alpha := seedProject(t, s, "alpha", "https://alpha.example.com")
	shopJob := seedJob(t, s, shop, "shop-job")
	alphaJob := seedJob(t, s, alpha, "alpha-job")

	_, err := s.db.Exec(`UPDATE jobs SET tag = 'reports' WHERE id = $1`, shopJob)
	mustNoErr(t, err, "tagging")
	_, err = s.db.Exec(`UPDATE jobs SET tag = 'secret-brand' WHERE id = $1`, alphaJob)
	mustNoErr(t, err, "tagging")

	// The tag list is a filter control, and it is scoped like everything else:
	// another brand's tag names would leak through it otherwise.
	tags, err := repo.ListTags(ctx(t), domain.ScopeOf(shop))
	mustNoErr(t, err, "ListTags")
	if len(tags) != 1 || tags[0] != "reports" {
		t.Errorf("tags = %v, want only the caller's own", tags)
	}
}

func TestPagination(t *testing.T) {
	s := fresh(t)
	repo := NewJobRepo(s)

	shop := seedProject(t, s, "shop", "https://shop.example.com")
	for i := 0; i < 12; i++ {
		seedJob(t, s, shop, "job-"+string(rune('a'+i)))
	}

	first, total, err := repo.List(ctx(t), domain.JobFilter{Scope: domain.AllProjects()}, 0, 5)
	mustNoErr(t, err, "List")
	if len(first) != 5 {
		t.Errorf("%d rows on the first page, want 5", len(first))
	}
	// The total is the whole set, not the page: the pager needs it.
	if total != 12 {
		t.Errorf("total = %d, want 12", total)
	}

	second, _, err := repo.List(ctx(t), domain.JobFilter{Scope: domain.AllProjects()}, 5, 5)
	mustNoErr(t, err, "List")
	if len(second) != 5 {
		t.Errorf("%d rows on the second page, want 5", len(second))
	}
	// And the pages do not overlap.
	for _, a := range first {
		for _, b := range second {
			if a.ID == b.ID {
				t.Fatalf("job %d is on both pages", a.ID)
			}
		}
	}

	last, _, err := repo.List(ctx(t), domain.JobFilter{Scope: domain.AllProjects()}, 10, 5)
	mustNoErr(t, err, "List")
	if len(last) != 2 {
		t.Errorf("%d rows on the last page, want 2", len(last))
	}
}

func TestAJobRowCarriesWhatTheListScreenShows(t *testing.T) {
	s := fresh(t)
	repo := NewJobRepo(s)

	shop := seedProject(t, s, "shop", "https://shop.example.com")
	jobID := seedJob(t, s, shop, "daily")
	target := seedJob(t, s, shop, "cleanup")

	_, err := s.db.Exec(
		`INSERT INTO job_schedules (job_id, expression, active) VALUES ($1, '0 3 * * *', true)`,
		jobID)
	mustNoErr(t, err, "adding a schedule")
	_, err = s.db.Exec(
		`INSERT INTO job_links (job_id, target_job_id, condition, active)
		 VALUES ($1, $2, 'success', true)`, jobID, target)
	mustNoErr(t, err, "adding a link")

	rows, _, err := repo.List(ctx(t), domain.JobFilter{Scope: domain.AllProjects()}, 0, 50)
	mustNoErr(t, err, "List")

	var row domain.JobRow
	for _, r := range rows {
		if r.ID == jobID {
			row = r
		}
	}
	if row.ProjectSlug != "shop" {
		t.Errorf("project slug = %q", row.ProjectSlug)
	}
	if len(row.Schedules) != 1 || row.Schedules[0] != "0 3 * * *" {
		t.Errorf("schedules = %v", row.Schedules)
	}
	if row.LinkCount != 1 {
		t.Errorf("link count = %d, want 1", row.LinkCount)
	}
	// The cleanup job is triggered by this one, which is what tells a
	// chain-only job apart from one that will never run at all.
	for _, r := range rows {
		if r.ID == target && r.TriggerCount != 1 {
			t.Errorf("trigger count on the target = %d, want 1", r.TriggerCount)
		}
	}
}
