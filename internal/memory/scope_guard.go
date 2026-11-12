package memory

import (
	"fmt"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	// MaxScopeKeys is the maximum number of keys in one scope.
	MaxScopeKeys = 16
	// MaxScopeKeyLen is the maximum byte length of one scope key.
	MaxScopeKeyLen = 64
	// MaxScopeValueLen is the maximum byte length of one scope value.
	MaxScopeValueLen = 128
)

// ScopeBoundError reports a scope that exceeds a configured bound.
type ScopeBoundError struct {
	Kind       string
	Key        string
	Limit, Got int
}

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

// ScopeKeyValueError reports a scope key or value that fails character validation.
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
	sort.Strings(keys)
	for _, k := range keys {
		if len(k) > MaxScopeKeyLen {
			return &ScopeBoundError{Kind: ScopeKeyTooLong, Limit: MaxScopeKeyLen, Got: len(k)}
		}
		if v := scope[k]; len(v) > MaxScopeValueLen {
			return &ScopeBoundError{Kind: ScopeValueTooLong, Key: k, Limit: MaxScopeValueLen, Got: len(v)}
		}
	}
	// Screen keys before values so a value refusal can name its key.
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
		// Check the key=value pair so key-sensitive detections apply.
		if err := rejectSecret(field, k+"="+scope[k]); err != nil {
			return err
		}
	}
	// Refuse FORMAT characters after token checks.
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
// byte, is not valid UTF-8, or contains a control character or a line separator
// (U+2028, U+2029) or next line (U+0085). Invisible Unicode FORMAT characters
// (category Cf) are refused separately, in checkScope, after the token guard.
// The field is the caller's
// name for the argument (e.g. "scope_key[0]" or "scope[environment]") so the
// refusal is actionable without quoting the offending text. For value errors,
// key is the original (unrendered) scope key so the export can screen it for
// credentials.
func validateScopeKeyValue(field, value, key string) error {
	if strings.TrimFunc(value, func(r rune) bool {
		return unicode.IsSpace(r) || IsFormatRune(r)
	}) == "" {
		return &ScopeKeyValueError{Field: field, Reason: "empty or blank", Key: key}
	}
	if !utf8.ValidString(value) {
		return &ScopeKeyValueError{Field: field, Reason: "invalid UTF-8", Key: key}
	}
	if strings.IndexByte(value, 0) >= 0 {
		return &ScopeKeyValueError{Field: field, Reason: "a NUL byte", Key: key}
	}
	for _, r := range value {
		if unicode.IsControl(r) || r == 0x2028 || r == 0x2029 {
			return &ScopeKeyValueError{Field: field, Reason: "a control character or line separator", Key: key}
		}
	}
	return nil
}
