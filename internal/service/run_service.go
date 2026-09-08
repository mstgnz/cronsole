package service

import (
	"context"
	"errors"
	"time"

	"github.com/mstgnz/cronsole/v2/internal/domain"
	"github.com/mstgnz/cronsole/v2/internal/repository"
)

// RunService is the management side of execution records: reading history, and
// asking for a run outside the schedule.
type RunService struct {
	runs     domain.RunRepository
	jobs     domain.JobRepository
	dispatch DispatchFunc
}

// NewRunService wires the service.
func NewRunService(runs domain.RunRepository, jobs domain.JobRepository, dispatch DispatchFunc) *RunService {
	return &RunService{runs: runs, jobs: jobs, dispatch: dispatch}
}

// List pages through runs. The scope rides on the filter.
func (s *RunService) List(ctx context.Context, f domain.RunFilter, offset, limit int) ([]domain.RunRow, int64, error) {
	return s.runs.List(ctx, f, offset, limit)
}

// owned reads a run and refuses it when its job is outside the scope.
//
// ErrNotFound rather than ErrForbidden, so a run id cannot be used to learn
// that a job exists in a project the caller cannot see.
func (s *RunService) owned(ctx context.Context, scope domain.ProjectScope, id int64) (*domain.RunRow, error) {
	row, err := s.runs.Get(ctx, id)
	if errors.Is(err, repository.ErrNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	job, err := s.jobs.Get(ctx, row.JobID)
	if err != nil {
		return nil, err
	}
	if !scope.Allows(job.ProjectID) {
		return nil, ErrNotFound
	}
	return row, nil
}

// Get reads one run.
func (s *RunService) Get(ctx context.Context, scope domain.ProjectScope, id int64) (*domain.RunRow, error) {
	return s.owned(ctx, scope, id)
}

// Children returns the runs a run triggered, so a chain can be followed
// forward from any point. This is the answer to "what did this set off".
func (s *RunService) Children(ctx context.Context, scope domain.ProjectScope, id int64) ([]domain.RunRow, error) {
	if _, err := s.owned(ctx, scope, id); err != nil {
		return nil, err
	}
	children, err := s.runs.Children(ctx, id)
	if err != nil {
		return nil, err
	}
	// A chain can cross projects, so a child may belong to one the caller
	// cannot see. Dropping those keeps the panel honest about what it shows
	// rather than naming a job from a project they have no access to.
	if scope.All {
		return children, nil
	}
	visible := make([]domain.RunRow, 0, len(children))
	for _, child := range children {
		job, err := s.jobs.Get(ctx, child.JobID)
		if err != nil {
			continue
		}
		if scope.Allows(job.ProjectID) {
			visible = append(visible, child)
		}
	}
	return visible, nil
}

// Trigger asks for a run now.
//
// It queues a row and hands it over; it does not execute anything itself. Both
// halves matter: the row means the request is durable and appears in the
// history like any other, and the hand-off means it obeys the same single run
// rule and concurrency cap as a scheduled one. An endpoint that fired the
// request directly would bypass every one of those.
func (s *RunService) Trigger(ctx context.Context, scope domain.ProjectScope, jobID int64, trigger string, userID *int64) (int64, error) {
	job, err := s.jobs.Get(ctx, jobID)
	if errors.Is(err, repository.ErrNotFound) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, err
	}
	if !scope.Allows(job.ProjectID) {
		return 0, ErrNotFound
	}

	runID, err := s.runs.Enqueue(ctx, jobID, trigger, userID)
	if err != nil {
		return 0, err
	}
	if s.dispatch != nil {
		s.dispatch(runID, job.Code)
	}
	return runID, nil
}

// RunFilterFromRange builds a filter over a time window, used by the run list
// where the caller works in days rather than timestamps.
func RunFilterFromRange(days int) domain.RunFilter {
	if days <= 0 {
		days = 1
	}
	start := time.Now().Add(-time.Duration(days) * 24 * time.Hour)
	return domain.RunFilter{Start: &start}
}
