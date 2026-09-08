package authz

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/mstgnz/cronsole/v2/internal/domain"
	"github.com/mstgnz/grantz"
)

// Errors callers map to a status code.
var (
	// ErrDenied is the answer to "may this user do this". It deliberately does
	// not say which permission was missing to the caller's caller: a 403 that
	// enumerates permissions is a map of the system.
	ErrDenied = errors.New("authz: not allowed")
	// ErrNoUser separates an unauthenticated request from a forbidden one, so
	// the transport can answer 401 rather than 403.
	ErrNoUser = errors.New("authz: no user")
)

// Service answers permission questions.
//
// It wraps grantz rather than re-implementing it: the library folds the grants
// and applies deny precedence, and this decides what a scope means and refuses
// the combinations that would be an escalation.
type Service struct {
	authz *grantz.Authorizer
	store Store
}

// Config wires the service.
type Config struct {
	// Store is the write side and the membership queries.
	Store Store
	// Grants is grantz's read side, sqlstore over the same database.
	Grants grantz.Store
	// CacheTTL bounds how long a grant change takes to reach a running
	// instance. Short, because the cache is per process: revoking somebody's
	// access on one replica does not reach the others any faster than this.
	CacheTTL time.Duration
}

// New builds the service.
func New(cfg Config) (*Service, error) {
	if cfg.Store == nil {
		return nil, errors.New("authz: Store is required")
	}
	if cfg.Grants == nil {
		return nil, errors.New("authz: Grants is required")
	}
	if cfg.CacheTTL <= 0 {
		cfg.CacheTTL = 30 * time.Second
	}

	authorizer, err := grantz.New(grantz.Config{
		Store:    cfg.Grants,
		CacheTTL: cfg.CacheTTL,
		UserID:   userIDFromContext,
		// The break-glass path. An administrator flag on the account rather
		// than a role, so it cannot be revoked by editing role mappings and
		// cannot silently miss a permission added in a later release.
		//
		// grantz returns no scopes for a superuser, which this package reads as
		// every project.
		Superuser: func(ctx context.Context, userID int64) bool {
			user := UserFrom(ctx)
			return user != nil && user.ID == userID && user.IsAdmin
		},
	})
	if err != nil {
		return nil, err
	}
	return &Service{authz: authorizer, store: cfg.Store}, nil
}

// Sync writes the permission catalogue and reconciles the built-in roles.
//
// Orphans are reported, never deleted: rolling back to an older binary would
// otherwise cascade away role mappings an administrator configured.
func (s *Service) Sync(ctx context.Context) (orphans []string, err error) {
	orphans, err = s.authz.Sync(ctx, Catalog)
	if err != nil {
		return nil, err
	}
	if err := Reconcile(ctx, s.store); err != nil {
		return orphans, err
	}
	return orphans, nil
}

// Can reports whether a user holds a permission anywhere.
//
// "Anywhere" is the important word: this answers the verb, not the row. A
// caller that acts on a project must follow it with RequireProject, or use
// Scope and filter.
func (s *Service) Can(ctx context.Context, user *domain.User, key string) (bool, error) {
	if user == nil {
		return false, ErrNoUser
	}
	allowed, err := s.authz.Can(ctx, user.ID, key)
	if err != nil {
		// A store error resolves to denied. A database blip must not open
		// every endpoint at once.
		return false, err
	}
	return allowed, nil
}

// Require returns ErrDenied when the user does not hold the permission.
func (s *Service) Require(ctx context.Context, user *domain.User, key string) error {
	allowed, err := s.Can(ctx, user, key)
	if err != nil {
		return err
	}
	if !allowed {
		return fmt.Errorf("%w: %s", ErrDenied, key)
	}
	return nil
}

// Scope resolves which projects a permission reaches for this user.
//
// This is the function the service layer actually needs. A list query puts it
// on its filter and the repository binds it into the WHERE clause; a single row
// check calls Allows on it.
//
// A user who does not hold the permission gets an empty scope rather than an
// error, because the caller almost always wants an empty list rather than a
// failure. Anything acting on a row must still use RequireProject.
func (s *Service) Scope(ctx context.Context, user *domain.User, key string) (domain.ProjectScope, error) {
	if user == nil {
		return domain.NoProjects(), ErrNoUser
	}

	decision, err := s.authz.Decide(ctx, user.ID, key)
	if err != nil {
		return domain.NoProjects(), err
	}
	if !decision.Allowed {
		// Not an error. A caller listing jobs for somebody who may not read
		// jobs wants an empty list, not a failure, and an empty scope produces
		// exactly that.
		return domain.NoProjects(), nil
	}
	return scopeFromGrants(decision.Scopes)
}

