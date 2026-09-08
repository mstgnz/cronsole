package service

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/mstgnz/cronsole/v2/internal/authz"
	"github.com/mstgnz/cronsole/v2/internal/domain"
	"github.com/mstgnz/cronsole/v2/internal/repository"
)

// fakeAuthorizer answers project checks from a fixed map and hands out a
// memAuthzStore. It does not consult grantz: what is under test here is the
// escalation rules, and a fake that resolved them through the real authorizer
// would be testing grantz instead.
type fakeAuthorizer struct {
	store *memAuthzStore
	// allowed is the set of "<userID>:<permission>:<projectID>" the caller
	// holds. A platform administrator is allowed everything.
	allowed     map[string]bool
	invalidated []int64
}

func (f *fakeAuthorizer) RequireProject(_ context.Context, user *domain.User, key string, projectID int64) error {
	if user == nil {
		return authz.ErrNoUser
	}
	if user.IsAdmin {
		return nil
	}
	if f.allowed[permKey(user.ID, key, projectID)] {
		return nil
	}
	return authz.ErrDenied
}

func (f *fakeAuthorizer) Store() authz.Store { return f.store }

func (f *fakeAuthorizer) Invalidate(_ context.Context, userID int64) {
	f.invalidated = append(f.invalidated, userID)
}

func permKey(userID int64, key string, projectID int64) string {
	return strconv.FormatInt(userID, 10) + ":" + key + ":" + strconv.FormatInt(projectID, 10)
}

// memAuthzStore is an in-memory authz.Store.
type memAuthzStore struct {
	roles   map[string]int64
	names   map[int64]authz.StoredRole
	members map[int64][]authz.Member     // projectID -> members
	grants  map[int64][]authz.Assignment // userID -> assignments
	fields  map[string][]string          // "<roleID>:<permission>" -> allow-list
}

func newAuthzStore() *memAuthzStore {
	s := &memAuthzStore{
		roles:   map[string]int64{},
		names:   map[int64]authz.StoredRole{},
		members: map[int64][]authz.Member{},
		grants:  map[int64][]authz.Assignment{},
		fields:  map[string][]string{},
	}
	for i, role := range authz.BuiltinRoles {
		id := int64(i + 1)
		s.roles[role.Key] = id
		s.names[id] = authz.StoredRole{
			ID: id, Key: role.Key, Name: role.Name,
			Description: role.Description, Active: true, Builtin: true,
		}
	}
	return s
}

func (s *memAuthzStore) EnsureRole(context.Context, authz.Role) (int64, error) { return 0, nil }
func (s *memAuthzStore) ReplaceRolePermissions(context.Context, int64, []string) error {
	return nil
}

func (s *memAuthzStore) RoleIDByKey(_ context.Context, key string) (int64, error) {
	if id, ok := s.roles[key]; ok {
		return id, nil
	}
	return 0, ErrNotFound
}

func (s *memAuthzStore) ListRoles(context.Context) ([]authz.StoredRole, error) {
	out := make([]authz.StoredRole, 0, len(s.names))
	for _, role := range s.names {
		out = append(out, role)
	}
	return out, nil
}

func (s *memAuthzStore) ListProjectMembers(_ context.Context, projectID int64) ([]authz.Member, error) {
	return s.members[projectID], nil
}

func (s *memAuthzStore) ListUserRoles(_ context.Context, userID int64) ([]authz.Assignment, error) {
	return s.grants[userID], nil
}

func (s *memAuthzStore) GrantProject(_ context.Context, userID, roleID, projectID int64) error {
	role := s.names[roleID]
	s.members[projectID] = append(s.members[projectID], authz.Member{
		UserID: userID, RoleID: roleID, RoleKey: role.Key, RoleName: role.Name, Active: true,
	})
	s.grants[userID] = append(s.grants[userID], authz.Assignment{
		RoleID: roleID, RoleKey: role.Key, RoleName: role.Name, ProjectIDs: []int64{projectID},
	})
	return nil
}

func (s *memAuthzStore) RevokeProject(_ context.Context, userID, roleID, projectID int64) error {
	kept := s.members[projectID][:0]
	for _, m := range s.members[projectID] {
		if m.UserID == userID && m.RoleID == roleID {
			continue
		}
		kept = append(kept, m)
	}
	s.members[projectID] = kept
	return nil
}

