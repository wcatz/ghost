package memory

import (
	"context"
	"errors"
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
