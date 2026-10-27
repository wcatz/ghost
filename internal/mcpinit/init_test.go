package mcpinit

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

// absProjectPath returns an absolute project path for this OS. Claude project
// paths are absolute, and filepath.IsAbs treats a volume-less POSIX path like
// "/home/test/..." as relative on Windows — writeRedirects skips it there — so
// the fixture must be rooted the way the current OS spells "absolute".
func absProjectPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "git", "myproject")
}

// projectSlug folds a project path to the directory name Claude Code stores it
// under: every character outside [a-zA-Z0-9] becomes "-" — not just the three
// path separators — matching claudeimport.EncodeProjectPath. Spelled out here
// so the expectation stays independent of production code. Claude Code also
// truncates names over 200 units and appends a hash; these fixtures are short
// enough that branch never runs, so it is not repeated.
func projectSlug(projectPath string) string {
	var b strings.Builder
	b.Grow(len(projectPath))
	for _, r := range projectPath {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	return b.String()
}

func TestHandleSessionStartHook(t *testing.T) {
	var out bytes.Buffer
	runSessionStartHook(t, `{"event":"SessionStart"}`, &out)

	output := out.String()
	if output == "" {
		t.Error("hook output should not be empty")
	}
	if !strings.Contains(output, "ghost_memory_save") && !strings.Contains(output, "Ghost context") && !strings.Contains(output, "Save to Ghost as it happens") {
		t.Error("hook output should mention ghost_memory_save or Ghost context")
	}
}

func TestWriteRedirects_CreatesFile(t *testing.T) {
	home := t.TempDir()
	setHome(t, home)
	projectPath := absProjectPath(t)

	projects := []projectInfo{
		{ID: "abc123", Path: projectPath, Name: "myproject"},
	}

	var out bytes.Buffer
	writeRedirects(&out, projects, false)

	encoded := projectSlug(projectPath)
	target := filepath.Join(home, ".claude", "projects", encoded, "memory", "MEMORY.md")

	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("expected redirect file to be created: %v", err)
	}

	content := string(data)
	if !strings.Contains(content, "stored in Ghost") {
		t.Error("redirect should contain 'stored in Ghost'")
	}
	if !strings.Contains(content, "myproject") {
		t.Error("redirect should contain project name")
	}

	output := out.String()
	if !strings.Contains(output, "created redirect") {
		t.Errorf("output should say 'created redirect', got: %s", output)
	}
}

func TestWriteRedirects_SkipsExisting(t *testing.T) {
	home := t.TempDir()
	setHome(t, home)
	projectPath := absProjectPath(t)

	// Pre-create the redirect file.
	encoded := projectSlug(projectPath)
	dir := filepath.Join(home, ".claude", "projects", encoded, "memory")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	redirect := "# myproject Project Memory\n\nAll project knowledge is stored in Ghost and injected automatically at session start via the SessionStart hook.\nProject context (memories + summary) is already in your system prompt — no tool calls needed to load it.\n\nUse `ghost_memory_save` to save new discoveries during work.\nUse `ghost_memory_search` to search for specific facts.\n"
	if err := os.WriteFile(filepath.Join(dir, "MEMORY.md"), []byte(redirect), 0644); err != nil {
		t.Fatal(err)
	}

	projects := []projectInfo{
		{ID: "abc123", Path: projectPath, Name: "myproject"},
	}

	var out bytes.Buffer
	writeRedirects(&out, projects, false)

	output := out.String()
	if !strings.Contains(output, "redirect exists") {
		t.Errorf("should report redirect exists, got: %s", output)
	}
}

func TestWriteRedirects_SkipsRelativePath(t *testing.T) {
	home := t.TempDir()
	setHome(t, home)

	projects := []projectInfo{
		{ID: "abc123", Path: "relative/path", Name: "rel"},
	}

	var out bytes.Buffer
	writeRedirects(&out, projects, false)

	// Should produce no output for relative paths.
	if out.String() != "" {
		t.Errorf("expected no output for relative path, got: %s", out.String())
	}
}

func TestWriteRedirects_DoesNotClobber(t *testing.T) {
	home := t.TempDir()
	setHome(t, home)
	projectPath := absProjectPath(t)

	// Pre-create a file with user content (not a Ghost redirect).
	encoded := projectSlug(projectPath)
	dir := filepath.Join(home, ".claude", "projects", encoded, "memory")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	original := "User's custom memory content here."
	if err := os.WriteFile(filepath.Join(dir, "MEMORY.md"), []byte(original), 0644); err != nil {
		t.Fatal(err)
	}

	projects := []projectInfo{
		{ID: "abc123", Path: projectPath, Name: "myproject"},
	}

	var out bytes.Buffer
	writeRedirects(&out, projects, false)

	// Verify it was NOT overwritten.
	data, _ := os.ReadFile(filepath.Join(dir, "MEMORY.md"))
	if string(data) != original {
		t.Error("writeRedirects clobbered existing non-Ghost MEMORY.md")
	}

	output := out.String()
	if !strings.Contains(output, "not overwriting") {
		t.Errorf("should say 'not overwriting', got: %s", output)
	}
}

func TestWriteRedirects_DryRun(t *testing.T) {
	home := t.TempDir()
	setHome(t, home)
	projectPath := absProjectPath(t)

	projects := []projectInfo{
		{ID: "abc123", Path: projectPath, Name: "myproject"},
	}

	var out bytes.Buffer
	writeRedirects(&out, projects, true)

	output := out.String()
	if !strings.Contains(output, "would create redirect") {
		t.Errorf("dry run should say 'would create redirect', got: %s", output)
	}

	// Verify no file was created.
	encoded := projectSlug(projectPath)
	target := filepath.Join(home, ".claude", "projects", encoded, "memory", "MEMORY.md")
	if _, err := os.Stat(target); err == nil {
		t.Error("dry run should not create files")
	}
}

func TestEnsureAutoMemoryDisabled_SetsFlag(t *testing.T) {
	home := t.TempDir()
	setHome(t, home)

	path := filepath.Join(home, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}

	sf, err := loadSettings(path)
	if err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := ensureAutoMemoryDisabled(&out, sf, false); err != nil {
		t.Fatalf("ensureAutoMemoryDisabled: %v", err)
	}

	output := out.String()
	if !strings.Contains(output, "set autoMemoryEnabled: false") {
		t.Errorf("expected 'set autoMemoryEnabled: false' in output, got: %s", output)
	}

	v, present := sf.getAutoMemoryEnabled()
	if !present || v {
		t.Errorf("expected autoMemoryEnabled=false, got present=%v value=%v", present, v)
	}
}

