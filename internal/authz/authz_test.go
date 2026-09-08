package authz

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mstgnz/cronsole/v2/internal/domain"
	"github.com/mstgnz/cronsole/v2/internal/httpx"
	"github.com/mstgnz/grantz"
)

// --- a grant store held in memory -------------------------------------------

type memGrants struct {
	// grants is what LoadUserGrants returns, per user.
	grants map[int64][]grantz.Grant
	// synced records the catalogue Sync was given.
	synced []grantz.Permission
	// orphans is what SyncPermissions reports back.
	orphans []string

	loadErr error
	syncErr error
	// loads counts reads, so the cache can be observed.
	loads int
}

func newMemGrants() *memGrants {
	return &memGrants{grants: map[int64][]grantz.Grant{}}
}

func (m *memGrants) LoadUserGrants(_ context.Context, userID int64) ([]grantz.Grant, error) {
	m.loads++
	if m.loadErr != nil {
		return nil, m.loadErr
	}
	return m.grants[userID], nil
}

func (m *memGrants) SyncPermissions(_ context.Context, perms []grantz.Permission) ([]string, error) {
	if m.syncErr != nil {
		return nil, m.syncErr
	}
	m.synced = perms
	return m.orphans, nil
}

// --- a role store held in memory --------------------------------------------

type memRoleStore struct {
	Store // embedded: a method these tests do not expect panics rather than lying

	roles       map[string]int64
	permissions map[int64][]string
	nextID      int64
	ensureErr   error
	replaceErr  error
}

func newMemRoleStore() *memRoleStore {
	return &memRoleStore{roles: map[string]int64{}, permissions: map[int64][]string{}, nextID: 1}
}

func (m *memRoleStore) EnsureRole(_ context.Context, role Role) (int64, error) {
	if m.ensureErr != nil {
		return 0, m.ensureErr
	}
	if id, ok := m.roles[role.Key]; ok {
		return id, nil
	}
	id := m.nextID
	m.nextID++
	m.roles[role.Key] = id
	return id, nil
}

func (m *memRoleStore) ReplaceRolePermissions(_ context.Context, roleID int64, keys []string) error {
	if m.replaceErr != nil {
		return m.replaceErr
	}
	m.permissions[roleID] = append([]string(nil), keys...)
	return nil
}

// --- helpers ----------------------------------------------------------------

func newService(t *testing.T, grants *memGrants, roles Store) *Service {
	t.Helper()

	s, err := New(Config{Store: roles, Grants: grants, CacheTTL: time.Millisecond})
	if err != nil {
		t.Fatalf("New = %v", err)
	}
	return s
}

// withUser is what the superuser hook reads. The hook compares the id on the
// context with the one being asked about, so a request cannot claim to be
// somebody else's administrator.
func withUser(user *domain.User) context.Context {
	return httpx.WithUser(context.Background(), user)
}

func allow(key string, scope map[string]any) grantz.Grant {
	return grantz.Grant{Key: key, Effect: grantz.EffectAllow, Scope: scope, FromRole: true}
}

func operator(id int64) *domain.User { return &domain.User{ID: id, Active: true} }
func admin(id int64) *domain.User    { return &domain.User{ID: id, Active: true, IsAdmin: true} }

// --- construction -----------------------------------------------------------

func TestNewRefusesAnIncompleteConfiguration(t *testing.T) {
	// Both halves are required and neither has a safe default: without a store
	// there is nothing to write roles to, and without grants every question
	// would answer denied, which looks like a permissions problem rather than a
	// wiring one.
	if _, err := New(Config{Grants: newMemGrants()}); err == nil {
		t.Error("a configuration with no Store was accepted")
	}
	if _, err := New(Config{Store: newMemRoleStore()}); err == nil {
		t.Error("a configuration with no Grants was accepted")
	}
}

func TestCacheTTLDefaults(t *testing.T) {
	// Zero would mean no cache at all, so every permission question becomes a
	// query, on a page that asks several.
	for _, ttl := range []time.Duration{0, -time.Second} {
		if _, err := New(Config{Store: newMemRoleStore(), Grants: newMemGrants(), CacheTTL: ttl}); err != nil {
			t.Errorf("New with CacheTTL %v = %v", ttl, err)
		}
	}
}

