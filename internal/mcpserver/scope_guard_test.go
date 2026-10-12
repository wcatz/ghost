package mcpserver

import (
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

func scopeTestToken() string { return "ghp_" + strings.Repeat("a1B2c3D4e5F6", 3) + "AbCd" }

// TestSaveRefusesATokenOrAnOversizedScope drives the real ghost_memory_save tool:
// a scope key or value shaped like a token is refused exactly as the same token in
// content is, an over-limit scope is refused with the limit named, and nothing is
// stored by a refused call.
func TestSaveRefusesATokenOrAnOversizedScope(t *testing.T) {
	tok := scopeTestToken()
	many := map[string]any{}
	for i := 0; i < memory.MaxScopeKeys+1; i++ {
		many[strings.Repeat("k", i+1)] = "v"
	}
	cases := []struct {
		name  string
		scope any
		want  string
	}{
		{"token value", map[string]any{"token": tok}, "refusing to store"},
		{"token key", map[string]any{tok: "production"}, "refusing to store"},
		{"token value sent as a JSON string", `{"token":"` + tok + `"}`, "refusing to store"},
		{"too many keys", many, "at most 16 keys"},
		{"value too long", map[string]any{"environment": strings.Repeat("v", 200000)}, "at most 128 bytes"},
		{"key too long", map[string]any{strings.Repeat("k", 65): "v"}, "at most 64 bytes"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db, _, srv := newStoreWithDB(t)
			session := connectedClient(t, srv)
			text := callToolErr(t, session, "ghost_memory_save", map[string]any{
				"project_id": "pa", "content": "deploy notes for alpha", "category": "fact", "scope": tc.scope,
			})
			if !strings.Contains(text, tc.want) {
				t.Errorf("refusal = %q, want it to contain %q", text, tc.want)
			}
			if strings.Contains(text, tok) {
				t.Errorf("the refusal printed the token: %q", text)
			}
			var n int
			if err := db.QueryRow(`SELECT COUNT(*) FROM memories`).Scan(&n); err != nil {
				t.Fatalf("count: %v", err)
			}
			if n != 0 {
				t.Errorf("%d row(s) stored by a refused save", n)
			}
		})
	}
}

// TestAnOrdinaryScopeStillSaves keeps the guard from being a refusal of every scope.
func TestAnOrdinaryScopeStillSaves(t *testing.T) {
	_, session := newCapSession(t)
	res := callTool(t, session, "ghost_memory_save", map[string]any{
		"project_id": "pa", "content": "deploy notes for alpha", "category": "fact",
		"scope": map[string]any{"environment": "production", "component": "api"},
	})
	if res.IsError {
		t.Fatalf("an ordinary scope was refused: %s", resultText(res))
	}
}

// TestAListingBoundsAScopeStoredBeforeTheLimit plants a row the way an old store
// holds one, directly in the table, and reads it back through the listing: the
// scope label must stay inside its bound.
func TestAListingBoundsAScopeStoredBeforeTheLimit(t *testing.T) {
	db, _, srv := newStoreWithDB(t)
	session := connectedClient(t, srv)
	huge := `{"environment":"` + strings.Repeat("v", 200000) + `"}`
	if _, err := db.Exec(
		`INSERT INTO memories (id, project_id, category, content, source, scope, created_at, updated_at)
		 VALUES ('OLD1', 'abc123', 'fact', 'deploy notes for alpha', 'mcp', ?, datetime('now'), datetime('now'))`, huge); err != nil {
		t.Fatalf("plant: %v", err)
	}
	res := callTool(t, session, "ghost_memories_list", map[string]any{"project_id": "abc123"})
	text := resultText(res)
	if !strings.Contains(text, "deploy notes for alpha") {
		t.Fatalf("the planted row is not listed: %.300s", text)
	}
	if len(text) > 4000 {
		t.Errorf("the listing is %d bytes for one row with a 200 KB scope", len(text))
	}
}
