package service

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/mstgnz/cronsole/v2/internal/domain"
	"github.com/mstgnz/cronsole/v2/internal/repository"
)

// memStore is an in-memory stand-in for the job and project tables, enough to
// exercise the job and sync services end to end.
//
// It enforces what the database enforces: a duplicate code in a project is
// rejected with the same sentinel, and a duplicate schedule or link is
// refused. A fake that skipped those would let a test pass while production
// returned an error.
//
// Two wrapper types sit on top because JobRepository and ProjectRepository
// both declare Get, List, Create, Update and SoftDelete; one Go type cannot
// satisfy both.
type memStore struct {
	mu sync.Mutex

	projects  map[int64]*domain.Project
	jobs      map[int64]*domain.Job
	schedules map[int64][]domain.JobSchedule
	headers   map[int64][]domain.JobHeader
	links     map[int64]*domain.JobLink

	nextJob      int64
	nextSchedule int64
	nextLink     int64
}

func newMemStore() *memStore {
	return &memStore{
		projects:     map[int64]*domain.Project{},
		jobs:         map[int64]*domain.Job{},
		schedules:    map[int64][]domain.JobSchedule{},
		headers:      map[int64][]domain.JobHeader{},
		links:        map[int64]*domain.JobLink{},
		nextJob:      1,
		nextSchedule: 1,
		nextLink:     1,
	}
}

func (m *memStore) addProject(id int64, slug, baseURL string) *domain.Project {
	m.mu.Lock()
	defer m.mu.Unlock()
	p := &domain.Project{ID: id, Name: slug, Slug: slug, BaseURL: baseURL, Active: true}
	m.projects[id] = p
	return p
}

func (m *memStore) job(id int64) *domain.Job {
	m.mu.Lock()
	defer m.mu.Unlock()
	if j, ok := m.jobs[id]; ok {
		copied := *j
		return &copied
	}
	return nil
}

// --- projects ---

type memProjectRepo struct{ *memStore }

func (m memProjectRepo) GetByID(_ context.Context, id int64) (*domain.Project, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if p, ok := m.projects[id]; ok {
		copied := *p
		return &copied, nil
	}
	return nil, repository.ErrNotFound
}

func (m memProjectRepo) GetBySlug(_ context.Context, slug string) (*domain.Project, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, p := range m.projects {
		if p.Slug == slug {
			copied := *p
			return &copied, nil
		}
	}
	return nil, repository.ErrNotFound
}

func (m memProjectRepo) GetByKeyPrefix(context.Context, string) (*domain.Project, error) {
	return nil, repository.ErrNotFound
}
func (m memProjectRepo) List(context.Context, domain.ProjectScope, string) ([]domain.ProjectRow, error) {
	return nil, nil
}

func (m memProjectRepo) ListNames(context.Context, domain.ProjectScope) ([]domain.Project, error) {
	return nil, nil
}
func (m memProjectRepo) Create(context.Context, *domain.Project) (int64, error) { return 0, nil }
func (m memProjectRepo) Update(context.Context, *domain.Project) error          { return nil }
func (m memProjectRepo) SetKey(context.Context, int64, string, string) error    { return nil }
func (m memProjectRepo) SoftDelete(context.Context, int64, time.Time) error     { return nil }

// --- jobs ---

type memJobRepo struct{ *memStore }

func (m memJobRepo) Get(_ context.Context, id int64) (*domain.Job, error) {
	if j := m.job(id); j != nil {
		return j, nil
	}
	return nil, repository.ErrNotFound
}

func (m memJobRepo) GetByCode(_ context.Context, projectID int64, code string) (*domain.Job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, j := range m.jobs {
		if j.ProjectID == projectID && strings.EqualFold(j.Code, code) {
			copied := *j
			return &copied, nil
		}
	}
	return nil, repository.ErrNotFound
}

