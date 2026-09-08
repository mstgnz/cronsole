package memrepo

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/mstgnz/cronsole/v2/internal/domain"
	"github.com/mstgnz/cronsole/v2/internal/repository"
)

// Compile-time proof that each type satisfies the interface it stands in for.
// Without these, a signature change lands as a confusing failure in whichever
// test happens to construct the service first.
var (
	_ domain.UserRepository         = Users{}
	_ domain.ProjectRepository      = Projects{}
	_ domain.JobRepository          = Jobs{}
	_ domain.RunRepository          = Runs{}
	_ domain.NotificationRepository = Notifications{}
	_ domain.AppLogRepository       = Logs{}
	_ domain.StatsRepository        = Stats{}
	_ domain.HostOverrideRepository = Hosts{}
)

// --- users ------------------------------------------------------------------

// Users is the account repository.
type Users struct{ *Store }

func (r Users) GetByID(_ context.Context, id int64) (*domain.User, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	u, ok := r.users[id]
	if !ok || r.deletedUsers[id] {
		return nil, repository.ErrNotFound
	}
	copied := *u
	return &copied, nil
}

func (r Users) GetByEmail(_ context.Context, email string) (*domain.User, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	for id, u := range r.users {
		if r.deletedUsers[id] {
			continue
		}
		if strings.EqualFold(u.Email, email) {
			copied := *u
			return &copied, nil
		}
	}
	return nil, repository.ErrNotFound
}

func (r Users) List(_ context.Context, search string, offset, limit int) ([]domain.User, int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	out := []domain.User{}
	for id, u := range r.users {
		if r.deletedUsers[id] {
			continue
		}
		if search != "" && !matches(u.Email, search) && !matches(u.Fullname, search) {
			continue
		}
		out = append(out, *u)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return page(out, offset, limit), int64(len(out)), nil
}

func (r Users) Count(_ context.Context) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	var n int64
	for id := range r.users {
		if !r.deletedUsers[id] {
			n++
		}
	}
	return n, nil
}

func (r Users) Create(_ context.Context, u *domain.User) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	// The unique index is on lower(email) where the row is live. Enforced here
	// too, because a store more permissive than Postgres lets a test pass on
	// behaviour the deployment does not have.
	for id, existing := range r.users {
		if !r.deletedUsers[id] && strings.EqualFold(existing.Email, u.Email) {
			return 0, repository.ErrDuplicate
		}
	}

	id := r.nextUser
	r.nextUser++
	copied := *u
	copied.ID = id
	if copied.CreatedAt.IsZero() {
		copied.CreatedAt = time.Now()
	}
	r.users[id] = &copied
	return id, nil
}

// CreateFirstAdmin mirrors the conditional insert: it refuses the moment any
// live account exists, and the whole check happens under the same lock so two
// callers cannot both find the table empty.
func (r Users) CreateFirstAdmin(_ context.Context, u *domain.User) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	for id := range r.users {
		if !r.deletedUsers[id] {
			return 0, repository.ErrForbidden
		}
	}

	id := r.nextUser
	r.nextUser++
	copied := *u
	copied.ID = id
	copied.IsAdmin, copied.Active = true, true
	if copied.CreatedAt.IsZero() {
		copied.CreatedAt = time.Now()
	}
	r.users[id] = &copied
	return id, nil
}

func (r Users) UpdateProfile(_ context.Context, u *domain.User) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	existing, ok := r.users[u.ID]
	if !ok || r.deletedUsers[u.ID] {
		return repository.ErrNotFound
	}
	for id, other := range r.users {
		if id != u.ID && !r.deletedUsers[id] && strings.EqualFold(other.Email, u.Email) {
			return repository.ErrDuplicate
		}
	}

	existing.Fullname, existing.Email, existing.Phone = u.Fullname, u.Email, u.Phone
	existing.IsAdmin, existing.Active = u.IsAdmin, u.Active
	now := time.Now()
	existing.UpdatedAt = &now
	return nil
}

