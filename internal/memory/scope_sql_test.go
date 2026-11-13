package memory

import (
	"encoding/json"
	"testing"
)

func TestScopesConflictSQLAgreesWithScopesConflict(t *testing.T) {
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
		name       string
		a, b       map[string]string
		rawA, rawB string
	}{
		{"same value agrees", map[string]string{"environment": "production"}, map[string]string{"environment": "production"}, "", ""},
		{"different value conflicts", map[string]string{"environment": "production"}, map[string]string{"environment": "development"}, "", ""},
		{"symmetric conflict", map[string]string{"environment": "development"}, map[string]string{"environment": "production"}, "", ""},
		{"unscoped against scoped agrees", nil, map[string]string{"environment": "development"}, "", ""},
		{"scoped against unscoped agrees", map[string]string{"environment": "development"}, nil, "", ""},
		{"disjoint keys agree", map[string]string{"environment": "production"}, map[string]string{"component": "api"}, "", ""},
		{"one conflict outweighs agreement", map[string]string{"environment": "production", "component": "api"}, map[string]string{"environment": "development", "component": "api"}, "", ""},
		{"both unscoped agree", nil, nil, "", ""},
		{"empty object is not a scope", nil, nil, "{}", `{"environment":"production"}`},
		{"malformed scope is not a scope", nil, nil, "{oops", `{"environment":"production"}`},
		{"malformed against malformed agrees", nil, nil, "{oops", "]nope"},
		{"null against anything agrees", nil, nil, "\x00NULL", `{"environment":"production"}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rawA, rawB := c.rawA, c.rawB
			if rawA == "" {
				rawA = scopeJSONString(c.a)
			}
			if rawB == "" {
				rawB = scopeJSONString(c.b)
			}
			idA, idB := insertRaw("a-"+c.name, rawA), insertRaw("b-"+c.name, rawB)
			var gotSQL int
			query := `SELECT CASE WHEN ` + scopesConflictSQL("a.scope", "b.scope") + ` THEN 1 ELSE 0 END FROM memories a, memories b WHERE a.id = ? AND b.id = ?`
			if err := s.db.QueryRowContext(ctx, query, idA, idB).Scan(&gotSQL); err != nil {
				t.Fatal(err)
			}
			if got, want := gotSQL == 1, ScopesConflict(c.a, c.b); got != want {
				t.Fatalf("sql=%v go=%v", got, want)
			}
		})
	}
}

func scopeJSONString(scope map[string]string) string {
	if len(scope) == 0 {
		return "\x00NULL"
	}
	b, err := json.Marshal(scope)
	if err != nil {
		panic(err)
	}
	return string(b)
}
