package mcpinit

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestHandleSessionStartHook(t *testing.T) {
	var out bytes.Buffer
	HandleSessionStartHook(strings.NewReader(`{"event":"SessionStart"}`), &out)

	output := out.String()
	if output == "" {
		t.Error("hook output should not be empty")
	}
	if !strings.Contains(output, "ghost_memory_save") && !strings.Contains(output, "Ghost context") {
		t.Error("hook output should mention ghost_memory_save or Ghost context")
	}
}

func TestWriteRedirects_CreatesFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	projects := []projectInfo{
		{ID: "abc123", Path: "/home/test/git/myproject", Name: "myproject"},
	}

	var out bytes.Buffer
	writeRedirects(&out, projects, false)

	encoded := strings.ReplaceAll("/home/test/git/myproject", "/", "-")
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
	t.Setenv("HOME", home)

	// Pre-create the redirect file.
	encoded := strings.ReplaceAll("/home/test/git/myproject", "/", "-")
	dir := filepath.Join(home, ".claude", "projects", encoded, "memory")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "MEMORY.md"), []byte("All project knowledge is stored in Ghost."), 0644); err != nil {
		t.Fatal(err)
	}

	projects := []projectInfo{
		{ID: "abc123", Path: "/home/test/git/myproject", Name: "myproject"},
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
	t.Setenv("HOME", home)

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
	t.Setenv("HOME", home)

	// Pre-create a file with user content (not a Ghost redirect).
	encoded := strings.ReplaceAll("/home/test/git/myproject", "/", "-")
	dir := filepath.Join(home, ".claude", "projects", encoded, "memory")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	original := "User's custom memory content here."
	if err := os.WriteFile(filepath.Join(dir, "MEMORY.md"), []byte(original), 0644); err != nil {
		t.Fatal(err)
	}

	projects := []projectInfo{
		{ID: "abc123", Path: "/home/test/git/myproject", Name: "myproject"},
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
	t.Setenv("HOME", home)

	projects := []projectInfo{
		{ID: "abc123", Path: "/home/test/git/myproject", Name: "myproject"},
	}

	var out bytes.Buffer
	writeRedirects(&out, projects, true)

	output := out.String()
	if !strings.Contains(output, "would create redirect") {
		t.Errorf("dry run should say 'would create redirect', got: %s", output)
	}

	// Verify no file was created.
	encoded := strings.ReplaceAll("/home/test/git/myproject", "/", "-")
	target := filepath.Join(home, ".claude", "projects", encoded, "memory", "MEMORY.md")
	if _, err := os.Stat(target); err == nil {
		t.Error("dry run should not create files")
	}
}

func TestEnsureAutoMemoryDisabled_SetsFlag(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

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
	t.Setenv("HOME", home)

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
	t.Setenv("HOME", home)

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
	// Verify the canonical list has the expected count.
	if len(ghostPermissions) != 19 {
		t.Errorf("expected 19 ghost permissions, got %d", len(ghostPermissions))
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
	if err := ensureStopHook(&out, sf, "/usr/local/bin/ghost"); err != nil {
		t.Fatalf("ensureStopHook: %v", err)
	}
	if !sf.hasHook("Stop", "hook stop") {
		t.Error("Stop hook should be present after ensureStopHook")
	}

	// Idempotent: second call reports already-configured, adds nothing.
	out.Reset()
	if err := ensureStopHook(&out, sf, "/usr/local/bin/ghost"); err != nil {
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

func TestReconcileHook_LeavesCustomCommandUntouched(t *testing.T) {
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
	if action != hookUnchanged {
		t.Errorf("action = %v, want hookUnchanged — a hand-edited command must never be clobbered", action)
	}
	got, _, _ := sf.findHookCommand("SessionStart", "hook session-start")
	if got != custom {
		t.Errorf("command changed to %q, want untouched %q", got, custom)
	}
}

// TestReconcileHook_MigratesExactLegacyCommandOnly covers the CodeRabbit
// finding on PR #255: a hand-edited wrapper that starts with ' and mentions
// "hook session-start" must never be mistaken for the legacy command, and the
// exact legacy command must be found and migrated regardless of which entry
// comes first in the hook list.
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