func (r Users) UpdatePassword(_ context.Context, id int64, hash string, at time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	u, ok := r.users[id]
	if !ok || r.deletedUsers[id] {
		return repository.ErrNotFound
	}
	u.Password = hash
	// Changing a password retires every token issued before now, which is what
	// makes the change end other sessions.
	u.TokensValidAfter = &at
	return nil
}

func (r Users) InvalidateTokens(_ context.Context, id int64, at time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	u, ok := r.users[id]
	if !ok || r.deletedUsers[id] {
		return repository.ErrNotFound
	}
	u.TokensValidAfter = &at
	return nil
}

func (r Users) TouchLogin(_ context.Context, id int64, at time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	u, ok := r.users[id]
	if !ok {
		return repository.ErrNotFound
	}
	u.LastLogin = &at
	return nil
}

func (r Users) SoftDelete(_ context.Context, id int64, _ time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, ok := r.users[id]; !ok || r.deletedUsers[id] {
		return repository.ErrNotFound
	}
	r.deletedUsers[id] = true
	return nil
}

// --- projects ---------------------------------------------------------------

// Projects is the project repository.
type Projects struct{ *Store }

func (r Projects) GetByID(_ context.Context, id int64) (*domain.Project, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	p, ok := r.projects[id]
	if !ok || r.deletedProjects[id] {
		return nil, repository.ErrNotFound
	}
	copied := *p
	return &copied, nil
}

func (r Projects) GetBySlug(_ context.Context, slug string) (*domain.Project, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	for id, p := range r.projects {
		if !r.deletedProjects[id] && p.Slug == slug {
			copied := *p
			return &copied, nil
		}
	}
	return nil, repository.ErrNotFound
}

func (r Projects) GetByKeyPrefix(_ context.Context, prefix string) (*domain.Project, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	for id, p := range r.projects {
		if !r.deletedProjects[id] && p.KeyPrefix == prefix {
			copied := *p
			return &copied, nil
		}
	}
	return nil, repository.ErrNotFound
}

