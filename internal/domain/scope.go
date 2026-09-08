package domain

import "sort"

// ProjectScope is which projects a caller may reach.
//
// It lives in domain rather than in the authorization package because both
// sides need it: authz produces one from a user's grants, and every filter and
// repository query consumes one. A type in the middle is what lets the compiler
// notice a query that forgot to take a scope.
//
// The zero value reaches NOTHING. That is the correct default for a type that
// decides access: a scope that has not been resolved must not read as
// unrestricted.
type ProjectScope struct {
	// All means every project, present and future. It is reached only through
	// an unscoped grant or the administrator flag.
	All bool
	// IDs is the union of the projects a scoped grant named, sorted and
	// deduplicated.
	IDs []int64
}

// AllProjects is the scope of a caller who may reach everything.
func AllProjects() ProjectScope { return ProjectScope{All: true} }

// NoProjects is the scope of a caller who may reach nothing. Written out rather
// than relying on the zero value at call sites, so the intent is visible.
func NoProjects() ProjectScope { return ProjectScope{} }

// ScopeOf builds a scope over a set of project ids.
func ScopeOf(ids ...int64) ProjectScope {
	out := ProjectScope{IDs: append([]int64(nil), ids...)}
	sort.Slice(out.IDs, func(i, j int) bool { return out.IDs[i] < out.IDs[j] })
	return out
}

// Allows reports whether a project is inside the scope.
func (s ProjectScope) Allows(projectID int64) bool {
	if s.All {
		return true
	}
	for _, id := range s.IDs {
		if id == projectID {
			return true
		}
	}
	return false
}

// Empty reports a scope that reaches nothing.
//
// A caller listing rows for an empty scope must return none, never all of them.
// The check is spelled out because `len(IDs) == 0` alone is also true for the
// unrestricted scope, and confusing the two opens everything.
func (s ProjectScope) Empty() bool { return !s.All && len(s.IDs) == 0 }

// Restricted reports whether the scope narrows anything at all.
func (s ProjectScope) Restricted() bool { return !s.All }

// Intersect narrows a scope to the projects also in ids.
//
// Used where two permissions have to hold at once, such as a chain link that
// crosses projects: the link may be created only over projects the caller can
// write on both ends.
func (s ProjectScope) Intersect(other ProjectScope) ProjectScope {
	if s.All {
		return other
	}
	if other.All {
		return s
	}
	keep := make(map[int64]struct{}, len(other.IDs))
	for _, id := range other.IDs {
		keep[id] = struct{}{}
	}
	out := ProjectScope{}
	for _, id := range s.IDs {
		if _, ok := keep[id]; ok {
			out.IDs = append(out.IDs, id)
		}
	}
	return out
}
