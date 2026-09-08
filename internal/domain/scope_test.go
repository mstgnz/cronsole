package domain

import "testing"

func TestZeroScopeReachesNothing(t *testing.T) {
	// The whole design rests on this. A query built before the caller's scope
	// was resolved gets the zero value, and the zero value has to be the
	// restrictive answer: a struct that defaults to "everything" turns a
	// forgotten argument into a data leak instead of an empty list.
	var zero ProjectScope

	if zero.Allows(1) {
		t.Error("the zero scope allowed a project")
	}
	if !zero.Empty() {
		t.Error("the zero scope did not report itself as empty")
	}
	if !zero.Restricted() {
		t.Error("the zero scope did not report itself as restricted")
	}
}

func TestAllProjectsIsNotEmpty(t *testing.T) {
	// The trap this guards: All carries no ids, so a len(IDs) == 0 test reads
	// the unrestricted scope as the empty one and hides everything from the
	// administrator, or worse, is inverted somewhere and shows everything to
	// everybody.
	all := AllProjects()

	if all.Empty() {
		t.Error("the unrestricted scope reported itself as empty")
	}
	if !all.Allows(999) {
		t.Error("the unrestricted scope refused a project")
	}
	if all.Restricted() {
		t.Error("the unrestricted scope reported itself as restricted")
	}
}

func TestScopeOfSortsAndAllows(t *testing.T) {
	scope := ScopeOf(9, 2, 5)

	for i := 1; i < len(scope.IDs); i++ {
		if scope.IDs[i-1] > scope.IDs[i] {
			t.Fatalf("ids = %v, want them sorted", scope.IDs)
		}
	}
	for _, id := range []int64{2, 5, 9} {
		if !scope.Allows(id) {
			t.Errorf("project %d was refused", id)
		}
	}
	if scope.Allows(3) {
		t.Error("a project outside the scope was allowed")
	}
	if scope.Empty() {
		t.Error("a scope with ids reported itself as empty")
	}
}

func TestScopeOfCopiesItsInput(t *testing.T) {
	// The caller's slice is often the decoded grant, reused for the next
	// permission. Sharing the backing array would let one scope's sort reorder
	// another's, which is the kind of bug that only shows up under load.
	ids := []int64{3, 1}
	scope := ScopeOf(ids...)
	ids[0] = 99

	if scope.Allows(99) {
		t.Error("mutating the caller's slice changed the scope")
	}
}

func TestIntersect(t *testing.T) {
	// Used for a chain link that crosses projects: the caller must be able to
	// write both ends, so the two scopes are intersected rather than either one
	// trusted alone.
	cases := []struct {
		name  string
		a, b  ProjectScope
		allow []int64
		deny  []int64
	}{
		{
			name:  "all with all stays all",
			a:     AllProjects(),
			b:     AllProjects(),
			allow: []int64{1, 2, 3},
		},
		{
			name:  "all narrows to the other side",
			a:     AllProjects(),
			b:     ScopeOf(2),
			allow: []int64{2},
			deny:  []int64{1, 3},
		},
		{
			name:  "the other side narrows all",
			a:     ScopeOf(2),
			b:     AllProjects(),
			allow: []int64{2},
			deny:  []int64{1, 3},
		},
		{
			name:  "overlap only",
			a:     ScopeOf(1, 2, 3),
			b:     ScopeOf(2, 3, 4),
			allow: []int64{2, 3},
			deny:  []int64{1, 4},
		},
		{
			name: "no overlap reaches nothing",
			a:    ScopeOf(1),
			b:    ScopeOf(2),
			deny: []int64{1, 2},
		},
		{
			name: "the empty scope stays empty",
			a:    NoProjects(),
			b:    AllProjects(),
			deny: []int64{1, 2},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.a.Intersect(tc.b)
			for _, id := range tc.allow {
				if !got.Allows(id) {
					t.Errorf("project %d was refused, want allowed", id)
				}
			}
			for _, id := range tc.deny {
				if got.Allows(id) {
					t.Errorf("project %d was allowed, want refused", id)
				}
			}
		})
	}
}
