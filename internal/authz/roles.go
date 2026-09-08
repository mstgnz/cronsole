package authz

import (
	"context"
	"fmt"
)

// Built-in role keys.
//
// These three are reconciled from code at every boot, so upgrading the service
// upgrades what they can do. A role an administrator creates by hand is never
// touched, which is how both work at once: the shipped roles stay correct and a
// custom one stays custom.
const (
	RoleProjectReader = "project_reader"
	RoleProjectWriter = "project_writer"
	RoleProjectAdmin  = "project_admin"
)

// Role is a built-in role definition.
type Role struct {
	Key         string
	Name        string
	Description string
	Permissions []string
}

// BuiltinRoles are the three shipped roles, narrowest first.
//
// Each includes the one before it. Written out rather than composed by
// concatenation so the whole of a role is readable in one place: a permission
// silently arriving through two levels of inheritance is how somebody ends up
// with an action nobody meant to give them.
var BuiltinRoles = []Role{
	{
		Key:         RoleProjectReader,
		Name:        "Reader",
		Description: "Sees the project's jobs and their history. Changes nothing.",
		Permissions: []string{
			ProjectsRead,
			JobsRead,
			RunsRead,
		},
	},
	{
		Key:         RoleProjectWriter,
		Name:        "Writer",
		Description: "Everything a reader sees, plus adding, editing and running jobs. Cannot switch a job on.",
		Permissions: []string{
			ProjectsRead,
			JobsRead, JobsCreate, JobsUpdate, JobsDelete, JobsRun,
			RunsRead,
			NotificationsRead,
		},
	},
	{
		Key:  RoleProjectAdmin,
		Name: "Project administrator",
		Description: "Everything in the project, including switching jobs on, " +
			"rotating the API key and granting access to others.",
		Permissions: []string{
			ProjectsRead, ProjectsUpdate, ProjectsDelete, ProjectsRotateKey,
			JobsRead, JobsCreate, JobsUpdate, JobsDelete, JobsRun, JobsActivate,
			RunsRead,
			MembersRead, MembersManage,
			NotificationsRead, NotificationsManage,
		},
	},
}

// RoleRank orders the built-in roles by how much they can do.
//
// Used by the escalation guard: a project administrator may grant a role, but
// never one above their own, and never to somebody who already holds a wider
// one. Roles outside this map are custom and rank zero, so a project
// administrator cannot hand one out at all.
var RoleRank = map[string]int{
	RoleProjectReader: 1,
	RoleProjectWriter: 2,
	RoleProjectAdmin:  3,
}

// IsBuiltin reports whether a role key is one this service ships.
func IsBuiltin(key string) bool {
	_, ok := RoleRank[key]
	return ok
}

// Store is the write side of authorization: the role catalogue and who holds
// what.
//
// grantz reads its own tables and deliberately offers no API for writing the
// role mapping, because that mapping is data an administrator edits. This is
// that administrator interface, purpose built rather than generic CRUD: there
// is no path here that lets somebody edit their own grants.
type Store interface {
	// EnsureRole creates or updates a role and returns its id.
	EnsureRole(ctx context.Context, role Role) (int64, error)
	// ReplaceRolePermissions sets exactly the permissions a role holds.
	ReplaceRolePermissions(ctx context.Context, roleID int64, keys []string) error
	// RoleIDByKey resolves a role key.
	RoleIDByKey(ctx context.Context, key string) (int64, error)
	// ListRoles returns every role, built-in and custom.
	ListRoles(ctx context.Context) ([]StoredRole, error)

	// ListProjectMembers returns who holds a role scoped to a project.
	ListProjectMembers(ctx context.Context, projectID int64) ([]Member, error)
	// ListUserRoles returns a user's assignments with the projects each covers.
	ListUserRoles(ctx context.Context, userID int64) ([]Assignment, error)
	// GrantProject adds a project to a user's assignment for a role, creating
	// the assignment if it does not exist.
	GrantProject(ctx context.Context, userID, roleID, projectID int64) error
	// RevokeProject removes a project from an assignment, deleting the
	// assignment when no project is left.
	RevokeProject(ctx context.Context, userID, roleID, projectID int64) error
	// RevokeUser removes every assignment and exception a user holds.
	RevokeUser(ctx context.Context, userID int64) error
	// SetRoleFields narrows a permission for a role. nil clears the
	// restriction.
	SetRoleFields(ctx context.Context, roleID int64, permissionKey string, fields []string) error
	// RoleFields reads that restriction back. restricted is false when the role
	// holds the permission over every field, which is not the same as holding it
	// over none.
	RoleFields(ctx context.Context, roleID int64, permissionKey string) (fields []string, restricted bool, err error)
}

// StoredRole is a role as the database holds it.
type StoredRole struct {
	ID          int64    `json:"id"`
	Key         string   `json:"key"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Active      bool     `json:"active"`
	Builtin     bool     `json:"builtin"`
	Permissions []string `json:"permissions"`
}

// Member is one person's access to one project.
type Member struct {
	UserID   int64  `json:"user_id"`
	Fullname string `json:"fullname"`
	Email    string `json:"email"`
	Active   bool   `json:"active"`
	IsAdmin  bool   `json:"is_admin"`
	RoleID   int64  `json:"role_id"`
	RoleKey  string `json:"role_key"`
	RoleName string `json:"role_name"`
}

// Assignment is a user's grant of one role over a set of projects.
type Assignment struct {
	RoleID     int64   `json:"role_id"`
	RoleKey    string  `json:"role_key"`
	RoleName   string  `json:"role_name"`
	ProjectIDs []int64 `json:"project_ids"`
	// Unscoped means the role was granted over every project.
	Unscoped bool `json:"unscoped"`
}

// Reconcile writes the built-in roles and their permissions.
//
// Run at boot, after the permission catalogue is synced. It is why upgrading
// the binary upgrades what a writer can do: a permission added to a role here
// takes effect on restart rather than needing somebody to remember a database
// edit.
//
// It touches ONLY the three built-in keys. A role an administrator created is
// left exactly as it is, including one that happens to hold the same
// permissions.
func Reconcile(ctx context.Context, store Store) error {
	for _, role := range BuiltinRoles {
		id, err := store.EnsureRole(ctx, role)
		if err != nil {
			return fmt.Errorf("authz: role %q: %w", role.Key, err)
		}
		if err := store.ReplaceRolePermissions(ctx, id, role.Permissions); err != nil {
			return fmt.Errorf("authz: role %q permissions: %w", role.Key, err)
		}
	}
	return nil
}
