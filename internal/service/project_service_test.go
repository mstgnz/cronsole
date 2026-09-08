package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mstgnz/cronsole/v2/internal/domain"
	"github.com/mstgnz/cronsole/v2/internal/repository"
)

// memProjects is a writable project store, separate from memStore because the
// project service exercises Create, Update, SetKey and the key lookup, which
// memStore stubs out.
type memProjects struct {
	mu     sync.Mutex
	rows   map[int64]*domain.Project
	nextID int64
}

func newProjectRepo() *memProjects {
	return &memProjects{rows: map[int64]*domain.Project{}, nextID: 1}
}

func (m *memProjects) GetByID(_ context.Context, id int64) (*domain.Project, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if p, ok := m.rows[id]; ok {
		copied := *p
		return &copied, nil
	}
	return nil, repository.ErrNotFound
}

func (m *memProjects) GetBySlug(_ context.Context, slug string) (*domain.Project, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, p := range m.rows {
		if p.Slug == slug {
			copied := *p
			return &copied, nil
		}
	}
	return nil, repository.ErrNotFound
}

func (m *memProjects) GetByKeyPrefix(_ context.Context, prefix string) (*domain.Project, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, p := range m.rows {
		if p.KeyPrefix != "" && p.KeyPrefix == prefix {
			copied := *p
			return &copied, nil
		}
	}
	return nil, repository.ErrNotFound
}

func (m *memProjects) List(context.Context, domain.ProjectScope, string) ([]domain.ProjectRow, error) {
	return nil, nil
}

func (m *memProjects) ListNames(context.Context, domain.ProjectScope) ([]domain.Project, error) {
	return nil, nil
}

func (m *memProjects) Create(_ context.Context, p *domain.Project) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, existing := range m.rows {
		if existing.Slug == p.Slug {
			return 0, repository.ErrDuplicate
		}
	}
	id := m.nextID
	m.nextID++
	copied := *p
	copied.ID = id
	m.rows[id] = &copied
	return id, nil
}

func (m *memProjects) Update(_ context.Context, p *domain.Project) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, existing := range m.rows {
		if existing.Slug == p.Slug && existing.ID != p.ID {
			return repository.ErrDuplicate
		}
	}
	stored, ok := m.rows[p.ID]
	if !ok {
		return repository.ErrNotFound
	}
	stored.Name, stored.Slug, stored.BaseURL, stored.Active = p.Name, p.Slug, p.BaseURL, p.Active
	return nil
}

func (m *memProjects) SetKey(_ context.Context, id int64, prefix, hash string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if p, ok := m.rows[id]; ok {
		p.KeyPrefix, p.KeyHash = prefix, hash
	}
	return nil
}

func (m *memProjects) SoftDelete(_ context.Context, id int64, _ time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.rows, id)
	return nil
}

func newTestProjectService() (*ProjectService, *memProjects, *memStore) {
	projects := newProjectRepo()
	jobs := newMemStore()
	return NewProjectService(projects, memJobRepo{memStore: jobs}, TargetPolicy{AllowPrivate: true}),
		projects, jobs
}

func TestCreateProjectIssuesAKeyOnce(t *testing.T) {
	// The key is returned here and never again: only its hash is stored. A key
	// that can be read back from the screen lives in every browser history and
	// screenshot of that screen.
	svc, repo, _ := newTestProjectService()

	project, key, err := svc.Create(context.Background(), ProjectInput{
		Name: "Demo Service", BaseURL: "https://service.example.com", Active: true,
	}, nil)
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}
	if project.Slug != "demo-service" {
		t.Errorf("slug = %q, want it derived from the name", project.Slug)
	}
	if !strings.HasPrefix(key, "cj_") || len(key) < 40 {
		t.Errorf("key = %q, which does not look like a generated key", key)
	}

	stored, err := repo.GetByID(context.Background(), project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stored.KeyHash, key) || stored.KeyHash == key {
		t.Error("the key was stored rather than its hash")
	}
	sum := sha256.Sum256([]byte(key))
	if stored.KeyHash != hex.EncodeToString(sum[:]) {
		t.Error("the stored hash does not match the issued key")
	}
	if stored.KeyPrefix != key[:8] {
		t.Errorf("prefix = %q, want the first 8 characters of the key", stored.KeyPrefix)
	}
}

func TestAuthenticateByKey(t *testing.T) {
	svc, _, _ := newTestProjectService()
	ctx := context.Background()

	project, key, err := svc.Create(ctx, ProjectInput{Name: "Demo", Active: true}, nil)
	if err != nil {
		t.Fatal(err)
	}

	got, err := svc.Authenticate(ctx, key)
	if err != nil {
		t.Fatalf("the issued key was rejected: %v", err)
	}
	if got.ID != project.ID {
		t.Errorf("authenticated project %d, want %d", got.ID, project.ID)
	}

	// The prefix alone authenticates nothing; the hash comparison decides.
	if _, err := svc.Authenticate(ctx, key[:8]+strings.Repeat("0", len(key)-8)); !errors.Is(err, ErrForbidden) {
		t.Errorf("a key with the right prefix and wrong body gave %v, want ErrForbidden", err)
	}
	for _, bad := range []string{"", "short", "cj_" + strings.Repeat("f", 48)} {
		if _, err := svc.Authenticate(ctx, bad); !errors.Is(err, ErrForbidden) {
			t.Errorf("Authenticate(%q) gave %v, want ErrForbidden", bad, err)
		}
	}
}