// --- Sync -------------------------------------------------------------------

func TestSyncWritesTheCatalogueAndTheBuiltinRoles(t *testing.T) {
	// Upgrading the binary upgrades what a role can do, which only holds if
	// both halves run at boot.
	grants := newMemGrants()
	grants.orphans = []string{"jobs.retired"}
	roles := newMemRoleStore()

	orphans, err := newService(t, grants, roles).Sync(context.Background())
	if err != nil {
		t.Fatalf("Sync = %v", err)
	}

	if len(grants.synced) != len(Catalog) {
		t.Errorf("%d permissions were synced, want the whole catalogue (%d)", len(grants.synced), len(Catalog))
	}
	// Reported, never deleted: rolling back to an older binary would otherwise
	// cascade away role mappings an administrator configured.
	if len(orphans) != 1 || orphans[0] != "jobs.retired" {
		t.Errorf("orphans = %v, want them reported back", orphans)
	}
	for _, role := range BuiltinRoles {
		id, ok := roles.roles[role.Key]
		if !ok {
			t.Fatalf("the built-in role %q was not written", role.Key)
		}
		if len(roles.permissions[id]) != len(role.Permissions) {
			t.Errorf("role %q holds %d permissions, want %d",
				role.Key, len(roles.permissions[id]), len(role.Permissions))
		}
	}
}

func TestSyncReportsAFailureRatherThanCarryingOn(t *testing.T) {
	// Boot fails on it. A service that starts with a half-written catalogue
	// denies things nobody can explain.
	grants := newMemGrants()
	grants.syncErr = errors.New("connection refused")
	if _, err := newService(t, grants, newMemRoleStore()).Sync(context.Background()); err == nil {
		t.Error("a failed catalogue sync was reported as success")
	}

	roles := newMemRoleStore()
	roles.ensureErr = errors.New("connection refused")
	if _, err := newService(t, newMemGrants(), roles).Sync(context.Background()); err == nil {
		t.Error("a failed role write was reported as success")
	}

	roles = newMemRoleStore()
	roles.replaceErr = errors.New("connection refused")
	if _, err := newService(t, newMemGrants(), roles).Sync(context.Background()); err == nil {
		t.Error("a failed permission write was reported as success")
	}
}

func TestReconcileNamesTheRoleThatFailed(t *testing.T) {
	roles := newMemRoleStore()
	roles.ensureErr = errors.New("connection refused")

	err := Reconcile(context.Background(), roles)
	if err == nil {
		t.Fatal("Reconcile = nil")
	}
	if !strings.Contains(err.Error(), BuiltinRoles[0].Key) {
		t.Errorf("err = %v, want it to name the role", err)
	}
}

// --- Can and Require --------------------------------------------------------

func TestCanAndRequireNeedAUser(t *testing.T) {
	// A distinct error, so the transport answers 401 rather than 403. The two
	// are different instructions to whoever is reading them.
	s := newService(t, newMemGrants(), newMemRoleStore())

	if _, err := s.Can(context.Background(), nil, JobsRead); !errors.Is(err, ErrNoUser) {
		t.Errorf("Can with no user = %v, want ErrNoUser", err)
	}
	if err := s.Require(context.Background(), nil, JobsRead); !errors.Is(err, ErrNoUser) {
		t.Errorf("Require with no user = %v, want ErrNoUser", err)
	}
	if _, err := s.Scope(context.Background(), nil, JobsRead); !errors.Is(err, ErrNoUser) {
		t.Errorf("Scope with no user = %v, want ErrNoUser", err)
	}
	if _, err := s.Fields(context.Background(), nil, RunsRead); !errors.Is(err, ErrNoUser) {
		t.Errorf("Fields with no user = %v, want ErrNoUser", err)
	}
}