func (m memJobRepo) List(_ context.Context, f domain.JobFilter, offset, limit int) ([]domain.JobRow, int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	var rows []domain.JobRow
	for _, j := range m.jobs {
		// The scope clause, applied first and unconditionally when it is not
		// "all", exactly as the SQL does. A fake that ignored it would let a
		// test of the scoping pass while proving nothing.
		if !f.Scope.Allows(j.ProjectID) {
			continue
		}
		if f.ProjectID != nil && j.ProjectID != *f.ProjectID {
			continue
		}
		if f.Active != nil && j.Active != *f.Active {
			continue
		}
		row := domain.JobRow{Job: *j}
		for _, s := range m.schedules[j.ID] {
			if s.Active {
				row.Schedules = append(row.Schedules, s.Expression)
			}
		}
		for _, l := range m.links {
			if !l.Active {
				continue
			}
			if l.TargetJobID == j.ID {
				row.TriggerCount++
			}
			if l.JobID == j.ID {
				row.LinkCount++
			}
		}
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })

	total := int64(len(rows))
	if offset >= len(rows) {
		return nil, total, nil
	}
	rows = rows[offset:]
	if limit < len(rows) {
		rows = rows[:limit]
	}
	return rows, total, nil
}

func (m memJobRepo) ListOptions(context.Context, domain.ProjectScope, int64) ([]domain.JobOption, error) {
	return nil, nil
}

func (m memJobRepo) ListTags(context.Context, domain.ProjectScope) ([]string, error) {
	return nil, nil
}

func (m memJobRepo) Create(_ context.Context, j *domain.Job) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, existing := range m.jobs {
		if existing.ProjectID == j.ProjectID && strings.EqualFold(existing.Code, j.Code) {
			return 0, repository.ErrDuplicate
		}
	}
	id := m.nextJob
	m.nextJob++
	copied := *j
	copied.ID = id
	m.jobs[id] = &copied
	return id, nil
}

func (m memJobRepo) Update(_ context.Context, j *domain.Job) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.jobs[j.ID]; !ok {
		return repository.ErrNotFound
	}
	copied := *j
	m.jobs[j.ID] = &copied
	return nil
}

func (m memJobRepo) SetActive(_ context.Context, id int64, active bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if j, ok := m.jobs[id]; ok {
		j.Active = active
	}
	return nil
}

func (m memJobRepo) SoftDelete(_ context.Context, id int64, _ time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.jobs, id)
	return nil
}

func (m memJobRepo) ListHeaders(_ context.Context, jobID int64) ([]domain.JobHeader, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.headers[jobID], nil
}

func (m memJobRepo) ReplaceHeaders(_ context.Context, jobID int64, headers []domain.JobHeader) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.headers[jobID] = headers
	return nil
}

func (m memJobRepo) ListSchedules(_ context.Context, jobID int64) ([]domain.JobSchedule, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]domain.JobSchedule(nil), m.schedules[jobID]...), nil
}

func (m memJobRepo) CreateSchedule(_ context.Context, s *domain.JobSchedule) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, existing := range m.schedules[s.JobID] {
		if existing.Expression == s.Expression {
			return 0, repository.ErrDuplicate
		}
	}
	id := m.nextSchedule
	m.nextSchedule++
	copied := *s
	copied.ID = id
	m.schedules[s.JobID] = append(m.schedules[s.JobID], copied)
	return id, nil
}

func (m memJobRepo) DeleteSchedule(_ context.Context, id, jobID int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	list := m.schedules[jobID]
	for i, s := range list {
		if s.ID == id {
			m.schedules[jobID] = append(list[:i:i], list[i+1:]...)
			return nil
		}
	}
	return repository.ErrNotFound
}

func (m memJobRepo) CountSchedules(_ context.Context, jobID int64) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, s := range m.schedules[jobID] {
		if s.Active {
			n++
		}
	}
	return n, nil
}

