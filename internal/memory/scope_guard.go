package memory

import (
	"fmt"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
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
// for the reason MaxSourceLen is: a shortened key is a different key, and a
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

// ScopeBoundError is a scope that breaks one of the bounds above. It carries what
// a report needs to say which bound without re-parsing the message: the Kind, the
// limit and the measured size, and — for a value — the KEY it was stored under,
// which the bounds check has already held to MaxScopeKeyLen. A key that is itself
// over its bound is never carried, so no field here can be as long as the scope
// it describes. Error keeps the sentence the write paths have always returned.
type ScopeBoundError struct {
	// Kind is ScopeTooManyKeys, ScopeKeyTooLong or ScopeValueTooLong.
	Kind string
	// Key is the key whose value is over its bound, and empty for the other kinds.
	Key string
	// Limit is the bound that was broken and Got the size that broke it.
	Limit, Got int
}

// The Kinds of ScopeBoundError.
const (
	ScopeTooManyKeys  = "keys"
	ScopeKeyTooLong   = "key"
	ScopeValueTooLong = "value"
)

func (e *ScopeBoundError) Error() string {
	switch e.Kind {
	case ScopeTooManyKeys:
		return fmt.Sprintf("scope must have at most %d keys, got %d — it is a few short labels such as environment and component, not a document", e.Limit, e.Got)
	case ScopeKeyTooLong:
		return fmt.Sprintf("a scope key must be at most %d bytes, got %d — it names a dimension such as environment, not a document", e.Limit, e.Got)
	default:
		return fmt.Sprintf("a scope value must be at most %d bytes, got %d — it is a short label such as production, not a document", e.Limit, e.Got)
	}
}

// ScopeKeyValueError is a scope key or value that breaks the character rules.
// It carries the field name (as the token guard does), the reason class, and
// for value errors the original key (so the export can screen it for credentials
// before SafeToken rendering).
type ScopeKeyValueError struct {
	Field, Reason, Key string
}

func (e *ScopeKeyValueError) Error() string {
	return fmt.Sprintf("%s must not be %s", e.Field, e.Reason)
}

// checkScope is the whole of what a scope is checked for before it reaches SQL:
// the bounds, the character rules (blank, NUL, invalid UTF-8, control, line-break),
// then the token guard over every key and every key=value pair. It is one
// function because the writers that store a scope are several — Create and
// CreateWithID (checkMemoryWrite), UpsertWithOptions and the portable importer
// (CheckImportedMemory) — and two copies of a guard eventually disagree about
// which half they apply.
//
// The bounds run first and quote nothing, so an over-long value that is also a
// token is refused for its length and never printed. The character rules run
// second, naming a key by position and a value by its (already bounded) key,
// so a refusal never carries the offending text. The guard then names a key by
// position and a pair by its (already guarded, already shape-safe) key. Keys are
// walked in sorted order so the field a caller is told about does not change
// between two identical calls.
//
// An empty or nil scope is no scope and passes.
func checkScope(scope map[string]string) error {
	if len(scope) == 0 {
		return nil
	}
	if len(scope) > MaxScopeKeys {
		return &ScopeBoundError{Kind: ScopeTooManyKeys, Limit: MaxScopeKeys, Got: len(scope)}
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
			return &ScopeBoundError{Kind: ScopeKeyTooLong, Limit: MaxScopeKeyLen, Got: len(k)}
		}
		if v := scope[k]; len(v) > MaxScopeValueLen {
			return &ScopeBoundError{Kind: ScopeValueTooLong, Key: k, Limit: MaxScopeValueLen, Got: len(v)}
		}
	}
	// Key character rules by position, then the token guard over every key, and
	// only then the value character rules: a value refusal names its key, so the
	// key must already have been screened or a token-shaped key would be printed.
	for i, k := range keys {
		if err := validateScopeKeyValue(fmt.Sprintf("scope_key[%d]", i), k, ""); err != nil {
			return err
		}
	}
	for i, k := range keys {
		if err := rejectSecret(fmt.Sprintf("scope_key[%d]", i), k); err != nil {
			return err
		}
	}
	for _, k := range keys {
		if err := validateScopeKeyValue(fmt.Sprintf("scope[%s]", SafeToken(k)), scope[k], k); err != nil {
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
	// Invisible Unicode FORMAT characters (category Cf) are refused last, after
	// the token guard, so a credential-shaped value carrying one is refused as
	// a credential — the same refusal the value gets without the character —
	// rather than as a character. A value holding nothing but such characters
	// is already refused as blank above.
	for i, k := range keys {
		if containsFormatRune(k) {
			return &ScopeKeyValueError{Field: fmt.Sprintf("scope_key[%d]", i), Reason: "an invisible Unicode FORMAT character", Key: ""}
		}
	}
	for _, k := range keys {
		if containsFormatRune(scope[k]) {
			return &ScopeKeyValueError{Field: fmt.Sprintf("scope[%s]", SafeToken(k)), Reason: "an invisible Unicode FORMAT character", Key: k}
		}
	}
	return nil
}

func containsFormatRune(s string) bool {
	for _, r := range s {
		if IsFormatRune(r) {
			return true
		}
	}
	return false
}

// validateScopeKeyValue refuses a scope key or value that is blank, carries a NUL
// byte, is not valid UTF-8, contains a control character, an invisible Unicode
// FORMAT character (category Cf), or contains a line separator (U+2028, U+2029)
// or next line (U+0085). The field is the caller's
// name for the argument (e.g. "scope_key[0]" or "scope[environment]") so the
// refusal is actionable without quoting the offending text. For value errors,
// key is the original (unrendered) scope key so the export can screen it for
// credentials.
func validateScopeKeyValue(field, value, key string) error {
	// Blank: empty or only whitespace/format characters.
	if strings.TrimFunc(value, func(r rune) bool {
		return unicode.IsSpace(r) || IsFormatRune(r)
	}) == "" {
		return &ScopeKeyValueError{Field: field, Reason: "empty or blank", Key: key}
	}
	// Invalid UTF-8.
	if !utf8.ValidString(value) {
		return &ScopeKeyValueError{Field: field, Reason: "invalid UTF-8", Key: key}
	}
	// NUL byte.
	if strings.IndexByte(value, 0) >= 0 {
		return &ScopeKeyValueError{Field: field, Reason: "a NUL byte", Key: key}
	}
	// Control characters and line separators (U+2028 LS, U+2029 PS, U+0085
	// NEL): these carry no width of their own, so a key or value mixing them
	// with ordinary text hides a spelling from a reader. Invisible Unicode
	// FORMAT characters (category Cf, e.g. U+200B) are refused separately,
	// below, so that a credential-shaped value carrying one is still refused
	// as a credential rather than as a character.
	for _, r := range value {
		if unicode.IsControl(r) || r == 0x2028 || r == 0x2029 {
			return &ScopeKeyValueError{Field: field, Reason: "a control character or line separator", Key: key}
		}
	}
	return nil
}
