package service

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/mstgnz/cronsole/v2/internal/authz"
	"github.com/mstgnz/cronsole/v2/internal/domain"
)

// Membership errors.
var (
	// ErrSelfMembership is returned when somebody tries to change their own
	// access. Refused outright rather than checked case by case: every
	// interesting escalation starts with granting yourself something, and the
	// legitimate case (an administrator fixing their own access) is better
	// served by another administrator or the platform flag.
	ErrSelfMembership = errors.New("you cannot change your own access")
	// ErrRoleTooHigh is returned when the grantor's own role does not reach the
	// one being handed out.
	ErrRoleTooHigh = errors.New("you cannot grant a role above your own")
	// ErrTargetIsAdmin is returned when the target holds the platform
	// administrator flag.
	ErrTargetIsAdmin = errors.New("platform administrators are managed under Settings")
	// ErrUnscopedMembership is returned when the target's role covers every
	// project, so it cannot be changed one project at a time.
	ErrUnscopedMembership = errors.New("this role covers every project and is managed under Settings")
)

// Authorizer is what membership management needs from authorization.
//
// Narrow on purpose: this service decides who may grant what, and it needs the
// project check, the store and cache invalidation. Depending on the whole
// authorization service would let a later change here start answering
// permission questions of its own, which is exactly the drift the layer split
// exists to prevent. It also makes the escalation guards testable without a
// database.
type Authorizer interface {
	RequireProject(ctx context.Context, user *domain.User, key string, projectID int64) error
	Store() authz.Store
	Invalidate(ctx context.Context, userID int64)
}

// MemberService manages who may reach a project.
//
// The rules here are what stop a project administrator from becoming a platform
// administrator. They are in the service rather than the handler because the
// interface and the API both reach them, and because a rule in a handler is a
// rule the next route does not have.
type MemberService struct {
	authz Authorizer
	users domain.UserRepository
}

// NewMemberService wires the service.
func NewMemberService(a Authorizer, users domain.UserRepository) *MemberService {
	return &MemberService{authz: a, users: users}
}

// ProjectMember is one person's access to a project, as the screen shows it.
type ProjectMember struct {
	authz.Member
	// Removable is false for a member this caller may not change, so the
	// interface can grey the control out rather than offering an action that
	// will be refused.
	Removable bool `json:"removable"`
}

// ListMembers returns who has access to a project.
func (s *MemberService) ListMembers(ctx context.Context, caller *domain.User, projectID int64) ([]ProjectMember, error) {
	if err := s.authz.RequireProject(ctx, caller, authz.MembersRead, projectID); err != nil {
		return nil, err
	}

	members, err := s.authz.Store().ListProjectMembers(ctx, projectID)
	if err != nil {
		return nil, err
	}

	callerRank, err := s.rankOf(ctx, caller, projectID)
	if err != nil {
		return nil, err
	}

	out := make([]ProjectMember, 0, len(members))
	for _, m := range members {
		out = append(out, ProjectMember{
			Member:    m,
			Removable: s.mayChange(caller, callerRank, m) == nil,
		})
	}
	return out, nil
}

// Roles lists the roles that may be granted on a project, narrowed to what the
// caller may hand out.
//
// Offering a role the grant would then refuse is worse than not offering it:
// the person reads the list as what they can do.
func (s *MemberService) Roles(ctx context.Context, caller *domain.User, projectID int64) ([]authz.StoredRole, error) {
	if err := s.authz.RequireProject(ctx, caller, authz.MembersManage, projectID); err != nil {
		return nil, err
	}

	all, err := s.authz.Store().ListRoles(ctx)
	if err != nil {
		return nil, err
	}
	callerRank, err := s.rankOf(ctx, caller, projectID)
	if err != nil {
		return nil, err
	}

	out := make([]authz.StoredRole, 0, len(all))
	for _, role := range all {
		if !role.Active {
			continue
		}
		if authz.RoleRank[role.Key] > callerRank {
			continue
		}
		// A custom role ranks zero, so only a platform administrator can hand
		// one out. Somebody could otherwise create a role holding every
		// permission and grant it from a project.
		if !role.Builtin && !caller.IsAdmin {
			continue
		}
		out = append(out, role)
	}
	return out, nil
}