func TestEnsureAutoMemoryDisabled_Idempotent(t *testing.T) {
	home := t.TempDir()
	setHome(t, home)

	path := filepath.Join(home, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"autoMemoryEnabled":false}`), 0600); err != nil {
		t.Fatal(err)
	}

	sf, err := loadSettings(path)
	if err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := ensureAutoMemoryDisabled(&out, sf, false); err != nil {
		t.Fatalf("ensureAutoMemoryDisabled: %v", err)
	}

	output := out.String()
	if !strings.Contains(output, "already false") {
		t.Errorf("expected 'already false' in output, got: %s", output)
	}
}

func TestEnsureAutoMemoryDisabled_DryRun(t *testing.T) {
	home := t.TempDir()
	setHome(t, home)

	path := filepath.Join(home, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}

	sf, err := loadSettings(path)
	if err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := ensureAutoMemoryDisabled(&out, sf, true); err != nil {
		t.Fatalf("ensureAutoMemoryDisabled dry run: %v", err)
	}

	output := out.String()
	if !strings.Contains(output, "would set autoMemoryEnabled: false") {
		t.Errorf("expected 'would set autoMemoryEnabled: false' in output, got: %s", output)
	}

	// In dry run, the in-memory state should not be modified.
	_, present := sf.getAutoMemoryEnabled()
	if present {
		t.Error("dry run should not modify settings in memory")
	}
}

func TestEnsureConfigBootstrap_CreatesFile(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmpDir)

	var out bytes.Buffer
	if err := ensureConfigBootstrap(&out, false); err != nil {
		t.Fatalf("ensureConfigBootstrap: %v", err)
	}

	path := filepath.Join(tmpDir, "ghost", "config.yaml")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected config file to be created: %v", err)
	}
	if !strings.Contains(out.String(), "created config file") {
		t.Errorf("expected 'created config file' in output, got: %s", out.String())
	}
}

func TestEnsureConfigBootstrap_Idempotent(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmpDir)

	var out bytes.Buffer
	if err := ensureConfigBootstrap(&out, false); err != nil {
		t.Fatalf("ensureConfigBootstrap (1st): %v", err)
	}
	out.Reset()
	if err := ensureConfigBootstrap(&out, false); err != nil {
		t.Fatalf("ensureConfigBootstrap (2nd): %v", err)
	}
	if !strings.Contains(out.String(), "config file exists") {
		t.Errorf("expected 'config file exists' in output, got: %s", out.String())
	}
}

func TestEnsureConfigBootstrap_DryRunDoesNotCreate(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmpDir)

	var out bytes.Buffer
	if err := ensureConfigBootstrap(&out, true); err != nil {
		t.Fatalf("ensureConfigBootstrap dry run: %v", err)
	}

	path := filepath.Join(tmpDir, "ghost", "config.yaml")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("dry run must not create the config file, stat err = %v", err)
	}
	if !strings.Contains(out.String(), "would create config file") {
		t.Errorf("expected 'would create config file' in output, got: %s", out.String())
	}
}

func TestRetryHint(t *testing.T) {
	err := retryHint(fmt.Errorf("something broke"))
	msg := err.Error()
	if !strings.Contains(msg, "something broke") {
		t.Error("should preserve original error")
	}
	if !strings.Contains(msg, "ghost mcp init") {
		t.Error("should include retry hint")
	}
}

func TestShellQuote(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"ghost", "'ghost'"},
		{"/usr/local/bin/ghost", "'/usr/local/bin/ghost'"},
		{"/path with spaces/ghost", "'/path with spaces/ghost'"},
		{"/path/with$dollar/ghost", "'/path/with$dollar/ghost'"},
		{"/path/with`backtick`/ghost", "'/path/with`backtick`/ghost'"},
		{"/path/it's/ghost", "'/path/it'\\''s/ghost'"},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := shellQuotePOSIX(tt.input)
			if got != tt.want {
				t.Errorf("shellQuotePOSIX(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestShellQuoteWindows(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"ghost", `"ghost"`},
		{`C:\Users\me\ghost.exe`, `"C:\Users\me\ghost.exe"`},
		{`C:\path with spaces\ghost.exe`, `"C:\path with spaces\ghost.exe"`},
		{`C:\path\with"quote\ghost.exe`, `"C:\path\with""quote\ghost.exe"`},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := shellQuoteWindows(tt.input)
			if got != tt.want {
				t.Errorf("shellQuoteWindows(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestShellQuoteDispatch(t *testing.T) {
	input := `C:\path with spaces\ghost.exe`
	want := shellQuotePOSIX(input)
	if runtime.GOOS == "windows" {
		want = shellQuoteWindows(input)
	}
	if got := shellQuote(input); got != want {
		t.Errorf("shellQuote(%q) = %q, want %q", input, got, want)
	}
}

func TestGhostPermissions_Complete(t *testing.T) {
	// Verify the canonical list has the expected count. It tracks the number of
	// tools the MCP server registers, so a tool added without a permission here
	// is a tool an operator is prompted about on every call.
	if len(ghostPermissions) != 23 {
		t.Errorf("expected 23 ghost permissions, got %d", len(ghostPermissions))
	}

	// All should start with the correct prefix.
	for _, p := range ghostPermissions {
		if !strings.HasPrefix(p, "mcp__ghost__ghost_") {
			t.Errorf("permission %q has unexpected prefix", p)
		}
	}
}

func TestEnsureStopHook(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	sf, err := loadSettings(path)
	if err != nil {
		t.Fatalf("loadSettings: %v", err)
	}

	var out strings.Builder
	if err := ensureStopHook(&out, sf, "/usr/local/bin/ghost", false); err != nil {
		t.Fatalf("ensureStopHook: %v", err)
	}
	if !sf.hasHook("Stop", "hook stop") {
		t.Error("Stop hook should be present after ensureStopHook")
	}

	// Idempotent: second call reports already-configured, adds nothing.
	out.Reset()
	if err := ensureStopHook(&out, sf, "/usr/local/bin/ghost", false); err != nil {
		t.Fatalf("ensureStopHook (second): %v", err)
	}
	if !strings.Contains(out.String(), "already configured") {
		t.Errorf("second run should be a no-op, got %q", out.String())
	}

	// SessionStart hooks are untouched.
	if sf.hasHook("SessionStart", "hook stop") {
		t.Error("Stop hook must not leak into SessionStart")
	}
}

const (
	testLegacyCmd  = `'C:\ghost\ghost.exe' hook session-start`
	testDesiredCmd = `"C:\ghost\ghost.exe" hook session-start`
)

func TestReconcileHook_AddsWhenAbsent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	sf, err := loadSettings(path)
	if err != nil {
		t.Fatalf("loadSettings: %v", err)
	}

	action, err := reconcileHook(sf, "SessionStart", "hook session-start", testDesiredCmd, testLegacyCmd, true)
	if err != nil {
		t.Fatalf("reconcileHook: %v", err)
	}
	if action != hookAdded {
		t.Errorf("action = %v, want hookAdded", action)
	}
	if !sf.hasHook("SessionStart", "hook session-start") {
		t.Error("hook should be present")
	}
}

func TestReconcileHook_MigratesLegacyQuotingOnWindows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	sf, err := loadSettings(path)
	if err != nil {
		t.Fatalf("loadSettings: %v", err)
	}
	if err := sf.addHook("SessionStart", hookEntry{Hooks: []hookAction{{Type: "command", Command: testLegacyCmd}}}); err != nil {
		t.Fatalf("addHook: %v", err)
	}

	action, err := reconcileHook(sf, "SessionStart", "hook session-start", testDesiredCmd, testLegacyCmd, true)
	if err != nil {
		t.Fatalf("reconcileHook: %v", err)
	}
	if action != hookMigrated {
		t.Errorf("action = %v, want hookMigrated", action)
	}
	got, ok, _ := sf.findHookCommand("SessionStart", "hook session-start")
	if !ok || got != testDesiredCmd {
		t.Errorf("findHookCommand = (%q, %v), want (%q, true)", got, ok, testDesiredCmd)
	}
}

func TestReconcileHook_LeavesLegacyQuotingUntouchedOnPOSIX(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	sf, err := loadSettings(path)
	if err != nil {
		t.Fatalf("loadSettings: %v", err)
	}
	legacy := "'/usr/local/bin/ghost' hook session-start"
	if err := sf.addHook("SessionStart", hookEntry{Hooks: []hookAction{{Type: "command", Command: legacy}}}); err != nil {
		t.Fatalf("addHook: %v", err)
	}

	action, err := reconcileHook(sf, "SessionStart", "hook session-start", legacy, legacy, false)
	if err != nil {
		t.Fatalf("reconcileHook: %v", err)
	}
	if action != hookUnchanged {
		t.Errorf("action = %v, want hookUnchanged — single-quoted is the correct POSIX form", action)
	}
	got, _, _ := sf.findHookCommand("SessionStart", "hook session-start")
	if got != legacy {
		t.Errorf("command changed to %q, want untouched %q", got, legacy)
	}
}

// TestReconcileHook_LeavesAGhostArgumentToItsCommand is #1358 on the Windows
// path: a hand-edited command whose ghost binary is only an ARGUMENT of another
// program — `"C:\tools\wrapper.exe" ghost hook session-start --verbose` runs
// wrapper.exe — is not ghost's own hook. Ghost's entry is added beside it and
// the wrapper is left byte-identical, because only a command whose LEADING
// token is a ghost binary is eligible for the legacy-quoting or stale-path
// repair.
func TestReconcileHook_LeavesAGhostArgumentToItsCommand(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	sf, err := loadSettings(path)
	if err != nil {
		t.Fatalf("loadSettings: %v", err)
	}
	custom := `"C:\tools\wrapper.exe" ghost hook session-start --verbose`
	if err := sf.addHook("SessionStart", hookEntry{Hooks: []hookAction{{Type: "command", Command: custom}}}); err != nil {
		t.Fatalf("addHook: %v", err)
	}

	action, err := reconcileHook(sf, "SessionStart", "hook session-start", testDesiredCmd, testLegacyCmd, true)
	if err != nil {
		t.Fatalf("reconcileHook: %v", err)
	}
	if action != hookAdded {
		t.Errorf("action = %v, want hookAdded — a ghost binary in argument position is not ghost's hook", action)
	}
	cmds, err := sf.hookCommands("SessionStart")
	if err != nil {
		t.Fatalf("hookCommands: %v", err)
	}
	if len(cmds) != 2 || cmds[0] != custom || cmds[1] != testDesiredCmd {
		t.Errorf("event holds %q, want the wrapper untouched and ghost's own hook beside it", cmds)
	}
}

// TestReconcileHook_MigratesExactLegacyCommandOnly covers the CodeRabbit
// finding on PR #255: a hand-edited wrapper that starts with ' and mentions
// "hook session-start" must never be mistaken for the legacy command, and the
// exact legacy command must be found and migrated regardless of which entry
// comes first in the hook list.
// TestMigratePreContractHook_MigratesBehindNonMatchingFirstEntry pins the
// all-entries rule: a stale duplicate sorting ahead of the real bare hook
// must not mask it. The first entry contains the bare tail as a substring
// but has extra args (a hand-edited wrapper), so only the second, genuinely
// bare command is migrated — regardless of order.
func TestMigratePreContractHook_MigratesBehindNonMatchingFirstEntry(t *testing.T) {
	desired := "/bin/ghost hook stop --source claude-code"
	for _, order := range []string{"wrapper-first", "bare-first"} {
		t.Run(order, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "settings.json")
			sf, err := loadSettings(path)
			if err != nil {
				t.Fatalf("loadSettings: %v", err)
			}

			wrapper := `/bin/ghost hook stop --verbose`
			bare := `/bin/ghost hook stop`
			first, second := wrapper, bare
			if order == "bare-first" {
				first, second = bare, wrapper
			}
			entry := hookEntry{Hooks: []hookAction{
				{Type: "command", Command: first},
				{Type: "command", Command: second},
			}}
			if err := sf.addHook("Stop", entry); err != nil {
				t.Fatalf("addHook: %v", err)
			}

			migrated, err := migratePreContractHook(sf, "Stop", "hook stop", desired)
			if err != nil {
				t.Fatalf("migratePreContractHook: %v", err)
			}
			if !migrated {
				t.Error("expected the bare hook to be migrated")
			}

			stillBare, err := sf.hasExactHookCommand("Stop", bare)
			if err != nil {
				t.Fatalf("hasExactHookCommand: %v", err)
			}
			if stillBare {
				t.Error("bare pre-contract hook survived the migration")
			}
			isDesired, err := sf.hasExactHookCommand("Stop", desired)
			if err != nil {
				t.Fatalf("hasExactHookCommand: %v", err)
			}
			if !isDesired {
				t.Error("contract-form command not found after migration")
			}
			wrapperIntact, err := sf.hasExactHookCommand("Stop", wrapper)
			if err != nil {
				t.Fatalf("hasExactHookCommand: %v", err)
			}
			if !wrapperIntact {
				t.Error("hand-edited wrapper was clobbered by the migration")
			}
		})
	}
}