func TestCanAnswersTheVerb(t *testing.T) {
	grants := newMemGrants()
	grants.grants[1] = []grantz.Grant{allow(JobsRead, nil)}
	s := newService(t, grants, newMemRoleStore())

	ctx := withUser(operator(1))
	if ok, err := s.Can(ctx, operator(1), JobsRead); err != nil || !ok {
		t.Errorf("Can(jobs.read) = %v, %v", ok, err)
	}
	// Absence of a grant is a denial, never a default-open.
	if ok, err := s.Can(ctx, operator(1), JobsDelete); err != nil || ok {
		t.Errorf("Can(jobs.delete) = %v, %v; want denied", ok, err)
	}
}

func TestRequireDoesNotNameTheMissingPermissionToTheCaller(t *testing.T) {
	// The error wraps ErrDenied, which is what a handler branches on. The key
	// is in the message for the log; what must not happen is a 403 body that
	// enumerates permissions, and that is the handler's job to keep generic.
	s := newService(t, newMemGrants(), newMemRoleStore())

	err := s.Require(withUser(operator(1)), operator(1), JobsDelete)
	if !errors.Is(err, ErrDenied) {
		t.Errorf("Require = %v, want ErrDenied", err)
	}
}

func TestAStoreFailureDeniesRatherThanAllows(t *testing.T) {
	// A database blip must not open every endpoint at once.
	grants := newMemGrants()
	grants.loadErr = errors.New("connection refused")
	s := newService(t, grants, newMemRoleStore())

	ctx := withUser(operator(1))
	if ok, err := s.Can(ctx, operator(1), JobsRead); err == nil || ok {
		t.Errorf("Can = %v, %v; a store failure must not allow", ok, err)
	}
	if err := s.Require(ctx, operator(1), JobsRead); err == nil {
		t.Error("Require allowed through a store failure")
	}
	scope, err := s.Scope(ctx, operator(1), JobsRead)
	if err == nil {
		t.Error("Scope allowed through a store failure")
	}
	// And the scope it hands back reaches nothing, so a caller that ignores the
	// error still cannot read anything.
	if !scope.Empty() {
		t.Error("a failed Scope came back reaching something")
	}
	if err := s.RequireProject(ctx, operator(1), JobsRead, 1); err == nil {
		t.Error("RequireProject allowed through a store failure")
	}
}

// --- Scope ------------------------------------------------------------------

func TestScopeIsEmptyRatherThanAnErrorWhenTheVerbIsMissing(t *testing.T) {
	// A caller listing jobs for somebody who may not read jobs wants an empty
	// list, not a failure: the screens are shared.
	s := newService(t, newMemGrants(), newMemRoleStore())

	scope, err := s.Scope(withUser(operator(1)), operator(1), JobsRead)
	if err != nil {
		t.Fatalf("Scope = %v", err)
	}
	if !scope.Empty() {
		t.Error("a missing permission produced a scope that reaches something")
	}
}

func TestScopeNarrowsToTheGrantedProjects(t *testing.T) {
	grants := newMemGrants()
	grants.grants[1] = []grantz.Grant{
		allow(JobsRead, map[string]any{ScopeKey: []any{float64(2), float64(5)}}),
	}
	s := newService(t, grants, newMemRoleStore())

	scope, err := s.Scope(withUser(operator(1)), operator(1), JobsRead)
	if err != nil {
		t.Fatalf("Scope = %v", err)
	}
	if !scope.Allows(2) || !scope.Allows(5) {
		t.Error("a granted project is out of scope")
	}
	if scope.Allows(3) {
		t.Error("a project that was never granted is in scope")
	}
}

func TestAnUnscopedGrantReachesEveryProject(t *testing.T) {
	grants := newMemGrants()
	grants.grants[1] = []grantz.Grant{allow(JobsRead, nil)}
	s := newService(t, grants, newMemRoleStore())

	scope, err := s.Scope(withUser(operator(1)), operator(1), JobsRead)
	if err != nil {
		t.Fatalf("Scope = %v", err)
	}
	if scope.Empty() || !scope.Allows(999) {
		t.Error("an unscoped grant did not reach every project")
	}
}

