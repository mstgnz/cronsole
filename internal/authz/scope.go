package authz

import (
	"encoding/json"
	"fmt"
	"sort"

	"github.com/mstgnz/cronsole/v2/internal/domain"
)

// ScopeKey is the field a project scope is written under.
//
// Plural, holding a list, which is grantz's own suggested convention and the
// only shape that works here: grantz_user_roles is keyed on (user_id, role_id),
// so one assignment carries one scope. Somebody who writes on two brands is one
// row naming both, not two rows.
//
// The singular form is accepted on read as well, because it is the shape people
// write by hand after seeing a one-project example.
const (
	ScopeKey       = "project_ids"
	ScopeKeySingle = "project_id"
)

// scopeFromGrants turns grantz's raw scope maps into a domain.ProjectScope.
//
// The scope type lives in domain because both this package and every repository
// filter need it; what stays here is the reading of grantz's maps into it.
//
// No scopes on an ALLOWED permission means unrestricted, and grantz guarantees
// that since v0.4.0: an unscoped grant clears the narrower scopes beside it, so
// an empty list can no longer mean "there were scopes and we dropped them".
// Against v0.3.0 this reading would have silently NARROWED a user who held both
// a wide role and a project role.
func scopeFromGrants(raw []map[string]any) (domain.ProjectScope, error) {
	if len(raw) == 0 {
		return domain.AllProjects(), nil
	}

	seen := map[int64]struct{}{}
	for _, scope := range raw {
		ids, err := projectIDsFrom(scope)
		if err != nil {
			return domain.NoProjects(), err
		}
		for _, id := range ids {
			seen[id] = struct{}{}
		}
	}

	out := domain.ProjectScope{IDs: make([]int64, 0, len(seen))}
	for id := range seen {
		out.IDs = append(out.IDs, id)
	}
	sort.Slice(out.IDs, func(i, j int) bool { return out.IDs[i] < out.IDs[j] })
	return out, nil
}

// projectIDsFrom reads the ids out of one scope map.
//
// A scope that carries neither key is an ERROR, not an empty result. An
// unreadable restriction that resolves to "nothing" looks exactly like a
// correctly configured user who has been given no projects, and the two need
// opposite fixes. Failing loudly here is the same choice grantz makes for a
// malformed field list.
func projectIDsFrom(scope map[string]any) ([]int64, error) {
	if raw, ok := scope[ScopeKey]; ok {
		list, ok := raw.([]any)
		if !ok {
			return nil, fmt.Errorf("authz: scope %q must be a list, got %T", ScopeKey, raw)
		}
		ids := make([]int64, 0, len(list))
		for _, item := range list {
			id, err := asID(item)
			if err != nil {
				return nil, err
			}
			ids = append(ids, id)
		}
		return ids, nil
	}

	if raw, ok := scope[ScopeKeySingle]; ok {
		id, err := asID(raw)
		if err != nil {
			return nil, err
		}
		return []int64{id}, nil
	}

	return nil, fmt.Errorf("authz: scope carries neither %q nor %q: %v", ScopeKey, ScopeKeySingle, scope)
}

// asID reads an id out of decoded JSON, where a number arrives as float64.
func asID(v any) (int64, error) {
	switch n := v.(type) {
	case float64:
		return int64(n), nil
	case int64:
		return n, nil
	case int:
		return int64(n), nil
	case json.Number:
		return n.Int64()
	}
	return 0, fmt.Errorf("authz: %v is not a project id", v)
}

// EncodeScope builds the scope value written to grantz_user_roles.
func EncodeScope(projectIDs []int64) (string, error) {
	ids := append([]int64(nil), projectIDs...)
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	out, err := json.Marshal(map[string]any{ScopeKey: ids})
	if err != nil {
		return "", err
	}
	return string(out), nil
}
