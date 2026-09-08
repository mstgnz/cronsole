package service

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/mstgnz/cronsole/v2/internal/domain"
	"github.com/mstgnz/cronsole/v2/internal/repository"
)

// ProjectService owns projects and the API keys that let them register their
// own jobs.
type ProjectService struct {
	projects domain.ProjectRepository
	jobs     domain.JobRepository
	policy   TargetPolicy
}

// NewProjectService wires the service.
func NewProjectService(projects domain.ProjectRepository, jobs domain.JobRepository, policy TargetPolicy) *ProjectService {
	return &ProjectService{projects: projects, jobs: jobs, policy: policy}
}

// ProjectInput is what a caller may set on a project.
type ProjectInput struct {
	Name        string `json:"name"`
	Slug        string `json:"slug"`
	Description string `json:"description"`
	BaseURL     string `json:"base_url"`
	Active      bool   `json:"active"`
}

// Create stores a project and returns it together with its API key.
//
// The key is returned ONCE, here, and never again: only its hash is stored. A
// key that can be read back from the screen is a key that lives in every
// browser history and screenshot of that screen.
func (s *ProjectService) Create(ctx context.Context, in ProjectInput, userID *int64) (*domain.Project, string, error) {
	project, err := s.validate(in, nil)
	if err != nil {
		return nil, "", err
	}
	project.UserID = userID

	key, prefix, hash := newAPIKey()
	project.KeyPrefix = prefix
	project.KeyHash = hash

	id, err := s.projects.Create(ctx, project)
	if errors.Is(err, repository.ErrDuplicate) {
		return nil, "", (&ValidationError{}).Add("slug", "a project with this slug already exists")
	}
	if err != nil {
		return nil, "", err
	}
	project.ID = id
	return project, key, nil
}

// Update writes a project.
func (s *ProjectService) Update(ctx context.Context, scope domain.ProjectScope, id int64, in ProjectInput) error {
	if !scope.Allows(id) {
		return ErrNotFound
	}
	existing, err := s.projects.GetByID(ctx, id)
	if errors.Is(err, repository.ErrNotFound) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}

	project, err := s.validate(in, existing)
	if err != nil {
		return err
	}
	project.ID = id

	err = s.projects.Update(ctx, project)
	if errors.Is(err, repository.ErrDuplicate) {
		return (&ValidationError{}).Add("slug", "a project with this slug already exists")
	}
	return err
}

func (s *ProjectService) validate(in ProjectInput, existing *domain.Project) (*domain.Project, error) {
	v := &ValidationError{}

	in.Name = strings.TrimSpace(in.Name)
	in.Slug = strings.ToLower(strings.TrimSpace(in.Slug))
	in.BaseURL = strings.TrimRight(strings.TrimSpace(in.BaseURL), "/")

	if in.Name == "" {
		v.Add("name", "required")
	}
	if in.Slug == "" {
		in.Slug = Slugify(in.Name)
	}
	if !ValidSlug(in.Slug) {
		v.Add("slug", "use lower case letters, digits, dash and underscore, 2 to 64 characters")
	}
	if in.BaseURL != "" {
		// The base address is validated with private targets allowed
		// regardless of policy: an internal base address is the normal case,
		// and the policy check belongs on the resolved target, not here.
		if _, err := (TargetPolicy{AllowPrivate: true}).Resolve("", in.BaseURL); err != nil {
			v.Add("base_url", err.Error())
		}
	}
	if !v.OK() {
		return nil, v
	}

	out := &domain.Project{
		Name:        in.Name,
		Slug:        in.Slug,
		Description: strings.TrimSpace(in.Description),
		BaseURL:     in.BaseURL,
		Active:      in.Active,
	}
	if existing != nil {
		out.KeyPrefix = existing.KeyPrefix
		out.KeyHash = existing.KeyHash
		out.UserID = existing.UserID
	}
	return out, nil
}

// RotateKey issues a new API key and returns it once.
func (s *ProjectService) RotateKey(ctx context.Context, scope domain.ProjectScope, id int64) (string, error) {
	if !scope.Allows(id) {
		return "", ErrNotFound
	}
	if _, err := s.projects.GetByID(ctx, id); err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return "", ErrNotFound
		}
		return "", err
	}
	key, prefix, hash := newAPIKey()
	if err := s.projects.SetKey(ctx, id, prefix, hash); err != nil {
		return "", err
	}
	return key, nil
}

