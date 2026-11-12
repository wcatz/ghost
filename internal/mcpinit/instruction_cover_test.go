package mcpinit

import (
	"path/filepath"
	"testing"

	"github.com/wcatz/ghost/internal/hostevent"
)

// TestInstructionCoverUserLevelOnly: with no directory, only the host's
// user-level file is read, which is all a cross-project bucket can rely on.
func TestInstructionCoverUserLevelOnly(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	t.Setenv("CLAUDE_CODE_DISABLE_CLAUDE_MDS", "")
	const rule = "NEVER add Co-Authored-By or any AI attribution to commit messages. All commits belong to the user."

	if cover := InstructionCover(hostevent.SourceClaudeCode, ""); cover != nil {
		t.Fatalf("no user file yet, want a nil cover")
	}
	writeFile(t, filepath.Join(cfg, "CLAUDE.md"), sampleInstructions)
	cover := InstructionCover(hostevent.SourceClaudeCode, "")
	if cover == nil || !cover(rule) {
		t.Fatalf("user-level CLAUDE.md should cover %q", rule)
	}
	if cover("NEVER stack PRs (a PR based on another PR's branch). Every piece of work gets its own branch against main.") {
		t.Error("a rule the file does not hold was covered")
	}
	if got := hostInstructionFiles(hostevent.SourceClaudeCode, "", t.TempDir()); len(got) != 1 {
		t.Errorf("files with no directory = %v, want only the user file", got)
	}
}
