// Package memrepo implements every repository interface in memory.
//
// It exists so the handlers and the router can be tested through the whole
// stack: a real request goes through the real middleware, the real services and
// the real authorization, and only the SQL is replaced. A handler test built on
// mocked services proves the handler calls a mock; this proves that a reader on
// one project cannot reach another project's jobs, which is the thing that
// actually has to be true.
//
// It is not a fake in the loose sense. Where a rule is enforced by the database
// it is enforced here too: the scope narrows every list, a soft-deleted row
// stops being visible, and a duplicate code on one project is refused. A store
// that is more permissive than Postgres would let a test pass on behaviour the
// deployment does not have.
//
// Only tests import it. It is ordinary code rather than a _test.go file because
// more than one package needs it, and a test helper that cannot be shared gets
// written twice and drifts.
package memrepo

import (
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/mstgnz/cronsole/v2/internal/domain"
)

// Store holds every table. One store behind every repository, so a job created
// through the job repository is visible to the run repository, the same way one
// database is.
type Store struct {
	mu sync.Mutex

	users     map[int64]*domain.User
	projects  map[int64]*domain.Project
	jobs      map[int64]*domain.Job
	schedules map[int64][]domain.JobSchedule
	headers   map[int64][]domain.JobHeader
	links     map[int64]*domain.JobLink
	runs      map[int64]*domain.Run
	notifs    map[int64]*domain.Notification
	logs      []domain.AppLog
	hosts     map[int64]*domain.HostOverride

	resets map[int64]*domain.PasswordReset

	nextUser, nextProject, nextJob  int64
	nextSchedule, nextLink, nextRun int64
	nextNotification, nextHost      int64
	nextReset                       int64
	deletedJobs, deletedProjects    map[int64]bool
	deletedUsers, deletedNotifs     map[int64]bool
}

// New builds an empty store.
func New() *Store {
	return &Store{
		users:            map[int64]*domain.User{},
		projects:         map[int64]*domain.Project{},
		jobs:             map[int64]*domain.Job{},
		schedules:        map[int64][]domain.JobSchedule{},
		headers:          map[int64][]domain.JobHeader{},
		links:            map[int64]*domain.JobLink{},
		runs:             map[int64]*domain.Run{},
		notifs:           map[int64]*domain.Notification{},
		hosts:            map[int64]*domain.HostOverride{},
		resets:           map[int64]*domain.PasswordReset{},
		nextReset:        1,
		nextUser:         1,
		nextProject:      1,
		nextJob:          1,
		nextSchedule:     1,
		nextLink:         1,
		nextRun:          1,
		nextNotification: 1,
		nextHost:         1,
		deletedJobs:      map[int64]bool{},
		deletedProjects:  map[int64]bool{},
		deletedUsers:     map[int64]bool{},
		deletedNotifs:    map[int64]bool{},
	}
}

// --- seeding ----------------------------------------------------------------
//
// These bypass the repository methods so a test can set up a world without
// asserting on the setup.

// AddUser stores an account and returns it.
func (s *Store) AddUser(u domain.User) *domain.User {
	s.mu.Lock()
	defer s.mu.Unlock()

	if u.ID == 0 {
		u.ID = s.nextUser
		s.nextUser++
	} else if u.ID >= s.nextUser {
		s.nextUser = u.ID + 1
	}
	if u.CreatedAt.IsZero() {
		u.CreatedAt = time.Now()
	}
	copied := u
	s.users[u.ID] = &copied
	return &copied
}

// AddProject stores a project and returns it.
func (s *Store) AddProject(p domain.Project) *domain.Project {
	s.mu.Lock()
	defer s.mu.Unlock()

	if p.ID == 0 {
		p.ID = s.nextProject
		s.nextProject++
	} else if p.ID >= s.nextProject {
		s.nextProject = p.ID + 1
	}
	if p.Name == "" {
		p.Name = p.Slug
	}
	if p.CreatedAt.IsZero() {
		p.CreatedAt = time.Now()
	}
	copied := p
	s.projects[p.ID] = &copied
	return &copied
}

// AddJob stores a job and returns it.
func (s *Store) AddJob(j domain.Job) *domain.Job {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.putJob(j)
}