// Authenticate resolves a raw API key to its project.
//
// The prefix narrows the lookup to one row; it decides nothing. The hash
// comparison is constant time, so a caller cannot learn how much of a key was
// correct from how long the answer took.
func (s *ProjectService) Authenticate(ctx context.Context, rawKey string) (*domain.Project, error) {
	rawKey = strings.TrimSpace(rawKey)
	if len(rawKey) < keyPrefixLen+8 {
		return nil, ErrForbidden
	}

	project, err := s.projects.GetByKeyPrefix(ctx, rawKey[:keyPrefixLen])
	if errors.Is(err, repository.ErrNotFound) {
		return nil, ErrForbidden
	}
	if err != nil {
		return nil, err
	}

	sum := sha256.Sum256([]byte(rawKey))
	given := hex.EncodeToString(sum[:])
	if subtle.ConstantTimeCompare([]byte(given), []byte(project.KeyHash)) != 1 {
		return nil, ErrForbidden
	}
	if !project.Active {
		return nil, ErrForbidden
	}
	return project, nil
}

// Get reads one project.
func (s *ProjectService) Get(ctx context.Context, scope domain.ProjectScope, id int64) (*domain.Project, error) {
	if !scope.Allows(id) {
		return nil, ErrNotFound
	}
	project, err := s.projects.GetByID(ctx, id)
	if errors.Is(err, repository.ErrNotFound) {
		return nil, ErrNotFound
	}
	return project, err
}

// GetBySlug reads one project by machine name.
func (s *ProjectService) GetBySlug(ctx context.Context, slug string) (*domain.Project, error) {
	project, err := s.projects.GetBySlug(ctx, slug)
	if errors.Is(err, repository.ErrNotFound) {
		return nil, ErrNotFound
	}
	return project, err
}

// List returns every project with its counters.
func (s *ProjectService) List(ctx context.Context, scope domain.ProjectScope, search string) ([]domain.ProjectRow, error) {
	return s.projects.List(ctx, scope, search)
}

// ListNames feeds the filter dropdown.
func (s *ProjectService) ListNames(ctx context.Context, scope domain.ProjectScope) ([]domain.Project, error) {
	return s.projects.ListNames(ctx, scope)
}

// Delete removes a project and, with it, its jobs.
//
// It refuses while active jobs remain. A cascade would be quieter and would
// stop scheduled work with no acknowledgement that it was about to happen.
func (s *ProjectService) Delete(ctx context.Context, scope domain.ProjectScope, id int64, force bool) error {
	if !scope.Allows(id) {
		return ErrNotFound
	}
	active := true
	rows, _, err := s.jobs.List(ctx,
		domain.JobFilter{Scope: scope, ProjectID: &id, Active: &active}, 0, 1)
	if err != nil {
		return err
	}
	if len(rows) > 0 && !force {
		return ErrProjectLocked
	}
	if err := s.projects.SoftDelete(ctx, id, time.Now()); err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return ErrNotFound
		}
		return err
	}
	return nil
}

// keyPrefixLen is how much of a key is stored in clear, for lookup. Eight
// characters of a 48 character key identify the row without narrowing a guess
// of the rest to anything useful.
const keyPrefixLen = 8

// newAPIKey mints a key and returns it, its prefix and its hash.
//
// crypto/rand, never math/rand: a key from a predictable generator is not a
// key. The hash is plain sha256 rather than bcrypt because this is a
// high entropy random string, not a password: there is nothing to brute force
// and the check sits on the hot path of every API call.
func newAPIKey() (key, prefix, hash string) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		// crypto/rand failing is not survivable: continuing would mean issuing
		// a predictable key, which is worse than no key at all.
		panic(fmt.Sprintf("service: secure random unavailable: %v", err))
	}
	key = "cj_" + hex.EncodeToString(buf)
	prefix = key[:keyPrefixLen]
	sum := sha256.Sum256([]byte(key))
	return key, prefix, hex.EncodeToString(sum[:])
}
