package memory

import (
	"database/sql"
	"encoding/json"
	"strings"
)

func scopeJSON(scope map[string]string) any {
	if len(scope) == 0 {
		return nil
	}
	b, err := json.Marshal(scope)
	if err != nil || len(b) == 0 {
		return nil
	}
	return string(b)
}

func parseScope(raw sql.NullString) map[string]string {
	if !raw.Valid {
		return nil
	}
	return parseScopeJSON([]byte(raw.String))
}

func parseScopeJSON(raw []byte) map[string]string {
	if len(raw) == 0 {
		return nil
	}
	var m map[string]string
	if err := json.Unmarshal(raw, &m); err != nil || len(m) == 0 {
		return nil
	}
	return m
}

// ParseScopeJSON turns a stored memories.scope column back into a map. It is
func ParseScopeJSON(raw []byte) map[string]string { return parseScopeJSON(raw) }

// ScopeMatches reports whether a memory's scope satisfies a request.
func ScopeMatches(scope, want map[string]string) bool {
	for key, wantVal := range want {
		if got, mentioned := scope[key]; mentioned && got != wantVal {
			return false
		}
	}
	return true
}

// ScopeEquals reports whether two scopes name exactly the same key/value
func ScopeEquals(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if got, ok := b[k]; !ok || got != v {
			return false
		}
	}
	return true
}

// ScopesConflict reports whether two memories assert different places.
func ScopesConflict(a, b map[string]string) bool {
	for key, aVal := range a {
		if bVal, mentioned := b[key]; mentioned && aVal != bVal {
			return true
		}
	}
	return false
}

func scopesConflictSQL(a, b string) string {
	return `(EXISTS (
		SELECT 1
		FROM json_each(` + scopeJSONExpr(a) + `) ja
		JOIN json_each(` + scopeJSONExpr(b) + `) jb
		  ON ja.key = jb.key AND ja.value <> jb.value
	))`
}

func scopeJSONExpr(scopeExpr string) string {
	return `CASE WHEN json_valid(` + scopeExpr + `) THEN ` + scopeExpr + ` ELSE '{}' END`
}

// ScopeMatchesSQL renders ScopeMatches as a SQL boolean expression: 1 exactly
func ScopeMatchesSQL(scopeExpr string, want map[string]string) string {
	if len(want) == 0 {
		return "1"
	}
	b, err := json.Marshal(want)
	if err != nil {
		return "1"
	}
	quoted := strings.ReplaceAll(string(b), "'", "''")
	return `NOT ` + scopesConflictSQL(scopeExpr, `'`+quoted+`'`)
}
