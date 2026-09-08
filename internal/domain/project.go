package domain

import (
	"context"
	"time"
)

// Project is a system that owns scheduled work. Every job belongs to exactly
// one, and an API key is scoped to one, so a project is the unit of ownership
// and of blast radius at the same time.
type Project struct {
	ID          int64      `json:"id"`
	Name        string     `json:"name"`
	Slug        string     `json:"slug"`
	Description string     `json:"description"`
	BaseURL     string     `json:"base_url"`
	KeyPrefix   string     `json:"api_key_prefix"`
	KeyHash     string     `json:"-"`
	Active      bool       `json:"active"`
	UserID      *int64     `json:"user_id,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   *time.Time `json:"updated_at,omitempty"`
}

// ProjectRow is a project plus the counters the list screen shows. Kept apart
// from Project because those counters are read only and must never reach the
// write path.
type ProjectRow struct {
	Project
	JobTotal   int `json:"job_total"`
	JobActive  int `json:"job_active"`
	DaySuccess int `json:"day_success"`
	DayFailed  int `json:"day_failed"`
}

// ProjectRepository is data access for projects.
type ProjectRepository interface {
	GetByID(ctx context.Context, id int64) (*Project, error)
	GetBySlug(ctx context.Context, slug string) (*Project, error)
	// GetByKeyPrefix finds the candidate for an API key. The prefix is not a
	// secret and not sufficient on its own: the caller still compares the hash
	// in constant time.
	GetByKeyPrefix(ctx context.Context, prefix string) (*Project, error)
	List(ctx context.Context, scope ProjectScope, search string) ([]ProjectRow, error)
	// ListNames is the picker source: id, name and slug only, because the
	// filter dropdown does not need the counters the list screen computes.
	ListNames(ctx context.Context, scope ProjectScope) ([]Project, error)
	Create(ctx context.Context, p *Project) (int64, error)
	Update(ctx context.Context, p *Project) error
	SetKey(ctx context.Context, id int64, prefix, hash string) error
	SoftDelete(ctx context.Context, id int64, at time.Time) error
}