// GrantByEmail adds a member named by their address.
//
// By address rather than by picking from a list of everybody: a project
// administrator has no business enumerating the platform's accounts, and an
// address is what they actually know about the person they are adding. The
// permission check runs before the lookup, so the endpoint cannot be used to
// test whether an address is registered without already holding the project.
func (s *MemberService) GrantByEmail(ctx context.Context, caller *domain.User, projectID int64, email, roleKey string) error {
	if err := s.authz.RequireProject(ctx, caller, authz.MembersManage, projectID); err != nil {
		return err
	}

	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" {
		return fmt.Errorf("%w: an email address is required", ErrValidation)
	}
	target, err := s.users.GetByEmail(ctx, email)
	if err != nil {
		return ErrNotFound
	}
	return s.Grant(ctx, caller, projectID, target.ID, roleKey)
}

// Grant gives a user a role on a project.
func (s *MemberService) Grant(ctx context.Context, caller *domain.User, projectID, userID int64, roleKey string) error {
	if err := s.authz.RequireProject(ctx, caller, authz.MembersManage, projectID); err != nil {
		return err
	}
	if caller.ID == userID {
		return ErrSelfMembership
	}

	target, err := s.users.GetByID(ctx, userID)
	if err != nil {
		return ErrNotFound
	}
	if target.IsAdmin {
		// Nothing to grant: the platform flag already covers every project, and
		// writing a narrower row beside it would look like a change and do
		// nothing.
		return ErrTargetIsAdmin
	}

	callerRank, err := s.rankOf(ctx, caller, projectID)
	if err != nil {
		return err
	}
	if rank := authz.RoleRank[roleKey]; rank == 0 || rank > callerRank {
		if !caller.IsAdmin {
			return ErrRoleTooHigh
		}
	}

	roleID, err := s.authz.Store().RoleIDByKey(ctx, roleKey)
	if err != nil {
		return fmt.Errorf("%w: unknown role %q", ErrValidation, roleKey)
	}
	if err := s.authz.Store().GrantProject(ctx, userID, roleID, projectID); err != nil {
		return err
	}

	// The person granted access should see it on their next request rather than
	// when the cache happens to expire.
	s.authz.Invalidate(ctx, userID)
	return nil
}

// Revoke removes a user's role on a project.
func (s *MemberService) Revoke(ctx context.Context, caller *domain.User, projectID, userID, roleID int64) error {
	if err := s.authz.RequireProject(ctx, caller, authz.MembersManage, projectID); err != nil {
		return err
	}
	if caller.ID == userID {
		return ErrSelfMembership
	}

	members, err := s.authz.Store().ListProjectMembers(ctx, projectID)
	if err != nil {
		return err
	}
	callerRank, err := s.rankOf(ctx, caller, projectID)
	if err != nil {
		return err
	}

	var target *authz.Member
	for i := range members {
		if members[i].UserID == userID && members[i].RoleID == roleID {
			target = &members[i]
			break
		}
	}
	if target == nil {
		return ErrNotFound
	}
	if err := s.mayChange(caller, callerRank, *target); err != nil {
		return err
	}

	// An assignment covering every project cannot be narrowed one project at a
	// time. Saying so is better than removing it entirely, which is what the
	// button would look like it was doing.
	assignments, err := s.authz.Store().ListUserRoles(ctx, userID)
	if err != nil {
		return err
	}
	for _, a := range assignments {
		if a.RoleID == roleID && a.Unscoped {
			return ErrUnscopedMembership
		}
	}

	if err := s.authz.Store().RevokeProject(ctx, userID, roleID, projectID); err != nil {
		return err
	}
	s.authz.Invalidate(ctx, userID)
	return nil
}

// RoleRunView is one role's run visibility, as the settings screen shows it.
type RoleRunView struct {
	Key         string        `json:"key"`
	Name        string        `json:"name"`
	Description string        `json:"description"`
	View        authz.RunView `json:"view"`
}

