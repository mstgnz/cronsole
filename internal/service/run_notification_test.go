package service

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/mstgnz/cronsole/v2/internal/domain"
	"github.com/mstgnz/cronsole/v2/internal/repository"
)

func TestTriggerQueuesAndHandsOver(t *testing.T) {
	// A manual run queues a row and hands it over; it does not execute
	// anything itself. Both halves matter: the row makes the request durable
	// and visible in the history, and the hand-off keeps it subject to the
	// same single run rule and concurrency cap as a scheduled run.
	store := newMemStore()
	store.addProject(1, "demo", "")
	jobs := newTestJobService(store)
	ctx := context.Background()

	id, err := jobs.Create(ctx, allProjects, &JobInput{
		ProjectID: 1, Code: "manual", Name: "Manual", URL: "https://example.com/x",
	})
	if err != nil {
		t.Fatal(err)
	}

	runs := &memRunRepo{}
	var handed []int64
	var mu sync.Mutex
	svc := NewRunService(runs, memJobRepo{memStore: store}, func(runID int64, _ string) {
		mu.Lock()
		defer mu.Unlock()
		handed = append(handed, runID)
	})

	userID := int64(7)
	runID, err := svc.Trigger(ctx, allProjects, id, domain.TriggerManual, &userID)
	if err != nil {
		t.Fatalf("trigger failed: %v", err)
	}
	if runID == 0 {
		t.Error("no run id was returned")
	}
	if len(runs.enqueued) != 1 || runs.enqueued[0] != id {
		t.Errorf("enqueued %v, want the job id %d", runs.enqueued, id)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(handed) != 1 || handed[0] != runID {
		t.Errorf("handed over %v, want the queued run %d", handed, runID)
	}
}

func TestTriggerRejectsAnUnknownJob(t *testing.T) {
	store := newMemStore()
	svc := NewRunService(&memRunRepo{}, memJobRepo{memStore: store}, nil)

	if _, err := svc.Trigger(context.Background(), allProjects, 999, domain.TriggerManual, nil); !errors.Is(err, ErrNotFound) {
		t.Errorf("trigger gave %v, want ErrNotFound", err)
	}
}

func TestRunFilterFromRange(t *testing.T) {
	filter := RunFilterFromRange(7)
	if filter.Start == nil {
		t.Fatal("no start was set")
	}
	if age := time.Since(*filter.Start); age < 6*24*time.Hour || age > 8*24*time.Hour {
		t.Errorf("the window is %s wide, want about 7 days", age)
	}
	// A window of zero days would return nothing at all, so it is clamped.
	if RunFilterFromRange(0).Start == nil {
		t.Error("a zero window produced no start")
	}
}

// memNotifications is an in-memory recipient store.
type memNotifications struct {
	mu     sync.Mutex
	rows   map[int64]*domain.Notification
	nextID int64
}

func newNotificationRepo() *memNotifications {
	return &memNotifications{rows: map[int64]*domain.Notification{}, nextID: 1}
}

func (m *memNotifications) Get(_ context.Context, id int64) (*domain.Notification, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if n, ok := m.rows[id]; ok {
		copied := *n
		return &copied, nil
	}
	return nil, repository.ErrNotFound
}

func (m *memNotifications) List(context.Context) ([]domain.Notification, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []domain.Notification
	for _, n := range m.rows {
		out = append(out, *n)
	}
	return out, nil
}

func (m *memNotifications) Create(_ context.Context, n *domain.Notification) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	id := m.nextID
	m.nextID++
	copied := *n
	copied.ID = id
	m.rows[id] = &copied
	return id, nil
}

func (m *memNotifications) Update(_ context.Context, n *domain.Notification) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.rows[n.ID]; !ok {
		return repository.ErrNotFound
	}
	copied := *n
	m.rows[n.ID] = &copied
	return nil
}

func (m *memNotifications) SoftDelete(_ context.Context, id int64, _ time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.rows, id)
	return nil
}

func (m *memNotifications) ReplaceEmails(_ context.Context, id int64, emails []string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if n, ok := m.rows[id]; ok {
		n.Emails = emails
	}
	return nil
}

func TestNotificationValidation(t *testing.T) {
	svc := NewNotificationService(newNotificationRepo())
	ctx := context.Background()

	_, err := svc.Create(ctx, NotificationInput{Name: "", Emails: nil}, nil)
	got := fieldErrors(t, err)
	if _, ok := got["name"]; !ok {
		t.Errorf("a missing name was not reported: %v", got)
	}
	if _, ok := got["emails"]; !ok {
		t.Errorf("an empty address list was not reported: %v", got)
	}

	// A list that reports neither outcome is attached to jobs and then never
	// sends anything, which reads as a broken mail server.
	_, err = svc.Create(ctx, NotificationInput{
		Name: "Silent", Emails: []string{"ops@example.com"},
		OnSuccess: false, OnFailure: false,
	}, nil)
	if _, ok := fieldErrors(t, err)["on_failure"]; !ok {
		t.Error("a list reporting no outcome was accepted")
	}

	if _, err := svc.Create(ctx, NotificationInput{
		Name: "Bad address", Emails: []string{"not-an-address"}, OnFailure: true,
	}, nil); err == nil {
		t.Error("an address with no @ was accepted")
	}
}

