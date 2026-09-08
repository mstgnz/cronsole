package service

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/mstgnz/cronsole/v2/internal/domain"
	"github.com/mstgnz/cronsole/v2/internal/repository"
)

// NotificationService owns the recipient sets a job can be attached to.
type NotificationService struct {
	repo domain.NotificationRepository
}

// NewNotificationService wires the service.
func NewNotificationService(repo domain.NotificationRepository) *NotificationService {
	return &NotificationService{repo: repo}
}

// NotificationInput is what a caller may set.
type NotificationInput struct {
	Name      string   `json:"name"`
	OnSuccess bool     `json:"on_success"`
	OnFailure bool     `json:"on_failure"`
	Active    bool     `json:"active"`
	Emails    []string `json:"emails"`
}

// List returns every recipient set.
func (s *NotificationService) List(ctx context.Context) ([]domain.Notification, error) {
	return s.repo.List(ctx)
}

// Get reads one recipient set.
func (s *NotificationService) Get(ctx context.Context, id int64) (*domain.Notification, error) {
	n, err := s.repo.Get(ctx, id)
	if errors.Is(err, repository.ErrNotFound) {
		return nil, ErrNotFound
	}
	return n, err
}

// Create stores a recipient set.
func (s *NotificationService) Create(ctx context.Context, in NotificationInput, userID *int64) (int64, error) {
	n, err := s.validate(in)
	if err != nil {
		return 0, err
	}
	n.UserID = userID
	return s.repo.Create(ctx, n)
}

// Update writes a recipient set.
func (s *NotificationService) Update(ctx context.Context, id int64, in NotificationInput) error {
	n, err := s.validate(in)
	if err != nil {
		return err
	}
	n.ID = id
	return s.repo.Update(ctx, n)
}

func (s *NotificationService) validate(in NotificationInput) (*domain.Notification, error) {
	v := &ValidationError{}
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" {
		v.Add("name", "required")
	}

	clean := make([]string, 0, len(in.Emails))
	for _, raw := range in.Emails {
		email := strings.ToLower(strings.TrimSpace(raw))
		if email == "" {
			continue
		}
		// A deliberately loose check. Address syntax is far more permissive
		// than most validators allow, and refusing a valid address is a worse
		// failure than accepting one that bounces.
		if !strings.Contains(email, "@") || strings.HasPrefix(email, "@") || strings.HasSuffix(email, "@") {
			v.Add("emails", email+" is not a valid address")
			continue
		}
		clean = append(clean, email)
	}
	if len(clean) == 0 {
		v.Add("emails", "add at least one address")
	}

	// A set that reports neither outcome is attached to jobs and then never
	// sends anything, which reads as a broken mail server.
	if !in.OnSuccess && !in.OnFailure {
		v.Add("on_failure", "choose at least one outcome to report")
	}
	if !v.OK() {
		return nil, v
	}

	return &domain.Notification{
		Name:      in.Name,
		OnSuccess: in.OnSuccess,
		OnFailure: in.OnFailure,
		Active:    in.Active,
		Emails:    clean,
	}, nil
}

// Delete removes a recipient set. Jobs pointing at it keep running with no
// recipient, because losing an on-call list must never stop the work.
func (s *NotificationService) Delete(ctx context.Context, id int64) error {
	// The repository's sentinel is translated here rather than passed on. A
	// handler branches on this package's errors, so one that leaks through
	// lands in the catch-all and a stale link is reported as a server fault.
	if err := s.repo.SoftDelete(ctx, id, time.Now()); err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return ErrNotFound
		}
		return err
	}
	return nil
}
