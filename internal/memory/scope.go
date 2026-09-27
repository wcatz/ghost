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
	if !raw.Valid {
		return nil
	}
	return parseScopeJSON([]byte(raw.String))
}

// parseScopeJSON is parseScope over the raw column text, for callers that
// already hold it as bytes — the vector search does, because it copies the
// column rather than decoding a string per row of the corpus, and a scan that
// parsed every scope to find the few rows that win a result slot was paying a
// json.Unmarshal per memory per query.
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

// ScopesConflict reports whether two memories assert different places.
//
// It answers the question ScopeMatches cannot: not "does this row satisfy a
// request", but "could these two rows be the same claim in different words".
// They cannot when each names a shared key with a different value —
// environment=production and environment=development are two facts about two
// places, however nearly their sentences are worded.
//
// Silence is not disagreement. If either side does not mention a key, there
// is no conflict to find, so an unscoped memory is compatible with every
// scope. That is not a convenience but the honest answer — a fact that names
// no environment asserts nothing about environment — and it carries a
// second benefit: every memory written before schema v12 has no scope, so
// this rule leaves dedup exactly where it was for the whole existing store,
// activating only where scope genuinely disagrees.
//
// The rule is symmetric. A fold target is chosen by text similarity from
// both directions, so a conflict has to block the edge whichever side
// arrives second; a one-way test would let a repeat save reinstate the edge
// it just refused.
func ScopesConflict(a, b map[string]string) bool {
	for key, aVal := range a {
		if bVal, mentioned := b[key]; mentioned && aVal != bVal {
			return true
		}
	}
	return false
}

// scopesConflictSQL renders ScopesConflict as a SQL predicate over two
// expressions that each yield a memories.scope value. It returns a boolean SQL
// expression that is 1 (true) exactly when ScopesConflict is true for the two
// stored scopes.
//
// It is a second statement of one rule, and that is a cost with a purpose. Every
// statement that needs it chooses its candidates with its own LIMIT — Upsert's
// two dedup probes and the two fold-target liveness checks — so a scope check
// applied after the cut would spend the candidate budget on rows the caller may
// not fold into and miss a compatible one ranked just below (#665). A second
// statement is the only way to decide inside the cut.
//
// Two copies of a rule drift, so TestScopesConflictSQLAgreesWithScopesConflict
// runs both forms over the same table and requires they answer identically,
// including the shapes a save would never produce (NULL, '{}', malformed JSON).
// Change one without the other and that test fails.
//
// json_each is SQLite's table-valued function over a JSON object, so a shared
// key with two different values is a self-join on key. json_valid guards each
// side: parseScope reads a value it cannot decode as no scope and deliberately
// does not fail the read, whereas json_each on a malformed document raises an
// error — and the callers treat a failed query as "no candidates", so an
// unguarded decode would make one bad row invisible to dedup rather than merely
// unscoped.
//
// Values are compared with <> on the JSON text. A scope value is always a
// string: scopeJSON marshals a map[string]string, and a non-string in the column
// would be a hand-edited row rather than something Ghost wrote. Go's decoder
// rejects such a row into "no scope" while this predicate would see a
// difference; the asymmetry needs a hand-edited database to reach and is
// recorded here rather than paid for with a stricter predicate that costs an
// explicit type check on every comparison.
func scopesConflictSQL(a, b string) string {
	return `(EXISTS (
		SELECT 1
		FROM json_each(` + scopeJSONExpr(a) + `) ja
		JOIN json_each(` + scopeJSONExpr(b) + `) jb
		  ON ja.key = jb.key AND ja.value <> jb.value
	))`
}

// scopeJSONExpr wraps a scope expression so a NULL, an empty document or a
// malformed one reads as the empty object — no keys, so no conflict can be
// found — instead of aborting the statement. The empty document matters on its
// own: scopeJSON never writes one (an empty scope is stored as NULL), but a row
// written before that convention, or by an import, can carry '{}', and parseScope
// already reads it as no scope.
//
// It names its expression TWICE, so a caller that passes a bound parameter ("?")
// as the expression must bind that argument to both placeholders it produces —
// a query that binds it once fails to prepare, which the probe reads as "no
// candidate" rather than as the error it is.
func scopeJSONExpr(scopeExpr string) string {
	return `CASE WHEN json_valid(` + scopeExpr + `) THEN ` + scopeExpr + ` ELSE '{}' END`
}
