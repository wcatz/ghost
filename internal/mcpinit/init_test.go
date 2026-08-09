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

func TestNeedsQuoteMigration(t *testing.T) {
	tests := []struct {
		name string
		cmd  string
		want bool
	}{
		{"legacy single-quoted", "'/usr/local/bin/ghost' hook session-start", true},
		{"cmd.exe double-quoted", `"C:\ghost\ghost.exe" hook session-start`, false},
		{"unquoted", "ghost hook session-start", false},
		{"empty", "", false},
	}
	for _, tt := range tests {
		if got := needsQuoteMigration(tt.cmd); got != tt.want {
			t.Errorf("needsQuoteMigration(%q) = %v, want %v", tt.cmd, got, tt.want)
		}
	}
}

func TestReconcileHook_AddsWhenAbsent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	sf, err := loadSettings(path)
	if err != nil {
		t.Fatalf("loadSettings: %v", err)
	}

	action, err := reconcileHook(sf, "SessionStart", "hook session-start", `"C:\ghost\ghost.exe" hook session-start`, true)
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
	legacy := `'C:\ghost\ghost.exe' hook session-start`
	if err := sf.addHook("SessionStart", hookEntry{Hooks: []hookAction{{Type: "command", Command: legacy}}}); err != nil {
		t.Fatalf("addHook: %v", err)
	}

	desired := `"C:\ghost\ghost.exe" hook session-start`
	action, err := reconcileHook(sf, "SessionStart", "hook session-start", desired, true)
	if err != nil {
		t.Fatalf("reconcileHook: %v", err)
	}
	if action != hookMigrated {
		t.Errorf("action = %v, want hookMigrated", action)
	}
	got, ok := sf.findHookCommand("SessionStart", "hook session-start")
	if !ok || got != desired {
		t.Errorf("findHookCommand = (%q, %v), want (%q, true)", got, ok, desired)
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

	action, err := reconcileHook(sf, "SessionStart", "hook session-start", legacy, false)
	if err != nil {
		t.Fatalf("reconcileHook: %v", err)
	}
	if action != hookUnchanged {
		t.Errorf("action = %v, want hookUnchanged — single-quoted is the correct POSIX form", action)
	}
	got, _ := sf.findHookCommand("SessionStart", "hook session-start")
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

	action, err := reconcileHook(sf, "SessionStart", "hook session-start", `"C:\ghost\ghost.exe" hook session-start`, true)
	if err != nil {
		t.Fatalf("reconcileHook: %v", err)
	}
	if action != hookUnchanged {
		t.Errorf("action = %v, want hookUnchanged — a hand-edited command must never be clobbered", action)
	}
	got, _ := sf.findHookCommand("SessionStart", "hook session-start")
	if got != custom {
		t.Errorf("command changed to %q, want untouched %q", got, custom)
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

	if _, err := reconcileHook(sf, "SessionStart", "hook session-start", `"C:\ghost\ghost.exe" hook session-start`, true); err != nil {
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
