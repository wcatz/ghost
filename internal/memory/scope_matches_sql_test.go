package memory

import (
	"testing"
)

// TestScopeMatchesSQLAgreesWithScopeMatches is the pin for the SQL form of the
// request-side scope rule, the sibling of TestScopesConflictSQLAgreesWithScopesConflict.
//
// ScopeMatches is the rule every scope consumer asks: the assembler applies it
// in Go over the rows retrieval returned, while a reader that chooses its
// candidate set with the same statement's LIMIT has to decide inside the cut —
// a check after it would spend the fetch budget on rows the session does not
// want and miss an eligible one ranked just below, which is the defect
// #573 was filed for on the search surface. The session-start loaders are that
// reader. So the rule is stated again in SQL, over one stored column and a
// request, and this test runs every case the Go rule covers through both forms,
// so a change to either side the other does not follow fails here.
func TestScopeMatchesSQLAgreesWithScopeMatches(t *testing.T) {
	s, ctx := newDedupStore(t)

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
		// stored is the scope as the Go rule reads it, which for a NULL, empty
		// or unreadable column is no scope at all.
		stored map[string]string
		want   map[string]string
		// raw is the exact bytes to store, when they are not what marshalling
		// stored would produce — the shapes a save never writes.
		raw string
	}{
		{
			name:   "a request naming nothing matches everything",
			stored: map[string]string{"environment": "production"},
			want:   map[string]string{},
		},
		{
			name:   "a nil request matches everything",
			stored: map[string]string{"environment": "production"},
		},
		{
			name:   "the same value on a requested key matches",
			stored: map[string]string{"environment": "production"},
			want:   map[string]string{"environment": "production"},
		},
		{
			name:   "a different value on a requested key does not match",
			stored: map[string]string{"environment": "production"},
			want:   map[string]string{"environment": "development"},
		},
		{
			name:   "an unmentioned key is not disagreement",
			stored: map[string]string{"component": "api"},
			want:   map[string]string{"environment": "development"},
		},
		{
			name:   "a key the row does not carry at all is not disagreement",
			stored: map[string]string{"environment": "production"},
			want:   map[string]string{"component": "api"},
		},
		{
			name:   "one matching key beside one disagreeing key does not match",
			stored: map[string]string{"environment": "production", "component": "api"},
			want:   map[string]string{"environment": "production", "component": "worker"},
		},
		{
			name: "an unscoped row matches any request",
			want: map[string]string{"environment": "development"},
			raw:  "\x00NULL",
		},
		{
			// A stored '{}' is the shape an emptied scope can take in a row
			// written before the NULL convention; parseScope reads it as no
			// scope, so it has to match every request here too.
			name: "an empty object is not a scope",
			want: map[string]string{"environment": "development"},
			raw:  "{}",
		},
		{
			// parseScope reads a value it cannot decode as no scope and does not
			// fail the read; json_each on a malformed document raises instead, so
			// the predicate has to guard the decode or one bad row would take the
			// whole statement down — and a session-start loader reads a failed
			// query as no rows at all.
			name: "a malformed scope is not a scope",
			want: map[string]string{"environment": "development"},
			raw:  "{oops",
		},
	}

	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			raw := c.raw
			if raw == "" {
				raw = scopeJSONString(c.stored)
			}
			id := insertRaw("case-"+c.name, raw)

			var gotSQL int
			query := `SELECT CASE WHEN ` + ScopeMatchesSQL("scope", c.want) + ` THEN 1 ELSE 0 END
				FROM memories WHERE id = ?`
			if err := s.db.QueryRowContext(ctx, query, id).Scan(&gotSQL); err != nil {
				t.Fatalf("scope predicate query: %v", err)
			}
			got := gotSQL == 1
			want := ScopeMatches(c.stored, c.want)
			if got != want {
				t.Errorf("case %d (%s): SQL says eligible=%v, ScopeMatches says %v — the SQL form of the rule has drifted from the Go one",
					i, c.name, got, want)
			}
		})
	}
}

// TestScopeMatchesSQLReadsARequestNeedingEscaping covers the request side of the
// same pin for the values a hand-written config can carry: a key encoding/json
// has to escape, and a value holding a single quote — the character a SQL string
// literal is delimited by. A literal that did not double that quote would end
// the statement early, and the session-start loader reads a failed query as no
// memories at all.
func TestScopeMatchesSQLReadsARequestNeedingEscaping(t *testing.T) {
	s, ctx := newDedupStore(t)

	stored := map[string]string{`qu"ote`: `it's production`}
	id, err := s.Create(ctx, testProject, Memory{
		Category: "fact", Content: "escaped request", Source: "manual", Importance: 0.7,
		Scope: stored,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	eligible := func(want map[string]string) bool {
		t.Helper()
		var got int
		query := `SELECT CASE WHEN ` + ScopeMatchesSQL("scope", want) + ` THEN 1 ELSE 0 END
			FROM memories WHERE id = ?`
		if err := s.db.QueryRowContext(ctx, query, id).Scan(&got); err != nil {
			t.Fatalf("scope predicate query for %v: %v", want, err)
		}
		return got == 1
	}

	if !eligible(stored) {
		t.Error("a request whose key needs JSON escaping and whose value holds a single quote: SQL says the row is not eligible, want eligible")
	}
	other := map[string]string{`qu"ote`: `it's development`}
	if got, want := eligible(other), ScopeMatches(stored, other); got != want {
		t.Errorf("a disagreeing value behind a single quote: SQL says eligible=%v, ScopeMatches says %v", got, want)
	}
}

// TestParseScopeJSON pins the exported read of a stored scope column, the one
// the session-start loaders use because they reach the column through their own
// read-only handle rather than through a Store scan. The shapes it must refuse
// are the ones parseScope refuses: a column nobody wrote, an empty one, and one
// holding something that is not a scope. A memory must not be dropped over a bad
// side-channel field.
func TestParseScopeJSON(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want map[string]string
	}{
		{name: "no column", raw: ""},
		{name: "empty document", raw: `{}`},
		{name: "malformed document", raw: "{oops"},
		{name: "a JSON array is not a scope", raw: `["environment"]`},
		{name: "a non-string value is not a scope", raw: `{"environment":1}`},
		{name: "one key", raw: `{"environment":"production"}`, want: map[string]string{"environment": "production"}},
		{name: "two keys", raw: `{"environment":"production","component":"api"}`,
			want: map[string]string{"environment": "production", "component": "api"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ParseScopeJSON([]byte(c.raw))
			if len(got) != len(c.want) {
				t.Fatalf("ParseScopeJSON(%q) = %v, want %v", c.raw, got, c.want)
			}
			for k, v := range c.want {
				if got[k] != v {
					t.Errorf("ParseScopeJSON(%q)[%q] = %q, want %q", c.raw, k, got[k], v)
				}
			}
		})
	}
}
