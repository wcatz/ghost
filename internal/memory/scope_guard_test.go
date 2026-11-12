package memory

import (
	"context"
	"errors"
	"strings"
	"testing"
)

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

func TestAScopeTokenIsRefusedOnEveryWriter(t *testing.T) {
	ctx := context.Background()
	tok := scopeToken()
	scopes := map[string]map[string]string{
		"value":         {"token": tok},
		"key":           {tok: "production"},
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

func TestScopeRefusalOrderNeverPrintsAToken(t *testing.T) {
	tok := scopeToken()
	cases := []struct {
		name       string
		scope      map[string]string
		want       string
		wantSecret bool
	}{
		{"over-long token value with a control character", map[string]string{"k": tok + strings.Repeat("x", MaxScopeValueLen) + "\n"}, "a scope value must be at most", false},
		{"over-long token key with a control character", map[string]string{tok + strings.Repeat("x", MaxScopeKeyLen) + "\n": "v"}, "a scope key must be at most", false},
		{"token value with a control character", map[string]string{"k": tok + "\n"}, "scope[k] must not be a control character", false},
		{"token key with a control character", map[string]string{tok + "\n": "v"}, "scope_key[0] must not be a control character", false},
		{"token value with a format character", map[string]string{"k": tok + "\u200b"}, "refusing to store credential-shaped content", true},
		{"token key with a format character", map[string]string{tok + "\u200b": "v"}, "refusing to store credential-shaped content", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := checkScope(tc.scope)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.want)
			}
			var secretErr *SecretContentError
			if tc.wantSecret {
				if !errors.As(err, &secretErr) {
					t.Errorf("not refused by the token guard, so the format character rule ran first: %v", err)
				}
			} else if errors.As(err, &secretErr) {
				t.Errorf("refused by the token guard, so the character rules did not run first: %v", err)
			}
			if strings.Contains(err.Error(), tok) || strings.Contains(err.Error(), tok[:12]) {
				t.Errorf("the refusal printed the token: %v", err)
			}
		})
	}
}

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
			if !strings.Contains(err.Error(), "an invisible Unicode FORMAT character") {
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
