package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/lib/pq"
	"github.com/mstgnz/cronsole/v2/internal/authz"
)

// AuthzRepo is the write side of authorization: the role catalogue and who
// holds what.
//
// It reads and writes grantz's own tables, which needs saying. grantz reads
// them for a decision and deliberately offers no API for writing the role
// mapping, because that mapping is data an administrator edits rather than
// something the library owns. This is that administrator interface.
//
// It is purpose built, not generic CRUD over those tables. There is no method
// here that lets somebody change their own grants, and the escalation rules
// live in the service above it.
type AuthzRepo struct{ *Store }

// NewAuthzRepo wires the repository onto a store.
func NewAuthzRepo(s *Store) *AuthzRepo { return &AuthzRepo{s} }

// EnsureRole creates or updates a role and returns its id.
func (r *AuthzRepo) EnsureRole(ctx context.Context, role authz.Role) (int64, error) {
	const q = `INSERT INTO grantz_roles (key, name, description, active)
		VALUES ($1, $2, $3, true)
		ON CONFLICT (key) DO UPDATE
		SET name = EXCLUDED.name, description = EXCLUDED.description, updated_at = now()
		RETURNING id`
	var id int64
	err := r.db.QueryRowContext(ctx, q, role.Key, role.Name, role.Description).Scan(&id)
	return id, err
}

// RoleIDByKey resolves a role key.
func (r *AuthzRepo) RoleIDByKey(ctx context.Context, key string) (int64, error) {
	var id int64
	err := r.db.QueryRowContext(ctx, `SELECT id FROM grantz_roles WHERE key = $1`, key).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	return id, err
}

// ReplaceRolePermissions sets exactly the permissions a role holds.
//
// Delete-then-insert inside one transaction, not a diff: the caller states the
// whole set, and a permission removed from a role in code has to actually be
// removed. A field restriction an administrator set on a permission that
// survives the change is preserved, because taking it away would widen access
// as a side effect of an upgrade.
func (r *AuthzRepo) ReplaceRolePermissions(ctx context.Context, roleID int64, keys []string) error {
	return r.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM grantz_role_permissions WHERE role_id = $1 AND permission_key <> ALL($2)`,
			roleID, pq.Array(keys)); err != nil {
			return err
		}
		for _, key := range keys {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO grantz_role_permissions (role_id, permission_key)
				 VALUES ($1, $2) ON CONFLICT (role_id, permission_key) DO NOTHING`,
				roleID, key); err != nil {
				return err
			}
		}
		return nil
	})
}

// SetRoleFields narrows a permission for a role. Nil clears the restriction.
//
// The stored shape is grantz's: {"allow": ["output", "error"]}. An empty list
// is stored as an empty allow-list, which grantz reads as "may act, on no
// field", and that is the intended meaning here: a reader who may see runs but
// none of their content.
func (r *AuthzRepo) SetRoleFields(ctx context.Context, roleID int64, permissionKey string, fields []string) error {
	var payload any
	if fields != nil {
		encoded, err := json.Marshal(map[string][]string{"allow": fields})
		if err != nil {
			return err
		}
		payload = string(encoded)
	}

	const q = `INSERT INTO grantz_role_permissions (role_id, permission_key, fields)
		VALUES ($1, $2, $3)
		ON CONFLICT (role_id, permission_key) DO UPDATE SET fields = EXCLUDED.fields`
	_, err := r.db.ExecContext(ctx, q, roleID, permissionKey, payload)
	return err
}

