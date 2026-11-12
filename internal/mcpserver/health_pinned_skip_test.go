package mcpserver

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// TestHealthPinnedSkip: for an identified host, a pinned row its user-level
// instruction file restates is skipped in every bucket; an unidentified client
// skips nothing.
func TestHealthPinnedSkip(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	t.Setenv("CLAUDE_CODE_DISABLE_CLAUDE_MDS", "")
	if err := os.WriteFile(filepath.Join(cfg, "CLAUDE.md"), []byte("## Git\n1. NEVER add `Co-Authored-By` or any AI attribution to commit messages.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	covered := memory.Memory{ID: "a", ProjectID: memory.GlobalProjectID, Content: "NEVER add Co-Authored-By or any AI attribution to commit messages. All commits belong to the user."}
	other := memory.Memory{ID: "b", ProjectID: memory.GlobalProjectID, Content: "NEVER stack PRs; every branch is cut from main."}

	if healthPinnedSkip("", nil) != nil {
		t.Error("an unidentified client must skip nothing")
	}
	skip := healthPinnedSkip("claude-code", map[string]string{"proj": "/nonexistent/proj"})
	if skip == nil || !skip(covered) {
		t.Fatal("a global the user file restates should be skipped")
	}
	if skip(other) {
		t.Error("a global the user file does not restate was skipped")
	}
	covered.ProjectID = "proj"
	if !skip(covered) {
		t.Error("a project row the user file restates should be skipped too")
	}
}
