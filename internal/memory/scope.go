package memory

import (
	"database/sql"
	"encoding/json"
)

// scopeJSON marshals scope for storage, mapping an empty or nil scope to NULL
// rather than to '{}'.
//
// The distinction matters on read: '{}' parses to an empty map, which is
// indistinguishable in Go from "no scope", but in SQL it reads as a scope
// that was deliberately set to nothing. Keeping both forms NULL means the
// column answers exactly one question — was a scope ever stated? — instead of
// encoding two subtly different answers to it.
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

// parseScope turns the stored column back into a map, treating anything it
// cannot understand as no scope. A malformed value must not fail a read: the
// memory's content is still valid, and dropping it over a bad side-channel
// field would lose knowledge for a formatting problem.
func parseScope(raw sql.NullString) map[string]string {
	if !raw.Valid || raw.String == "" || raw.String == "{}" {
		return nil
	}
	var m map[string]string
	if err := json.Unmarshal([]byte(raw.String), &m); err != nil || len(m) == 0 {
		return nil
	}
	return m
}

// ScopeMatches reports whether a memory's scope satisfies a request.
//
// The rule is deliberately asymmetric, and it is the decision that makes scope
// usable rather than merely present: a memory that does not MENTION a
// requested key counts as in scope. A fact with no stated environment applies
// to every environment, so the alternative — requiring an explicit match —
// would make missing scope a reason to hide the most general, most reusable
// knowledge in the store. Only an explicit mention that disagrees excludes.
//
// Scope never invents agreement either. A row that says
// environment=development does not satisfy a request for production however
// similar the text reads, which is the whole point: the two stop being
// separated by wording and start being separated by a value a query can name.
//
// An empty or nil want matches everything, so requesting no scope is a no-op
// rather than a filter that drops everything.
func ScopeMatches(scope, want map[string]string) bool {
	for key, wantVal := range want {
		if got, mentioned := scope[key]; mentioned && got != wantVal {
			return false
		}
	}
	return true
}

// ScopeEquals reports whether two scopes name exactly the same key/value
// pairs. Used to decide whether a save actually changes scope, so an
// unchanged scope does not rewrite the row or bump updated_at.
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