func TestAuthenticateRefusesAnInactiveProject(t *testing.T) {
	// Deactivating a project has to stop everything it can do, not only its
	// scheduled work.
	svc, repo, _ := newTestProjectService()
	ctx := context.Background()

	project, key, err := svc.Create(ctx, ProjectInput{Name: "Demo", Active: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Update(ctx, allProjects, project.ID, ProjectInput{Name: "Demo", Slug: project.Slug, Active: false}); err != nil {
		t.Fatal(err)
	}
	if stored, _ := repo.GetByID(ctx, project.ID); stored.Active {
		t.Fatal("the project was not deactivated")
	}
	if _, err := svc.Authenticate(ctx, key); !errors.Is(err, ErrForbidden) {
		t.Errorf("an inactive project authenticated: %v", err)
	}
}

func TestRotateKeyInvalidatesTheOldOne(t *testing.T) {
	svc, _, _ := newTestProjectService()
	ctx := context.Background()

	project, first, err := svc.Create(ctx, ProjectInput{Name: "Demo", Active: true}, nil)
	if err != nil {
		t.Fatal(err)
	}

	second, err := svc.RotateKey(ctx, allProjects, project.ID)
	if err != nil {
		t.Fatalf("rotate failed: %v", err)
	}
	if second == first {
		t.Fatal("rotation produced the same key")
	}
	if _, err := svc.Authenticate(ctx, first); !errors.Is(err, ErrForbidden) {
		t.Error("the previous key still works after a rotation")
	}
	if _, err := svc.Authenticate(ctx, second); err != nil {
		t.Errorf("the new key was rejected: %v", err)
	}

	if _, err := svc.RotateKey(ctx, allProjects, 9999); !errors.Is(err, ErrNotFound) {
		t.Errorf("rotating an unknown project gave %v, want ErrNotFound", err)
	}
}

func TestUpdateDoesNotTouchTheKey(t *testing.T) {
	// Rotating a key is a separate action with its own confirmation, so it can
	// never be a side effect of saving a form.
	svc, repo, _ := newTestProjectService()
	ctx := context.Background()

	project, key, err := svc.Create(ctx, ProjectInput{Name: "Demo", Active: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := repo.GetByID(ctx, project.ID)

	if err := svc.Update(ctx, allProjects, project.ID, ProjectInput{
		Name: "Demo Renamed", Slug: "demo-renamed", Active: true,
	}); err != nil {
		t.Fatal(err)
	}

	after, _ := repo.GetByID(ctx, project.ID)
	if after.KeyHash != before.KeyHash {
		t.Error("the key changed as a side effect of an update")
	}
	if _, err := svc.Authenticate(ctx, key); err != nil {
		t.Errorf("the key stopped working after an unrelated update: %v", err)
	}
}

func TestProjectValidation(t *testing.T) {
	svc, _, _ := newTestProjectService()
	ctx := context.Background()

	if _, _, err := svc.Create(ctx, ProjectInput{Name: ""}, nil); err == nil {
		t.Error("a project with no name was accepted")
	}
	if _, _, err := svc.Create(ctx, ProjectInput{Name: "Demo", BaseURL: "not-a-url"}, nil); err == nil {
		t.Error("an unusable base address was accepted")
	}
	if _, _, err := svc.Create(ctx, ProjectInput{Name: "Demo", Slug: "Bad Slug"}, nil); err == nil {
		t.Error("an invalid slug was accepted")
	}

	if _, _, err := svc.Create(ctx, ProjectInput{Name: "Demo", Active: true}, nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.Create(ctx, ProjectInput{Name: "Demo", Active: true}, nil); err == nil {
		t.Error("a duplicate slug was accepted")
	}
}

func TestDeleteRefusesWhileActiveJobsRemain(t *testing.T) {
	// A cascade would be quieter and would stop scheduled work with no
	// acknowledgement that it was about to happen.
	svc, projects, jobs := newTestProjectService()
	ctx := context.Background()

	project, _, err := svc.Create(ctx, ProjectInput{Name: "Demo", Active: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	jobs.addProject(project.ID, project.Slug, "")
	if _, err := (memJobRepo{memStore: jobs}).Create(ctx, &domain.Job{
		ProjectID: project.ID, Code: "busy", Name: "Busy", Method: "GET",
		URL: "https://example.com/x", Active: true,
	}); err != nil {
		t.Fatal(err)
	}

	if err := svc.Delete(ctx, allProjects, project.ID, false); !errors.Is(err, ErrProjectLocked) {
		t.Errorf("delete gave %v, want ErrProjectLocked", err)
	}
	if err := svc.Delete(ctx, allProjects, project.ID, true); err != nil {
		t.Fatalf("a confirmed delete failed: %v", err)
	}
	if _, err := projects.GetByID(ctx, project.ID); !errors.Is(err, repository.ErrNotFound) {
		t.Error("the project survived a confirmed delete")
	}
}

func TestNewAPIKeyIsUnique(t *testing.T) {
	// A key from a predictable generator is not a key.
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		key, prefix, hash := newAPIKey()
		if seen[key] {
			t.Fatal("newAPIKey repeated a key")
		}
		seen[key] = true
		if prefix != key[:keyPrefixLen] {
			t.Fatalf("prefix %q does not match key %q", prefix, key)
		}
		sum := sha256.Sum256([]byte(key))
		if hash != hex.EncodeToString(sum[:]) {
			t.Fatal("the hash does not match the key")
		}
	}
}