func (r Projects) List(_ context.Context, scope domain.ProjectScope, search string) ([]domain.ProjectRow, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	out := []domain.ProjectRow{}
	for id, p := range r.projects {
		// The scope is applied here, exactly as the WHERE clause applies it. An
		// empty scope reaches nothing, which is the whole reason it is not
		// optional on the filter.
		if r.deletedProjects[id] || !scope.Allows(id) {
			continue
		}
		if search != "" && !matches(p.Name, search) && !matches(p.Slug, search) {
			continue
		}

		row := domain.ProjectRow{Project: *p}
		for jobID, j := range r.jobs {
			if r.deletedJobs[jobID] || j.ProjectID != id {
				continue
			}
			row.JobTotal++
			if j.Active {
				row.JobActive++
			}
		}
		out = append(out, row)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (r Projects) ListNames(_ context.Context, scope domain.ProjectScope) ([]domain.Project, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	out := []domain.Project{}
	for id, p := range r.projects {
		if r.deletedProjects[id] || !scope.Allows(id) {
			continue
		}
		out = append(out, *p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (r Projects) Create(_ context.Context, p *domain.Project) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	for id, existing := range r.projects {
		if !r.deletedProjects[id] && existing.Slug == p.Slug {
			return 0, repository.ErrDuplicate
		}
	}

	id := r.nextProject
	r.nextProject++
	copied := *p
	copied.ID = id
	if copied.CreatedAt.IsZero() {
		copied.CreatedAt = time.Now()
	}
	r.projects[id] = &copied
	return id, nil
}

func (r Projects) Update(_ context.Context, p *domain.Project) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	existing, ok := r.projects[p.ID]
	if !ok || r.deletedProjects[p.ID] {
		return repository.ErrNotFound
	}
	for id, other := range r.projects {
		if id != p.ID && !r.deletedProjects[id] && other.Slug == p.Slug {
			return repository.ErrDuplicate
		}
	}

	existing.Name, existing.Slug = p.Name, p.Slug
	existing.Description, existing.BaseURL = p.Description, p.BaseURL
	existing.Active = p.Active
	now := time.Now()
	existing.UpdatedAt = &now
	return nil
}

func (r Projects) SetKey(_ context.Context, id int64, prefix, hash string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	p, ok := r.projects[id]
	if !ok || r.deletedProjects[id] {
		return repository.ErrNotFound
	}
	p.KeyPrefix, p.KeyHash = prefix, hash
	return nil
}

func (r Projects) SoftDelete(_ context.Context, id int64, _ time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, ok := r.projects[id]; !ok || r.deletedProjects[id] {
		return repository.ErrNotFound
	}
	r.deletedProjects[id] = true
	return nil
}

// --- jobs -------------------------------------------------------------------

// Jobs is the job repository.
type Jobs struct{ *Store }

func (r Jobs) Get(_ context.Context, id int64) (*domain.Job, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	j, ok := r.jobs[id]
	if !ok || r.deletedJobs[id] {
		return nil, repository.ErrNotFound
	}
	copied := *j
	return &copied, nil
}

func (r Jobs) GetByCode(_ context.Context, projectID int64, code string) (*domain.Job, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	for id, j := range r.jobs {
		if r.deletedJobs[id] || j.ProjectID != projectID {
			continue
		}
		if strings.EqualFold(j.Code, code) {
			copied := *j
			return &copied, nil
		}
	}
	return nil, repository.ErrNotFound
}

func (r Jobs) List(_ context.Context, f domain.JobFilter, offset, limit int) ([]domain.JobRow, int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	out := []domain.JobRow{}
	for id, j := range r.jobs {
		if r.deletedJobs[id] || !f.Scope.Allows(j.ProjectID) {
			continue
		}
		if f.ProjectID != nil && j.ProjectID != *f.ProjectID {
			continue
		}
		if f.Tag != nil && j.Tag != *f.Tag {
			continue
		}
		if f.Active != nil && j.Active != *f.Active {
			continue
		}
		if f.Search != nil && !matches(j.Code, *f.Search) && !matches(j.Name, *f.Search) {
			continue
		}

		row := domain.JobRow{Job: *j}
		if p, ok := r.projects[j.ProjectID]; ok {
			row.ProjectName, row.ProjectSlug = p.Name, p.Slug
		}
		for _, sched := range r.schedules[id] {
			if sched.Active {
				row.Schedules = append(row.Schedules, sched.Expression)
			}
		}
		for _, link := range r.links {
			if link.JobID == id {
				row.LinkCount++
			}
			if link.TargetJobID == id {
				row.TriggerCount++
			}
		}
		out = append(out, row)
	}

	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return page(out, offset, limit), int64(len(out)), nil
}

func (r Jobs) ListOptions(_ context.Context, scope domain.ProjectScope, excludeID int64) ([]domain.JobOption, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	out := []domain.JobOption{}
	for id, j := range r.jobs {
		if r.deletedJobs[id] || id == excludeID || !scope.Allows(j.ProjectID) {
			continue
		}
		option := domain.JobOption{ID: id, Code: j.Code, Name: j.Name, Active: j.Active}
		if p, ok := r.projects[j.ProjectID]; ok {
			option.ProjectSlug = p.Slug
		}
		out = append(out, option)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (r Jobs) ListTags(_ context.Context, scope domain.ProjectScope) ([]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	seen := map[string]bool{}
	for id, j := range r.jobs {
		if r.deletedJobs[id] || j.Tag == "" || !scope.Allows(j.ProjectID) {
			continue
		}
		seen[j.Tag] = true
	}
	out := make([]string, 0, len(seen))
	for tag := range seen {
		out = append(out, tag)
	}
	sort.Strings(out)
	return out, nil
}

func (r Jobs) Create(_ context.Context, j *domain.Job) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	// UNIQUE (project_id, lower(code)) WHERE deleted_at IS NULL.
	for id, existing := range r.jobs {
		if r.deletedJobs[id] || existing.ProjectID != j.ProjectID {
			continue
		}
		if strings.EqualFold(existing.Code, j.Code) {
			return 0, repository.ErrDuplicate
		}
	}
	return r.putJob(*j).ID, nil
}

func (r Jobs) Update(_ context.Context, j *domain.Job) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, ok := r.jobs[j.ID]; !ok || r.deletedJobs[j.ID] {
		return repository.ErrNotFound
	}
	for id, existing := range r.jobs {
		if id == j.ID || r.deletedJobs[id] || existing.ProjectID != j.ProjectID {
			continue
		}
		if strings.EqualFold(existing.Code, j.Code) {
			return repository.ErrDuplicate
		}
	}

	copied := *j
	now := time.Now()
	copied.UpdatedAt = &now
	r.jobs[j.ID] = &copied
	return nil
}

func (r Jobs) SetActive(_ context.Context, id int64, active bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	j, ok := r.jobs[id]
	if !ok || r.deletedJobs[id] {
		return repository.ErrNotFound
	}
	j.Active = active
	return nil
}

func (r Jobs) SoftDelete(_ context.Context, id int64, _ time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, ok := r.jobs[id]; !ok || r.deletedJobs[id] {
		return repository.ErrNotFound
	}
	r.deletedJobs[id] = true
	return nil
}

func (r Jobs) ListHeaders(_ context.Context, jobID int64) ([]domain.JobHeader, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	out := append([]domain.JobHeader(nil), r.headers[jobID]...)
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

func (r Jobs) ReplaceHeaders(_ context.Context, jobID int64, headers []domain.JobHeader) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	stored := make([]domain.JobHeader, 0, len(headers))
	for i, h := range headers {
		h.ID = int64(i + 1)
		h.JobID = jobID
		stored = append(stored, h)
	}
	r.headers[jobID] = stored
	return nil
}

func (r Jobs) ListSchedules(_ context.Context, jobID int64) ([]domain.JobSchedule, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]domain.JobSchedule(nil), r.schedules[jobID]...), nil
}

func (r Jobs) CreateSchedule(_ context.Context, s *domain.JobSchedule) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	// UNIQUE (job_id, expression): the same expression twice would queue the
	// same minute twice.
	for _, existing := range r.schedules[s.JobID] {
		if existing.Expression == s.Expression {
			return 0, repository.ErrDuplicate
		}
	}

	id := r.nextSchedule
	r.nextSchedule++
	row := *s
	row.ID = id
	if row.CreatedAt.IsZero() {
		row.CreatedAt = time.Now()
	}
	r.schedules[s.JobID] = append(r.schedules[s.JobID], row)
	return id, nil
}

