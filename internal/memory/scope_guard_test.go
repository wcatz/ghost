package memory

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// scopeToken is assembled rather than written out: GitHub push protection matches
// the PAT format anywhere in a diff.
func scopeToken() string { return "ghp_" + strings.Repeat("a1B2c3D4e5F6", 3) + "AbCd" }

func countAllMemories(t *testing.T, s *Store) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM memories`).Scan(&n); err != nil {
		t.Fatalf("count memories: %v", err)
	}
	return n
}

func wantScopeRefusal(t *testing.T, err error, token string) *SecretContentError {
	t.Helper()
	if err == nil {
		t.Fatal("a token-shaped scope was accepted")
	}
	var refused *SecretContentError
	if !errors.As(err, &refused) {
		t.Fatalf("refused for a reason other than the token guard: %v", err)
	}
	if strings.Contains(err.Error(), token) {
		t.Fatalf("the refusal printed the value: %v", err)
	}
	return refused
}

// TestAScopeTokenIsRefusedOnEveryWriter holds the write half of the scope guard:
// a scope key or value shaped like a token is refused by the save path, by Create
// and by the importer, and nothing is stored.
func TestAScopeTokenIsRefusedOnEveryWriter(t *testing.T) {
	ctx := context.Background()
	tok := scopeToken()
	scopes := map[string]map[string]string{
		"value": {"token": tok},
		"key":   {tok: "production"},
		// Neither half is a token alone; the assignment is.
		"assigned pair": {"api_secret": "Zk3pQ9vXw2LmN8rT5yBc7HdF4sGj"},
	}
	for name, scope := range scopes {
		t.Run("save/"+name, func(t *testing.T) {
			s := testStore(t)
			_, _, _, err := s.UpsertWithOptions(ctx, testProject, "fact", "deploy notes for alpha", "mcp", 0.5, nil,
				UpsertOptions{Scope: scope})
			refused := wantScopeRefusal(t, err, tok)
			if !strings.HasPrefix(refused.Field, "scope") {
				t.Errorf("the refusal names %q, want a scope field", refused.Field)
			}
			if n := countAllMemories(t, s); n != 0 {
				t.Errorf("%d row(s) stored after a refused save", n)
			}
		})
		t.Run("create/"+name, func(t *testing.T) {
			s := testStore(t)
			_, err := s.Create(ctx, testProject, Memory{Category: "fact", Content: "deploy notes", Source: "mcp", Scope: scope})
			_ = wantScopeRefusal(t, err, tok)
			if n := countAllMemories(t, s); n != 0 {
				t.Errorf("%d row(s) stored after a refused create", n)
			}
		})
		t.Run("import/"+name, func(t *testing.T) {
			s := portableTestStore(t)
			if err := s.EnsureProject(ctx, "p1", "/src/p1", "p1"); err != nil {
				t.Fatalf("EnsureProject: %v", err)
			}
			pm := PortableMemory{ID: "M1", ProjectID: "p1", Category: "fact", Content: "an artifact's claim", Source: "mcp", Scope: scope}
			_ = wantScopeRefusal(t, CheckImportedMemory(pm), tok)
			for _, apply := range []bool{false, true} {
				_, _, _, err := s.ImportMemory(ctx, pm, ImportOptions{Apply: apply, TrustProvenance: true})
				_ = wantScopeRefusal(t, err, tok)
			}
			if n := countAllMemories(t, s); n != 0 {
				t.Errorf("%d row(s) stored after a refused import", n)
			}
		})
	}
}

// TestARefusedScopeLeavesAStoredRowUnchanged is the "update" half. No tool edits a
// scope in place, so the path that rewrites an existing row with a caller's scope
// is a save whose content duplicates it: the row is strengthened. A refused scope
// must stop that before the row moves.
func TestARefusedScopeLeavesAStoredRowUnchanged(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	id, _, _, err := s.UpsertWithOptions(ctx, testProject, "fact", "deploy notes for alpha", "mcp", 0.5, nil,
		UpsertOptions{Scope: map[string]string{"environment": "production"}})
	if err != nil {
		t.Fatalf("first save: %v", err)
	}
	before, err := s.GetByIDs(ctx, []string{id})
	if err != nil || len(before) != 1 {
		t.Fatalf("GetByIDs: %v rows=%d", err, len(before))
	}
	_, _, _, err = s.UpsertWithOptions(ctx, testProject, "fact", "deploy notes for alpha", "mcp", 0.9, nil,
		UpsertOptions{Scope: map[string]string{"token": scopeToken()}})
	_ = wantScopeRefusal(t, err, scopeToken())
	after, err := s.GetByIDs(ctx, []string{id})
	if err != nil || len(after) != 1 {
		t.Fatalf("GetByIDs: %v rows=%d", err, len(after))
	}
	if !reflect.DeepEqual(before[0], after[0]) {
		t.Errorf("a refused save changed the stored row:\nbefore %+v\nafter  %+v", before[0], after[0])
	}
	if n := countAllMemories(t, s); n != 1 {
		t.Errorf("%d rows after a refused duplicate save, want 1", n)
	}
}

func TestAnOverLimitScopeIsRefusedAndTheLimitsAreAccepted(t *testing.T) {
	ctx := context.Background()
	many := map[string]string{}
	for i := 0; i <= MaxScopeKeys; i++ {
		many[strings.Repeat("k", i+1)] = "v"
	}
	atCount := map[string]string{}
	for i := 0; i < MaxScopeKeys; i++ {
		atCount[strings.Repeat("k", i+1)] = "v"
	}
	cases := []struct {
		name    string
		scope   map[string]string
		wantErr string
	}{
		{"too many keys", many, "at most 16 keys"},
		{"key too long", map[string]string{strings.Repeat("k", MaxScopeKeyLen+1): "v"}, "key must be at most 64 bytes"},
		{"value too long", map[string]string{"environment": strings.Repeat("v", MaxScopeValueLen+1)}, "value must be at most 128 bytes"},
		{"huge value", map[string]string{"environment": strings.Repeat("v", 200000)}, "value must be at most 128 bytes"},
		{"exactly the key count", atCount, ""},
		{"exactly the key length", map[string]string{strings.Repeat("k", MaxScopeKeyLen): "v"}, ""},
		{"exactly the value length", map[string]string{"environment": strings.Repeat("v", MaxScopeValueLen)}, ""},
		{"an ordinary scope", map[string]string{"environment": "production", "component": "api"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := testStore(t)
			_, _, _, err := s.UpsertWithOptions(ctx, testProject, "fact", "deploy notes for alpha", "mcp", 0.5, nil,
				UpsertOptions{Scope: tc.scope})
			importErr := CheckImportedMemory(PortableMemory{ID: "M1", ProjectID: "p1", Category: "fact", Content: "c", Source: "mcp", Scope: tc.scope})
			if tc.wantErr == "" {
				if err != nil || importErr != nil {
					t.Fatalf("a scope within the limits was refused: save=%v import=%v", err, importErr)
				}
				return
			}
			for who, got := range map[string]error{"save": err, "import": importErr} {
				if got == nil || !strings.Contains(got.Error(), tc.wantErr) {
					t.Errorf("%s: err = %v, want it to contain %q", who, got, tc.wantErr)
				}
			}
			if n := countAllMemories(t, s); n != 0 {
				t.Errorf("%d row(s) stored after a refused save", n)
			}
		})
	}
}

// TestTheBoundReportedDoesNotVaryBetweenIdenticalCalls: a scope that breaches two
// bounds at once must name the same one every time, whatever the map's iteration
// order, so an operator is not sent to fix a different half on each retry.
func TestTheBoundReportedDoesNotVaryBetweenIdenticalCalls(t *testing.T) {
	scope := map[string]string{
		strings.Repeat("a", MaxScopeKeyLen+1): "v",
		"environment":                         strings.Repeat("v", MaxScopeValueLen+1),
		"component":                           strings.Repeat("v", MaxScopeValueLen+1),
	}
	first := checkScope(scope)
	if first == nil {
		t.Fatal("an over-limit scope was accepted")
	}
	for i := 0; i < 200; i++ {
		if got := checkScope(scope); got == nil || got.Error() != first.Error() {
			t.Fatalf("call %d reported %v, the first reported %v", i, got, first)
		}
	}
}

// TestScopeCharacterRulesAreRefusedOnEveryWriter verifies that the character
// rules (blank, NUL, invalid UTF-8, control, line-break) are refused on the
// save path, by Create, and by the importer, and nothing is stored.
func TestScopeCharacterRulesAreRefusedOnEveryWriter(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name  string
		scope map[string]string
		// The expected error substring for the save path (field position may vary).
		wantSaveErr   string
		wantCreateErr string
		wantImportErr string
	}{
		{
			name:          "blank key",
			scope:         map[string]string{"": "value"},
			wantSaveErr:   "scope_key[0] must not be empty or blank",
			wantCreateErr: "scope_key[0] must not be empty or blank",
			wantImportErr: "scope_key[0] must not be empty or blank",
		},
		{
			name:          "blank value",
			scope:         map[string]string{"key": ""},
			wantSaveErr:   "scope[key] must not be empty or blank",
			wantCreateErr: "scope[key] must not be empty or blank",
			wantImportErr: "scope[key] must not be empty or blank",
		},
		{
			name:          "whitespace-only key",
			scope:         map[string]string{"   ": "value"},
			wantSaveErr:   "scope_key[0] must not be empty or blank",
			wantCreateErr: "scope_key[0] must not be empty or blank",
			wantImportErr: "scope_key[0] must not be empty or blank",
		},
		{
			name:          "whitespace-only value",
			scope:         map[string]string{"key": "   "},
			wantSaveErr:   "scope[key] must not be empty or blank",
			wantCreateErr: "scope[key] must not be empty or blank",
			wantImportErr: "scope[key] must not be empty or blank",
		},
		{
			name:          "NUL byte in key",
			scope:         map[string]string{"key\000nul": "value"},
			wantSaveErr:   "scope_key[0] must not be a NUL byte",
			wantCreateErr: "scope_key[0] must not be a NUL byte",
			wantImportErr: "scope_key[0] must not be a NUL byte",
		},
		{
			name:          "NUL byte in value",
			scope:         map[string]string{"key": "value\000withnul"},
			wantSaveErr:   "scope[key] must not be a NUL byte",
			wantCreateErr: "scope[key] must not be a NUL byte",
			wantImportErr: "scope[key] must not be a NUL byte",
		},
		{
			name:          "line feed in key",
			scope:         map[string]string{"key\nwithnl": "value"},
			wantSaveErr:   "scope_key[0] must not be a control character or line separator",
			wantCreateErr: "scope_key[0] must not be a control character or line separator",
			wantImportErr: "scope_key[0] must not be a control character or line separator",
		},
		{
			name:          "line feed in value",
			scope:         map[string]string{"key": "value\nwithnl"},
			wantSaveErr:   "scope[key] must not be a control character or line separator",
			wantCreateErr: "scope[key] must not be a control character or line separator",
			wantImportErr: "scope[key] must not be a control character or line separator",
		},
		{
			name:          "U+2028 in key",
			scope:         map[string]string{"key\u2028withls": "value"},
			wantSaveErr:   "scope_key[0] must not be a control character or line separator",
			wantCreateErr: "scope_key[0] must not be a control character or line separator",
			wantImportErr: "scope_key[0] must not be a control character or line separator",
		},
		{
			name:          "U+2029 in key",
			scope:         map[string]string{"key\u2029withps": "value"},
			wantSaveErr:   "scope_key[0] must not be a control character or line separator",
			wantCreateErr: "scope_key[0] must not be a control character or line separator",
			wantImportErr: "scope_key[0] must not be a control character or line separator",
		},
		{
			name:          "U+0085 in key",
			scope:         map[string]string{"key\u0085withnel": "value"},
			wantSaveErr:   "scope_key[0] must not be a control character or line separator",
			wantCreateErr: "scope_key[0] must not be a control character or line separator",
			wantImportErr: "scope_key[0] must not be a control character or line separator",
		},
		{
			name:          "U+2028 in value",
			scope:         map[string]string{"key": "value\u2028withls"},
			wantSaveErr:   "scope[key] must not be a control character or line separator",
			wantCreateErr: "scope[key] must not be a control character or line separator",
			wantImportErr: "scope[key] must not be a control character or line separator",
		},
		{
			name:          "tab in key",
			scope:         map[string]string{"key\twithtab": "value"},
			wantSaveErr:   "scope_key[0] must not be a control character or line separator",
			wantCreateErr: "scope_key[0] must not be a control character or line separator",
			wantImportErr: "scope_key[0] must not be a control character or line separator",
		},
		{
			name:          "carriage return in value",
			scope:         map[string]string{"key": "value\rwithcr"},
			wantSaveErr:   "scope[key] must not be a control character or line separator",
			wantCreateErr: "scope[key] must not be a control character or line separator",
			wantImportErr: "scope[key] must not be a control character or line separator",
		},
		{
			name:          "invalid UTF-8 in key",
			scope:         map[string]string{"key\xffinvalid": "value"},
			wantSaveErr:   "scope_key[0] must not be invalid UTF-8",
			wantCreateErr: "scope_key[0] must not be invalid UTF-8",
			wantImportErr: "scope_key[0] must not be invalid UTF-8",
		},
		{
			name:          "invalid UTF-8 in value",
			scope:         map[string]string{"key": "value\xffinvalid"},
			wantSaveErr:   "scope[key] must not be invalid UTF-8",
			wantCreateErr: "scope[key] must not be invalid UTF-8",
			wantImportErr: "scope[key] must not be invalid UTF-8",
		},
	}
	for _, tc := range cases {
		t.Run("save/"+tc.name, func(t *testing.T) {
			s := testStore(t)
			_, _, _, err := s.UpsertWithOptions(ctx, testProject, "fact", "deploy notes for alpha", "mcp", 0.5, nil,
				UpsertOptions{Scope: tc.scope})
			if err == nil || !strings.Contains(err.Error(), tc.wantSaveErr) {
				t.Fatalf("save: err = %v, want it to contain %q", err, tc.wantSaveErr)
			}
			if n := countAllMemories(t, s); n != 0 {
				t.Errorf("%d row(s) stored after a refused save", n)
			}
		})
		t.Run("create/"+tc.name, func(t *testing.T) {
			s := testStore(t)
			_, err := s.Create(ctx, testProject, Memory{Category: "fact", Content: "deploy notes", Source: "mcp", Scope: tc.scope})
			if err == nil || !strings.Contains(err.Error(), tc.wantCreateErr) {
				t.Fatalf("create: err = %v, want it to contain %q", err, tc.wantCreateErr)
			}
			if n := countAllMemories(t, s); n != 0 {
				t.Errorf("%d row(s) stored after a refused create", n)
			}
		})
		t.Run("import/"+tc.name, func(t *testing.T) {
			s := portableTestStore(t)
			if err := s.EnsureProject(ctx, "p1", "/src/p1", "p1"); err != nil {
				t.Fatalf("EnsureProject: %v", err)
			}
			pm := PortableMemory{ID: "M1", ProjectID: "p1", Category: "fact", Content: "an artifact's claim", Source: "mcp", Scope: tc.scope}
			importErr := CheckImportedMemory(pm)
			if importErr == nil || !strings.Contains(importErr.Error(), tc.wantImportErr) {
				t.Fatalf("import (predicate): err = %v, want it to contain %q", importErr, tc.wantImportErr)
			}
			for _, apply := range []bool{false, true} {
				_, _, _, err := s.ImportMemory(ctx, pm, ImportOptions{Apply: apply, TrustProvenance: true})
				if err == nil || !strings.Contains(err.Error(), tc.wantImportErr) {
					t.Fatalf("import (apply=%v): err = %v, want it to contain %q", apply, err, tc.wantImportErr)
				}
			}
			if n := countAllMemories(t, s); n != 0 {
				t.Errorf("%d row(s) stored after a refused import", n)
			}
		})
	}
}

// TestScopeCharacterRulesAreConsistent verifies that a scope that breaches two
// character rules at once names the same one every time, whatever the map's
// iteration order, so an operator is not sent to fix a different half on each retry.
func TestScopeCharacterRulesAreConsistent(t *testing.T) {
	// Both key and value have a NUL byte.
	scope := map[string]string{
		"key\000nul": "value\000withnul",
	}
	first := checkScope(scope)
	if first == nil {
		t.Fatal("a scope with NUL bytes was accepted")
	}
	for i := 0; i < 200; i++ {
		if got := checkScope(scope); got == nil || got.Error() != first.Error() {
			t.Fatalf("call %d reported %v, the first reported %v", i, got, first)
		}
	}
}

// TestScopeAcceptsValidKeysAndValues verifies that ordinary scope keys and values
// with allowed characters are accepted.
func TestScopeAcceptsValidKeysAndValues(t *testing.T) {
	ctx := context.Background()
	validScopes := []map[string]string{
		{"environment": "production"},
		{"component": "api", "version": "v1.2.3"},
		{"key-with-dashes": "value_with_underscores"},
		{"key:with:colons": "value/with/slashes"},
		{"key@with@ats": "value+with+plus"},
		{"a": "b"},
		{"key with spaces": "value with spaces"},
		{"key.with.dots": "value:with:colons"},
	}
	for i, scope := range validScopes {
		t.Run(fmt.Sprintf("valid_scope_%d", i), func(t *testing.T) {
			s := testStore(t)
			_, _, _, err := s.UpsertWithOptions(ctx, testProject, "fact", "deploy notes", "mcp", 0.5, nil,
				UpsertOptions{Scope: scope})
			if err != nil {
				t.Fatalf("a valid scope was refused: %v", err)
			}
			if n := countAllMemories(t, s); n != 1 {
				t.Errorf("%d row(s) stored, want 1", n)
			}
			// Also check import path.
			s2 := portableTestStore(t)
			if err := s2.EnsureProject(ctx, "p1", "/src/p1", "p1"); err != nil {
				t.Fatalf("EnsureProject: %v", err)
			}
			pm := PortableMemory{ID: "M1", ProjectID: "p1", Category: "fact", Content: "c", Source: "mcp", Scope: scope}
			if err := CheckImportedMemory(pm); err != nil {
				t.Fatalf("import predicate refused a valid scope: %v", err)
			}
			_, _, _, err = s2.ImportMemory(ctx, pm, ImportOptions{Apply: true, TrustProvenance: true})
			if err != nil {
				t.Fatalf("import apply refused a valid scope: %v", err)
			}
			if n := countAllMemories(t, s2); n != 1 {
				t.Errorf("%d row(s) stored on import, want 1", n)
			}
		})
	}
}

// TestScopeRefusalOrderNeverPrintsAToken holds the ordering: the bounds run first,
// then the character rules, then the token guard, and each refusal is the first
// one in that order. A token-shaped value over its length bound is refused for
// length, and a token that also carries a control character is refused for the
// character, so in neither case is the token quoted back.
func TestScopeRefusalOrderNeverPrintsAToken(t *testing.T) {
	tok := scopeToken()
	cases := []struct {
		name  string
		scope map[string]string
		want  string
	}{
		{"over-long token value with a control character", map[string]string{"k": tok + strings.Repeat("x", MaxScopeValueLen) + "\n"}, "a scope value must be at most"},
		{"over-long token key with a control character", map[string]string{tok + strings.Repeat("x", MaxScopeKeyLen) + "\n": "v"}, "a scope key must be at most"},
		{"token value with a control character", map[string]string{"k": tok + "\n"}, "scope[k] must not be a control character"},
		{"token key with a control character", map[string]string{tok + "\n": "v"}, "scope_key[0] must not be a control character"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := checkScope(tc.scope)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.want)
			}
			var secretErr *SecretContentError
			if errors.As(err, &secretErr) {
				t.Errorf("refused by the token guard, so the character rules did not run first: %v", err)
			}
			if strings.Contains(err.Error(), tok) || strings.Contains(err.Error(), tok[:12]) {
				t.Errorf("the refusal printed the token: %v", err)
			}
		})
	}
}

// TestACharacterRefusalNeverPrintsATokenShapedKey: a value that breaks a character
// rule is refused by naming its key, so a token-shaped key must be screened first
// and no part of it may appear in the error.
func TestACharacterRefusalNeverPrintsATokenShapedKey(t *testing.T) {
	key := scopeToken()
	for name, value := range map[string]string{"NUL": "a\x00b", "blank": "  ", "newline": "a\nb"} {
		t.Run(name, func(t *testing.T) {
			err := checkScope(map[string]string{key: value})
			if err == nil {
				t.Fatal("accepted")
			}
			if strings.Contains(err.Error(), key) || strings.Contains(err.Error(), key[:12]) {
				t.Errorf("the refusal printed the key: %v", err)
			}
		})
	}
}

// TestScopeRejectsInvisibleFormatCharactersMixedIn covers the #1888 regression:
// an invisible Unicode format rune (category Cf, e.g. U+200B) mixed into a key
// or a value must be refused on every writer.
func TestScopeRejectsInvisibleFormatCharactersMixedIn(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name  string
		scope map[string]string
	}{
		{"key with trailing zero-width", map[string]string{"env\u200b": "prod"}},
		{"key with leading zero-width", map[string]string{"\u200benv": "prod"}},
		{"value with trailing zero-width", map[string]string{"env": "prod\u200b"}},
		{"value with leading zero-width", map[string]string{"env": "\u200bprod"}},
	}
	for _, tc := range cases {
		t.Run("checkScope/"+tc.name, func(t *testing.T) {
			err := checkScope(tc.scope)
			if err == nil {
				t.Fatal("checkScope accepted a scope with an invisible format character")
			}
			if !strings.Contains(err.Error(), "control character or line separator") {
				t.Fatalf("checkScope refused for unexpected reason: %v", err)
			}
			var b *ScopeKeyValueError
			if !errors.As(err, &b) {
				t.Fatalf("checkScope refused as %T, want *ScopeKeyValueError", err)
			}
		})
		t.Run("create/"+tc.name, func(t *testing.T) {
			s := testStore(t)
			_, err := s.Create(ctx, testProject, Memory{Category: "fact", Content: "deploy notes", Source: "mcp", Scope: tc.scope})
			if err == nil {
				t.Fatal("Create accepted a scope with an invisible format character")
			}
			if n := countAllMemories(t, s); n != 0 {
				t.Errorf("%d row(s) stored after a refused create", n)
			}
		})
		t.Run("upsert/"+tc.name, func(t *testing.T) {
			s := testStore(t)
			_, _, _, err := s.UpsertWithOptions(ctx, testProject, "fact", "deploy notes for alpha", "mcp", 0.5, nil,
				UpsertOptions{Scope: tc.scope})
			if err == nil {
				t.Fatal("UpsertWithOptions accepted a scope with an invisible format character")
			}
			if n := countAllMemories(t, s); n != 0 {
				t.Errorf("%d row(s) stored after a refused upsert", n)
			}
		})
		t.Run("import/"+tc.name, func(t *testing.T) {
			s := portableTestStore(t)
			if err := s.EnsureProject(ctx, "p1", "/src/p1", "p1"); err != nil {
				t.Fatalf("EnsureProject: %v", err)
			}
			pm := PortableMemory{ID: "M1", ProjectID: "p1", Category: "fact", Content: "an artifact's claim", Source: "mcp", Scope: tc.scope}
			importErr := CheckImportedMemory(pm)
			if importErr == nil {
				t.Fatal("CheckImportedMemory accepted a scope with an invisible format character")
			}
			for _, apply := range []bool{false, true} {
				_, _, _, err := s.ImportMemory(ctx, pm, ImportOptions{Apply: apply, TrustProvenance: true})
				if err == nil {
					t.Fatalf("ImportMemory accepted a scope with an invisible format character (apply=%v)", apply)
				}
			}
			if n := countAllMemories(t, s); n != 0 {
				t.Errorf("%d row(s) stored after a refused import", n)
			}
		})
	}
}