// RoleFields reads the field restriction a role holds on a permission.
//
// restricted is false both when the row carries no restriction and when the row
// does not exist, because a role that does not hold the permission at all is not
// a role restricted to no fields. The caller decides what to do with that; here
// it only has to be reported truthfully.
func (r *AuthzRepo) RoleFields(ctx context.Context, roleID int64, permissionKey string) ([]string, bool, error) {
	const q = `SELECT fields FROM grantz_role_permissions WHERE role_id = $1 AND permission_key = $2`

	var raw []byte
	err := r.db.QueryRowContext(ctx, q, roleID, permissionKey).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if len(raw) == 0 || string(raw) == "null" {
		return nil, false, nil
	}

	var decoded struct {
		Allow []string `json:"allow"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, false, err
	}
	// An allow-list of ["*"] is grantz's explicit "every field", so it reads back
	// as unrestricted rather than as a field literally named "*".
	if len(decoded.Allow) == 1 && decoded.Allow[0] == "*" {
		return nil, false, nil
	}
	// A restriction that decodes to an empty list is a real restriction: the role
	// may act, on no field.
	if decoded.Allow == nil {
		decoded.Allow = []string{}
	}
	return decoded.Allow, true, nil
}

// ListRoles returns every role with the permissions it holds.
func (r *AuthzRepo) ListRoles(ctx context.Context) ([]authz.StoredRole, error) {
	const q = `SELECT r.id, r.key, r.name, COALESCE(r.description, ''), r.active,
			COALESCE(array_agg(rp.permission_key ORDER BY rp.permission_key)
			         FILTER (WHERE rp.permission_key IS NOT NULL), ARRAY[]::text[])
		FROM grantz_roles r
		LEFT JOIN grantz_role_permissions rp ON rp.role_id = r.id
		GROUP BY r.id, r.key, r.name, r.description, r.active
		ORDER BY r.key`

	rows, err := r.db.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []authz.StoredRole
	for rows.Next() {
		var (
			role  authz.StoredRole
			perms pq.StringArray
		)
		if err := rows.Scan(&role.ID, &role.Key, &role.Name, &role.Description, &role.Active, &perms); err != nil {
			return nil, err
		}
		role.Permissions = perms
		role.Builtin = authz.IsBuiltin(role.Key)
		out = append(out, role)
	}
	return out, rows.Err()
}

// ListProjectMembers returns who holds a role scoped to a project.
//
// The containment test is on the scope's project list. An assignment with no
// scope is unrestricted and therefore covers every project, so it is included:
// leaving it out would show a project as having no members while somebody was
// quietly administering it.
func (r *AuthzRepo) ListProjectMembers(ctx context.Context, projectID int64) ([]authz.Member, error) {
	const q = `SELECT u.id, u.fullname, u.email, u.active, u.is_admin,
			r.id, r.key, r.name
		FROM grantz_user_roles ur
		JOIN grantz_roles r ON r.id = ur.role_id
		JOIN users u ON u.id = ur.user_id
		WHERE u.deleted_at IS NULL AND r.active
		  AND (ur.scope IS NULL
		       OR ur.scope = '{}'::jsonb
		       OR ur.scope->'project_ids' @> to_jsonb($1::bigint)
		       OR ur.scope->>'project_id' = $1::text)
		ORDER BY u.fullname`

	rows, err := r.db.QueryContext(ctx, q, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []authz.Member
	for rows.Next() {
		var m authz.Member
		if err := rows.Scan(&m.UserID, &m.Fullname, &m.Email, &m.Active, &m.IsAdmin,
			&m.RoleID, &m.RoleKey, &m.RoleName); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// ListUserRoles returns a user's assignments with the projects each covers.
func (r *AuthzRepo) ListUserRoles(ctx context.Context, userID int64) ([]authz.Assignment, error) {
	const q = `SELECT r.id, r.key, r.name, ur.scope
		FROM grantz_user_roles ur
		JOIN grantz_roles r ON r.id = ur.role_id
		WHERE ur.user_id = $1
		ORDER BY r.key`

	rows, err := r.db.QueryContext(ctx, q, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []authz.Assignment
	for rows.Next() {
		var (
			a   authz.Assignment
			raw []byte
		)
		if err := rows.Scan(&a.RoleID, &a.RoleKey, &a.RoleName, &raw); err != nil {
			return nil, err
		}
		ids, unscoped, err := decodeScope(raw)
		if err != nil {
			return nil, err
		}
		a.ProjectIDs, a.Unscoped = ids, unscoped
		out = append(out, a)
	}
	return out, rows.Err()
}

// decodeScope reads the stored scope.
//
// A scope that is absent, null or an empty object is unrestricted, which is the
// same reading grantz applies. Anything else must name projects, and a shape it
// cannot read is an error rather than an empty list: an unreadable restriction
// that resolves to "nothing" looks exactly like a user who has been granted no
// projects, and the two need opposite fixes.
func decodeScope(raw []byte) (ids []int64, unscoped bool, err error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, true, nil
	}

	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, false, err
	}
	if len(decoded) == 0 {
		return nil, true, nil
	}

	if list, ok := decoded["project_ids"].([]any); ok {
		for _, item := range list {
			n, ok := item.(float64)
			if !ok {
				return nil, false, errors.New("repository: project_ids holds a non numeric entry")
			}
			ids = append(ids, int64(n))
		}
		return ids, false, nil
	}
	if single, ok := decoded["project_id"].(float64); ok {
		return []int64{int64(single)}, false, nil
	}
	return nil, false, errors.New("repository: scope names no project")
}

// GrantProject adds a project to a user's assignment for a role.
//
// One statement, because read-modify-write here loses a project when two
// administrators grant access at the same moment: both read the same list, both
// write their own addition, and the second overwrites the first.
//
// It refuses to narrow an unscoped assignment. Somebody who holds the role over
// every project is not made narrower by being "added" to one, and silently
// doing so would take access away.
func (r *AuthzRepo) GrantProject(ctx context.Context, userID, roleID, projectID int64) error {
	const q = `
		INSERT INTO grantz_user_roles (user_id, role_id, scope)
		VALUES ($1, $2, jsonb_build_object('project_ids', jsonb_build_array($3::bigint)))
		ON CONFLICT (user_id, role_id) DO UPDATE
		SET scope = CASE
			WHEN grantz_user_roles.scope IS NULL OR grantz_user_roles.scope = '{}'::jsonb
				THEN grantz_user_roles.scope
			ELSE jsonb_build_object('project_ids', (
				SELECT jsonb_agg(DISTINCT value)
				FROM jsonb_array_elements(
					COALESCE(grantz_user_roles.scope->'project_ids', '[]'::jsonb)
					|| CASE WHEN grantz_user_roles.scope ? 'project_id'
					        THEN jsonb_build_array(grantz_user_roles.scope->'project_id')
					        ELSE '[]'::jsonb END
					|| jsonb_build_array($3::bigint)
				) AS value
			))
		END`
	_, err := r.db.ExecContext(ctx, q, userID, roleID, projectID)
	return err
}

// RevokeProject removes a project from an assignment, deleting the assignment
// when nothing is left.
//
// An unscoped assignment cannot be narrowed one project at a time: revoking
// there means removing the whole assignment, and the service says so rather
// than letting this look like it worked.
func (r *AuthzRepo) RevokeProject(ctx context.Context, userID, roleID, projectID int64) error {
	return r.tx(ctx, func(tx *sql.Tx) error {
		const update = `
			UPDATE grantz_user_roles
			SET scope = jsonb_build_object('project_ids', COALESCE((
				SELECT jsonb_agg(value)
				FROM jsonb_array_elements(COALESCE(scope->'project_ids', '[]'::jsonb)) AS value
				WHERE value <> to_jsonb($3::bigint)
			), '[]'::jsonb))
			WHERE user_id = $1 AND role_id = $2
			  AND scope IS NOT NULL AND scope <> '{}'::jsonb`
		if _, err := tx.ExecContext(ctx, update, userID, roleID, projectID); err != nil {
			return err
		}

		// An assignment covering nothing is not the same as one covering
		// everything, and leaving an empty list behind would show the user as a
		// member of no project while still holding the role.
		const prune = `DELETE FROM grantz_user_roles
			WHERE user_id = $1 AND role_id = $2
			  AND scope IS NOT NULL AND scope <> '{}'::jsonb
			  AND COALESCE(jsonb_array_length(scope->'project_ids'), 0) = 0`
		_, err := tx.ExecContext(ctx, prune, userID, roleID)
		return err
	})
}

// RevokeUser removes every assignment and exception a user holds.
//
// Called when an account is deactivated. The foreign key on grantz_user_roles
// cascades on a real DELETE, and accounts here are deleted softly, so the
// cascade never fires: without this an account that was reactivated, or an
// address that was reused, would silently come back with its old access.
func (r *AuthzRepo) RevokeUser(ctx context.Context, userID int64) error {
	return r.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM grantz_user_roles WHERE user_id = $1`, userID); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx,
			`DELETE FROM grantz_user_permissions WHERE user_id = $1`, userID)
		return err
	})
}