func TestRequireProject(t *testing.T) {
	// The check middleware cannot do: middleware runs before the row is read,
	// so it can only answer "may this user update jobs at all".
	grants := newMemGrants()
	grants.grants[1] = []grantz.Grant{
		allow(JobsUpdate, map[string]any{ScopeKey: []any{float64(2)}}),
	}
	s := newService(t, grants, newMemRoleStore())
	ctx := withUser(operator(1))

	if err := s.RequireProject(ctx, operator(1), JobsUpdate, 2); err != nil {
		t.Errorf("the granted project was refused: %v", err)
	}
	err := s.RequireProject(ctx, operator(1), JobsUpdate, 3)
	if !errors.Is(err, ErrDenied) {
		t.Errorf("another project = %v, want ErrDenied", err)
	}
	// The same error whether the verb is missing or the project is out of
	// scope. Telling them apart confirms the project exists.
	missing := s.RequireProject(ctx, operator(1), JobsDelete, 2)
	if !errors.Is(missing, ErrDenied) {
		t.Errorf("a missing verb = %v, want ErrDenied", missing)
	}
}

// --- the administrator flag -------------------------------------------------

func TestAnAdministratorReachesEverythingWithoutARole(t *testing.T) {
	// The break-glass path: a flag on the account rather than a role, so it
	// cannot be revoked by editing role mappings and cannot silently miss a
	// permission added in a later release.
	s := newService(t, newMemGrants(), newMemRoleStore()) // no grants at all
	user := admin(1)
	ctx := withUser(user)

	for _, key := range []string{JobsRead, JobsDelete, UsersManage, SettingsRead} {
		if ok, err := s.Can(ctx, user, key); err != nil || !ok {
			t.Errorf("an administrator was refused %s: %v, %v", key, ok, err)
		}
	}

	scope, err := s.Scope(ctx, user, JobsRead)
	if err != nil {
		t.Fatalf("Scope = %v", err)
	}
	// grantz returns no scopes for a superuser, which this package reads as
	// every project.
	if scope.Empty() || !scope.Allows(12345) {
		t.Error("an administrator's scope does not reach every project")
	}
}

func TestTheSuperuserHookCannotBeClaimedForSomebodyElse(t *testing.T) {
	// The hook compares the id on the context with the id being asked about. If
	// it did not, a request carrying an administrator on its context would make
	// every other user an administrator for the length of that request.
	s := newService(t, newMemGrants(), newMemRoleStore())

	// An administrator is on the context, but the question is about user 2.
	ctx := withUser(admin(1))
	if ok, err := s.Can(ctx, operator(2), JobsDelete); err != nil || ok {
		t.Errorf("Can for another user = %v, %v; want denied", ok, err)
	}
}

func TestPermissionsListsEverythingForAnAdministrator(t *testing.T) {
	// The superuser hook bypasses the store, so the list would come back empty
	// for the one account that can do everything, and the interface would draw
	// itself with every control hidden.
	s := newService(t, newMemGrants(), newMemRoleStore())
	user := admin(1)

	perms, err := s.Permissions(withUser(user), user)
	if err != nil {
		t.Fatalf("Permissions = %v", err)
	}
	for _, p := range Catalog {
		if !perms[p.Key] {
			t.Errorf("an administrator is missing %s", p.Key)
		}
	}
}

func TestPermissionsForAnOrdinaryOperator(t *testing.T) {
	grants := newMemGrants()
	grants.grants[1] = []grantz.Grant{allow(JobsRead, nil), allow(RunsRead, nil)}
	s := newService(t, grants, newMemRoleStore())
	user := operator(1)

	perms, err := s.Permissions(withUser(user), user)
	if err != nil {
		t.Fatalf("Permissions = %v", err)
	}
	if !perms[JobsRead] || !perms[RunsRead] {
		t.Errorf("perms = %v, want the granted keys", perms)
	}
	if perms[JobsDelete] || perms[UsersManage] {
		t.Errorf("perms = %v, want nothing beyond what was granted", perms)
	}
}

