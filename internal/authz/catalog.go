// Package authz decides who may do what, and to which projects.
//
// It is a thin layer over github.com/mstgnz/grantz. The library answers the
// verb ("may this user update jobs at all") and hands back the scope a role was
// granted with; deciding what that scope MEANS is this package's job, because
// "does this record belong to that scope" is domain knowledge.
//
// The division matters and is easy to get wrong. Middleware runs before a row
// is read, so it can only ever answer the verb. Without the scope check in the
// service layer, a reader on one project can edit another project's job while
// every middleware in front of them says yes.
package authz

import "github.com/mstgnz/grantz"

// The permission catalogue.
//
// Declared in code, not in the database: a typo is a compile error rather than
// a permission nobody holds, every key is greppable, and a new capability shows
// up in code review. What an administrator edits is the mapping from roles to
// these keys.
//
// Keys are business verbs. jobs.activate is separate from jobs.update because
// switching a job on starts it firing at real services, and that is a different
// decision from correcting its timeout.
const (
	ProjectsRead      = "projects.read"
	ProjectsCreate    = "projects.create"
	ProjectsUpdate    = "projects.update"
	ProjectsDelete    = "projects.delete"
	ProjectsRotateKey = "projects.rotate_key"

	JobsRead     = "jobs.read"
	JobsCreate   = "jobs.create"
	JobsUpdate   = "jobs.update"
	JobsDelete   = "jobs.delete"
	JobsActivate = "jobs.activate"
	JobsRun      = "jobs.run"

	RunsRead = "runs.read"

	MembersRead   = "members.read"
	MembersManage = "members.manage"

	NotificationsRead   = "notifications.read"
	NotificationsManage = "notifications.manage"

	UsersRead    = "users.read"
	UsersManage  = "users.manage"
	SettingsRead = "settings.read"
)

// Catalog is every permission this service knows about. Synced at boot.
//
// HasFields is set only where a field list is meaningful. runs.read carries it
// because a reader who may see that a job failed does not necessarily get to
// see the response body it failed with, and that is a decision the person
// granting access should be able to make per role rather than in code.
var Catalog = []grantz.Permission{
	{Key: ProjectsRead, Description: "See a project and its settings", HasFields: true},
	{Key: ProjectsCreate, Description: "Create a project"},
	{Key: ProjectsUpdate, Description: "Change a project's name, slug or base address"},
	{Key: ProjectsDelete, Description: "Delete a project and deactivate its jobs"},
	{Key: ProjectsRotateKey, Description: "Issue a new API key, invalidating the current one"},

	{Key: JobsRead, Description: "See job definitions", HasFields: true},
	{Key: JobsCreate, Description: "Add a job"},
	{Key: JobsUpdate, Description: "Change a job, its schedules, headers and chain"},
	{Key: JobsDelete, Description: "Delete a job definition"},
	{Key: JobsActivate, Description: "Switch a job on or off"},
	{Key: JobsRun, Description: "Run a job now"},

	{Key: RunsRead, Description: "See execution history and results", HasFields: true},

	{Key: MembersRead, Description: "See who has access to a project"},
	{Key: MembersManage, Description: "Grant and revoke access to a project"},

	{Key: NotificationsRead, Description: "See alert recipient lists"},
	{Key: NotificationsManage, Description: "Create and change alert recipient lists"},

	{Key: UsersRead, Description: "See accounts"},
	{Key: UsersManage, Description: "Create, change and deactivate accounts"},
	{Key: SettingsRead, Description: "See the service log and settings"},
}

// Run field names that a field restriction on RunsRead may narrow.
//
// These are the names the run list and detail expose, not column names: the
// permission is a business verb and the restriction should read the same way to
// whoever sets it.
const (
	RunFieldOutput  = "output"
	RunFieldError   = "error"
	RunFieldRequest = "request_url"
)

// Global permissions are not scoped to a project. They are held through the
// superuser hook rather than through a role, because a project scope has no
// meaning for them: there is no project that owns an account.
//
// Listed so the membership screen can refuse to offer them on a project role,
// which would otherwise look like it worked and quietly grant nothing.
var Global = map[string]bool{
	UsersRead:      true,
	UsersManage:    true,
	SettingsRead:   true,
	ProjectsCreate: true,
}
