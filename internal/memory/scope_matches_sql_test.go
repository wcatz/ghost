package memory

import "testing"

// TestScopeMatchesSQLAgreesWithScopeMatches pins the SQL form of the request-side
// scope rule against ScopeMatches (#573).
func TestScopeMatchesSQLAgreesWithScopeMatches(t *testing.T) {
	s, ctx := newDedupStore(t)
	insertRaw := func(name, raw string) string {
		t.Helper()
		var scopeVal any
		if raw != "\x00NULL" {
			scopeVal = raw
		}
		id, err := s.Create(ctx, testProject, Memory{Category: "fact", Content: name, Source: "manual", Importance: 0.7})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.ExecContext(ctx, `UPDATE memories SET scope = ? WHERE id = ?`, scopeVal, id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	cases := []struct {
		name   string
		stored map[string]string
		want   map[string]string
		raw    string
	}{
		{"request naming nothing matches everything", map[string]string{"environment": "production"}, map[string]string{}, ""},
		{"nil request matches everything", map[string]string{"environment": "production"}, nil, ""},
		{"same value matches", map[string]string{"environment": "production"}, map[string]string{"environment": "production"}, ""},
		{"different value does not match", map[string]string{"environment": "production"}, map[string]string{"environment": "development"}, ""},
		{"unmentioned key is not disagreement", map[string]string{"component": "api"}, map[string]string{"environment": "development"}, ""},
		{"missing key is not disagreement", map[string]string{"environment": "production"}, map[string]string{"component": "api"}, ""},
		{"one matching one disagreeing", map[string]string{"environment": "production", "component": "api"}, map[string]string{"environment": "production", "component": "worker"}, ""},
		{"unscoped row matches any request", nil, map[string]string{"environment": "development"}, "\x00NULL"},
		{"empty object is not a scope", nil, map[string]string{"environment": "development"}, "{}"},
		{"malformed scope is not a scope", nil, map[string]string{"environment": "development"}, "{oops"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			raw := c.raw
			if raw == "" {
				raw = scopeJSONString(c.stored)
			}
			id := insertRaw(c.name, raw)
			var gotSQL int
			query := `SELECT CASE WHEN ` + ScopeMatchesSQL("scope", c.want) + ` THEN 1 ELSE 0 END FROM memories WHERE id = ?`
			if err := s.db.QueryRowContext(ctx, query, id).Scan(&gotSQL); err != nil {
				t.Fatal(err)
			}
			if got, want := gotSQL == 1, ScopeMatches(c.stored, c.want); got != want {
				t.Fatalf("sql=%v go=%v", got, want)
			}
		})
	}
}

func TestScopeMatchesSQLReadsARequestNeedingEscaping(t *testing.T) {
	s, ctx := newDedupStore(t)
	stored := map[string]string{`qu"ote`: `it's production`}
	id, err := s.Create(ctx, testProject, Memory{Category: "fact", Content: "escaped request", Source: "manual", Importance: 0.7, Scope: stored})
	if err != nil {
		t.Fatal(err)
	}
	eligible := func(want map[string]string) bool {
		t.Helper()
		var got int
		query := `SELECT CASE WHEN ` + ScopeMatchesSQL("scope", want) + ` THEN 1 ELSE 0 END FROM memories WHERE id = ?`
		if err := s.db.QueryRowContext(ctx, query, id).Scan(&got); err != nil {
			t.Fatal(err)
		}
		return got == 1
	}
	if !eligible(stored) {
		t.Fatal("expected eligible")
	}
	other := map[string]string{`qu"ote`: `it's development`}
	if got, want := eligible(other), ScopeMatches(stored, other); got != want {
		t.Fatalf("sql=%v go=%v", got, want)
	}
}

func TestScopeMatchesSQLExcludesTheNonStringValueGoAccepts(t *testing.T) {
	s, ctx := newDedupStore(t)
	id, err := s.Create(ctx, testProject, Memory{Category: "fact", Content: "a hand-edited scope", Source: "manual", Importance: 0.7})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE memories SET scope = '{"environment":1}' WHERE id = ?`, id); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"environment": "production"}
	var got int
	query := `SELECT CASE WHEN ` + ScopeMatchesSQL("scope", want) + ` THEN 1 ELSE 0 END FROM memories WHERE id = ?`
	if err := s.db.QueryRowContext(ctx, query, id).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if !ScopeMatches(nil, want) || got == 1 {
		t.Fatalf("sql=%v", got == 1)
	}
}

func TestParseScopeJSON(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want map[string]string
	}{
		{"no column", "", nil},
		{"empty document", `{}`, nil},
		{"malformed", "{oops", nil},
		{"array", `["environment"]`, nil},
		{"non-string value", `{"environment":1}`, nil},
		{"one key", `{"environment":"production"}`, map[string]string{"environment": "production"}},
		{"two keys", `{"environment":"production","component":"api"}`, map[string]string{"environment": "production", "component": "api"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ParseScopeJSON([]byte(c.raw))
			if len(got) != len(c.want) {
				t.Fatalf("got=%v want=%v", got, c.want)
			}
			for k, v := range c.want {
				if got[k] != v {
					t.Fatalf("got=%v want=%v", got, c.want)
				}
			}
		})
	}
}