func TestNotificationNormalisesAddresses(t *testing.T) {
	repo := newNotificationRepo()
	svc := NewNotificationService(repo)
	ctx := context.Background()

	id, err := svc.Create(ctx, NotificationInput{
		Name:      "On call",
		Emails:    []string{"  OPS@Example.com  ", "", "second@example.com"},
		OnFailure: true,
		Active:    true,
	}, nil)
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}

	stored, err := svc.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored.Emails) != 2 {
		t.Fatalf("emails = %v, want the blank one dropped", stored.Emails)
	}
	if stored.Emails[0] != "ops@example.com" {
		t.Errorf("emails[0] = %q, want it trimmed and lower case", stored.Emails[0])
	}
}

func TestNotificationUpdateAndDelete(t *testing.T) {
	repo := newNotificationRepo()
	svc := NewNotificationService(repo)
	ctx := context.Background()

	id, err := svc.Create(ctx, NotificationInput{
		Name: "On call", Emails: []string{"ops@example.com"}, OnFailure: true, Active: true,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}

	if err := svc.Update(ctx, id, NotificationInput{
		Name: "Renamed", Emails: []string{"new@example.com"}, OnSuccess: true, Active: true,
	}); err != nil {
		t.Fatalf("update failed: %v", err)
	}
	stored, _ := svc.Get(ctx, id)
	if stored.Name != "Renamed" || !stored.OnSuccess {
		t.Errorf("the update did not apply: %+v", stored)
	}

	if err := svc.Delete(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Get(ctx, id); !errors.Is(err, ErrNotFound) {
		t.Errorf("get after delete gave %v, want ErrNotFound", err)
	}

	if list, _ := svc.List(ctx); len(list) != 0 {
		t.Errorf("list = %v after delete, want empty", list)
	}
}

func TestJobServicePassThroughs(t *testing.T) {
	store := newMemStore()
	store.addProject(1, "demo", "")
	svc := newTestJobService(store)
	ctx := context.Background()

	id, err := svc.Create(ctx, allProjects, &JobInput{
		ProjectID: 1, Code: "listed", Name: "Listed", URL: "https://example.com/x",
		Schedules: []string{"*/5 * * * *"},
	})
	if err != nil {
		t.Fatal(err)
	}

	rows, total, err := svc.List(ctx, domain.JobFilter{Scope: allProjects}, 0, 20)
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 || len(rows) != 1 {
		t.Fatalf("list returned %d of %d, want 1", len(rows), total)
	}
	// The list computes the next run per row; a list that showed nothing there
	// would leave the operator unable to tell a live job from a dormant one.
	if rows[0].NextRun == nil {
		t.Error("the list row carried no next run")
	}

	if err := svc.SetActive(ctx, allProjects, id, true); err != nil {
		t.Fatal(err)
	}
	if job := store.job(id); !job.Active {
		t.Error("SetActive did not take effect")
	}

	added, err := svc.AddSchedule(ctx, allProjects, id, " 0 4 * * * ")
	if err != nil {
		t.Fatalf("AddSchedule failed: %v", err)
	}
	if added.Expression != "0 4 * * *" {
		t.Errorf("expression = %q, want it normalised", added.Expression)
	}
	if added.NextRun == nil || added.Description == "" {
		t.Error("the added schedule was not annotated")
	}
	if _, err := svc.AddSchedule(ctx, allProjects, id, "nonsense"); err == nil {
		t.Error("an invalid expression was added")
	}

	if err := svc.Delete(ctx, allProjects, id); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Get(ctx, allProjects, id); !errors.Is(err, ErrNotFound) {
		t.Errorf("get after delete gave %v, want ErrNotFound", err)
	}
}

func TestValidationErrorMessage(t *testing.T) {
	v := &ValidationError{}
	if v.OK() != true || v.ErrOrNil() != nil {
		t.Error("an empty ValidationError is not treated as success")
	}

	v.Add("code", "required").Add("url", "invalid")
	if !errors.Is(v, ErrValidation) {
		t.Error("a ValidationError does not unwrap to ErrValidation")
	}
	msg := v.Error()
	if msg == "" || msg == "validation failed" {
		t.Errorf("Error() = %q, want it to name the first failure", msg)
	}
	if v.ErrOrNil() == nil {
		t.Error("ErrOrNil returned nil with failures recorded")
	}
}

func TestReadPassThroughs(t *testing.T) {
	// These carry no logic of their own, but they are on every screen: a
	// silent failure here shows an empty page rather than an error, which is
	// the shape of bug that gets attributed to "no data yet".
	ctx := context.Background()

	store := newMemStore()
	store.addProject(1, "demo", "")
	jobs := newTestJobService(store)

	id, err := jobs.Create(ctx, allProjects, &JobInput{
		ProjectID: 1, Code: "reads", Name: "Reads", URL: "https://example.com/x",
		Schedules: []string{"* * * * *"},
	})
	if err != nil {
		t.Fatal(err)
	}
	other, err := jobs.Create(ctx, allProjects, &JobInput{
		ProjectID: 1, Code: "other", Name: "Other", URL: "https://example.com/y",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := jobs.AddLink(ctx, allProjects, id, other, "success", 5); err != nil {
		t.Fatal(err)
	}

	if _, err := jobs.ListOptions(ctx, allProjects, id); err != nil {
		t.Errorf("ListOptions failed: %v", err)
	}
	if _, err := jobs.ListTags(ctx, allProjects); err != nil {
		t.Errorf("ListTags failed: %v", err)
	}

	detail, err := jobs.Get(ctx, allProjects, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(detail.Links) != 1 {
		t.Fatalf("links = %d, want 1", len(detail.Links))
	}
	if err := jobs.RemoveLink(ctx, allProjects, id, detail.Links[0].ID); err != nil {
		t.Errorf("RemoveLink failed: %v", err)
	}
	if err := jobs.RemoveLink(ctx, allProjects, id, 9999); !errors.Is(err, ErrNotFound) {
		t.Errorf("removing an unknown link gave %v, want ErrNotFound", err)
	}

	runs := NewRunService(&memRunRepo{}, memJobRepo{memStore: store}, nil)
	if _, _, err := runs.List(ctx, domain.RunFilter{Scope: allProjects}, 0, 10); err != nil {
		t.Errorf("run list failed: %v", err)
	}
	if _, err := runs.Get(ctx, allProjects, 1); !errors.Is(err, ErrNotFound) {
		t.Errorf("run get gave %v, want ErrNotFound", err)
	}
	if _, err := runs.Children(ctx, allProjects, 1); !errors.Is(err, ErrNotFound) {
		t.Errorf("run children gave %v, want ErrNotFound", err)
	}

	projects, _, _ := newTestProjectService()
	project, _, err := projects.Create(ctx, ProjectInput{Name: "Demo", Active: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := projects.Get(ctx, allProjects, project.ID); err != nil || got.ID != project.ID {
		t.Errorf("project get = %v, %v", got, err)
	}
	if got, err := projects.GetBySlug(ctx, project.Slug); err != nil || got.ID != project.ID {
		t.Errorf("project get by slug = %v, %v", got, err)
	}
	if _, err := projects.Get(ctx, allProjects, 9999); !errors.Is(err, ErrNotFound) {
		t.Errorf("an unknown project gave %v, want ErrNotFound", err)
	}
	if _, err := projects.GetBySlug(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("an unknown slug gave %v, want ErrNotFound", err)
	}
	if _, err := projects.List(ctx, allProjects, ""); err != nil {
		t.Errorf("project list failed: %v", err)
	}
	if _, err := projects.ListNames(ctx, allProjects); err != nil {
		t.Errorf("project names failed: %v", err)
	}
}

func TestAdminAccountManagement(t *testing.T) {
	svc, repo := newTestAuthService()
	ctx := context.Background()

	id, err := svc.CreateUser(ctx, UserInput{
		Fullname: "Operator", Email: "op@example.com",
		Password: "correct-horse-battery", Active: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	if got, err := svc.GetUser(ctx, id); err != nil || got.Email != "op@example.com" {
		t.Errorf("GetUser = %v, %v", got, err)
	}
	if _, err := svc.GetUser(ctx, 9999); !errors.Is(err, ErrNotFound) {
		t.Errorf("an unknown account gave %v, want ErrNotFound", err)
	}
	if _, _, err := svc.ListUsers(ctx, "", 0, 20); err != nil {
		t.Errorf("ListUsers failed: %v", err)
	}

	if err := svc.UpdateUser(ctx, id, UserInput{
		Fullname: "Renamed", Email: "renamed@example.com", IsAdmin: true, Active: true,
	}); err != nil {
		t.Fatalf("UpdateUser failed: %v", err)
	}
	stored, _ := repo.GetByID(ctx, id)
	if stored.Fullname != "Renamed" || !stored.IsAdmin {
		t.Errorf("the update did not apply: %+v", stored)
	}
	if err := svc.UpdateUser(ctx, id, UserInput{Fullname: "", Email: "bad"}); err == nil {
		t.Error("an invalid update was accepted")
	}
	if err := svc.UpdateUser(ctx, 9999, UserInput{
		Fullname: "X", Email: "x@example.com",
	}); !errors.Is(err, ErrNotFound) {
		t.Errorf("updating an unknown account gave %v, want ErrNotFound", err)
	}

	// An admin reset sets a password without knowing the old one, and still
	// ends the target's sessions.
	if err := svc.ResetPassword(ctx, id, "short"); err == nil {
		t.Error("a short reset password was accepted")
	}
	if err := svc.ResetPassword(ctx, id, "a-reset-password-long-enough"); err != nil {
		t.Fatalf("ResetPassword failed: %v", err)
	}
	stored, _ = repo.GetByID(ctx, id)
	if stored.TokensValidAfter == nil {
		t.Error("a password reset did not retire existing tokens")
	}
}