func (r Jobs) DeleteSchedule(_ context.Context, id, jobID int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	rows := r.schedules[jobID]
	for i, row := range rows {
		if row.ID == id {
			r.schedules[jobID] = append(rows[:i:i], rows[i+1:]...)
			return nil
		}
	}
	return repository.ErrNotFound
}

func (r Jobs) CountSchedules(_ context.Context, jobID int64) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	n := 0
	for _, row := range r.schedules[jobID] {
		if row.Active {
			n++
		}
	}
	return n, nil
}

func (r Jobs) ReplaceSchedules(_ context.Context, jobID int64, expressions []string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	rows := make([]domain.JobSchedule, 0, len(expressions))
	seen := map[string]bool{}
	for _, expression := range expressions {
		if seen[expression] {
			continue
		}
		seen[expression] = true
		rows = append(rows, domain.JobSchedule{
			ID: r.nextSchedule, JobID: jobID, Expression: expression,
			Active: true, CreatedAt: time.Now(),
		})
		r.nextSchedule++
	}
	r.schedules[jobID] = rows
	return nil
}

func (r Jobs) ListLinks(_ context.Context, jobID int64) ([]domain.JobLinkRow, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	out := []domain.JobLinkRow{}
	for _, link := range r.links {
		if link.JobID != jobID {
			continue
		}
		row := domain.JobLinkRow{JobLink: *link}
		if target, ok := r.jobs[link.TargetJobID]; ok {
			row.TargetCode, row.TargetName, row.TargetActive = target.Code, target.Name, target.Active
		}
		out = append(out, row)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (r Jobs) ListTriggers(_ context.Context, jobID int64) ([]domain.JobLinkRow, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	out := []domain.JobLinkRow{}
	for _, link := range r.links {
		if link.TargetJobID != jobID {
			continue
		}
		row := domain.JobLinkRow{JobLink: *link}
		if source, ok := r.jobs[link.JobID]; ok {
			row.SourceCode, row.SourceName = source.Code, source.Name
		}
		if target, ok := r.jobs[link.TargetJobID]; ok {
			row.TargetCode, row.TargetName, row.TargetActive = target.Code, target.Name, target.Active
		}
		out = append(out, row)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (r Jobs) CreateLink(_ context.Context, l *domain.JobLink) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	// UNIQUE (job_id, target_job_id, condition).
	for _, existing := range r.links {
		if existing.JobID == l.JobID && existing.TargetJobID == l.TargetJobID &&
			existing.Condition == l.Condition {
			return 0, repository.ErrDuplicate
		}
	}

	id := r.nextLink
	r.nextLink++
	copied := *l
	copied.ID = id
	if copied.CreatedAt.IsZero() {
		copied.CreatedAt = time.Now()
	}
	r.links[id] = &copied
	return id, nil
}

func (r Jobs) DeleteLink(_ context.Context, id, jobID int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	link, ok := r.links[id]
	if !ok || link.JobID != jobID {
		return repository.ErrNotFound
	}
	delete(r.links, id)
	return nil
}

// ReachesJob walks the chain forward, which is what refuses a cycle before it
// is stored. Depth-bounded like the query it stands in for: a cycle that
// already exists must not make this recurse forever.
func (r Jobs) ReachesJob(_ context.Context, fromID, targetID int64, maxDepth int) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	seen := map[int64]bool{fromID: true}
	frontier := []int64{fromID}

	for depth := 0; depth < maxDepth && len(frontier) > 0; depth++ {
		next := []int64{}
		for _, id := range frontier {
			for _, link := range r.links {
				if link.JobID != id || !link.Active {
					continue
				}
				if link.TargetJobID == targetID {
					return true, nil
				}
				if !seen[link.TargetJobID] {
					seen[link.TargetJobID] = true
					next = append(next, link.TargetJobID)
				}
			}
		}
		frontier = next
	}
	return false, nil
}

// --- runs -------------------------------------------------------------------

// Runs is the run repository.
type Runs struct{ *Store }

// row decorates a run with the identity of its job. The caller holds the lock.
func (r Runs) row(run *domain.Run) domain.RunRow {
	out := domain.RunRow{Run: *run}
	job, ok := r.jobs[run.JobID]
	if !ok {
		return out
	}
	out.JobCode, out.JobName = job.Code, job.Name
	if p, ok := r.projects[job.ProjectID]; ok {
		out.ProjectSlug, out.ProjectName = p.Slug, p.Name
	}
	return out
}

func (r Runs) Get(_ context.Context, id int64) (*domain.RunRow, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	run, ok := r.runs[id]
	if !ok {
		return nil, repository.ErrNotFound
	}
	row := r.row(run)
	return &row, nil
}

func (r Runs) List(_ context.Context, f domain.RunFilter, offset, limit int) ([]domain.RunRow, int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	out := []domain.RunRow{}
	for _, run := range r.runs {
		job, ok := r.jobs[run.JobID]
		if !ok {
			continue
		}
		// The scope reaches a run through its job's project. A run list that
		// forgot this would show another brand's output, which is the field
		// most likely to carry customer data.
		if !f.Scope.Allows(job.ProjectID) {
			continue
		}
		if f.JobID != nil && run.JobID != *f.JobID {
			continue
		}
		if f.ProjectID != nil && job.ProjectID != *f.ProjectID {
			continue
		}
		if f.Status != nil && run.Status != *f.Status {
			continue
		}
		if f.Trigger != nil && run.Trigger != *f.Trigger {
			continue
		}
		if f.Start != nil && run.CreatedAt.Before(*f.Start) {
			continue
		}
		if f.End != nil && run.CreatedAt.After(*f.End) {
			continue
		}
		if f.Search != nil && !matches(job.Code, *f.Search) && !matches(run.Error, *f.Search) {
			continue
		}
		out = append(out, r.row(run))
	}

	// Newest first, which is how every screen reads them.
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	return page(out, offset, limit), int64(len(out)), nil
}

func (r Runs) Recent(_ context.Context, jobID int64, n int) ([]domain.Run, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	out := []domain.Run{}
	for _, run := range r.runs {
		if run.JobID == jobID {
			out = append(out, *run)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	return page(out, 0, n), nil
}

func (r Runs) Children(_ context.Context, runID int64) ([]domain.RunRow, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	out := []domain.RunRow{}
	for _, run := range r.runs {
		if run.ParentRunID != nil && *run.ParentRunID == runID {
			out = append(out, r.row(run))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (r Runs) Enqueue(_ context.Context, jobID int64, trigger string, userID *int64) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	id := r.nextRun
	r.nextRun++
	r.runs[id] = &domain.Run{
		ID: id, JobID: jobID, Trigger: trigger, UserID: userID,
		Status: domain.StatusPending, CreatedAt: time.Now(),
	}
	return id, nil
}

// --- notifications ----------------------------------------------------------

// Notifications is the recipient-set repository.
type Notifications struct{ *Store }

func (r Notifications) Get(_ context.Context, id int64) (*domain.Notification, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	n, ok := r.notifs[id]
	if !ok || r.deletedNotifs[id] {
		return nil, repository.ErrNotFound
	}
	copied := *n
	return &copied, nil
}

func (r Notifications) List(_ context.Context) ([]domain.Notification, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	out := []domain.Notification{}
	for id, n := range r.notifs {
		if !r.deletedNotifs[id] {
			out = append(out, *n)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (r Notifications) Create(_ context.Context, n *domain.Notification) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	id := r.nextNotification
	r.nextNotification++
	copied := *n
	copied.ID = id
	if copied.CreatedAt.IsZero() {
		copied.CreatedAt = time.Now()
	}
	r.notifs[id] = &copied
	return id, nil
}

func (r Notifications) Update(_ context.Context, n *domain.Notification) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	existing, ok := r.notifs[n.ID]
	if !ok || r.deletedNotifs[n.ID] {
		return repository.ErrNotFound
	}
	existing.Name, existing.OnSuccess = n.Name, n.OnSuccess
	existing.OnFailure, existing.Active = n.OnFailure, n.Active
	return nil
}

func (r Notifications) SoftDelete(_ context.Context, id int64, _ time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, ok := r.notifs[id]; !ok || r.deletedNotifs[id] {
		return repository.ErrNotFound
	}
	r.deletedNotifs[id] = true
	return nil
}

func (r Notifications) ReplaceEmails(_ context.Context, notificationID int64, emails []string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	n, ok := r.notifs[notificationID]
	if !ok || r.deletedNotifs[notificationID] {
		return repository.ErrNotFound
	}
	n.Emails = append([]string(nil), emails...)
	return nil
}

// --- application log --------------------------------------------------------

// Logs is the application log repository.
type Logs struct{ *Store }

func (r Logs) List(_ context.Context, level string, offset, limit int) ([]domain.AppLog, int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	out := []domain.AppLog{}
	for _, entry := range r.logs {
		if level != "" && entry.Level != level {
			continue
		}
		out = append(out, entry)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return page(out, offset, limit), int64(len(out)), nil
}

func (r Logs) DeleteBefore(_ context.Context, before time.Time) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	kept := r.logs[:0]
	var removed int64
	for _, entry := range r.logs {
		if entry.CreatedAt.Before(before) {
			removed++
			continue
		}
		kept = append(kept, entry)
	}
	r.logs = kept
	return removed, nil
}

// --- statistics -------------------------------------------------------------

// Stats feeds the dashboard.
type Stats struct{ *Store }

func (r Stats) Summary(_ context.Context, scope domain.ProjectScope) (*domain.Summary, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	out := &domain.Summary{}
	for id := range r.projects {
		if !r.deletedProjects[id] || scope.Allows(id) {
			if !r.deletedProjects[id] && scope.Allows(id) {
				out.ProjectTotal++
			}
		}
	}
	for id, j := range r.jobs {
		if r.deletedJobs[id] || !scope.Allows(j.ProjectID) {
			continue
		}
		out.JobTotal++
		if j.Active {
			out.JobActive++
		}
	}
	for _, run := range r.runs {
		job, ok := r.jobs[run.JobID]
		if !ok || !scope.Allows(job.ProjectID) {
			continue
		}
		switch run.Status {
		case domain.StatusSuccess:
			out.DaySuccess++
		case domain.StatusFailed:
			out.DayFailed++
		case domain.StatusTimeout:
			out.DayTimeout++
		case domain.StatusSkipped:
			out.DaySkipped++
		case domain.StatusRunning:
			out.Running++
		case domain.StatusPending:
			out.Pending++
		}
	}
	elapsed := 5
	out.ElapsedSec = &elapsed
	return out, nil
}

func (r Stats) Activity(_ context.Context, scope domain.ProjectScope, hours int) ([]domain.HourBucket, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if hours <= 0 {
		hours = 24
	}
	now := time.Now().Truncate(time.Hour)
	out := make([]domain.HourBucket, 0, hours)
	for i := hours - 1; i >= 0; i-- {
		out = append(out, domain.HourBucket{Hour: now.Add(-time.Duration(i) * time.Hour)})
	}

	for _, run := range r.runs {
		job, ok := r.jobs[run.JobID]
		if !ok || !scope.Allows(job.ProjectID) {
			continue
		}
		bucket := &out[len(out)-1]
		switch run.Status {
		case domain.StatusSuccess:
			bucket.Success++
		case domain.StatusFailed:
			bucket.Failed++
		case domain.StatusTimeout:
			bucket.Timeout++
		case domain.StatusSkipped:
			bucket.Skipped++
		}
	}
	return out, nil
}

func (r Stats) SlowestJobs(_ context.Context, scope domain.ProjectScope, _, limit int) ([]domain.JobDuration, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	out := []domain.JobDuration{}
	for id, j := range r.jobs {
		if r.deletedJobs[id] || !scope.Allows(j.ProjectID) {
			continue
		}
		row := domain.JobDuration{JobID: id, Code: j.Code}
		if p, ok := r.projects[j.ProjectID]; ok {
			row.ProjectSlug = p.Slug
		}
		for _, run := range r.runs {
			if run.JobID != id || run.DurationMs == nil {
				continue
			}
			row.Runs++
			row.AvgMs = *run.DurationMs
			if *run.DurationMs > row.MaxMs {
				row.MaxMs = *run.DurationMs
			}
		}
		if row.Runs > 0 {
			out = append(out, row)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].MaxMs > out[j].MaxMs })
	return page(out, 0, limit), nil
}

// RunningNow lists what is executing, for the dashboard's in-flight panel.
func (r Stats) RunningNow(_ context.Context, scope domain.ProjectScope, limit int) ([]domain.RunRow, error) {
	return Runs(r).byStatus(scope, domain.StatusRunning, limit, false)
}

// RecentFailures lists what failed most recently.
func (r Stats) RecentFailures(_ context.Context, scope domain.ProjectScope, limit int) ([]domain.RunRow, error) {
	return Runs(r).byStatus(scope, domain.StatusFailed, limit, true)
}

// byStatus is the shared body of the two above. newestFirst mirrors the two
// queries: failures are read newest first, and what is running is read oldest
// first so the longest-running job is at the top.
func (r Runs) byStatus(scope domain.ProjectScope, status string, limit int, newestFirst bool) ([]domain.RunRow, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	out := []domain.RunRow{}
	for _, run := range r.runs {
		job, ok := r.jobs[run.JobID]
		if !ok || !scope.Allows(job.ProjectID) || run.Status != status {
			continue
		}
		out = append(out, r.row(run))
	}
	sort.Slice(out, func(i, j int) bool {
		if newestFirst {
			return out[i].ID > out[j].ID
		}
		return out[i].ID < out[j].ID
	})
	return page(out, 0, limit), nil
}

func (r Stats) JobHistory(_ context.Context, scope domain.ProjectScope, jobID int64, limit int) ([]domain.RunPoint, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	job, ok := r.jobs[jobID]
	if !ok || !scope.Allows(job.ProjectID) {
		return []domain.RunPoint{}, nil
	}

	out := []domain.RunPoint{}
	for _, run := range r.runs {
		if run.JobID != jobID {
			continue
		}
		point := domain.RunPoint{RunID: run.ID, At: run.CreatedAt, Status: run.Status}
		if run.DurationMs != nil {
			point.DurationMs = *run.DurationMs
		}
		if run.HTTPStatus != nil {
			point.HTTPStatus = *run.HTTPStatus
		}
		out = append(out, point)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RunID < out[j].RunID })
	return page(out, 0, limit), nil
}

// --- host overrides ---------------------------------------------------------

// Hosts is the host-route repository.
type Hosts struct{ *Store }

func (r Hosts) all(activeOnly bool) []domain.HostOverride {
	out := []domain.HostOverride{}
	for _, o := range r.hosts {
		if activeOnly && !o.Active {
			continue
		}
		out = append(out, *o)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (r Hosts) List(context.Context) ([]domain.HostOverride, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.all(false), nil
}

func (r Hosts) ListActive(context.Context) ([]domain.HostOverride, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.all(true), nil
}

func (r Hosts) Get(_ context.Context, id int64) (*domain.HostOverride, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	o, ok := r.hosts[id]
	if !ok {
		return nil, repository.ErrNotFound
	}
	copied := *o
	return &copied, nil
}

// hostKey mirrors the unique index: one route per hostname and port, with the
// "every port" row folded into the same rule.
func hostKey(hostname string, port *int) string {
	p := "any"
	if port != nil {
		p = strings.TrimSpace(time.Duration(*port).String())
	}
	return strings.ToLower(hostname) + "/" + p
}

func (r Hosts) Create(_ context.Context, o *domain.HostOverride) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	for _, existing := range r.hosts {
		if hostKey(existing.Hostname, existing.Port) == hostKey(o.Hostname, o.Port) {
			return 0, repository.ErrDuplicate
		}
	}

	id := r.nextHost
	r.nextHost++
	copied := *o
	copied.ID = id
	copied.CreatedAt, copied.UpdatedAt = time.Now(), time.Now()
	r.hosts[id] = &copied
	return id, nil
}

func (r Hosts) Update(_ context.Context, o *domain.HostOverride) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, ok := r.hosts[o.ID]; !ok {
		return repository.ErrNotFound
	}
	for id, existing := range r.hosts {
		if id != o.ID && hostKey(existing.Hostname, existing.Port) == hostKey(o.Hostname, o.Port) {
			return repository.ErrDuplicate
		}
	}
	copied := *o
	copied.UpdatedAt = time.Now()
	r.hosts[o.ID] = &copied
	return nil
}

func (r Hosts) Delete(_ context.Context, id int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, ok := r.hosts[id]; !ok {
		return repository.ErrNotFound
	}
	delete(r.hosts, id)
	return nil
}