func TestReconcileHook_MigratesExactLegacyCommandOnly(t *testing.T) {
	wrapper := `'C:\tools\wrap.exe' --run 'hook session-start' --extra`

	for _, order := range []string{"legacy-first", "wrapper-first"} {
		t.Run(order, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "settings.json")
			sf, err := loadSettings(path)
			if err != nil {
				t.Fatalf("loadSettings: %v", err)
			}

			first, second := testLegacyCmd, wrapper
			if order == "wrapper-first" {
				first, second = wrapper, testLegacyCmd
			}
			entry := hookEntry{Hooks: []hookAction{
				{Type: "command", Command: first},
				{Type: "command", Command: second},
			}}
			if err := sf.addHook("SessionStart", entry); err != nil {
				t.Fatalf("addHook: %v", err)
			}

			action, err := reconcileHook(sf, "SessionStart", "hook session-start", testDesiredCmd, testLegacyCmd, true)
			if err != nil {
				t.Fatalf("reconcileHook: %v", err)
			}
			if action != hookMigrated {
				t.Errorf("action = %v, want hookMigrated", action)
			}

			isLegacy, err := sf.hasExactHookCommand("SessionStart", testLegacyCmd)
			if err != nil {
				t.Fatalf("hasExactHookCommand: %v", err)
			}
			if isLegacy {
				t.Error("legacy command was not migrated")
			}
			isDesired, err := sf.hasExactHookCommand("SessionStart", testDesiredCmd)
			if err != nil {
				t.Fatalf("hasExactHookCommand: %v", err)
			}
			if !isDesired {
				t.Error("migrated command not found")
			}
			wrapperStillPresent, err := sf.hasExactHookCommand("SessionStart", wrapper)
			if err != nil {
				t.Fatalf("hasExactHookCommand: %v", err)
			}
			if !wrapperStillPresent {
				t.Error("hand-edited wrapper was clobbered by the migration")
			}
		})
	}
}

// TestReconcileHook_RepairsStaleGhostPathWindows covers #273: after an
// upgrade moves the ghost binary, a previously registered hook pointing at
// the old path must be rewritten to the new one, not left stale.
func TestReconcileHook_RepairsStaleGhostPathWindows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	sf, err := loadSettings(path)
	if err != nil {
		t.Fatalf("loadSettings: %v", err)
	}
	stale := `"C:\old\ghost.exe" hook session-start`
	desired := `"C:\new\ghost.exe" hook session-start`
	legacy := `'C:\new\ghost.exe' hook session-start`
	if err := sf.addHook("SessionStart", hookEntry{Hooks: []hookAction{{Type: "command", Command: stale}}}); err != nil {
		t.Fatalf("addHook: %v", err)
	}

	action, err := reconcileHook(sf, "SessionStart", "hook session-start", desired, legacy, true)
	if err != nil {
		t.Fatalf("reconcileHook: %v", err)
	}
	if action != hookMigrated {
		t.Errorf("action = %v, want hookMigrated", action)
	}
	got, ok, _ := sf.findHookCommand("SessionStart", "hook session-start")
	if !ok || got != desired {
		t.Errorf("findHookCommand = (%q, %v), want (%q, true)", got, ok, desired)
	}
}

// TestIsGhostBinaryName pins the launcher extensions a ghost command may carry.
// A Windows install can resolve to a "ghost.bat"/"ghost.cmd" shim rather than
// the native ".exe", and a hook command embedding one of those must still be
// recognised as ghost's — that recognition is what makes the codex merge
// idempotent on a re-run, not only on the platform the shim appears on.
func TestIsGhostBinaryName(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"ghost", true},
		{"/usr/local/bin/ghost", true},
		{"C:\\old\\ghost.exe", true},
		{"C:\\old\\GHOST.EXE", true},
		{"C:\\old\\ghost.bat", true},
		{"C:\\old\\GHOST.BAT", true},
		{"C:\\old\\ghost.cmd", true},
		{"ghost.bat", true},
		{"/usr/local/bin/ghost-helper", false},
		{"C:\\old\\ghost.batx", false},
		{"ghostly", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := isGhostBinaryName(tc.path); got != tc.want {
			t.Errorf("isGhostBinaryName(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

// TestReconcileHook_RepairsStaleGhostPathPOSIX is the POSIX counterpart of
// TestReconcileHook_RepairsStaleGhostPathWindows — the repair must not be
// gated on isWindows, since binaries move on every platform.
func TestReconcileHook_RepairsStaleGhostPathPOSIX(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	sf, err := loadSettings(path)
	if err != nil {
		t.Fatalf("loadSettings: %v", err)
	}
	stale := `'/opt/old/ghost' hook session-start`
	desired := `'/opt/new/ghost' hook session-start`
	if err := sf.addHook("SessionStart", hookEntry{Hooks: []hookAction{{Type: "command", Command: stale}}}); err != nil {
		t.Fatalf("addHook: %v", err)
	}

	action, err := reconcileHook(sf, "SessionStart", "hook session-start", desired, desired, false)
	if err != nil {
		t.Fatalf("reconcileHook: %v", err)
	}
	if action != hookMigrated {
		t.Errorf("action = %v, want hookMigrated", action)
	}
	got, ok, _ := sf.findHookCommand("SessionStart", "hook session-start")
	if !ok || got != desired {
		t.Errorf("findHookCommand = (%q, %v), want (%q, true)", got, ok, desired)
	}
}

// TestReconcileHook_StopsOnMalformedHooks covers the CodeRabbit finding on
// PR #255: a structurally invalid "hooks" value (valid JSON, wrong shape)
// must halt reconciliation with an error rather than being silently
// discarded by addHook's own parse-failure fallback.
func TestReconcileHook_StopsOnMalformedHooks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(path, []byte(`{"hooks":[]}`), 0600); err != nil {
		t.Fatalf("write settings: %v", err)
	}
	sf, err := loadSettings(path)
	if err != nil {
		t.Fatalf("loadSettings: %v", err)
	}

	if _, err := reconcileHook(sf, "SessionStart", "hook session-start", testDesiredCmd, testLegacyCmd, true); err == nil {
		t.Fatal("expected reconcileHook to return an error for malformed hooks")
	}

	// The malformed value must survive untouched — no silent overwrite.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !strings.Contains(string(raw), `"hooks":[]`) {
		t.Errorf("malformed hooks value was altered: %s", raw)
	}
}

// TestReconcileHook_StopsOnNullHooks covers the CodeRabbit Critical finding
// on PR #255: {"hooks":null} must halt reconciliation with an error rather
// than reaching addHook, which assigns into the "hooks" map unconditionally
// and would panic on the nil map produced by unmarshaling a null root.
func TestReconcileHook_StopsOnNullHooks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(path, []byte(`{"hooks":null}`), 0600); err != nil {
		t.Fatalf("write settings: %v", err)
	}
	sf, err := loadSettings(path)
	if err != nil {
		t.Fatalf("loadSettings: %v", err)
	}

	if _, err := reconcileHook(sf, "SessionStart", "hook session-start", testDesiredCmd, testLegacyCmd, true); err == nil {
		t.Fatal("expected reconcileHook to return an error for hooks:null")
	}
}

// TestReconcileHook_StopsOnNullEventHooks covers the same finding for a null
// value scoped to a single event rather than the whole "hooks" object.
func TestReconcileHook_StopsOnNullEventHooks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(path, []byte(`{"hooks":{"SessionStart":null}}`), 0600); err != nil {
		t.Fatalf("write settings: %v", err)
	}
	sf, err := loadSettings(path)
	if err != nil {
		t.Fatalf("loadSettings: %v", err)
	}

	if _, err := reconcileHook(sf, "SessionStart", "hook session-start", testDesiredCmd, testLegacyCmd, true); err == nil {
		t.Fatal("expected reconcileHook to return an error for hooks.SessionStart:null")
	}
}

func TestReconcileHook_PreservesOtherKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(path, []byte(`{
		"otherTopLevelKey": "keep-me",
		"hooks": {
			"PreToolUse": [{"matcher": "Bash", "hooks": [{"type": "command", "command": "echo hi", "timeout": 30}]}],
			"SessionStart": [{"matcher": "", "hooks": [{"type": "command", "command": "'C:\\ghost\\ghost.exe' hook session-start"}]}]
		}
	}`), 0600); err != nil {
		t.Fatalf("write settings: %v", err)
	}
	sf, err := loadSettings(path)
	if err != nil {
		t.Fatalf("loadSettings: %v", err)
	}

	if _, err := reconcileHook(sf, "SessionStart", "hook session-start", testDesiredCmd, testLegacyCmd, true); err != nil {
		t.Fatalf("reconcileHook: %v", err)
	}
	if err := sf.save(); err != nil {
		t.Fatalf("save: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out["otherTopLevelKey"] != "keep-me" {
		t.Errorf("otherTopLevelKey was dropped: %v", out)
	}
	hooks, _ := out["hooks"].(map[string]any)
	preToolUse, _ := json.Marshal(hooks["PreToolUse"])
	if !strings.Contains(string(preToolUse), `"timeout":30`) {
		t.Errorf("PreToolUse entry lost its timeout field: %s", preToolUse)
	}
}