// RequireProject is the check that middleware cannot do.
//
// Middleware runs before the row is read, so it can only answer "may this user
// update jobs at all". This answers "...this one", and it is what stands
// between a reader on one project and another project's jobs.
func (s *Service) RequireProject(ctx context.Context, user *domain.User, key string, projectID int64) error {
	scope, err := s.Scope(ctx, user, key)
	if err != nil {
		return err
	}
	if !scope.Allows(projectID) {
		// The same error whether the verb is missing or the project is out of
		// scope. Telling them apart would confirm the project exists.
		return fmt.Errorf("%w: %s", ErrDenied, key)
	}
	return nil
}

// Fields returns the field allow-list for a permission, or nil for no
// restriction.
//
// Used by the run list: a reader who may see that a job failed does not
// necessarily get to see the response body it failed with, and whoever granted
// the access decides which.
func (s *Service) Fields(ctx context.Context, user *domain.User, key string) ([]string, error) {
	if user == nil {
		return nil, ErrNoUser
	}
	fields, err := s.authz.Fields(ctx, user.ID, key)
	if err != nil {
		if errors.Is(err, grantz.ErrDenied) {
			return nil, fmt.Errorf("%w: %s", ErrDenied, key)
		}
		return nil, err
	}
	return fields, nil
}

// RunView says which parts of a run this user may see.
//
// nil fields from grantz means unrestricted, which is not the same as an empty
// list. Collapsing the two here rather than at every call site is the point:
// the interface asks "may I show the output" and gets a boolean.
type RunView struct {
	Output     bool
	Error      bool
	RequestURL bool
}

// FullRunView is what an unrestricted grant sees.
func FullRunView() RunView { return RunView{Output: true, Error: true, RequestURL: true} }

// RunViewOf reads a stored allow-list into a view.
func RunViewOf(fields []string) RunView {
	view := RunView{}
	for _, f := range fields {
		switch f {
		case RunFieldOutput:
			view.Output = true
		case RunFieldError:
			view.Error = true
		case RunFieldRequest:
			view.RequestURL = true
		}
	}
	return view
}

// Fields is the allow-list this view stores as. The order is fixed so a
// restriction that did not change does not rewrite the row.
func (v RunView) Fields() []string {
	fields := []string{}
	if v.Output {
		fields = append(fields, RunFieldOutput)
	}
	if v.Error {
		fields = append(fields, RunFieldError)
	}
	if v.RequestURL {
		fields = append(fields, RunFieldRequest)
	}
	return fields
}

// RunView resolves the field restriction on runs.read.
func (s *Service) RunView(ctx context.Context, user *domain.User) (RunView, error) {
	fields, err := s.Fields(ctx, user, RunsRead)
	if err != nil {
		if errors.Is(err, ErrDenied) {
			return RunView{}, nil
		}
		return RunView{}, err
	}
	if fields == nil {
		return FullRunView(), nil
	}
	return RunViewOf(fields), nil
}

// Permissions lists every key the user holds, for the interface to draw itself
// from.
//
// A convenience, never an authority: every endpoint still checks. Because both
// sides read the same grants, a hidden button and a refusing endpoint cannot
// drift apart.
func (s *Service) Permissions(ctx context.Context, user *domain.User) (map[string]bool, error) {
	if user == nil {
		return map[string]bool{}, nil
	}
	keys, err := s.authz.UserPermissions(ctx, user.ID)
	if err != nil {
		return nil, err
	}

	out := make(map[string]bool, len(keys))
	for _, key := range keys {
		out[key] = true
	}
	// The superuser hook bypasses the store, so the list would come back empty
	// for the one account that can do everything.
	if user.IsAdmin {
		for _, p := range Catalog {
			out[p.Key] = true
		}
	}
	return out, nil
}

// Invalidate drops a user's cached grants. Called after their access changes,
// so the person doing the granting sees it take effect.
//
// It reaches this process only. The cache TTL is what bounds the delay on the
// other replicas, which is why it is short.
func (s *Service) Invalidate(ctx context.Context, userID int64) {
	s.authz.Invalidate(ctx, userID)
}

// Store exposes the membership store for the handlers that manage access.
func (s *Service) Store() Store { return s.store }
