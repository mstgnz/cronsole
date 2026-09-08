package memrepo

import (
	"context"
	"github.com/mstgnz/cronsole/v2/internal/domain"
	"sort"
	"sync"

	"github.com/mstgnz/cronsole/v2/internal/authz"
	"github.com/mstgnz/cronsole/v2/internal/repository"
	"github.com/mstgnz/grantz"
)

// Authz is the grant store, in memory.
//
// It implements BOTH sides: grantz's read side, which folds grants into a
// decision, and this service's write side, which an administrator edits. Real
// grants rather than a canned answer, so a test that gives somebody the reader
// role on project 2 is testing the same path a deployment takes.
type Authz struct {
	mu sync.Mutex

	roles       map[string]*storedRole
	byID        map[int64]*storedRole
	assignments map[int64][]*assignment // by user id
	permissions map[string]grantz.Permission
	nextRoleID  int64

	// users is read to fill in the membership list, which shows names.
	store *Store
}

type storedRole struct {
	id          int64
	key         string
	name        string
	description string
	active      bool
	permissions []string
	// fields is the per-permission allow-list, nil meaning unrestricted.
	fields map[string][]string
}

type assignment struct {
	roleID int64
	// projectIDs is empty and unscoped true for a grant over every project.
	projectIDs []int64
	unscoped   bool
}

// Compile-time proof of both contracts.
var (
	_ grantz.Store = (*Authz)(nil)
	_ authz.Store  = (*Authz)(nil)
)

// NewAuthz builds an empty grant store over a data store.
func NewAuthz(store *Store) *Authz {
	return &Authz{
		roles:       map[string]*storedRole{},
		byID:        map[int64]*storedRole{},
		assignments: map[int64][]*assignment{},
		permissions: map[string]grantz.Permission{},
		nextRoleID:  1,
		store:       store,
	}
}

// --- grantz's read side -----------------------------------------------------

// LoadUserGrants returns every grant that applies to a user.
//
// One Grant per (role, permission), carrying the role's scope. That shape is
// what makes the fold meaningful: grantz has to see each scope separately to
// decide that an unscoped grant clears the narrower ones beside it.
func (a *Authz) LoadUserGrants(_ context.Context, userID int64) ([]grantz.Grant, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	out := []grantz.Grant{}
	for _, assigned := range a.assignments[userID] {
		role, ok := a.byID[assigned.roleID]
		if !ok || !role.active {
			continue
		}

		var scope map[string]any
		if !assigned.unscoped {
			ids := make([]any, 0, len(assigned.projectIDs))
			for _, id := range assigned.projectIDs {
				ids = append(ids, float64(id))
			}
			scope = map[string]any{authz.ScopeKey: ids}
		}

		for _, key := range role.permissions {
			out = append(out, grantz.Grant{
				Key:      key,
				Effect:   grantz.EffectAllow,
				Fields:   role.fields[key],
				Scope:    scope,
				FromRole: true,
			})
		}
	}
	return out, nil
}

// SyncPermissions reconciles the catalogue and reports what the database holds
// that the code no longer declares.
func (a *Authz) SyncPermissions(_ context.Context, perms []grantz.Permission) ([]string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	declared := map[string]bool{}
	for _, p := range perms {
		declared[p.Key] = true
		a.permissions[p.Key] = p
	}

	orphans := []string{}
	for key := range a.permissions {
		if !declared[key] {
			orphans = append(orphans, key)
		}
	}
	sort.Strings(orphans)
	return orphans, nil
}

// --- the write side ---------------------------------------------------------

func (a *Authz) EnsureRole(_ context.Context, role authz.Role) (int64, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if existing, ok := a.roles[role.Key]; ok {
		existing.name, existing.description = role.Name, role.Description
		return existing.id, nil
	}

	stored := &storedRole{
		id: a.nextRoleID, key: role.Key, name: role.Name,
		description: role.Description, active: true,
		fields: map[string][]string{},
	}
	a.nextRoleID++
	a.roles[role.Key] = stored
	a.byID[stored.id] = stored
	return stored.id, nil
}

func (a *Authz) ReplaceRolePermissions(_ context.Context, roleID int64, keys []string) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	role, ok := a.byID[roleID]
	if !ok {
		return repository.ErrNotFound
	}
	role.permissions = append([]string(nil), keys...)
	return nil
}

func (a *Authz) RoleIDByKey(_ context.Context, key string) (int64, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	role, ok := a.roles[key]
	if !ok {
		return 0, repository.ErrNotFound
	}
	return role.id, nil
}

