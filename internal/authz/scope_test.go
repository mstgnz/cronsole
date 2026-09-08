package authz

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestNoScopesMeansUnrestricted(t *testing.T) {
	// grantz returns no scopes for a permission that is allowed without one, and
	// since v0.4.0 an unscoped grant clears the narrower scopes beside it. So an
	// empty list unambiguously means "every project" here. Against v0.3.0 the
	// same reading would have silently narrowed somebody who held both a wide
	// role and a project role.
	scope, err := scopeFromGrants(nil)
	if err != nil {
		t.Fatalf("no scopes gave an error: %v", err)
	}
	if !scope.All {
		t.Fatal("no scopes did not resolve to every project")
	}
	if scope.Empty() {
		t.Error("the unrestricted scope reported itself as empty")
	}
}

func TestScopeFromGrantsUnionsAndSorts(t *testing.T) {
	// Two assignments can each name projects; the caller reaches the union.
	// Deduplicated because the same project appearing under two roles is
	// ordinary, not a conflict.
	scope, err := scopeFromGrants([]map[string]any{
		{ScopeKey: []any{float64(3), float64(1)}},
		{ScopeKey: []any{float64(1), float64(7)}},
	})
	if err != nil {
		t.Fatal(err)
	}

	if scope.All {
		t.Fatal("a scoped grant resolved to every project")
	}
	if got, want := len(scope.IDs), 3; got != want {
		t.Fatalf("ids = %v, want %d after dedupe", scope.IDs, want)
	}
	for i := 1; i < len(scope.IDs); i++ {
		if scope.IDs[i-1] > scope.IDs[i] {
			t.Fatalf("ids = %v, want them sorted", scope.IDs)
		}
	}
	for _, id := range []int64{1, 3, 7} {
		if !scope.Allows(id) {
			t.Errorf("project %d was refused", id)
		}
	}
	if scope.Allows(2) {
		t.Error("a project outside both grants was allowed")
	}
}

func TestScopeAcceptsTheSingularKey(t *testing.T) {
	// The plural key is what this service writes. The singular one is what
	// somebody writes by hand after reading a one-project example, and refusing
	// it would fail closed in a way that looks like a permission bug.
	scope, err := scopeFromGrants([]map[string]any{{ScopeKeySingle: float64(4)}})
	if err != nil {
		t.Fatal(err)
	}
	if !scope.Allows(4) || scope.Allows(5) {
		t.Errorf("scope = %+v, want only project 4", scope)
	}
}

func TestUnreadableScopeIsAnError(t *testing.T) {
	// An unreadable restriction must not resolve to "nothing": that looks
	// identical to a correctly configured user with no projects, and the two
	// need opposite fixes. It must not resolve to "everything" either, which is
	// why the error is returned rather than swallowed.
	cases := []struct {
		name string
		raw  []map[string]any
	}{
		{"neither key", []map[string]any{{"tenant_id": float64(1)}}},
		{"plural key is not a list", []map[string]any{{ScopeKey: float64(1)}}},
		{"a list entry is not a number", []map[string]any{{ScopeKey: []any{"one"}}}},
		{"the singular key is not a number", []map[string]any{{ScopeKeySingle: "four"}}},
		{"one bad grant among good ones", []map[string]any{
			{ScopeKey: []any{float64(1)}},
			{"nonsense": true},
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			scope, err := scopeFromGrants(tc.raw)
			if err == nil {
				t.Fatalf("scope = %+v, want an error", scope)
			}
			if !scope.Empty() {
				t.Errorf("scope = %+v on failure, want it to reach nothing", scope)
			}
		})
	}
}

func TestScopeSurvivesJSONRoundTrip(t *testing.T) {
	// EncodeScope writes what grantz stores, and grantz hands it back decoded
	// into map[string]any with float64 numbers. The two halves have to agree, so
	// the test goes through real JSON rather than trusting the shapes to match.
	encoded, err := EncodeScope([]int64{9, 2, 2})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(encoded, ScopeKey) {
		t.Fatalf("encoded = %s, want the %q key", encoded, ScopeKey)
	}

	var decoded map[string]any
	if err := json.Unmarshal([]byte(encoded), &decoded); err != nil {
		t.Fatal(err)
	}

	scope, err := scopeFromGrants([]map[string]any{decoded})
	if err != nil {
		t.Fatal(err)
	}
	if !scope.Allows(2) || !scope.Allows(9) {
		t.Errorf("scope = %+v, want projects 2 and 9", scope)
	}
	if scope.Allows(5) {
		t.Error("a project that was never encoded was allowed")
	}
}

func TestRunViewFieldsCollapseToBooleans(t *testing.T) {
	// grantz distinguishes "no restriction" (nil) from "an empty allow-list",
	// and the interface only ever asks whether it may show a field. Collapsing
	// the two readings here rather than at every call site is the point.
	full := FullRunView()
	if !full.Output || !full.Error || !full.RequestURL {
		t.Errorf("the unrestricted view = %+v, want everything visible", full)
	}

	var none RunView
	if none.Output || none.Error || none.RequestURL {
		t.Errorf("the zero view = %+v, want nothing visible", none)
	}
}