func (m memJobRepo) ReplaceSchedules(_ context.Context, jobID int64, expressions []string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []domain.JobSchedule
	for _, e := range expressions {
		id := m.nextSchedule
		m.nextSchedule++
		out = append(out, domain.JobSchedule{ID: id, JobID: jobID, Expression: e, Active: true})
	}
	m.schedules[jobID] = out
	return nil
}

func (m memJobRepo) ListLinks(_ context.Context, jobID int64) ([]domain.JobLinkRow, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []domain.JobLinkRow
	for _, l := range m.links {
		if l.JobID != jobID {
			continue
		}
		row := domain.JobLinkRow{JobLink: *l}
		if target, ok := m.jobs[l.TargetJobID]; ok {
			row.TargetCode = target.Code
			row.TargetName = target.Name
			row.TargetActive = target.Active
		}
		out = append(out, row)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (m memJobRepo) ListTriggers(_ context.Context, jobID int64) ([]domain.JobLinkRow, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []domain.JobLinkRow
	for _, l := range m.links {
		if l.TargetJobID != jobID {
			continue
		}
		row := domain.JobLinkRow{JobLink: *l}
		if source, ok := m.jobs[l.JobID]; ok {
			row.SourceCode = source.Code
		}
		out = append(out, row)
	}
	return out, nil
}

func (m memJobRepo) CreateLink(_ context.Context, l *domain.JobLink) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, existing := range m.links {
		if existing.JobID == l.JobID && existing.TargetJobID == l.TargetJobID &&
			existing.Condition == l.Condition {
			return 0, repository.ErrDuplicate
		}
	}
	id := m.nextLink
	m.nextLink++
	copied := *l
	copied.ID = id
	m.links[id] = &copied
	return id, nil
}

func (m memJobRepo) DeleteLink(_ context.Context, id, jobID int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if l, ok := m.links[id]; ok && l.JobID == jobID {
		delete(m.links, id)
		return nil
	}
	return repository.ErrNotFound
}

// ReachesJob walks the chain forward, the way the recursive query does.
func (m memJobRepo) ReachesJob(_ context.Context, fromID, targetID int64, maxDepth int) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	frontier := []int64{fromID}
	for depth := 0; depth <= maxDepth && len(frontier) > 0; depth++ {
		var next []int64
		for _, id := range frontier {
			if id == targetID {
				return true, nil
			}
			for _, l := range m.links {
				if l.JobID == id && l.Active {
					next = append(next, l.TargetJobID)
				}
			}
		}
		frontier = next
	}
	return false, nil
}

// --- runs, management side ---

type memRunRepo struct {
	mu       sync.Mutex
	enqueued []int64
}

func (m *memRunRepo) Get(context.Context, int64) (*domain.RunRow, error) {
	return nil, repository.ErrNotFound
}

func (m *memRunRepo) List(context.Context, domain.RunFilter, int, int) ([]domain.RunRow, int64, error) {
	return nil, 0, nil
}

func (m *memRunRepo) Recent(context.Context, int64, int) ([]domain.Run, error) { return nil, nil }
func (m *memRunRepo) Children(context.Context, int64) ([]domain.RunRow, error) { return nil, nil }

func (m *memRunRepo) Enqueue(_ context.Context, jobID int64, _ string, _ *int64) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.enqueued = append(m.enqueued, jobID)
	return int64(len(m.enqueued)), nil
}

// newTestJobService wires a job service over the in-memory store.
func newTestJobService(store *memStore) *JobService {
	return NewJobService(memJobRepo{store}, memProjectRepo{store}, &memRunRepo{},
		TargetPolicy{AllowPrivate: true}, time.UTC)
}

// newTestSyncService wires a sync service over the same store.
func newTestSyncService(store *memStore) (*SyncService, *JobService) {
	jobs := newTestJobService(store)
	return NewSyncService(memJobRepo{store}, memProjectRepo{store}, jobs), jobs
}