// TestMigratePreContractHook_LeavesAForeignCommandAlone pins the executable
// test the migration shares with isStaleGhostHookCommand: a bare `… hook
// <event>` is rewritten only when its leading token is a ghost binary, so a
// wrapper or another tool whose command happens to end in the same argument
// tail keeps its command. The genuine bare ghost hook beside it must still be
// migrated, for both events.
func TestMigratePreContractHook_LeavesAForeignCommandAlone(t *testing.T) {
	for _, tc := range []struct {
		event    string
		bareTail string
		desired  string
		foreign  []string
		ghost    string
	}{
		{
			event:    "SessionStart",
			bareTail: "hook session-start",
			desired:  "/opt/ghost hook session-start --source claude-code",
			foreign: []string{
				"/usr/local/bin/othertool hook session-start",
				`'/usr/local/bin/othertool' hook session-start`,
				`"C:\tools\othertool.exe" hook session-start`,
			},
			ghost: "'/usr/local/bin/ghost' hook session-start",
		},
		{
			event:    "Stop",
			bareTail: "hook stop",
			desired:  "/opt/ghost hook stop --source claude-code",
			foreign: []string{
				"/usr/local/bin/othertool hook stop",
				`'/usr/local/bin/othertool' hook stop`,
				`"C:\tools\othertool.exe" hook stop`,
			},
			ghost: "'/usr/local/bin/ghost' hook stop",
		},
	} {
		t.Run(tc.event, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "settings.json")
			sf, err := loadSettings(path)
			if err != nil {
				t.Fatalf("loadSettings: %v", err)
			}
			for _, cmd := range tc.foreign {
				if err := sf.addHook(tc.event, hookEntry{Hooks: []hookAction{{Type: "command", Command: cmd}}}); err != nil {
					t.Fatalf("addHook foreign %q: %v", cmd, err)
				}
			}
			if err := sf.addHook(tc.event, hookEntry{Hooks: []hookAction{{Type: "command", Command: tc.ghost}}}); err != nil {
				t.Fatalf("addHook ghost: %v", err)
			}

			migrated, err := migratePreContractHook(sf, tc.event, tc.bareTail, tc.desired)
			if err != nil {
				t.Fatalf("migratePreContractHook: %v", err)
			}
			if !migrated {
				t.Error("expected the genuine bare ghost hook to be migrated")
			}
			for _, cmd := range tc.foreign {
				intact, err := sf.hasExactHookCommand(tc.event, cmd)
				if err != nil {
					t.Fatalf("hasExactHookCommand(%q): %v", cmd, err)
				}
				if !intact {
					t.Errorf("foreign command %q was rewritten by the migration", cmd)
				}
			}
			stillGhost, err := sf.hasExactHookCommand(tc.event, tc.ghost)
			if err != nil {
				t.Fatalf("hasExactHookCommand(ghost): %v", err)
			}
			if stillGhost {
				t.Error("genuine bare ghost hook survived the migration")
			}
			isDesired, err := sf.hasExactHookCommand(tc.event, tc.desired)
			if err != nil {
				t.Fatalf("hasExactHookCommand(desired): %v", err)
			}
			if !isDesired {
				t.Error("contract-form command not found after migration")
			}
		})
	}
}

// TestEnsureHook_InstallsItsOwnHookBesideAForeignCommand is #1326 end to end,
// for both events. A settings entry whose command is another tool's but ends
// in ghost's own argument tail is not a ghost install: reconcileHook found it
// by substring, reported "already configured" and wrote nothing, so Ghost's
// session context and stop hook never ran under a success line. The foreign
// command must survive untouched, ghost's own must be added beside it, and the
// second run must recognise the hook it just installed rather than appending
// another copy of it on every re-run.
func TestEnsureHook_InstallsItsOwnHookBesideAForeignCommand(t *testing.T) {
	const ghostBin = "/usr/local/bin/ghost"
	for _, tc := range []struct {
		event, other, tail, foreign string
	}{
		{
			event:   "SessionStart",
			other:   "Stop",
			tail:    "hook session-start",
			foreign: "/usr/local/bin/othertool hook session-start",
		},
		{
			event:   "Stop",
			other:   "SessionStart",
			tail:    "hook stop",
			foreign: "/usr/local/bin/othertool hook stop",
		},
	} {
		t.Run(tc.event, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "settings.json")
			sf, err := loadSettings(path)
			if err != nil {
				t.Fatalf("loadSettings: %v", err)
			}
			if err := sf.addHook(tc.event, hookEntry{Hooks: []hookAction{{Type: "command", Command: tc.foreign}}}); err != nil {
				t.Fatalf("addHook foreign: %v", err)
			}
			want := shellQuote(ghostBin) + " " + tc.tail + " --source claude-code"

			ensure := ensureStopHook
			if tc.event == "SessionStart" {
				ensure = ensureHook
			}
			countGhost := func() int {
				t.Helper()
				cmds, err := sf.hookCommands(tc.event)
				if err != nil {
					t.Fatalf("hookCommands: %v", err)
				}
				n := 0
				for _, cmd := range cmds {
					if cmd == want {
						n++
					}
				}
				return n
			}

			var out bytes.Buffer
			if err := ensure(&out, sf, ghostBin, false); err != nil {
				t.Fatalf("ensure (first): %v", err)
			}
			if strings.Contains(out.String(), "already configured") {
				t.Errorf("a first run that installed nothing reported the hook as configured:\n%s", out.String())
			}
			if intact, err := sf.hasExactHookCommand(tc.event, tc.foreign); err != nil {
				t.Fatalf("hasExactHookCommand(foreign): %v", err)
			} else if !intact {
				t.Error("the foreign command was rewritten")
			}
			if n := countGhost(); n != 1 {
				t.Errorf("ghost's own hook is registered %d times, want 1", n)
			}

			// A re-run must find the hook the first run installed: the foreign
			// entry still sorts first, so a substring read alone would append
			// a second copy and claim it was already configured.
			out.Reset()
			if err := ensure(&out, sf, ghostBin, false); err != nil {
				t.Fatalf("ensure (second): %v", err)
			}
			if !strings.Contains(out.String(), "already configured") {
				t.Errorf("second run should be a no-op, got:\n%s", out.String())
			}
			if n := countGhost(); n != 1 {
				t.Errorf("second run left %d copies of ghost's hook, want 1", n)
			}
			if intact, err := sf.hasExactHookCommand(tc.event, tc.foreign); err != nil {
				t.Fatalf("hasExactHookCommand(foreign, second): %v", err)
			} else if !intact {
				t.Error("the second run rewrote the foreign command")
			}
			if sf.hasHook(tc.other, tc.tail) {
				t.Errorf("%s hook leaked into %s", tc.event, tc.other)
			}
		})
	}
}

// TestEnsureHook_TreatsAWrappedGhostCommandAsItsOwn is #1326 end to end for
// both events and for four wrapper shapes: a leading process (`env X=1 ghost
// …`), a `cd … && ghost …` prefix, leading whitespace, and a quoted argument
// carrying the whole invocation (`sh -c "ghost …"`). Each invokes ghost, so
// init must recognise the event as already wired — the wrapper is left
// byte-identical, exactly one command stays registered, and a second run is a
// fixed point with no second copy appended.
func TestEnsureHook_TreatsAWrappedGhostCommandAsItsOwn(t *testing.T) {
	const ghostBin = "/usr/local/bin/ghost"
	for _, tc := range []struct {
		event, other, tail string
	}{
		{"SessionStart", "Stop", "hook session-start"},
		{"Stop", "SessionStart", "hook stop"},
	} {
		for _, w := range []struct{ label, command string }{
			{"env-prefix", "env X=1 " + ghostBin + " " + tc.tail + " --source claude-code"},
			{"cd-and", "cd /somewhere && " + ghostBin + " " + tc.tail + " --source claude-code"},
			{"leading-space", "   " + ghostBin + " " + tc.tail + " --source claude-code"},
			{"quoted-argument", `/bin/sh -c "` + ghostBin + ` ` + tc.tail + ` --source claude-code"`},
			{"sudo-prefix", "sudo " + ghostBin + " " + tc.tail + " --source claude-code"},
		} {
			t.Run(tc.event+"/"+w.label, func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "settings.json")
				sf, err := loadSettings(path)
				if err != nil {
					t.Fatalf("loadSettings: %v", err)
				}
				if err := sf.addHook(tc.event, hookEntry{Hooks: []hookAction{{Type: "command", Command: w.command}}}); err != nil {
					t.Fatalf("addHook: %v", err)
				}

				ensure := ensureStopHook
				if tc.event == "SessionStart" {
					ensure = ensureHook
				}

				var out bytes.Buffer
				if err := ensure(&out, sf, ghostBin, false); err != nil {
					t.Fatalf("ensure (first): %v", err)
				}
				if strings.Contains(out.String(), "added") {
					t.Errorf("a wrapped ghost invocation was duplicated instead of recognised:\n%s", out.String())
				}
				if !strings.Contains(out.String(), "already configured") {
					t.Errorf("a wrapped ghost invocation was not reported as configured:\n%s", out.String())
				}
				cmds, err := sf.hookCommands(tc.event)
				if err != nil {
					t.Fatalf("hookCommands: %v", err)
				}
				if len(cmds) != 1 || cmds[0] != w.command {
					t.Errorf("event holds %q, want only the untouched wrapper %q", cmds, w.command)
				}

				// A second run is a fixed point: the same single wrapper stands.
				out.Reset()
				if err := ensure(&out, sf, ghostBin, false); err != nil {
					t.Fatalf("ensure (second): %v", err)
				}
				if strings.Contains(out.String(), "added") {
					t.Errorf("the second run appended a second entry:\n%s", out.String())
				}
				after, err := sf.hookCommands(tc.event)
				if err != nil {
					t.Fatalf("hookCommands (second): %v", err)
				}
				if len(after) != 1 || after[0] != w.command {
					t.Errorf("the second run changed the event to %q", after)
				}
				if sf.hasHook(tc.other, tc.tail) {
					t.Errorf("%s hook leaked into %s", tc.event, tc.other)
				}
			})
		}
	}
}