func (s *memAuthzStore) RevokeUser(_ context.Context, userID int64) error {
	delete(s.grants, userID)
	for projectID := range s.members {
		_ = s.RevokeProject(context.Background(), userID, 0, projectID)
	}
	return nil
}

func (s *memAuthzStore) SetRoleFields(_ context.Context, roleID int64, key string, fields []string) error {
	if s.fields == nil {
		s.fields = map[string][]string{}
	}
	if fields == nil {
		delete(s.fields, fieldKey(roleID, key))
		return nil
	}
	s.fields[fieldKey(roleID, key)] = fields
	return nil
}

func (s *memAuthzStore) RoleFields(_ context.Context, roleID int64, key string) ([]string, bool, error) {
	fields, ok := s.fields[fieldKey(roleID, key)]
	return fields, ok, nil
}

func fieldKey(roleID int64, permissionKey string) string {
	return strconv.FormatInt(roleID, 10) + ":" + permissionKey
}

// addMember puts somebody on a project without going through the service, so a
// test can set up a state the service itself would refuse to create.
func (s *memAuthzStore) addMember(projectID int64, m authz.Member, unscoped bool) {
	role := s.names[s.roles[m.RoleKey]]
	m.RoleID, m.RoleName = role.ID, role.Name
	s.members[projectID] = append(s.members[projectID], m)

	assignment := authz.Assignment{RoleID: role.ID, RoleKey: role.Key, RoleName: role.Name, Unscoped: unscoped}
	if !unscoped {
		assignment.ProjectIDs = []int64{projectID}
	}
	s.grants[m.UserID] = append(s.grants[m.UserID], assignment)
}

// memUsers is a minimal domain.UserRepository for the membership tests.
type memUsers struct{ rows map[int64]*domain.User }

func (m *memUsers) GetByID(_ context.Context, id int64) (*domain.User, error) {
	if u, ok := m.rows[id]; ok {
		copied := *u
		return &copied, nil
	}
	return nil, ErrNotFound
}

func (m *memUsers) GetByEmail(_ context.Context, email string) (*domain.User, error) {
	for _, u := range m.rows {
		if u.Email == email {
			copied := *u
			return &copied, nil
		}
	}
	return nil, ErrNotFound
}

func (m *memUsers) List(context.Context, string, int, int) ([]domain.User, int64, error) {
	return nil, 0, nil
}
func (m *memUsers) Count(context.Context) (int64, error)                { return 0, nil }
func (m *memUsers) Create(context.Context, *domain.User) (int64, error) { return 0, nil }
func (m *memUsers) CreateFirstAdmin(context.Context, *domain.User) (int64, error) {
	// The membership tests never reach setup, and a table with five accounts is
	// never empty.
	return 0, repository.ErrForbidden
}
func (m *memUsers) UpdateProfile(context.Context, *domain.User) error              { return nil }
func (m *memUsers) UpdatePassword(context.Context, int64, string, time.Time) error { return nil }
func (m *memUsers) InvalidateTokens(context.Context, int64, time.Time) error       { return nil }
func (m *memUsers) TouchLogin(context.Context, int64, time.Time) error             { return nil }
func (m *memUsers) SoftDelete(context.Context, int64, time.Time) error             { return nil }

// members builds the service with one project (id 1) and the people named.
func newMemberFixture(t *testing.T) (*MemberService, *fakeAuthorizer, *memAuthzStore) {
	t.Helper()

	store := newAuthzStore()
	users := &memUsers{rows: map[int64]*domain.User{
		1: {ID: 1, Fullname: "Platform admin", Email: "admin@example.com", Active: true, IsAdmin: true},
		2: {ID: 2, Fullname: "Project admin", Email: "padmin@example.com", Active: true},
		3: {ID: 3, Fullname: "Writer", Email: "writer@example.com", Active: true},
		4: {ID: 4, Fullname: "Reader", Email: "reader@example.com", Active: true},
		5: {ID: 5, Fullname: "Outsider", Email: "outsider@example.com", Active: true},
	}}

	auth := &fakeAuthorizer{store: store, allowed: map[string]bool{
		permKey(2, authz.MembersRead, 1):   true,
		permKey(2, authz.MembersManage, 1): true,
	}}

	store.addMember(1, authz.Member{UserID: 2, Fullname: "Project admin",
		Email: "padmin@example.com", Active: true, RoleKey: authz.RoleProjectAdmin}, false)
	store.addMember(1, authz.Member{UserID: 3, Fullname: "Writer",
		Email: "writer@example.com", Active: true, RoleKey: authz.RoleProjectWriter}, false)
	store.addMember(1, authz.Member{UserID: 4, Fullname: "Reader",
		Email: "reader@example.com", Active: true, RoleKey: authz.RoleProjectReader}, false)

	return NewMemberService(auth, users), auth, store
}

