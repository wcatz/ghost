package mcpinit

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestHoldsOnlyGhostManagedAcceptsPreToolUse(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	cmd := shellQuote("ghost") + " hook pre-tool-use --source claude-code"
	perms, _ := json.Marshal(map[string]any{"allow": ghostPermissions})
	hooks, _ := json.Marshal(map[string]any{
		"PreToolUse": []any{
			map[string]any{
				"matcher": "Bash",
				"hooks":   []any{map[string]any{"type": "command", "command": cmd}},
			},
		},
	})
	doc := map[string]json.RawMessage{
		"permissions":       perms,
		"hooks":             hooks,
		"autoMemoryEnabled": json.RawMessage("false"),
	}
	b, _ := json.Marshal(doc)
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	sf, err := loadSettings(path)
	if err != nil {
		t.Fatal(err)
	}
	if !sf.holdsOnlyGhostManaged() {
		t.Fatal("settings with only ghost-managed PreToolUse hook should be recognized")
	}
}
