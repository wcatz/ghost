package mcpinit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wcatz/ghost/internal/hostevent"
	"github.com/wcatz/ghost/internal/memory"
)

const sampleInstructions = `# Rules

## Git
1. Never commit directly to ` + "`main`" + `. Always feature branch + PR.
2. NEVER add ` + "`Co-Authored-By`" + ` or any AI attribution to commit messages.
3. Never revert, downgrade, pin back, or hold a dependency bump as a "fix".
   Root-cause it in the owning repo.

## Stack
- Go 1.24.0+
- Monitoring: kube-prometheus-stack
`

// TestInstructionCoverage: a memory restating one rule is covered; a memory
// that shares only a few words with a rule, or that a short generic line
// happens to overlap, is not.
func TestInstructionCoverage(t *testing.T) {
	blocks := instructionBlocks(sampleInstructions)
	cases := []struct {
		content string
		want    bool
	}{
		{"NEVER add Co-Authored-By or any AI attribution to commit messages. All commits belong to the user.", true},
		{`NEVER revert, downgrade, pin back or hold a bump as a "fix". Root-cause the bug in the repo that owns it, fix it there, link it into the PR.`, true},
		{"NEVER stack PRs (a PR based on another PR's branch). Every piece of work gets its own branch against main.", false},
		{"Dashboards are deployed only from kube-prometheus-stack/dashboards via the sidecar; a ConfigMap edited by hand is overwritten on the next sync.", false},
		{"The local Go toolchain lives in the module cache, so builds pass -trimpath.", false},
	}
	for _, c := range cases {
		if got := coveredByInstructions(c.content, blocks); got != c.want {
			t.Errorf("covered(%q) = %v, want %v", c.content, got, c.want)
		}
	}
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestHostInstructionFiles: each host gets the files that host loads and no
// other, because withholding a row over a file the host never read would hide
// the row's only copy.
func TestHostInstructionFiles(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	repo := filepath.Join(home, "git", "repo")
	cwd := filepath.Join(repo, "sub")
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("CLAUDE_CODE_DISABLE_CLAUDE_MDS", "")
	t.Setenv("OPENCODE_CONFIG_DIR", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	for _, p := range []string{
		filepath.Join(home, ".claude", "CLAUDE.md"),
		filepath.Join(repo, "CLAUDE.md"),
		filepath.Join(cwd, ".claude", "CLAUDE.md"),
		filepath.Join(repo, "AGENTS.md"),
		filepath.Join(home, ".config", "opencode", "AGENTS.md"),
	} {
		writeFile(t, p, "x")
	}

	has := func(files []string, p string) bool {
		for _, f := range files {
			if f == p {
				return true
			}
		}
		return false
	}

	claude := hostInstructionFiles(hostevent.SourceClaudeCode, cwd, home)
	for _, p := range []string{
		filepath.Join(home, ".claude", "CLAUDE.md"),
		filepath.Join(repo, "CLAUDE.md"),
		filepath.Join(cwd, ".claude", "CLAUDE.md"),
	} {
		if !has(claude, p) {
			t.Errorf("claude-code files %v: missing %s", claude, p)
		}
	}
	for _, f := range claude {
		if strings.HasSuffix(f, "AGENTS.md") {
			t.Errorf("claude-code files include %s, which Claude Code does not load", f)
		}
	}

	oc := hostInstructionFiles(hostevent.SourceOpencode, cwd, home)
	for _, p := range []string{
		filepath.Join(home, ".config", "opencode", "AGENTS.md"),
		filepath.Join(repo, "AGENTS.md"),
	} {
		if !has(oc, p) {
			t.Errorf("opencode files %v: missing %s", oc, p)
		}
	}
	for _, f := range oc {
		if strings.HasSuffix(f, "CLAUDE.md") {
			t.Errorf("opencode files include %s, which opencode does not load", f)
		}
	}

	if got := hostInstructionFiles(hostevent.Source("codex"), cwd, home); len(got) != 0 {
		t.Errorf("a host with no known file list got %v, want none", got)
	}

	t.Setenv("CLAUDE_CODE_DISABLE_CLAUDE_MDS", "1")
	if got := hostInstructionFiles(hostevent.SourceClaudeCode, cwd, home); len(got) != 0 {
		t.Errorf("with CLAUDE_CODE_DISABLE_CLAUDE_MDS set, got %v, want none", got)
	}
}

// TestSessionStartWithholdsGlobalCoveredByHostInstructions: end to end through
// the session loader, a pinned global restating a loaded rule is withheld, the
// header counts it, and an uncovered pinned global stays.
func TestSessionStartWithholdsGlobalCoveredByHostInstructions(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "ghost.db")
	db, err := memory.OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	projectPath := filepath.Join(dir, "proj")
	if err := os.MkdirAll(projectPath, 0o755); err != nil {
		t.Fatal(err)
	}
	canonical, err := filepath.EvalSymlinks(projectPath)
	if err != nil {
		t.Fatal(err)
	}
	insertProject(t, db, "pcover", canonical, "cover")
	if _, err := db.Exec(`INSERT OR IGNORE INTO projects (id, path, name) VALUES ('_global', '_global', 'global')`); err != nil {
		t.Fatalf("insert _global: %v", err)
	}
	for id, content := range map[string]string{
		"COVERED0": "NEVER add Co-Authored-By or any AI attribution to commit messages. All commits belong to the user.",
		"KEPTROW0": "NEVER stack PRs (a PR based on another PR's branch). Every piece of work gets its own branch against main.",
	} {
		if _, err := db.Exec(`INSERT INTO memories (id, project_id, category, content, importance, source, pinned) VALUES (?, '_global', 'preference', ?, 1.0, 'mcp', 1)`, id, content); err != nil {
			t.Fatal(err)
		}
	}
	_ = db.Close()

	blocks := instructionBlocks(sampleInstructions)
	covered := func(c memory.Candidate) bool { return coveredByInstructions(c.Content, blocks) }
	now := time.Now().UTC()
	_, _, _, globals, _, _, _, _, tally :=
		loadSessionContextFrom(dbPath, canonical, mustHookConfig(t), func() time.Time { return now }, "", covered)
	var ids []string
	for _, g := range globals {
		ids = append(ids, g.ID)
	}
	if len(ids) != 1 || ids[0] != "KEPTROW0" {
		t.Errorf("globals = %v, want only KEPTROW0", ids)
	}
	if tally.globals.Withheld != 1 {
		t.Errorf("global tally = %+v, want 1 withheld", tally.globals)
	}
}