func user(id int64, admin bool) *domain.User {
	return &domain.User{ID: id, Active: true, IsAdmin: admin}
}

func TestGrantRefusesSelfService(t *testing.T) {
	// Refused outright rather than checked case by case. Every interesting
	// escalation starts with granting yourself something, and the legitimate
	// case is better served by another administrator.
	svc, _, _ := newMemberFixture(t)
	ctx := context.Background()

	err := svc.Grant(ctx, user(2, false), 1, 2, authz.RoleProjectAdmin)
	if !errors.Is(err, ErrSelfMembership) {
		t.Errorf("granting to yourself gave %v, want ErrSelfMembership", err)
	}
	if err := svc.Revoke(ctx, user(2, false), 1, 2, 3); !errors.Is(err, ErrSelfMembership) {
		t.Errorf("revoking your own access gave %v, want ErrSelfMembership", err)
	}
}

func TestProjectAdminCannotTargetAPlatformAdministrator(t *testing.T) {
	// Without this a project administrator could add the platform administrator
	// as a reader on their project, or remove them, and the flag would be the
	// only thing left standing.
	svc, _, store := newMemberFixture(t)
	ctx := context.Background()

	if err := svc.Grant(ctx, user(2, false), 1, 1, authz.RoleProjectReader); !errors.Is(err, ErrTargetIsAdmin) {
		t.Errorf("granting to a platform administrator gave %v, want ErrTargetIsAdmin", err)
	}

	store.addMember(1, authz.Member{UserID: 1, Fullname: "Platform admin",
		Email: "admin@example.com", Active: true, IsAdmin: true, RoleKey: authz.RoleProjectReader}, false)
	roleID := store.roles[authz.RoleProjectReader]
	if err := svc.Revoke(ctx, user(2, false), 1, 1, roleID); !errors.Is(err, ErrTargetIsAdmin) {
		t.Errorf("revoking a platform administrator gave %v, want ErrTargetIsAdmin", err)
	}
}

func TestGrantRefusesARoleAboveTheGrantersOwn(t *testing.T) {
	// The role-target guard from the other direction: a writer who somehow
	// reached this endpoint cannot hand out an administrator role, and a project
	// administrator cannot hand out a custom role at all.
	svc, auth, _ := newMemberFixture(t)
	ctx := context.Background()

	// A writer with members.manage but only writer rank.
	auth.allowed[permKey(3, authz.MembersManage, 1)] = true

	if err := svc.Grant(ctx, user(3, false), 1, 5, authz.RoleProjectAdmin); !errors.Is(err, ErrRoleTooHigh) {
		t.Errorf("a writer granting an administrator role gave %v, want ErrRoleTooHigh", err)
	}
	if err := svc.Grant(ctx, user(3, false), 1, 5, authz.RoleProjectWriter); err != nil {
		t.Errorf("a writer granting their own level failed: %v", err)
	}
	// A role nobody ships ranks zero, so only the platform administrator hands
	// one out. Otherwise somebody creates a role holding every permission and
	// grants it from inside a project.
	if err := svc.Grant(ctx, user(2, false), 1, 5, "custom_role"); !errors.Is(err, ErrRoleTooHigh) {
		t.Errorf("a project administrator granting a custom role gave %v, want ErrRoleTooHigh", err)
	}
}