func (a *Authz) ListRoles(_ context.Context) ([]authz.StoredRole, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	out := []authz.StoredRole{}
	for _, role := range a.roles {
		out = append(out, authz.StoredRole{
			ID: role.id, Key: role.key, Name: role.name,
			Description: role.description, Active: role.active,
			Builtin:     authz.IsBuiltin(role.key),
			Permissions: append([]string(nil), role.permissions...),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (a *Authz) ListProjectMembers(_ context.Context, projectID int64) ([]authz.Member, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	out := []authz.Member{}
	for userID, assigned := range a.assignments {
		for _, entry := range assigned {
			// An unscoped assignment is not project membership: it reaches
			// every project and is not something the project screen manages.
			if entry.unscoped || !containsID(entry.projectIDs, projectID) {
				continue
			}
			role, ok := a.byID[entry.roleID]
			if !ok {
				continue
			}
			member := authz.Member{
				UserID: userID, RoleID: role.id,
				RoleKey: role.key, RoleName: role.name,
			}
			if user := a.store.userByID(userID); user != nil {
				member.Fullname, member.Email = user.Fullname, user.Email
				member.Active, member.IsAdmin = user.Active, user.IsAdmin
			}
			out = append(out, member)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].UserID != out[j].UserID {
			return out[i].UserID < out[j].UserID
		}
		return out[i].RoleID < out[j].RoleID
	})
	return out, nil
}

func (a *Authz) ListUserRoles(_ context.Context, userID int64) ([]authz.Assignment, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	out := []authz.Assignment{}
	for _, entry := range a.assignments[userID] {
		role, ok := a.byID[entry.roleID]
		if !ok {
			continue
		}
		out = append(out, authz.Assignment{
			RoleID: role.id, RoleKey: role.key, RoleName: role.name,
			ProjectIDs: append([]int64(nil), entry.projectIDs...),
			Unscoped:   entry.unscoped,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RoleID < out[j].RoleID })
	return out, nil
}

func (a *Authz) GrantProject(_ context.Context, userID, roleID, projectID int64) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	if _, ok := a.byID[roleID]; !ok {
		return repository.ErrNotFound
	}
	for _, entry := range a.assignments[userID] {
		if entry.roleID != roleID {
			continue
		}
		if !containsID(entry.projectIDs, projectID) {
			entry.projectIDs = append(entry.projectIDs, projectID)
			sort.Slice(entry.projectIDs, func(i, j int) bool {
				return entry.projectIDs[i] < entry.projectIDs[j]
			})
		}
		return nil
	}
	a.assignments[userID] = append(a.assignments[userID],
		&assignment{roleID: roleID, projectIDs: []int64{projectID}})
	return nil
}

func (a *Authz) RevokeProject(_ context.Context, userID, roleID, projectID int64) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	entries := a.assignments[userID]
	for i, entry := range entries {
		if entry.roleID != roleID {
			continue
		}
		kept := entry.projectIDs[:0]
		for _, id := range entry.projectIDs {
			if id != projectID {
				kept = append(kept, id)
			}
		}
		entry.projectIDs = kept
		// An assignment with no project left is deleted rather than kept as an
		// empty scope, which grantz would read as unscoped and therefore as
		// every project.
		if len(entry.projectIDs) == 0 && !entry.unscoped {
			a.assignments[userID] = append(entries[:i:i], entries[i+1:]...)
		}
		return nil
	}
	return repository.ErrNotFound
}

func (a *Authz) RevokeUser(_ context.Context, userID int64) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.assignments, userID)
	return nil
}

func (a *Authz) SetRoleFields(_ context.Context, roleID int64, permissionKey string, fields []string) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	role, ok := a.byID[roleID]
	if !ok {
		return repository.ErrNotFound
	}
	if fields == nil {
		delete(role.fields, permissionKey)
		return nil
	}
	role.fields[permissionKey] = append([]string(nil), fields...)
	return nil
}

func (a *Authz) RoleFields(_ context.Context, roleID int64, permissionKey string) ([]string, bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	role, ok := a.byID[roleID]
	if !ok {
		return nil, false, repository.ErrNotFound
	}
	fields, restricted := role.fields[permissionKey]
	if !restricted {
		return nil, false, nil
	}
	return append([]string(nil), fields...), true, nil
}

// --- seeding ----------------------------------------------------------------

// GrantRole gives a user a role over the named projects. No projects means an
// unscoped grant, which reaches every project.
func (a *Authz) GrantRole(userID int64, roleKey string, projectIDs ...int64) {
	a.mu.Lock()
	defer a.mu.Unlock()

	role, ok := a.roles[roleKey]
	if !ok {
		return
	}
	entry := &assignment{roleID: role.id, unscoped: len(projectIDs) == 0}
	entry.projectIDs = append(entry.projectIDs, projectIDs...)
	sort.Slice(entry.projectIDs, func(i, j int) bool {
		return entry.projectIDs[i] < entry.projectIDs[j]
	})
	a.assignments[userID] = append(a.assignments[userID], entry)
}

// RoleID resolves a role key for a test that needs the id.
func (a *Authz) RoleID(key string) int64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	if role, ok := a.roles[key]; ok {
		return role.id
	}
	return 0
}

func containsID(ids []int64, want int64) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

// userByID reads an account. It takes the Store's own lock, never the Authz
// one, so the two can be held in either order without a cycle.
func (s *Store) userByID(id int64) *domain.User {
	s.mu.Lock()
	defer s.mu.Unlock()

	u, ok := s.users[id]
	if !ok || s.deletedUsers[id] {
		return nil
	}
	copied := *u
	return &copied
}