func TestPermissionsWithNoUser(t *testing.T) {
	// The login screen draws the same layout. An empty map rather than an error
	// keeps that from being a special case.
	s := newService(t, newMemGrants(), newMemRoleStore())

	perms, err := s.Permissions(context.Background(), nil)
	if err != nil {
		t.Fatalf("Permissions = %v", err)
	}
	if len(perms) != 0 {
		t.Errorf("perms = %v, want none", perms)
	}
}

func TestPermissionsReportsAStoreFailure(t *testing.T) {
	// Unlike Scope, this one errors: the caller is middleware that renders a
	// page offering nothing extra, and it needs to know it is doing that.
	grants := newMemGrants()
	grants.loadErr = errors.New("connection refused")
	s := newService(t, grants, newMemRoleStore())

	if _, err := s.Permissions(withUser(operator(1)), operator(1)); err == nil {
		t.Error("a store failure was reported as an empty permission set")
	}
}

// --- field restrictions -----------------------------------------------------

func TestRunViewIsUnrestrictedWithoutAFieldList(t *testing.T) {
	// nil fields from grantz means unrestricted, which is NOT the same as an
	// empty list. Collapsing the two anywhere but here would mean a reader with
	// full access seeing nothing.
	grants := newMemGrants()
	grants.grants[1] = []grantz.Grant{allow(RunsRead, nil)}
	s := newService(t, grants, newMemRoleStore())

	view, err := s.RunView(withUser(operator(1)), operator(1))
	if err != nil {
		t.Fatalf("RunView = %v", err)
	}
	if view != FullRunView() {
		t.Errorf("view = %+v, want everything", view)
	}
}

func TestRunViewHonoursAFieldList(t *testing.T) {
	// A reader who may know that a job failed does not necessarily get the
	// response body it failed with, which often carries customer data.
	grants := newMemGrants()
	grants.grants[1] = []grantz.Grant{{
		Key: RunsRead, Effect: grantz.EffectAllow, FromRole: true,
		Fields: []string{RunFieldError},
	}}
	s := newService(t, grants, newMemRoleStore())

	view, err := s.RunView(withUser(operator(1)), operator(1))
	if err != nil {
		t.Fatalf("RunView = %v", err)
	}
	if !view.Error {
		t.Error("the granted field is hidden")
	}
	if view.Output || view.RequestURL {
		t.Errorf("view = %+v, want only the error text", view)
	}
}

func TestRunViewOfSomebodyWhoMayNotReadRunsAtAll(t *testing.T) {
	// Denied resolves to a view showing nothing, rather than to an error: the
	// run list renders empty and the columns are simply absent.
	s := newService(t, newMemGrants(), newMemRoleStore())

	view, err := s.RunView(withUser(operator(1)), operator(1))
	if err != nil {
		t.Fatalf("RunView = %v", err)
	}
	if view != (RunView{}) {
		t.Errorf("view = %+v, want nothing visible", view)
	}
}

func TestRunViewReportsAStoreFailure(t *testing.T) {
	grants := newMemGrants()
	grants.loadErr = errors.New("connection refused")
	s := newService(t, grants, newMemRoleStore())

	if _, err := s.RunView(withUser(operator(1)), operator(1)); err == nil {
		t.Error("a store failure was reported as a view")
	}
}

func TestFieldsMapsDeniedToOurOwnError(t *testing.T) {
	// So a handler branches on ErrDenied rather than on a library's sentinel.
	s := newService(t, newMemGrants(), newMemRoleStore())

	_, err := s.Fields(withUser(operator(1)), operator(1), RunsRead)
	if !errors.Is(err, ErrDenied) {
		t.Errorf("Fields = %v, want ErrDenied", err)
	}
}

// --- the cache --------------------------------------------------------------