func TestRevokeRefusesAMemberSeniorToTheCaller(t *testing.T) {
	// Without it a project administrator could remove somebody senior to them
	// and take the project over.
	svc, auth, store := newMemberFixture(t)
	ctx := context.Background()

	auth.allowed[permKey(3, authz.MembersRead, 1)] = true
	auth.allowed[permKey(3, authz.MembersManage, 1)] = true

	adminRole := store.roles[authz.RoleProjectAdmin]
	if err := svc.Revoke(ctx, user(3, false), 1, 2, adminRole); !errors.Is(err, ErrRoleTooHigh) {
		t.Errorf("a writer removing an administrator gave %v, want ErrRoleTooHigh", err)
	}

	// The project administrator may remove the writer below them.
	writerRole := store.roles[authz.RoleProjectWriter]
	if err := svc.Revoke(ctx, user(2, false), 1, 3, writerRole); err != nil {
		t.Errorf("an administrator removing a writer failed: %v", err)
	}
}

func TestRevokeRefusesAnUnscopedAssignment(t *testing.T) {
	// An assignment covering every project cannot be narrowed one project at a
	// time. Saying so is better than removing it entirely, which is what the
	// button would look like it was doing.
	svc, _, store := newMemberFixture(t)
	ctx := context.Background()

	store.addMember(1, authz.Member{UserID: 5, Fullname: "Outsider",
		Email: "outsider@example.com", Active: true, RoleKey: authz.RoleProjectReader}, true)

	roleID := store.roles[authz.RoleProjectReader]
	if err := svc.Revoke(ctx, user(2, false), 1, 5, roleID); !errors.Is(err, ErrUnscopedMembership) {
		t.Errorf("revoking an unscoped assignment gave %v, want ErrUnscopedMembership", err)
	}
}

func TestGrantRequiresThePermissionOnThatProject(t *testing.T) {
	// The check that middleware cannot do: holding members.manage somewhere is
	// not holding it here.
	svc, _, _ := newMemberFixture(t)
	ctx := context.Background()

	if err := svc.Grant(ctx, user(2, false), 2, 5, authz.RoleProjectReader); !errors.Is(err, authz.ErrDenied) {
		t.Errorf("granting on another project gave %v, want a refusal", err)
	}
	if err := svc.Grant(ctx, user(4, false), 1, 5, authz.RoleProjectReader); !errors.Is(err, authz.ErrDenied) {
		t.Errorf("a reader granting access gave %v, want a refusal", err)
	}
}

func TestGrantByEmailAndCacheInvalidation(t *testing.T) {
	// By address rather than by picking from a list of everybody: a project
	// administrator has no business enumerating the platform's accounts.
	svc, auth, store := newMemberFixture(t)
	ctx := context.Background()

	if err := svc.GrantByEmail(ctx, user(2, false), 1, " Outsider@Example.com ", authz.RoleProjectReader); err != nil {
		t.Fatalf("grant by email failed: %v", err)
	}

	var found bool
	for _, m := range store.members[1] {
		if m.UserID == 5 {
			found = true
		}
	}
	if !found {
		t.Error("the person was not added to the project")
	}
	// They should see the access on their next request rather than when the
	// cache happens to expire.
	if len(auth.invalidated) == 0 || auth.invalidated[len(auth.invalidated)-1] != 5 {
		t.Errorf("invalidated = %v, want the granted user", auth.invalidated)
	}

	if err := svc.GrantByEmail(ctx, user(2, false), 1, "nobody@example.com", authz.RoleProjectReader); !errors.Is(err, ErrNotFound) {
		t.Errorf("an unknown address gave %v, want ErrNotFound", err)
	}
	if err := svc.GrantByEmail(ctx, user(2, false), 1, "  ", authz.RoleProjectReader); err == nil {
		t.Error("a blank address was accepted")
	}
}