// TestEnsureHook_AnimatesAQuotedInvocationAndAFailSoftCommand pins the two
// shapes #1337 found the token walk missing. A quoted argument can carry
// ghost's whole invocation (`sh -c "ghost hook stop …"`, `bash -lc "…"`), and a
// command whose last token's quoting never closes still holds a valid ghost
// invocation at its front. Both must read as ghost's own, or init appends a
// second entry and the hook fires twice per session.
//
// The false positive is pinned alongside: a ghost binary named inside another
// tool's quoted ARGUMENT is not a command that runs, and reading it as one is
// how a foreign command came to suppress ghost's install (#1326).
func TestEnsureHook_AnimatesAQuotedInvocationAndAFailSoftCommand(t *testing.T) {
	const ghostBin = "/usr/local/bin/ghost"
	shell := "/bin/sh"
	desired := ghostBin + " hook stop --source claude-code"
	legacy := `"C:\ghost\ghost.exe" hook stop`

	for _, tc := range []struct {
		label, command string
		// statusWired is false where the ownership rule recognises the wrapper
		// but status's own --source read cannot, because the tail's value token
		// carries a shell separator (`claude-code;`). Ownership is what stops a
		// second entry; this narrower read is noted, not claimed.
		statusWired bool
	}{
		{"sh-c-quoted", shell + ` -c "` + ghostBin + ` hook stop --source claude-code"`, true},
		{"bash-qlc", `bash -lc "` + ghostBin + ` hook stop --source claude-code"`, true},
		{"sh-e", shell + ` -e -c "` + ghostBin + ` hook stop --source claude-code"`, true},
		{"bash-o-posix", `bash -o posix -c "` + ghostBin + ` hook stop --source claude-code"`, true},
		{"bash-posix", `bash --posix -c "` + ghostBin + ` hook stop --source claude-code"`, true},
		{"sh-c-exec-assignment", shell + ` -c "exec ` + ghostBin + ` hook stop --source claude-code"`, true},
		{"sh-c-env-assignment", shell + ` -c "env X=1 ` + ghostBin + ` hook stop --source claude-code"`, true},
		{"sh-c-joined-directory", shell + ` -c "cd /d && exec ` + ghostBin + ` hook stop --source claude-code"`, true},
		{"bash-qlc-positional", "/usr/local/bin/zsh -c '" + ghostBin + ` hook stop --source claude-code'`, true},
		{"busybox-prefixed", "busybox sh -c '" + ghostBin + ` hook stop --source claude-code'`, true},
		{"env-then-shell", "env -i " + shell + ` -c "` + ghostBin + ` hook stop --source claude-code"`, true},
		{"trailing-unclosed-quote", ghostBin + ` hook stop --source claude-code; echo "oops`, false},
		{"trailing-unclosed-quote-first-token", `X=1 ` + ghostBin + ` hook stop --source claude-code`, true},
	} {
		t.Run("recognised:"+tc.label, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "settings.json")
			sf, err := loadSettings(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := sf.addHook("Stop", hookEntry{Hooks: []hookAction{{Type: "command", Command: tc.command}}}); err != nil {
				t.Fatalf("addHook: %v", err)
			}

			var out bytes.Buffer
			if err := ensureStopHook(&out, sf, ghostBin, false); err != nil {
				t.Fatalf("ensureStopHook: %v", err)
			}
			if strings.Contains(out.String(), "added") {
				t.Errorf("a wrapped ghost invocation got a second entry:\n%s", out.String())
			}
			if !strings.Contains(out.String(), "already configured") {
				t.Errorf("a wrapped ghost invocation was not reported as configured:\n%s", out.String())
			}
			cmds, err := sf.hookCommands("Stop")
			if err != nil {
				t.Fatal(err)
			}
			if len(cmds) != 1 || cmds[0] != tc.command {
				t.Errorf("event = %q, want only the untouched wrapper %q", cmds, tc.command)
			}

			// Status agrees: a run that reads its own wrapper as missing
			// sends the operator to an init that only duplicates. The same
			// in-memory file, because ensure*Hook reports on the entry it
			// holds rather than re-reading one it never persists.
			if tc.statusWired && !contractHookWired(sf, "Stop", "stop", "claude-code", ghostBin) {
				t.Errorf("status reads the wrapper as unwired, which would ask for a repair that duplicates it")
			}
		})
	}

	// The same shapes must never be REWRITTEN: only a leading-token ghost
	// command is eligible for the legacy or stale-path repair.
	for _, tc := range []struct{ label, command string }{
		{"sh-c-quoted", shell + ` -c "` + ghostBin + ` hook stop --source claude-code"`},
		{"trailing-unclosed-quote", ghostBin + ` hook stop --source claude-code; echo "oops`},
	} {
		t.Run("not-rewritten:"+tc.label, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "settings.json")
			sf, err := loadSettings(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := sf.addHook("Stop", hookEntry{Hooks: []hookAction{{Type: "command", Command: tc.command}}}); err != nil {
				t.Fatal(err)
			}
			for _, isWin := range []bool{false, true} {
				action, err := reconcileHook(sf, "Stop", "hook stop", desired, legacy, isWin)
				if err != nil {
					t.Fatal(err)
				}
				if action != hookUnchanged {
					t.Errorf("isWindows=%v: action = %v, want hookUnchanged", isWin, action)
				}
				if isStaleGhostHookCommand(tc.command, desired) || isStaleGhostHookCommand(tc.command, legacy) {
					t.Errorf("isWindows=%v: the stale-path repair would rewrite the wrapper", isWin)
				}
			}
			if has, _ := sf.hasExactHookCommand("Stop", legacy); has {
				t.Error("the legacy repair would rewrite the wrapper")
			}
			cmds, _ := sf.hookCommands("Stop")
			if len(cmds) != 1 || cmds[0] != tc.command {
				t.Errorf("event = %q, want the wrapper untouched", cmds)
			}
		})
	}

	// A ghost binary named inside another tool's quoted argument is NOT a
	// command that runs. Reading it as one is the #1326 false positive, and the
	// interior rule must not reintroduce it. The gate itself belongs to a
	// shell, not to any flag (#1339): a single-dash flag ending in `c` only
	// opens the argument directly after a shell name, so another tool's `-c`
	// or `--sync` hands ghost's invocation over as data, exactly like
	// `--note`.
	//
	// The top level is held to the same command-position rule (#1358): an
	// unquoted `othertool --note ghost hook stop …` is foreign there too,
	// because the ghost binary is an ARGUMENT of othertool and not the command
	// `startsCommand` reaches. The wrappers that genuinely run ghost — an
	// assignment, `sudo`, `cd d && ghost` — still walk back to a command
	// position and stay ghost's own.
	for _, foreign := range []string{
		`/usr/local/bin/othertool --note "the ghost hook stop --source claude-code tail"`,
		`/usr/local/bin/othertool --note "run: ghost hook stop --source claude-code; done"`,
		"/usr/local/bin/othertool --note 'ghost hook stop // source claude-code'",
		`/usr/local/bin/othertool --note 'the ghost hook session-start tail'`,
		`/usr/local/bin/othertool --message "ghost hook stop --source claude-code"`,
		`/usr/local/bin/othertool "echo ghost hook stop --source claude-code"`,
		`/usr/local/bin/othertool --note "sh: ghost hook stop --source claude-code"`,
		`/usr/local/bin/othertool --note "x && ghost hook stop --source claude-code"`,
		`/usr/local/bin/othertool -c "ghost hook stop --source claude-code"`,
		`/usr/local/bin/othertool -xc "ghost hook stop --source claude-code"`,
		`/usr/local/bin/othertool --sync "ghost hook stop --source claude-code"`,
	} {
		t.Run("foreign:"+foreign, func(t *testing.T) {
			if isGhostHookCommand(foreign, "hook stop", desired) {
				t.Error("a ghost binary inside another tool's argument was read as ghost's own command")
			}
			if _, ok := ghostHookTail(foreign, "stop", ghostBin); ok {
				t.Error("ghostHookTail recognised a foreign command")
			}
		})
	}
}

