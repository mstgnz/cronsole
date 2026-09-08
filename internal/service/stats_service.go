package service

import (
	"context"
	"sort"
	"time"

	"github.com/mstgnz/cronsole/v2/internal/domain"
)

// StatsStore is what the dashboard reads. Declared here rather than in domain
// because the dashboard is the only consumer and its shape follows the screen.
type StatsStore interface {
	domain.StatsRepository
	RunningNow(ctx context.Context, scope domain.ProjectScope, limit int) ([]domain.RunRow, error)
	RecentFailures(ctx context.Context, scope domain.ProjectScope, limit int) ([]domain.RunRow, error)
}

// StatsService assembles the dashboard.
type StatsService struct {
	stats StatsStore
	jobs  domain.JobRepository
	job   *JobService
}

// NewStatsService wires the service.
func NewStatsService(stats StatsStore, jobs domain.JobRepository, jobService *JobService) *StatsService {
	return &StatsService{stats: stats, jobs: jobs, job: jobService}
}

// Dashboard is everything the front page shows.
type Dashboard struct {
	Summary  *domain.Summary      `json:"summary"`
	Activity []domain.HourBucket  `json:"activity"`
	Running  []domain.RunRow      `json:"running"`
	Failures []domain.RunRow      `json:"failures"`
	Slowest  []domain.JobDuration `json:"slowest"`
	Upcoming []UpcomingRun        `json:"upcoming"`
}

// JobTrend is one job's recent behaviour, for the chart on its page.
//
// The aggregates are computed over the SAME window the chart draws, not over
// all time. A number underneath a chart that describes a different period is
// worse than no number: the reader takes it as the chart's own summary.
type JobTrend struct {
	Points  []domain.RunPoint `json:"points"`
	Total   int               `json:"total"`
	Success int               `json:"success"`
	Failed  int               `json:"failed"`
	Timeout int               `json:"timeout"`
	Skipped int               `json:"skipped"`
	// AvgMs and P95Ms cover the runs that actually executed and finished.
	// A skipped run has no duration, and folding its zero into the average
	// would make a job look faster the more often it fails to start.
	AvgMs int `json:"avg_ms"`
	P95Ms int `json:"p95_ms"`
	MaxMs int `json:"max_ms"`
	// SuccessRate is a percentage over Total, or -1 when there is nothing to
	// divide by. Zero would read as "everything failed".
	SuccessRate int `json:"success_rate"`
}

// JobTrendFor reads one job's recent runs and summarises them.
//
// The scope is checked by the query rather than here: a job outside it returns
// no points, which is the same answer as a job that has never run. Telling the
// two apart is the job page's business, and it has already refused the job.
func (s *StatsService) JobTrendFor(ctx context.Context, scope domain.ProjectScope, jobID, limit int64) (*JobTrend, error) {
	points, err := s.stats.JobHistory(ctx, scope, jobID, int(limit))
	if err != nil {
		return nil, err
	}

	trend := &JobTrend{Points: points, Total: len(points), SuccessRate: -1}
	durations := make([]int, 0, len(points))
	for _, p := range points {
		switch p.Status {
		case domain.StatusSuccess:
			trend.Success++
		case domain.StatusFailed:
			trend.Failed++
		case domain.StatusTimeout:
			trend.Timeout++
		case domain.StatusSkipped:
			trend.Skipped++
		}
		if p.DurationMs > 0 {
			durations = append(durations, p.DurationMs)
		}
	}

	if trend.Total > 0 {
		trend.SuccessRate = trend.Success * 100 / trend.Total
	}
	if len(durations) > 0 {
		sort.Ints(durations)
		sum := 0
		for _, d := range durations {
			sum += d
		}
		trend.AvgMs = sum / len(durations)
		trend.MaxMs = durations[len(durations)-1]
		// Nearest-rank p95: the smallest value at least 95% of the runs are at
		// or below. On a short window it lands on the slowest run, which is the
		// honest answer rather than an interpolation between two points.
		idx := (len(durations)*95 + 99) / 100
		trend.P95Ms = durations[idx-1]
	}
	return trend, nil
}

// UpcomingRun is a job's next firing.
type UpcomingRun struct {
	JobID       int64     `json:"job_id"`
	Code        string    `json:"code"`
	Name        string    `json:"name"`
	ProjectSlug string    `json:"project_slug"`
	At          time.Time `json:"at"`
}

// Build assembles the dashboard.
//
// The pieces are read in sequence, and a failure in any of them fails the
// whole page. That is the right trade here: a dashboard that silently drops
// the panel that would have shown the outage is worse than one that says it
// could not be built.
func (s *StatsService) Build(ctx context.Context, scope domain.ProjectScope, activityHours int) (*Dashboard, error) {
	summary, err := s.stats.Summary(ctx, scope)
	if err != nil {
		return nil, err
	}
	activity, err := s.stats.Activity(ctx, scope, activityHours)
	if err != nil {
		return nil, err
	}
	running, err := s.stats.RunningNow(ctx, scope, 20)
	if err != nil {
		return nil, err
	}
	failures, err := s.stats.RecentFailures(ctx, scope, 12)
	if err != nil {
		return nil, err
	}
	slowest, err := s.stats.SlowestJobs(ctx, scope, 24, 8)
	if err != nil {
		return nil, err
	}
	upcoming, err := s.upcoming(ctx, scope, 12)
	if err != nil {
		return nil, err
	}

	return &Dashboard{
		Summary:  summary,
		Activity: activity,
		Running:  running,
		Failures: failures,
		Slowest:  slowest,
		Upcoming: upcoming,
	}, nil
}

// upcoming lists the next firings across every active job.
//
// It is computed from the expressions rather than read from a table, because
// nothing schedules ahead: the dispatcher decides a minute at a time, so
// "what runs next" exists only as a calculation.
func (s *StatsService) upcoming(ctx context.Context, scope domain.ProjectScope, limit int) ([]UpcomingRun, error) {
	active := true
	rows, _, err := s.jobs.List(ctx, domain.JobFilter{Scope: scope, Active: &active}, 0, 500)
	if err != nil {
		return nil, err
	}

	out := make([]UpcomingRun, 0, len(rows))
	for _, row := range rows {
		next := s.job.NextRunFor(row.Schedules)
		if next == nil {
			continue
		}
		out = append(out, UpcomingRun{
			JobID:       row.ID,
			Code:        row.Code,
			Name:        row.Name,
			ProjectSlug: row.ProjectSlug,
			At:          *next,
		})
	}

	// Insertion sort: the list is at most a few hundred entries and already
	// close to grouped, and pulling in a comparator for it earns nothing.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].At.Before(out[j-1].At); j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}
