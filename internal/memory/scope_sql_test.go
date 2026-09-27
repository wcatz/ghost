package memory

import (
	"encoding/json"
	"testing"
)

// TestScopesConflictSQLAgreesWithScopesConflict is the pin that makes the
// duplicated rule safe.
//
// ScopesConflict is one rule, and it is a Go function. Two fold-target
// liveness checks have to ask the same question from inside a single SQL
// statement, because the candidate set is chosen by that statement's LIMIT —
// filtering after the cut would spend the candidate budget on rows the caller
// may not fold into, which is the defect the store-side narrowing exists to
// avoid. So the rule is stated a second time, in SQL, over the same two scope
// columns.
//
// Two copies of one rule drift. This test is what stops them: every case the
// Go table covers is run through the SQL predicate too, so a change to either
// side that the other does not follow fails here rather than in a store.
func TestScopesConflictSQLAgreesWithScopesConflict(t *testing.T) {
	s, ctx := newDedupStore(t)

	// A row with a given stored scope, inserted directly so the cases below
	// can include the shapes a save would never produce: a NULL scope, an
	// empty JSON object, and a malformed value.
	insertRaw := func(name, storedScope string) string {
		t.Helper()
		var scopeVal any
		if storedScope != "\x00NULL" {
			scopeVal = storedScope
		}
		id, err := s.Create(ctx, testProject, Memory{
			Category: "fact", Content: name, Source: "manual", Importance: 0.7,
		})
		if err != nil {
			t.Fatalf("Create(%s): %v", name, err)
		}
		if _, err := s.db.ExecContext(ctx, `UPDATE memories SET scope = ? WHERE id = ?`, scopeVal, id); err != nil {
			t.Fatalf("set scope(%s): %v", name, err)
		}
		return id
	}

	cases := []struct {
		name string
		a, b map[string]string
		// The exact bytes stored in memories.scope, so the malformed and
		// empty-object cases are reachable.
		rawA, rawB string
	}{
		{
			name: "same value on a shared key agrees",
			a:    map[string]string{"environment": "production"},
			b:    map[string]string{"environment": "production"},
		},
		{
			name: "different value on a shared key conflicts",
			a:    map[string]string{"environment": "production"},
			b:    map[string]string{"environment": "development"},
		},
		{
			name: "conflict is symmetric",
			a:    map[string]string{"environment": "development"},
			b:    map[string]string{"environment": "production"},
		},
		{
			name: "unscoped against scoped agrees",
			a:    nil,
			b:    map[string]string{"environment": "development"},
		},
		{
			name: "scoped against unscoped agrees",
			a:    map[string]string{"environment": "development"},
			b:    nil,
		},
		{
			name: "disjoint keys agree",
			a:    map[string]string{"environment": "production"},
			b:    map[string]string{"component": "api"},
		},
		{
			name: "one shared key conflicting outweighs the agreeing ones",
			a:    map[string]string{"environment": "production", "component": "api"},
			b:    map[string]string{"environment": "development", "component": "api"},
		},
		{
			name: "both unscoped agree",
		},
		{
			// A stored '{}' is what a save writes for a map that became
			// empty; parseScope reads it as no scope, and the SQL must agree
			// rather than treating it as a scope that conflicts with nothing
			// in particular.
			name: "empty object is not a scope",
			rawA: "{}", rawB: `{"environment":"production"}`,
		},
		{
			// parseScope treats a value it cannot decode as no scope, and
			// deliberately does not fail the read. json_each on a malformed
			// document raises an error instead, so the SQL has to guard the
			// decode itself — otherwise one bad row would take down the whole
			// probe query, and the probe treats an error as "no candidate",
			// which is how a row becomes invisible to dedup.
			name: "malformed scope is not a scope",
			rawA: "{oops", rawB: `{"environment":"production"}`,
		},
		{
			name: "malformed against malformed agrees",
			rawA: "{oops", rawB: "]nope",
		},
		{
			name: "null against anything agrees",
			rawA: "\x00NULL", rawB: `{"environment":"production"}`,
		},
	}

	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rawA, rawB := c.rawA, c.rawB
			if rawA == "" {
				rawA = scopeJSONString(c.a)
			}
			if rawB == "" {
				rawB = scopeJSONString(c.b)
			}
			idA := insertRaw("case-a-"+c.name, rawA)
			idB := insertRaw("case-b-"+c.name, rawB)

			var gotSQL int
			query := `SELECT CASE WHEN ` + scopesConflictSQL("a.scope", "b.scope") + ` THEN 1 ELSE 0 END
				FROM memories a, memories b WHERE a.id = ? AND b.id = ?`
			if err := s.db.QueryRowContext(ctx, query, idA, idB).Scan(&gotSQL); err != nil {
				t.Fatalf("scope predicate query: %v", err)
			}
			got := gotSQL == 1
			want := ScopesConflict(c.a, c.b)
			if got != want {
				t.Errorf("case %d (%s): SQL says conflict=%v, ScopesConflict says %v — the SQL form of the rule has drifted from the Go one",
					i, c.name, got, want)
			}
		})
	}
}

// scopeJSONString is the stored form of a scope, or the NULL sentinel for a
// nil scope, matching what scopeJSON writes on a save.
func scopeJSONString(scope map[string]string) string {
	if len(scope) == 0 {
		return "\x00NULL"
	}
	b, err := json.Marshal(scope)
	if err != nil {
		panic(err) // a map[string]string always marshals
	}
	return string(b)
}
