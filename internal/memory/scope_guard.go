package memory

import (
	"fmt"
	"sort"
)

// The write-time bounds on a memory's scope. A scope is a handful of short labels
// — {"environment": "production", "component": "api"} — and the shared item line
// prints every key and value of it on every listing, outside the data delimiters,
// so an unbounded one is a megabyte echoed into every answer that touches the row.
//
// The numbers follow the caps beside them rather than introducing a new scale: a
// key is as long as a tag (the MCP tools cut a tag at 64 bytes), a value is as long
// as an agent (MaxAgentLen, 128 bytes), and the count equals maxScopeKeys, the
// number of keys an explanation names. All three are refused rather than clamped,
// for the reason MaxSourceRefLen is: a shortened key is a different key, and a
// scope that silently matched something other than what the caller wrote would
// filter retrieval on a label nobody stated.
const (
	// MaxScopeKeys is how many keys one scope may carry.
	MaxScopeKeys = 16
	// MaxScopeKeyLen is the byte length of one scope key.
	MaxScopeKeyLen = 64
	// MaxScopeValueLen is the byte length of one scope value.
	MaxScopeValueLen = 128
)

// checkScope is the whole of what a scope is checked for before it reaches SQL:
// the bounds, then the token guard over every key and every key=value pair. It is one
// function because the writers that store a scope are several — Create and
// CreateWithID (checkMemoryWrite), UpsertWithOptions and the portable importer
// (CheckImportedMemory) — and two copies of a guard eventually disagree about
// which half they apply.
//
// The bounds run first and quote nothing, so an over-long value that is also a
// token is refused for its length and never printed. The guard then names a key by
// position and a pair by its (already guarded, already shape-safe) key, so a
// refusal never carries the offending text. Keys are walked in sorted order so the
// field a caller is told about does not change between two identical calls.
//
// An empty or nil scope is no scope and passes.
func checkScope(scope map[string]string) error {
	if len(scope) == 0 {
		return nil
	}
	if len(scope) > MaxScopeKeys {
		return fmt.Errorf("scope must have at most %d keys, got %d — it is a few short labels such as environment and component, not a document", MaxScopeKeys, len(scope))
	}
	keys := make([]string, 0, len(scope))
	for k := range scope {
		keys = append(keys, k)
	}
	// Sorted before ANY check, the length bounds included: map iteration order is
	// random, so a scope that breaches two bounds would otherwise name a different
	// one on each call.
	sort.Strings(keys)
	for _, k := range keys {
		if len(k) > MaxScopeKeyLen {
			return fmt.Errorf("a scope key must be at most %d bytes, got %d — it names a dimension such as environment, not a document", MaxScopeKeyLen, len(k))
		}
		if v := scope[k]; len(v) > MaxScopeValueLen {
			return fmt.Errorf("a scope value must be at most %d bytes, got %d — it is a short label such as production, not a document", MaxScopeValueLen, len(v))
		}
	}
	for i, k := range keys {
		if err := rejectSecret(fmt.Sprintf("scope_key[%d]", i), k); err != nil {
			return err
		}
	}
	for _, k := range keys {
		field := "scope[" + SafeToken(k) + "]"
		// The pair rather than the value alone: the detector reads a bare value by
		// shape and an assigned one by its key and value together, and the pair
		// is a superset of both — a token under any key, and a harmless-looking
		// value under a key that names a secret.
		if err := rejectSecret(field, k+"="+scope[k]); err != nil {
			return err
		}
	}
	return nil
}