// putJob stores a job. The caller holds the lock.
func (s *Store) putJob(j domain.Job) *domain.Job {
	if j.ID == 0 {
		j.ID = s.nextJob
		s.nextJob++
	} else if j.ID >= s.nextJob {
		s.nextJob = j.ID + 1
	}
	if j.Method == "" {
		j.Method = "GET"
	}
	if j.SuccessMin == 0 && j.SuccessMax == 0 {
		j.SuccessMin, j.SuccessMax = 200, 299
	}
	if j.TimeoutSec == 0 {
		j.TimeoutSec = domain.DefaultTimeoutSec
	}
	if j.CreatedAt.IsZero() {
		j.CreatedAt = time.Now()
	}
	copied := j
	s.jobs[j.ID] = &copied
	return &copied
}

// AddSchedule attaches a cron expression to a job.
func (s *Store) AddSchedule(jobID int64, expression string) domain.JobSchedule {
	s.mu.Lock()
	defer s.mu.Unlock()

	row := domain.JobSchedule{
		ID: s.nextSchedule, JobID: jobID, Expression: expression,
		Active: true, CreatedAt: time.Now(),
	}
	s.nextSchedule++
	s.schedules[jobID] = append(s.schedules[jobID], row)
	return row
}

// AddRun stores an execution record.
func (s *Store) AddRun(r domain.Run) *domain.Run {
	s.mu.Lock()
	defer s.mu.Unlock()

	if r.ID == 0 {
		r.ID = s.nextRun
		s.nextRun++
	} else if r.ID >= s.nextRun {
		s.nextRun = r.ID + 1
	}
	if r.Status == "" {
		r.Status = domain.StatusPending
	}
	if r.Trigger == "" {
		r.Trigger = domain.TriggerSchedule
	}
	if r.CreatedAt.IsZero() {
		r.CreatedAt = time.Now()
	}
	copied := r
	s.runs[r.ID] = &copied
	return &copied
}

// AddLog appends an application log entry.
func (s *Store) AddLog(entry domain.AppLog) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if entry.CreatedAt.IsZero() {
		entry.CreatedAt = time.Now()
	}
	s.logs = append(s.logs, entry)
}

// --- reading back, for assertions -------------------------------------------

// Job returns a copy of a job, or nil.
func (s *Store) Job(id int64) *domain.Job {
	s.mu.Lock()
	defer s.mu.Unlock()
	if j, ok := s.jobs[id]; ok && !s.deletedJobs[id] {
		copied := *j
		return &copied
	}
	return nil
}

// User returns a copy of an account, or nil.
func (s *Store) User(id int64) *domain.User {
	s.mu.Lock()
	defer s.mu.Unlock()
	if u, ok := s.users[id]; ok && !s.deletedUsers[id] {
		copied := *u
		return &copied
	}
	return nil
}

// UserByEmail returns a copy of an account by address, or nil.
func (s *Store) UserByEmail(email string) *domain.User {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, u := range s.users {
		if !s.deletedUsers[id] && strings.EqualFold(u.Email, email) {
			copied := *u
			return &copied
		}
	}
	return nil
}

// Project returns a copy of a project, or nil.
func (s *Store) Project(id int64) *domain.Project {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p, ok := s.projects[id]; ok && !s.deletedProjects[id] {
		copied := *p
		return &copied
	}
	return nil
}

// Run returns a copy of a run, or nil.
func (s *Store) Run(id int64) *domain.Run {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.runs[id]; ok {
		copied := *r
		return &copied
	}
	return nil
}

// HostOverrides returns every stored route.
func (s *Store) HostOverrides() []domain.HostOverride {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []domain.HostOverride{}
	for _, o := range s.hosts {
		out = append(out, *o)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// RunCount is how many execution records exist.
func (s *Store) RunCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.runs)
}

// --- helpers ----------------------------------------------------------------

func matches(haystack, needle string) bool {
	return strings.Contains(strings.ToLower(haystack), strings.ToLower(needle))
}

func page[T any](rows []T, offset, limit int) []T {
	if offset >= len(rows) {
		return []T{}
	}
	rows = rows[offset:]
	if limit > 0 && limit < len(rows) {
		rows = rows[:limit]
	}
	return rows
}