func TestListMembersMarksWhatTheCallerMayChange(t *testing.T) {
	// The interface greys out a control rather than offering an action that will
	// be refused, and it reads that from the same rule the write path enforces.
	svc, auth, _ := newMemberFixture(t)
	ctx := context.Background()

	auth.allowed[permKey(3, authz.MembersRead, 1)] = true

	members, err := svc.ListMembers(ctx, user(3, false), 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(members) != 3 {
		t.Fatalf("members = %d, want 3", len(members))
	}

	for _, m := range members {
		switch m.UserID {
		case 3:
			if m.Removable {
				t.Error("the caller was offered a control on their own access")
			}
		case 2:
			if m.Removable {
				t.Error("a writer was offered a control on an administrator")
			}
		case 4:
			if !m.Removable {
				t.Error("a writer was not offered a control on a reader")
			}
		}
	}
}

func TestRunVisibilityIsThePlatformAdministratorsAlone(t *testing.T) {
	// A role is global: narrowing "reader" here changes what every reader sees,
	// on every project. A project administrator narrowing it for their own
	// project would silently change three other brands.
	svc, _, _ := newMemberFixture(t)
	ctx := context.Background()

	if _, err := svc.RunVisibility(ctx, user(2, false)); !errors.Is(err, ErrForbidden) {
		t.Errorf("a project administrator read the role settings: %v", err)
	}
	err := svc.SetRunVisibility(ctx, user(2, false), authz.RoleProjectReader, authz.RunView{})
	if !errors.Is(err, ErrForbidden) {
		t.Errorf("a project administrator changed a role: %v", err)
	}
}

func TestRunVisibilityRoundTrip(t *testing.T) {
	svc, _, _ := newMemberFixture(t)
	ctx := context.Background()
	admin := user(1, true)

	// Unrestricted until somebody restricts it. A role that holds runs.read sees
	// all of a run by default, which is what an upgrade must not silently change.
	roles, err := svc.RunVisibility(ctx, admin)
	if err != nil {
		t.Fatal(err)
	}
	if len(roles) != len(authz.BuiltinRoles) {
		t.Fatalf("roles = %d, want the %d built-in ones", len(roles), len(authz.BuiltinRoles))
	}
	for _, role := range roles {
		if !role.View.Output || !role.View.Error || !role.View.RequestURL {
			t.Errorf("%s starts at %+v, want everything visible", role.Key, role.View)
		}
	}

	// Hide the response body from readers, keep the rest.
	narrowed := authz.RunView{Error: true, RequestURL: true}
	if err := svc.SetRunVisibility(ctx, admin, authz.RoleProjectReader, narrowed); err != nil {
		t.Fatal(err)
	}

	roles, err = svc.RunVisibility(ctx, admin)
	if err != nil {
		t.Fatal(err)
	}
	for _, role := range roles {
		switch role.Key {
		case authz.RoleProjectReader:
			if role.View != narrowed {
				t.Errorf("reader = %+v, want %+v", role.View, narrowed)
			}
		default:
			if role.View != authz.FullRunView() {
				t.Errorf("%s = %+v, want it untouched", role.Key, role.View)
			}
		}
	}

	// Turning everything back on clears the restriction rather than storing a
	// list that happens to name every field today. The two behave the same now
	// and diverge the moment a field is added.
	if err := svc.SetRunVisibility(ctx, admin, authz.RoleProjectReader, authz.FullRunView()); err != nil {
		t.Fatal(err)
	}
	roles, _ = svc.RunVisibility(ctx, admin)
	for _, role := range roles {
		if role.Key == authz.RoleProjectReader && role.View != authz.FullRunView() {
			t.Errorf("reader = %+v after clearing, want everything visible", role.View)
		}
	}

	if err := svc.SetRunVisibility(ctx, admin, "custom_role", authz.RunView{}); err == nil {
		t.Error("a role this service does not ship was accepted")
	}
}

func TestRolesOfferedAreOnlyThoseTheCallerMayGrant(t *testing.T) {
	// Offering a role the grant would then refuse is worse than not offering it:
	// the person reads the list as what they can do.
	svc, auth, store := newMemberFixture(t)
	ctx := context.Background()

	store.names[99] = authz.StoredRole{ID: 99, Key: "custom_role", Name: "Custom", Active: true}
	auth.allowed[permKey(3, authz.MembersManage, 1)] = true

	roles, err := svc.Roles(ctx, user(3, false), 1)
	if err != nil {
		t.Fatal(err)
	}
	for _, role := range roles {
		if role.Key == authz.RoleProjectAdmin {
			t.Error("a writer was offered the administrator role")
		}
		if role.Key == "custom_role" {
			t.Error("a project member was offered a custom role")
		}
	}

	adminRoles, err := svc.Roles(ctx, user(1, true), 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(adminRoles) != len(store.names) {
		t.Errorf("the platform administrator was offered %d of %d roles",
			len(adminRoles), len(store.names))
	}
}
