package mcpserver

import (
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

func scopeTestToken() string { return "ghp_" + strings.Repeat("a1B2c3D4e5F6", 3) + "AbCd" }

func TestScopeGuardRefusesBadScopesAndBoundsLegacyListing(t *testing.T) {
	tok := scopeTestToken()
	many := map[string]any{}
	for i := 0; i < memory.MaxScopeKeys+1; i++ {
		many[strings.Repeat("k", i+1)] = "v"
	}
	for _, tc := range []struct {
		name  string
		scope any
		want  string
	}{
		{"token value", map[string]any{"token": tok}, "refusing to store"},
		{"token key", map[string]any{tok: "production"}, "refusing to store"},
		{"too many keys", many, "at most 16 keys"},
		{"value too long", map[string]any{"environment": strings.Repeat("v", 200000)}, "at most 128 bytes"},
	} {
		db, _, srv := newStoreWithDB(t)
		session := connectedClient(t, srv)
		text := callToolErr(t, session, "ghost_memory_save", map[string]any{
			"project_id": "pa", "content": "deploy notes for alpha", "category": "fact", "scope": tc.scope,
		})
		if !strings.Contains(text, tc.want) || strings.Contains(text, tok) {
			t.Fatalf("%s: %q", tc.name, text)
		}
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM memories`).Scan(&n); err != nil || n != 0 {
			t.Fatalf("%s stored rows n=%d err=%v", tc.name, n, err)
		}
	}
	_, session := newCapSession(t)
	if res := callTool(t, session, "ghost_memory_save", map[string]any{
		"project_id": "pa", "content": "deploy notes for alpha", "category": "fact",
		"scope": map[string]any{"environment": "production", "component": "api"},
	}); res.IsError {
		t.Fatalf("ordinary scope refused: %s", resultText(res))
	}
	db, _, srv := newStoreWithDB(t)
	session = connectedClient(t, srv)
	huge := `{"environment":"` + strings.Repeat("v", 200000) + `"}`
	if _, err := db.Exec(`INSERT INTO memories (id, project_id, category, content, source, scope, created_at, updated_at)
		VALUES ('OLD1', 'abc123', 'fact', 'deploy notes for alpha', 'mcp', ?, datetime('now'), datetime('now'))`, huge); err != nil {
		t.Fatal(err)
	}
	text := resultText(callTool(t, session, "ghost_memories_list", map[string]any{"project_id": "abc123"}))
	if !strings.Contains(text, "deploy notes for alpha") || len(text) > 4000 {
		t.Fatalf("listing not bounded: len=%d %q", len(text), text[:min(200, len(text))])
	}
}
