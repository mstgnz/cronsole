package domain

import (
	"context"
	"time"
)

// Notification is a named recipient set plus the outcomes it wants to hear
// about. Attached to a job, so several jobs can share one on-call list.
type Notification struct {
	ID        int64      `json:"id"`
	UserID    *int64     `json:"user_id,omitempty"`
	Name      string     `json:"name"`
	OnSuccess bool       `json:"on_success"`
	OnFailure bool       `json:"on_failure"`
	Active    bool       `json:"active"`
	Emails    []string   `json:"emails"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt *time.Time `json:"updated_at,omitempty"`
}

// NotificationRepository is data access for recipient sets.
type NotificationRepository interface {
	Get(ctx context.Context, id int64) (*Notification, error)
	List(ctx context.Context) ([]Notification, error)
	Create(ctx context.Context, n *Notification) (int64, error)
	Update(ctx context.Context, n *Notification) error
	SoftDelete(ctx context.Context, id int64, at time.Time) error
	ReplaceEmails(ctx context.Context, notificationID int64, emails []string) error
}

// AppLog is one application level log row. Runtime faults land here rather
// than only on stdout, because stdout is gone the moment a container restarts
// and the operator is looking at a screen, not a terminal.
type AppLog struct {
	ID        int64     `json:"id"`
	Level     string    `json:"level"`
	Message   string    `json:"message"`
	Detail    string    `json:"detail"`
	CreatedAt time.Time `json:"created_at"`
}

// AppLogRepository is data access for application logs.
type AppLogRepository interface {
	List(ctx context.Context, level string, offset, limit int) ([]AppLog, int64, error)
	DeleteBefore(ctx context.Context, before time.Time) (int64, error)
}
