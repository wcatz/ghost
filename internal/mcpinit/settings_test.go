package mcpinit

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func tempSettings(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	if content != "" {
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

func TestLoadSettings_Empty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	sf, err := loadSettings(path)
	if err != nil {
		t.Fatalf("loadSettings: %v", err)
	}
	if len(sf.raw) != 0 {
		t.Errorf("expected empty raw, got %d keys", len(sf.raw))
	}
}

func TestLoadSettings_Existing(t *testing.T) {
	path := tempSettings(t, `{"permissions":{"allow":["Bash"]},"effortLevel":"high"}`)
	sf, err := loadSettings(path)
	if err != nil {
		t.Fatalf("loadSettings: %v", err)
	}
	if _, ok := sf.raw["permissions"]; !ok {
		t.Error("expected permissions key")
	}
	if _, ok := sf.raw["effortLevel"]; !ok {
		t.Error("expected effortLevel key")
	}
}

func TestAddPermissions_AllNew(t *testing.T) {
	path := tempSettings(t, `{"permissions":{"allow":["Bash"]}}`)
	sf, err := loadSettings(path)
	if err != nil {
		t.Fatal(err)
	}

	added, err := sf.addPermissions([]string{"mcp__ghost__ghost_health", "mcp__ghost__ghost_list_projects"})
	if err != nil {
		t.Fatal(err)
	}
	if len(added) != 2 {
		t.Errorf("expected 2 added, got %d", len(added))
	}

	// Verify the allow list now has 3 entries.
	perms, err := sf.getPermissions()
	if err != nil {
		t.Fatal(err)
	}
	if len(perms) != 3 {
		t.Errorf("expected 3 permissions, got %d", len(perms))
	}
}

func TestAddPermissions_Idempotent(t *testing.T) {
	path := tempSettings(t, `{"permissions":{"allow":["mcp__ghost__ghost_health"]}}`)
	sf, err := loadSettings(path)
	if err != nil {
		t.Fatal(err)
	}

	added, err := sf.addPermissions([]string{"mcp__ghost__ghost_health"})
	if err != nil {
		t.Fatal(err)
	}
	if len(added) != 0 {
		t.Errorf("expected 0 added (idempotent), got %d", len(added))
	}
}

func TestAddPermissions_NoExistingPerms(t *testing.T) {
	path := tempSettings(t, `{}`)
	sf, err := loadSettings(path)
	if err != nil {
		t.Fatal(err)
	}

	added, err := sf.addPermissions([]string{"mcp__ghost__ghost_health"})
	if err != nil {
		t.Fatal(err)
	}
	if len(added) != 1 {
		t.Errorf("expected 1 added, got %d", len(added))
	}
}

func TestHasHook_NotPresent(t *testing.T) {
	path := tempSettings(t, `{}`)
	sf, err := loadSettings(path)
	if err != nil {
		t.Fatal(err)
	}
	if sf.hasHook("SessionStart", "ghost hook session-start") {
		t.Error("expected hasHook to return false")
	}
}

func TestAddHook_AndHasHook(t *testing.T) {
	path := tempSettings(t, `{}`)
	sf, err := loadSettings(path)
	if err != nil {
		t.Fatal(err)
	}

	entry := hookEntry{
		Matcher: "",
		Hooks:   []hookAction{{Type: "command", Command: "ghost hook session-start"}},
	}
	if err := sf.addHook("SessionStart", entry); err != nil {
		t.Fatal(err)
	}

	if !sf.hasHook("SessionStart", "ghost hook session-start") {
		t.Error("expected hasHook to return true after addHook")
	}
}

func TestAddHook_PreservesExisting(t *testing.T) {
	existing := `{"hooks":{"PreToolUse":[{"matcher":"Edit","hooks":[{"type":"command","command":"check.sh"}]}]}}`
	path := tempSettings(t, existing)
	sf, err := loadSettings(path)
	if err != nil {
		t.Fatal(err)
	}

	entry := hookEntry{
		Matcher: "",
		Hooks:   []hookAction{{Type: "command", Command: "ghost hook session-start"}},
	}
	if err := sf.addHook("SessionStart", entry); err != nil {
		t.Fatal(err)
	}

	// PreToolUse should still be there.
	var hooks map[string]json.RawMessage
	if err := json.Unmarshal(sf.raw["hooks"], &hooks); err != nil {
		t.Fatal(err)
	}
	if _, ok := hooks["PreToolUse"]; !ok {
		t.Error("existing PreToolUse hook was clobbered")
	}
	if _, ok := hooks["SessionStart"]; !ok {
		t.Error("SessionStart hook was not added")
	}
}

func TestFindHookCommand_NotPresent(t *testing.T) {
	path := tempSettings(t, `{}`)
	sf, err := loadSettings(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := sf.findHookCommand("SessionStart", "hook session-start"); ok || err != nil {
		t.Errorf("expected findHookCommand to return (_, false, nil), got (_, %v, %v)", ok, err)
	}
}

func TestFindHookCommand_Present(t *testing.T) {
	path := tempSettings(t, `{}`)
	sf, err := loadSettings(path)
	if err != nil {
		t.Fatal(err)
	}
	entry := hookEntry{Hooks: []hookAction{{Type: "command", Command: "ghost hook session-start"}}}
	if err := sf.addHook("SessionStart", entry); err != nil {
		t.Fatal(err)
	}
	got, ok, err := sf.findHookCommand("SessionStart", "hook session-start")
	if err != nil {
		t.Fatal(err)
	}
	if !ok || got != "ghost hook session-start" {
		t.Errorf("findHookCommand = (%q, %v), want (%q, true)", got, ok, "ghost hook session-start")
	}
}

func TestFindHookCommand_MalformedHooksReturnsError(t *testing.T) {
	path := tempSettings(t, `{"hooks":[]}`)
	sf, err := loadSettings(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := sf.findHookCommand("SessionStart", "hook session-start"); err == nil {
		t.Error("expected findHookCommand to return an error for a malformed hooks value")
	}
}

func TestFindHookCommand_NullHooksReturnsError(t *testing.T) {
	path := tempSettings(t, `{"hooks":null}`)
	sf, err := loadSettings(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := sf.findHookCommand("SessionStart", "hook session-start"); err == nil {
		t.Error("expected findHookCommand to return an error for hooks:null")
	}
}

func TestFindHookCommand_NullEventReturnsError(t *testing.T) {
	path := tempSettings(t, `{"hooks":{"SessionStart":null}}`)
	sf, err := loadSettings(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := sf.findHookCommand("SessionStart", "hook session-start"); err == nil {
		t.Error("expected findHookCommand to return an error for hooks.SessionStart:null")
	}
}

// TestAddHook_NeverCalledWithNullHooks documents the invariant that prevents
// the CodeRabbit-flagged panic: addHook assigns into the "hooks" map
// unconditionally, so it must never be reached when "hooks" is null. In
// production the only caller is reconcileHook, which is gated behind
// findHookCommand's error return — this test proves addHook itself no
// longer panics even if that gate is bypassed, since json.Unmarshal of a
// null root leaves the local map nil and the assignment below would panic
// without the nil-guard added alongside these findHookCommand changes.
func TestAddHook_NullHooksDoesNotPanic(t *testing.T) {
	path := tempSettings(t, `{"hooks":null}`)
	sf, err := loadSettings(path)
	if err != nil {
		t.Fatal(err)
	}
	entry := hookEntry{Hooks: []hookAction{{Type: "command", Command: "ghost hook session-start"}}}
	if err := sf.addHook("SessionStart", entry); err != nil {
		t.Fatal(err)
	}
}

func TestHasExactHookCommand(t *testing.T) {
	path := tempSettings(t, `{}`)
	sf, err := loadSettings(path)
	if err != nil {
		t.Fatal(err)
	}
	entry := hookEntry{Hooks: []hookAction{{Type: "command", Command: "'/usr/local/bin/ghost' hook session-start"}}}
	if err := sf.addHook("SessionStart", entry); err != nil {
		t.Fatal(err)
	}

	exact, err := sf.hasExactHookCommand("SessionStart", "'/usr/local/bin/ghost' hook session-start")
	if err != nil {
		t.Fatal(err)
	}
	if !exact {
		t.Error("expected exact match to be found")
	}

	partial, err := sf.hasExactHookCommand("SessionStart", "hook session-start")
	if err != nil {
		t.Fatal(err)
	}
	if partial {
		t.Error("expected substring-only match to NOT be treated as exact")
	}
}

func TestReplaceHookCommand_NoMatchingEvent(t *testing.T) {
	path := tempSettings(t, `{}`)
	sf, err := loadSettings(path)
	if err != nil {
		t.Fatal(err)
	}
	replaced, err := sf.replaceHookCommand("SessionStart", "hook session-start", "new command")
	if err != nil {
		t.Fatal(err)
	}
	if replaced {
		t.Error("expected replaceHookCommand to return false when hooks are absent")
	}
}

func TestReplaceHookCommand_PreservesOtherEventsAndFields(t *testing.T) {
	legacy := "'/usr/local/bin/ghost' hook session-start"
	existing := `{"hooks":{
		"PreToolUse":[{"matcher":"Edit","hooks":[{"type":"command","command":"check.sh","timeout":30}]}],
		"SessionStart":[{"matcher":"","hooks":[{"type":"command","command":"` + legacy + `"}]}]
	}}`
	path := tempSettings(t, existing)
	sf, err := loadSettings(path)
	if err != nil {
		t.Fatal(err)
	}

	desired := `"C:\ghost\ghost.exe" hook session-start`
	replaced, err := sf.replaceHookCommand("SessionStart", legacy, desired)
	if err != nil {
		t.Fatal(err)
	}
	if !replaced {
		t.Fatal("expected replaceHookCommand to report a match")
	}

	got, ok, err := sf.findHookCommand("SessionStart", "hook session-start")
	if err != nil {
		t.Fatal(err)
	}
	if !ok || got != desired {
		t.Errorf("SessionStart command = (%q, %v), want the replaced command", got, ok)
	}

	var hooks map[string]json.RawMessage
	if err := json.Unmarshal(sf.raw["hooks"], &hooks); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(hooks["PreToolUse"]), `"timeout":30`) {
		t.Errorf("PreToolUse entry lost its timeout field: %s", hooks["PreToolUse"])
	}
}

func TestReplaceHookCommand_DoesNotTouchNonExactMatch(t *testing.T) {
	wrapper := "'/opt/wrap.sh' --run 'hook session-start' --extra"
	path := tempSettings(t, `{}`)
	sf, err := loadSettings(path)
	if err != nil {
		t.Fatal(err)
	}
	entry := hookEntry{Hooks: []hookAction{{Type: "command", Command: wrapper}}}
	if err := sf.addHook("SessionStart", entry); err != nil {
		t.Fatal(err)
	}

	replaced, err := sf.replaceHookCommand("SessionStart", "'/usr/local/bin/ghost' hook session-start", "new command")
	if err != nil {
		t.Fatal(err)
	}
	if replaced {
		t.Error("expected replaceHookCommand to report no match for a command that only contains the substring")
	}

	got, ok, err := sf.findHookCommand("SessionStart", "hook session-start")
	if err != nil {
		t.Fatal(err)
	}
	if !ok || got != wrapper {
		t.Errorf("wrapper command was altered: got (%q, %v), want untouched %q", got, ok, wrapper)
	}
}

func TestSetAutoMemoryEnabled_FromAbsent(t *testing.T) {
	path := tempSettings(t, `{}`)
	sf, err := loadSettings(path)
	if err != nil {
		t.Fatal(err)
	}

	changed, err := sf.setAutoMemoryEnabled(false)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Error("expected changed=true when key was absent")
	}

	v, present := sf.getAutoMemoryEnabled()
	if !present {
		t.Error("expected key to be present after set")
	}
	if v {
		t.Error("expected value to be false")
	}
}

func TestSetAutoMemoryEnabled_Idempotent(t *testing.T) {
	path := tempSettings(t, `{"autoMemoryEnabled":false}`)
	sf, err := loadSettings(path)
	if err != nil {
		t.Fatal(err)
	}

	changed, err := sf.setAutoMemoryEnabled(false)
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Error("expected changed=false when value is already false (idempotent)")
	}
}

func TestSetAutoMemoryEnabled_OverridesTrue(t *testing.T) {
	path := tempSettings(t, `{"autoMemoryEnabled":true}`)
	sf, err := loadSettings(path)
	if err != nil {
		t.Fatal(err)
	}

	changed, err := sf.setAutoMemoryEnabled(false)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Error("expected changed=true when overriding true→false")
	}

	v, present := sf.getAutoMemoryEnabled()
	if !present || v {
		t.Errorf("expected autoMemoryEnabled=false, got present=%v value=%v", present, v)
	}
}

func TestSetAutoMemoryEnabled_PreservesOtherKeys(t *testing.T) {
	path := tempSettings(t, `{"permissions":{"allow":["Bash"]},"effortLevel":"high"}`)
	sf, err := loadSettings(path)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := sf.setAutoMemoryEnabled(false); err != nil {
		t.Fatal(err)
	}

	// Other keys must still be present.
	if _, ok := sf.raw["permissions"]; !ok {
		t.Error("permissions key was lost")
	}
	if _, ok := sf.raw["effortLevel"]; !ok {
		t.Error("effortLevel key was lost")
	}

	v, present := sf.getAutoMemoryEnabled()
	if !present || v {
		t.Errorf("expected autoMemoryEnabled=false, got present=%v value=%v", present, v)
	}
}

func TestSetAutoMemoryEnabled_RoundTrip(t *testing.T) {
	path := tempSettings(t, `{"effortLevel":"high"}`)
	sf, err := loadSettings(path)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := sf.setAutoMemoryEnabled(false); err != nil {
		t.Fatal(err)
	}
	if err := sf.save(); err != nil {
		t.Fatal(err)
	}

	// Re-read and verify.
	sf2, err := loadSettings(path)
	if err != nil {
		t.Fatal(err)
	}

	if _, ok := sf2.raw["effortLevel"]; !ok {
		t.Error("effortLevel was lost during save")
	}

	v, present := sf2.getAutoMemoryEnabled()
	if !present || v {
		t.Errorf("expected autoMemoryEnabled=false after round-trip, got present=%v value=%v", present, v)
	}
}

func TestGetAutoMemoryEnabled_Absent(t *testing.T) {
	path := tempSettings(t, `{}`)
	sf, err := loadSettings(path)
	if err != nil {
		t.Fatal(err)
	}

	v, present := sf.getAutoMemoryEnabled()
	if present {
		t.Errorf("expected present=false for absent key, got present=true value=%v", v)
	}
}

func TestSave_RoundTrip(t *testing.T) {
	path := tempSettings(t, `{"permissions":{"allow":["Bash"]},"effortLevel":"high"}`)
	sf, err := loadSettings(path)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := sf.addPermissions([]string{"mcp__ghost__ghost_health"}); err != nil {
		t.Fatal(err)
	}
	if err := sf.save(); err != nil {
		t.Fatal(err)
	}

	// Re-read and verify.
	sf2, err := loadSettings(path)
	if err != nil {
		t.Fatal(err)
	}

	// effortLevel should be preserved.
	if _, ok := sf2.raw["effortLevel"]; !ok {
		t.Error("effortLevel was lost during save")
	}

	perms, err := sf2.getPermissions()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, p := range perms {
		if p == "mcp__ghost__ghost_health" {
			found = true
		}
	}
	if !found {
		t.Error("mcp__ghost__ghost_health not found after round-trip")
	}

	// Backup should exist.
	if _, err := os.Stat(path + ".bak"); err != nil {
		t.Error("backup file not created")
	}
}