// RunVisibility reports what each built-in role may see of a run.
//
// A role that holds runs.read without a restriction sees all of it; the settings
// screen is where that gets narrowed. Only the built-in roles are listed: a role
// an administrator created by hand is theirs to configure, and quietly rewriting
// its field list from a screen that does not say so would be a surprise.
func (s *MemberService) RunVisibility(ctx context.Context, caller *domain.User) ([]RoleRunView, error) {
	if caller == nil || !caller.IsAdmin {
		return nil, ErrForbidden
	}

	out := make([]RoleRunView, 0, len(authz.BuiltinRoles))
	for _, role := range authz.BuiltinRoles {
		roleID, err := s.authz.Store().RoleIDByKey(ctx, role.Key)
		if err != nil {
			return nil, err
		}
		fields, restricted, err := s.authz.Store().RoleFields(ctx, roleID, authz.RunsRead)
		if err != nil {
			return nil, err
		}

		view := authz.FullRunView()
		if restricted {
			view = authz.RunViewOf(fields)
		}
		out = append(out, RoleRunView{
			Key: role.Key, Name: role.Name, Description: role.Description, View: view,
		})
	}
	return out, nil
}

// SetRunVisibility narrows what a role sees of a run.
//
// Platform administrators only, because a role is global: a project
// administrator narrowing "reader" would change what readers see on every other
// project too.
func (s *MemberService) SetRunVisibility(ctx context.Context, caller *domain.User, roleKey string, view authz.RunView) error {
	if caller == nil || !caller.IsAdmin {
		return ErrForbidden
	}
	if !authz.IsBuiltin(roleKey) {
		return fmt.Errorf("%w: unknown role %q", ErrValidation, roleKey)
	}

	roleID, err := s.authz.Store().RoleIDByKey(ctx, roleKey)
	if err != nil {
		return ErrNotFound
	}

	// nil clears the restriction rather than storing a list that happens to name
	// everything. The two behave the same today and diverge the moment a field is
	// added: an explicit list would keep the new one hidden, which is the right
	// default for a restriction somebody set on purpose and the wrong one for a
	// role nobody restricted.
	var fields []string
	if !view.Output || !view.Error || !view.RequestURL {
		fields = view.Fields()
	}
	if err := s.authz.Store().SetRoleFields(ctx, roleID, authz.RunsRead, fields); err != nil {
		return err
	}

	// Everyone holding the role is affected, and there is no list of them here.
	// The cache TTL is what bounds the delay, which is why it is short.
	return nil
}

// mayChange reports whether the caller may change this member's access.
func (s *MemberService) mayChange(caller *domain.User, callerRank int, target authz.Member) error {
	if caller == nil {
		return ErrForbidden
	}
	if caller.ID == target.UserID {
		return ErrSelfMembership
	}
	if caller.IsAdmin {
		return nil
	}
	if target.IsAdmin {
		return ErrTargetIsAdmin
	}
	// The role-target guard: an administrator may act on the roles below their
	// own and on their own level, never above. Without it a project
	// administrator could remove somebody senior to them and take the project
	// over.
	if authz.RoleRank[target.RoleKey] > callerRank {
		return ErrRoleTooHigh
	}
	return nil
}

// rankOf is the caller's own standing on a project.
//
// A platform administrator outranks everything. Everyone else is ranked by the
// highest built-in role they hold that reaches this project.
func (s *MemberService) rankOf(ctx context.Context, caller *domain.User, projectID int64) (int, error) {
	if caller == nil {
		return 0, ErrForbidden
	}
	if caller.IsAdmin {
		return authz.RoleRank[authz.RoleProjectAdmin] + 1, nil
	}

	assignments, err := s.authz.Store().ListUserRoles(ctx, caller.ID)
	if err != nil {
		return 0, err
	}

	best := 0
	for _, a := range assignments {
		if !a.Unscoped && !containsID(a.ProjectIDs, projectID) {
			continue
		}
		if rank := authz.RoleRank[a.RoleKey]; rank > best {
			best = rank
		}
	}
	return best, nil
}

func containsID(ids []int64, want int64) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}