// TestEnsureHook_AGhostTokenInArgumentPositionIsForeign is #1358: at the top
// level a ghost binary counts only in COMMAND position, under the same
// startsCommand walk the interior of a quoted command list already applies.
// The any-position read at depth 0 counted a ghost binary that is only an
// ARGUMENT of another command as ghost's own — `echo /opt/bin/ghost hook stop
// …` runs echo, not ghost — so init added nothing beside it and reported the
// event as already configured while ghost's own hook never ran. The wrappers
// that genuinely run ghost keep their command position and stay recognised.
func TestEnsureHook_AGhostTokenInArgumentPositionIsForeign(t *testing.T) {
	const ghostBin = "/usr/local/bin/ghost"
	desired := shellQuote(ghostBin) + " hook stop --source claude-code"

	for _, command := range []string{
		"echo /opt/bin/ghost hook stop --source claude-code",
		"/usr/local/bin/othertool --note " + ghostBin + " hook stop --source claude-code",
		"printf '%s' " + ghostBin + " hook stop --source claude-code",
		"env OTHER=1 /usr/local/bin/othertool " + ghostBin + " hook stop --source claude-code",
		"grep pattern " + ghostBin + " hook stop --source claude-code",
		"time -p echo " + ghostBin + " hook stop --source claude-code",
	} {
		t.Run(command, func(t *testing.T) {
			if isGhostHookCommand(command, "hook stop", desired) {
				t.Error("a ghost binary in argument position was read as ghost's own command")
			}
			if _, ok := ghostHookTail(command, "stop", ghostBin); ok {
				t.Error("ghostHookTail recognised a ghost binary in argument position")
			}

			path := filepath.Join(t.TempDir(), "settings.json")
			sf, err := loadSettings(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := sf.addHook("Stop", hookEntry{Hooks: []hookAction{{Type: "command", Command: command}}}); err != nil {
				t.Fatal(err)
			}

			var out bytes.Buffer
			if err := ensureStopHook(&out, sf, ghostBin, false); err != nil {
				t.Fatalf("ensureStopHook: %v", err)
			}
			if !strings.Contains(out.String(), "added") {
				t.Errorf("a foreign command was reported as already configured:\n%s", out.String())
			}
			cmds, err := sf.hookCommands("Stop")
			if err != nil {
				t.Fatal(err)
			}
			if len(cmds) != 2 || cmds[0] != command || cmds[1] != desired {
				t.Errorf("event = %q, want the foreign command untouched and ghost's own hook beside it", cmds)
			}
		})
	}
}

// TestEnsureHook_AQuotedOperatorInAnArgumentStaysForeign pins the other side of
// the glued-operator read: hookCommandTokens has already reversed the quoting,
// so a `;`, `&` or `|` that survives INSIDE a token came from a quoted argument
// — `othertool --note "a;b"`, a grep pattern `'x|y'`, a URL with `&`. That is
// the user's text and not the shell's operator, so it must not open command
// position: the ghost binary after it is that command's ARGUMENT, and reading
// it as ghost's own is the #1358 failure again — init reports the event as
// already configured, writes nothing, and the hook never runs.
func TestEnsureHook_AQuotedOperatorInAnArgumentStaysForeign(t *testing.T) {
	const ghostBin = "/usr/local/bin/ghost"
	desired := shellQuote(ghostBin) + " hook stop --source claude-code"

	for _, command := range []string{
		`/usr/local/bin/othertool --note "a;b" ` + ghostBin + ` hook stop --source claude-code`,
		`grep 'x|y' ` + ghostBin + ` hook stop --source claude-code`,
		`/usr/local/bin/othertool "https://example.test/?a=1&b=2" ` + ghostBin + ` hook stop --source claude-code`,
	} {
		t.Run(command, func(t *testing.T) {
			if isGhostHookCommand(command, "hook stop", desired) {
				t.Error("a ghost binary after a QUOTED operator was read as ghost's own command")
			}
			if _, ok := ghostHookTail(command, "stop", ghostBin); ok {
				t.Error("ghostHookTail credited a ghost binary after a quoted operator")
			}

			path := filepath.Join(t.TempDir(), "settings.json")
			sf, err := loadSettings(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := sf.addHook("Stop", hookEntry{Hooks: []hookAction{{Type: "command", Command: command}}}); err != nil {
				t.Fatal(err)
			}

			var out bytes.Buffer
			if err := ensureStopHook(&out, sf, ghostBin, false); err != nil {
				t.Fatalf("ensureStopHook: %v", err)
			}
			if !strings.Contains(out.String(), "added") {
				t.Errorf("a foreign command was reported as already configured:\n%s", out.String())
			}
			cmds, err := sf.hookCommands("Stop")
			if err != nil {
				t.Fatal(err)
			}
			if len(cmds) != 2 || cmds[0] != command || cmds[1] != desired {
				t.Errorf("event = %q, want the foreign command untouched and ghost's own hook beside it", cmds)
			}
		})
	}
}

// TestEnsureHook_AnUnquotedOperatorEndsTheList is the positive side: an
// UNQUOTED operator is the shell's own, so the word after it starts a command
// and ghost's binary there is ghost's — `cd /d;ghost …` with no space after the
// `;` and `cd /d && ghost …` both run ghost, so init must leave them
// byte-identical and add nothing, or the hook fires twice per session.
func TestEnsureHook_AnUnquotedOperatorEndsTheList(t *testing.T) {
	const ghostBin = "/usr/local/bin/ghost"
	for _, command := range []string{
		"cd /d;" + ghostBin + " hook stop --source claude-code",
		"cd /d && " + ghostBin + " hook stop --source claude-code",
	} {
		t.Run(command, func(t *testing.T) {
			if _, ok := ghostHookTail(command, "stop", ghostBin); !ok {
				t.Error("ghostHookTail did not credit a ghost binary after an unquoted operator")
			}

			path := filepath.Join(t.TempDir(), "settings.json")
			sf, err := loadSettings(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := sf.addHook("Stop", hookEntry{Hooks: []hookAction{{Type: "command", Command: command}}}); err != nil {
				t.Fatal(err)
			}

			var out bytes.Buffer
			if err := ensureStopHook(&out, sf, ghostBin, false); err != nil {
				t.Fatalf("ensureStopHook: %v", err)
			}
			if strings.Contains(out.String(), "added") {
				t.Errorf("a command after an unquoted operator got a second entry:\n%s", out.String())
			}
			cmds, err := sf.hookCommands("Stop")
			if err != nil {
				t.Fatal(err)
			}
			if len(cmds) != 1 || cmds[0] != command {
				t.Errorf("event = %q, want only the untouched command %q", cmds, command)
			}
		})
	}
}

// TestEnsureHook_APrefixSlotOfTheWrongShapeIsForeign pins the shape test on the
// word a prefix command takes for itself. `timeout` names its command AFTER a
// DURATION, `taskset` after a mask or list, and `chrt` after a priority, so a
// word of any other shape is the command itself: `taskset -c 0,1 echo /opt/bin
// /ghost …`, `timeout -k 5 echo …` and `chrt -f echo …` run echo with ghost as
// its argument, and crediting them would report the event as already configured
// while the hook never runs — #1358 again, one word deeper. The wrong shape is
// the whole test for these rows: the option before each one carries its own
// operand, so exactly one plain word remains and only a shape-blind slot could
// accept it. `timeout 5 echo …` is foreign by the slot COUNT as well, since a
// duration and a command word is one word too many.
func TestEnsureHook_APrefixSlotOfTheWrongShapeIsForeign(t *testing.T) {
	const ghostBin = "/usr/local/bin/ghost"
	desired := shellQuote(ghostBin) + " hook stop --source claude-code"

	for _, command := range []string{
		"taskset -c 0,1 echo /opt/bin/ghost hook stop --source claude-code",
		"taskset nothex /opt/bin/ghost hook stop --source claude-code",
		"timeout 5 echo /opt/bin/ghost hook stop --source claude-code",
		"timeout -k 5 echo /opt/bin/ghost hook stop --source claude-code",
		"chrt -f echo /opt/bin/ghost hook stop --source claude-code",
	} {
		t.Run(command, func(t *testing.T) {
			if _, ok := ghostHookTail(command, "stop", ghostBin); ok {
				t.Error("ghostHookTail credited a ghost binary behind a prefix slot of the wrong shape")
			}

			path := filepath.Join(t.TempDir(), "settings.json")
			sf, err := loadSettings(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := sf.addHook("Stop", hookEntry{Hooks: []hookAction{{Type: "command", Command: command}}}); err != nil {
				t.Fatal(err)
			}

			var out bytes.Buffer
			if err := ensureStopHook(&out, sf, ghostBin, false); err != nil {
				t.Fatalf("ensureStopHook: %v", err)
			}
			if !strings.Contains(out.String(), "added") {
				t.Errorf("a foreign command was reported as already configured:\n%s", out.String())
			}
			cmds, err := sf.hookCommands("Stop")
			if err != nil {
				t.Fatal(err)
			}
			if len(cmds) != 2 || cmds[0] != command || cmds[1] != desired {
				t.Errorf("event = %q, want the foreign command untouched and ghost's own hook beside it", cmds)
			}
		})
	}
}

// TestEnsureHook_APathQualifiedTransparentPrefixIsStillGhost pins the prefix
// walk against a PATH-QUALIFIED wrapper: `/usr/bin/env ghost hook stop …` and
// `/usr/bin/sudo ghost hook stop …` run ghost just as the bare words do, so the
// command-position walk must see through them — reading them as foreign writes
// a second entry beside a wrapper that already fires the hook, and the hook
// then runs twice per session.
func TestEnsureHook_APathQualifiedTransparentPrefixIsStillGhost(t *testing.T) {
	const ghostBin = "/usr/local/bin/ghost"
	for _, command := range []string{
		"/usr/bin/env " + ghostBin + " hook stop --source claude-code",
		"/usr/bin/sudo " + ghostBin + " hook stop --source claude-code",
		"/usr/bin/env -i X=1 " + ghostBin + " hook stop --source claude-code",
		"/usr/bin/sudo -u user " + ghostBin + " hook stop --source claude-code",
	} {
		t.Run(command, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "settings.json")
			sf, err := loadSettings(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := sf.addHook("Stop", hookEntry{Hooks: []hookAction{{Type: "command", Command: command}}}); err != nil {
				t.Fatal(err)
			}

			var out bytes.Buffer
			if err := ensureStopHook(&out, sf, ghostBin, false); err != nil {
				t.Fatalf("ensureStopHook: %v", err)
			}
			if strings.Contains(out.String(), "added") {
				t.Errorf("a path-qualified transparent prefix got a second entry:\n%s", out.String())
			}
			if !strings.Contains(out.String(), "already configured") {
				t.Errorf("a path-qualified transparent prefix was not reported as configured:\n%s", out.String())
			}
			cmds, err := sf.hookCommands("Stop")
			if err != nil {
				t.Fatal(err)
			}
			if len(cmds) != 1 || cmds[0] != command {
				t.Errorf("event = %q, want only the untouched wrapper %q", cmds, command)
			}
		})
	}
}

// TestEnsureHook_AdditionalRunGhostPrefixesAreRecognised pins the rest of the
// wrappers that genuinely run ghost after the command-position walk became the
// top-level rule: a DURATION or mask slot before the command (timeout, taskset,
// chrt), the words that run their operand as a command (nohup, time, doas,
// stdbuf, ionice), a NEWLINE as a command separator, a parenthesised subshell
// and a `{ …; }` group. Each must read as ghost's own, be left byte-identical
// and get no second entry, or the hook fires twice per session. The negative
// controls in TestEnsureHook_AGhostTokenInArgumentPositionIsForeign — a ghost
// binary that is another program's argument — stay foreign: echo, printf,
// othertool and grep do not run it.
func TestEnsureHook_AdditionalRunGhostPrefixesAreRecognised(t *testing.T) {
	const ghostBin = "/usr/local/bin/ghost"
	for _, tc := range []struct {
		command string
		// statusWired is whether `ghost mcp status` credits the wrapper's
		// --source. The subshell's closing `)` is its own token, so the value
		// reads exactly as `claude-code` and status agrees with the ownership
		// walk that leaves the wrapper byte-identical.
		statusWired bool
	}{
		{"timeout 5 " + ghostBin + " hook stop --source claude-code", true},
		{"timeout -k 5 10 " + ghostBin + " hook stop --source claude-code", true},
		{"timeout --signal=TERM 10 " + ghostBin + " hook stop --source claude-code", true},
		{"/usr/bin/timeout 5 " + ghostBin + " hook stop --source claude-code", true},
		{"nohup " + ghostBin + " hook stop --source claude-code", true},
		{"time " + ghostBin + " hook stop --source claude-code", true},
		{"time -p " + ghostBin + " hook stop --source claude-code", true},
		{"doas " + ghostBin + " hook stop --source claude-code", true},
		{"doas -u user " + ghostBin + " hook stop --source claude-code", true},
		{"stdbuf -oL -eL " + ghostBin + " hook stop --source claude-code", true},
		{"ionice -c 2 -n 4 " + ghostBin + " hook stop --source claude-code", true},
		{"chrt -f 10 " + ghostBin + " hook stop --source claude-code", true},
		{"taskset 0x3 " + ghostBin + " hook stop --source claude-code", true},
		{"taskset -c 0,1 " + ghostBin + " hook stop --source claude-code", true},
		{"cd /d\n" + ghostBin + " hook stop --source claude-code", true},
		{"sudo --user alice " + ghostBin + " hook stop --source claude-code", true},
		{"env --unset FOO " + ghostBin + " hook stop --source claude-code", true},
		{"timeout --signal TERM 5 " + ghostBin + " hook stop --source claude-code", true},
		{"timeout --kill-after 5s 10 " + ghostBin + " hook stop --source claude-code", true},
		{"nice --adjustment 10 " + ghostBin + " hook stop --source claude-code", true},
		{"stdbuf --output L " + ghostBin + " hook stop --source claude-code", true},
		{"ionice --class 2 --classdata 4 " + ghostBin + " hook stop --source claude-code", true},
		{"sudo --user=alice " + ghostBin + " hook stop --source claude-code", true},
		{"(cd /d; " + ghostBin + " hook stop --source claude-code)", true},
		{"{ " + ghostBin + " hook stop --source claude-code; }", true},
		{"X=1 timeout -k 5 10 " + ghostBin + " hook stop --source claude-code", true},
	} {
		command := tc.command
		t.Run(command, func(t *testing.T) {
			if _, ok := ghostHookTail(command, "stop", ghostBin); !ok {
				t.Error("ghostHookTail does not recognise a wrapper that runs ghost")
			}

			path := filepath.Join(t.TempDir(), "settings.json")
			sf, err := loadSettings(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := sf.addHook("Stop", hookEntry{Hooks: []hookAction{{Type: "command", Command: command}}}); err != nil {
				t.Fatal(err)
			}

			var out bytes.Buffer
			if err := ensureStopHook(&out, sf, ghostBin, false); err != nil {
				t.Fatalf("ensureStopHook: %v", err)
			}
			if strings.Contains(out.String(), "added") {
				t.Errorf("a wrapper that runs ghost got a second entry:\n%s", out.String())
			}
			if !strings.Contains(out.String(), "already configured") {
				t.Errorf("a wrapper that runs ghost was not reported as configured:\n%s", out.String())
			}
			cmds, err := sf.hookCommands("Stop")
			if err != nil {
				t.Fatal(err)
			}
			if len(cmds) != 1 || cmds[0] != command {
				t.Errorf("event = %q, want only the untouched wrapper %q", cmds, command)
			}
			// Status reads the same file with the same ownership rule.
			if wired := contractHookWired(sf, "Stop", "stop", "claude-code", ghostBin); wired != tc.statusWired {
				t.Errorf("status wired = %v, want %v", wired, tc.statusWired)
			}
		})
	}
}

// TestEnsureHook_RecognisesTransparentPrefixesInsideAShellScript is #1339:
// inside a shell script the ghost binary may sit behind a word that runs the
// command behind it unchanged — `sudo ghost hook stop …`, `env -i ghost hook
// stop …`, `nice …`, `command …`, `exec …` — so init must read the whole
// invocation as ghost's own, or it appends a second entry and the hook fires
// twice per session. A prefix word only counts where the scan reaches a
// command position behind it, so `echo sudo ghost hook stop …` (echo runs)
// and `othertool -i ghost hook stop …` (-i is othertool's argument, not
// env's) stay foreign. Status agrees with the reconcile on every recognised
// shape: a run that reads its own wrapper as missing sends the operator to an
// init that only duplicates.
func TestEnsureHook_RecognisesTransparentPrefixesInsideAShellScript(t *testing.T) {
	const ghostBin = "/usr/local/bin/ghost"
	const shell = "/bin/sh"
	desired := shellQuote(ghostBin) + " hook stop --source claude-code"

	script := func(inner string) string {
		return shell + ` -c "` + inner + " " + ghostBin + ` hook stop --source claude-code"`
	}

	for _, tc := range []struct{ label, inner string }{
		{"sudo", "sudo"},
		{"env-i", "env -i"},
		{"env-i-assignment", "env -i X=1"},
		{"env-assignment", "env X=1"},
		{"nice", "nice"},
		{"nice-n", "nice -n 10"},
		{"command", "command"},
		{"exec", "exec"},
		{"sudo-env-assignment", "sudo env X=1"},
		{"sudo-u", "sudo -u user"},
		{"env-u", "env -u FOO"},
		{"env-then-nice", "env X=1 nice"},
	} {
		t.Run("recognised:"+tc.label, func(t *testing.T) {
			command := script(tc.inner)
			path := filepath.Join(t.TempDir(), "settings.json")
			sf, err := loadSettings(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := sf.addHook("Stop", hookEntry{Hooks: []hookAction{{Type: "command", Command: command}}}); err != nil {
				t.Fatalf("addHook: %v", err)
			}

			var out bytes.Buffer
			if err := ensureStopHook(&out, sf, ghostBin, false); err != nil {
				t.Fatalf("ensureStopHook: %v", err)
			}
			if strings.Contains(out.String(), "added") {
				t.Errorf("a prefixed ghost invocation got a second entry:\n%s", out.String())
			}
			if !strings.Contains(out.String(), "already configured") {
				t.Errorf("a prefixed ghost invocation was not reported as configured:\n%s", out.String())
			}
			cmds, err := sf.hookCommands("Stop")
			if err != nil {
				t.Fatal(err)
			}
			if len(cmds) != 1 || cmds[0] != command {
				t.Errorf("event = %q, want only the untouched wrapper %q", cmds, command)
			}
			if !contractHookWired(sf, "Stop", "stop", "claude-code", ghostBin) {
				t.Error("status reads the wrapper as unwired, which would ask for a repair that duplicates it")
			}
		})
	}

	for _, tc := range []struct{ label, inner string }{
		{"echo-sudo", "echo sudo"},
		{"othertool-sudo", "othertool sudo"},
		{"othertool-env-assignment", "othertool env X=1"},
		{"othertool-i", "othertool -i"},
		{"othertool-n", "othertool -n 10"},
		{"assignment-then-othertool", "X=1 othertool"},
	} {
		t.Run("foreign:"+tc.label, func(t *testing.T) {
			command := script(tc.inner)
			path := filepath.Join(t.TempDir(), "settings.json")
			sf, err := loadSettings(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := sf.addHook("Stop", hookEntry{Hooks: []hookAction{{Type: "command", Command: command}}}); err != nil {
				t.Fatalf("addHook: %v", err)
			}

			var out bytes.Buffer
			if err := ensureStopHook(&out, sf, ghostBin, false); err != nil {
				t.Fatalf("ensureStopHook: %v", err)
			}
			if !strings.Contains(out.String(), "added") {
				t.Errorf("a foreign script was read as ghost's own hook:\n%s", out.String())
			}
			cmds, err := sf.hookCommands("Stop")
			if err != nil {
				t.Fatal(err)
			}
			if len(cmds) != 2 || cmds[0] != command || cmds[1] != desired {
				t.Errorf("event = %q, want the foreign script untouched and ghost's own hook beside it", cmds)
			}
		})
	}
}

// TestEnsureHook_RecognisesTheBinaryItWouldWrite pins the other arm of the
// ownership rule behind #1326: a ghost binary installed under another name runs
// ghost's hook just as one called "ghost" does, so init recognises its own
// command under that name, adds nothing beside it and reports the event
// configured. An ownership test on the basename alone would install a second
// entry beside the first, and the hook would fire twice per session.
func TestEnsureHook_RecognisesTheBinaryItWouldWrite(t *testing.T) {
	const ghostBin = "/usr/local/bin/g"
	for _, tc := range []struct {
		event, other, tail string
	}{
		{"SessionStart", "Stop", "hook session-start"},
		{"Stop", "SessionStart", "hook stop"},
	} {
		t.Run(tc.event, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "settings.json")
			sf, err := loadSettings(path)
			if err != nil {
				t.Fatalf("loadSettings: %v", err)
			}
			own := ghostBin + " " + tc.tail + " --source claude-code"
			if err := sf.addHook(tc.event, hookEntry{Hooks: []hookAction{{Type: "command", Command: own}}}); err != nil {
				t.Fatalf("addHook: %v", err)
			}

			ensure := ensureStopHook
			if tc.event == "SessionStart" {
				ensure = ensureHook
			}
			var out bytes.Buffer
			if err := ensure(&out, sf, ghostBin, false); err != nil {
				t.Fatalf("ensure: %v", err)
			}
			if !strings.Contains(out.String(), "already configured") {
				t.Errorf("ghost's own command under its installed name was not recognised:\n%s", out.String())
			}
			cmds, err := sf.hookCommands(tc.event)
			if err != nil {
				t.Fatalf("hookCommands: %v", err)
			}
			if len(cmds) != 1 {
				t.Errorf("event holds %d commands, want 1: %q", len(cmds), cmds)
			}
			if sf.hasHook(tc.other, tc.tail) {
				t.Errorf("%s hook leaked into %s", tc.event, tc.other)
			}
		})
	}
}

func TestWarnPercentPath(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("warnPercentPath only warns on windows")
	}
	var out strings.Builder
	warnPercentPath(&out, `C:\Users\%USERNAME%\ghost.exe`)
	if !strings.Contains(out.String(), "warning") {
		t.Errorf("expected a warning, got %q", out.String())
	}
}

// TestEnsureHook_AnOptionOperandIsNotTheCommand pins the word an option takes
// as its own value: in `timeout -k /opt/bin/ghost hook stop …`, `env -u …` and
// `sudo -u …` the ghost binary is the option's operand, so the prefix runs
// `hook` and the stop hook never fires.
func TestEnsureHook_AnOptionOperandIsNotTheCommand(t *testing.T) {
	const ghostBin = "/usr/local/bin/ghost"

	for _, command := range []string{
		"timeout -k /opt/bin/ghost hook stop --source claude-code",
		"env -u /opt/bin/ghost hook stop --source claude-code",
		"sudo -u /opt/bin/ghost hook stop --source claude-code",
	} {
		t.Run(command, func(t *testing.T) {
			if _, ok := ghostHookTail(command, "stop", ghostBin); ok {
				t.Error("ghostHookTail credited a ghost binary that is an option's operand")
			}
		})
	}
}

// TestSplitUnquotedOperators_CutsASubshellCloser: the `)` that closes a
// subshell is its own token, a `)` inside quotes or paired with a `(` in the
// same word is left alone.
func TestSplitUnquotedOperators_CutsASubshellCloser(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want []string
	}{
		{"claude-code)", []string{"claude-code", ")"}},
		{"claude-code))", []string{"claude-code", ")", ")"}},
		{"x;y)", []string{"x", ";", "y", ")"}},
		{"$(date)", []string{"$(date)"}},
		{")", []string{")"}},
	} {
		if got := splitUnquotedOperators(tc.in); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("splitUnquotedOperators(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	got, _ := hookCommandTokens(`echo "a)" b`)
	if want := []string{"echo", "a)", "b"}; !reflect.DeepEqual(got, want) {
		t.Errorf("quoted closer tokens = %q, want %q", got, want)
	}
}

const currentRedirectTemplate = "# myproject Project Memory\n\nAll project knowledge is stored in Ghost and injected automatically at session start via the SessionStart hook.\nProject context (memories + summary) is already in your system prompt — no tool calls needed to load it.\n\nUse `ghost_memory_save` to save new discoveries during work.\nUse `ghost_memory_search` to search for specific facts.\n"

const oldRedirectTemplate = "# myproject Project Memory\n\nAll project knowledge is stored in Ghost. At session start, run:\n1. `ghost_list_projects` to discover projects\n2. `ghost_project_context` with project_id \"myproject\" to load accumulated knowledge\n"

func TestWriteRedirects_UpgradesUneditedOldRedirect(t *testing.T) {
	home := t.TempDir()
	setHome(t, home)
	projectPath := absProjectPath(t)
	projects := []projectInfo{{ID: "abc123", Path: projectPath, Name: "myproject"}}

	target := filepath.Join(home, ".claude", "projects", projectSlug(projectPath), "memory", "MEMORY.md")
	if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte(oldRedirectTemplate), 0644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	writeRedirects(&out, projects, false)

	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != currentRedirectTemplate {
		t.Fatalf("redirect not upgraded:\n got: %q\nwant: %q", string(data), currentRedirectTemplate)
	}
	if !strings.Contains(out.String(), "created redirect") {
		t.Fatalf("expected 'created redirect', got: %s", out.String())
	}
}

func TestWriteRedirects_KeepsEditedOldRedirect(t *testing.T) {
	home := t.TempDir()
	setHome(t, home)
	projectPath := absProjectPath(t)
	projects := []projectInfo{{ID: "abc123", Path: projectPath, Name: "myproject"}}

	target := filepath.Join(home, ".claude", "projects", projectSlug(projectPath), "memory", "MEMORY.md")
	if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
		t.Fatal(err)
	}
	edited := oldRedirectTemplate + "USER NOTE: also see ghost_list_projects\n"
	if err := os.WriteFile(target, []byte(edited), 0644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	writeRedirects(&out, projects, false)

	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != edited {
		t.Fatalf("edited redirect was changed:\n got: %q\nwant: %q", string(data), edited)
	}
	if !strings.Contains(out.String(), "not overwriting") {
		t.Fatalf("expected 'not overwriting', got: %s", out.String())
	}
	if strings.Contains(out.String(), "created redirect") {
		t.Fatalf("unexpected rewrite: %s", out.String())
	}
}

func TestWriteRedirects_KeepsEditedCurrentRedirect(t *testing.T) {
	home := t.TempDir()
	setHome(t, home)
	projectPath := absProjectPath(t)
	projects := []projectInfo{{ID: "abc123", Path: projectPath, Name: "myproject"}}

	target := filepath.Join(home, ".claude", "projects", projectSlug(projectPath), "memory", "MEMORY.md")
	if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
		t.Fatal(err)
	}
	edited := currentRedirectTemplate + "USER NOTE: edited current redirect\n"
	if err := os.WriteFile(target, []byte(edited), 0644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	writeRedirects(&out, projects, false)

	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != edited {
		t.Fatalf("edited redirect was changed")
	}
	if !strings.Contains(out.String(), "not overwriting") {
		t.Fatalf("expected 'not overwriting', got: %s", out.String())
	}
}

func TestWriteRedirects_IdempotentOnSecondRun(t *testing.T) {
	home := t.TempDir()
	setHome(t, home)
	projectPath := absProjectPath(t)
	projects := []projectInfo{{ID: "abc123", Path: projectPath, Name: "myproject"}}

	var out bytes.Buffer
	writeRedirects(&out, projects, false)
	target := filepath.Join(home, ".claude", "projects", projectSlug(projectPath), "memory", "MEMORY.md")
	first, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}

	out.Reset()
	writeRedirects(&out, projects, false)
	second, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Fatal("second run changed the redirect")
	}
	if !strings.Contains(out.String(), "redirect exists") {
		t.Fatalf("expected 'redirect exists', got: %s", out.String())
	}
	if strings.Contains(out.String(), "created redirect") {
		t.Fatalf("unexpected rewrite on second run: %s", out.String())
	}
}