func TestGrantsAreCachedAndInvalidated(t *testing.T) {
	// The cache is what keeps a page that asks several permission questions
	// from being several queries. Invalidate is what makes the person doing the
	// granting see it take effect.
	grants := newMemGrants()
	grants.grants[1] = []grantz.Grant{allow(JobsRead, nil)}
	s, err := New(Config{Store: newMemRoleStore(), Grants: grants, CacheTTL: time.Hour})
	if err != nil {
		t.Fatalf("New = %v", err)
	}

	user := operator(1)
	ctx := withUser(user)
	for i := 0; i < 5; i++ {
		if _, err := s.Can(ctx, user, JobsRead); err != nil {
			t.Fatalf("Can = %v", err)
		}
	}
	if grants.loads != 1 {
		t.Errorf("the store was read %d times for five questions", grants.loads)
	}

	// A grant taken away is still cached until it is invalidated, which is why
	// the handler that revokes calls it.
	grants.grants[1] = nil
	if ok, _ := s.Can(ctx, user, JobsRead); !ok {
		t.Error("the cache was not used")
	}

	s.Invalidate(ctx, 1)
	if ok, _ := s.Can(ctx, user, JobsRead); ok {
		t.Error("Invalidate did not drop the cached grants")
	}
}

// --- context ----------------------------------------------------------------

func TestUserFrom(t *testing.T) {
	if UserFrom(context.Background()) != nil {
		t.Error("an empty context produced a user")
	}
	user := operator(7)
	if got := UserFrom(withUser(user)); got == nil || got.ID != 7 {
		t.Errorf("UserFrom = %+v", got)
	}
}

// --- roles ------------------------------------------------------------------

func TestIsBuiltin(t *testing.T) {
	// The three shipped roles are rewritten from code at every boot, so the
	// interface must not offer to edit them.
	for _, role := range BuiltinRoles {
		if !IsBuiltin(role.Key) {
			t.Errorf("%q is not recognised as built in", role.Key)
		}
	}
	for _, key := range []string{"", "custom", "READER", "reader "} {
		if IsBuiltin(key) {
			t.Errorf("%q was treated as built in", key)
		}
	}
}

func TestTheBuiltinRolesAreOrderedAndDistinct(t *testing.T) {
	// A reader must not hold anything a writer does not, or the names stop
	// meaning what they say on the membership screen.
	byKey := map[string]map[string]bool{}
	for _, role := range BuiltinRoles {
		set := map[string]bool{}
		for _, key := range role.Permissions {
			if set[key] {
				t.Errorf("role %q lists %q twice", role.Key, key)
			}
			set[key] = true
		}
		byKey[role.Key] = set
	}

	if len(BuiltinRoles) < 2 {
		t.Fatal("there are fewer built-in roles than the ordering test assumes")
	}
	for i := 1; i < len(BuiltinRoles); i++ {
		lower, higher := BuiltinRoles[i-1], BuiltinRoles[i]
		for key := range byKey[lower.Key] {
			if !byKey[higher.Key][key] {
				t.Errorf("%q holds %q and %q does not, so the roles do not nest",
					lower.Key, key, higher.Key)
			}
		}
	}
}

func TestEveryBuiltinPermissionIsInTheCatalogue(t *testing.T) {
	// A role naming a key the catalogue does not declare fails the foreign key
	// at boot, on a deployment rather than in a test.
	known := map[string]bool{}
	for _, p := range Catalog {
		known[p.Key] = true
	}
	for _, role := range BuiltinRoles {
		for _, key := range role.Permissions {
			if !known[key] {
				t.Errorf("role %q names %q, which is not in the catalogue", role.Key, key)
			}
		}
	}
}

func TestAProjectRoleNeverCarriesAGlobalPermission(t *testing.T) {
	// A project scope has no meaning for these, so offering one on a project
	// role would look like it worked and grant nothing.
	for _, role := range BuiltinRoles {
		for _, key := range role.Permissions {
			if Global[key] {
				t.Errorf("the project role %q holds the global permission %q", role.Key, key)
			}
		}
	}
}

func TestStoreIsExposed(t *testing.T) {
	roles := newMemRoleStore()
	if got := newService(t, newMemGrants(), roles).Store(); got != Store(roles) {
		t.Error("Store did not return the store it was built with")
	}
}
