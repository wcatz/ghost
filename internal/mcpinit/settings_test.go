package mcpinit

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
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

// TestSettingsFile_SaveBacksUpOnlyOnce pins that the .bak is the user's
// pre-ghost file, not ghost's previous output. Rolling the backup forward on
// every save means a second init destroys the only pristine copy, so a bad
// merge can no longer be undone.
func TestSettingsFile_SaveBacksUpOnlyOnce(t *testing.T) {
	path := tempSettings(t, `{"effortLevel":"high"}`)
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	sf, err := loadSettings(path)
	if err != nil {
		t.Fatalf("loadSettings: %v", err)
	}
	if err := sf.save(); err != nil {
		t.Fatalf("first save: %v", err)
	}
	bak, err := os.ReadFile(path + ".bak")
	if err != nil {
		t.Fatalf("first save should back up the original: %v", err)
	}
	if string(bak) != string(original) {
		t.Errorf("first backup = %q, want the original %q", bak, original)
	}

	// The user edits the file, then a second init merges into it again.
	edited := `{"autoMemoryEnabled":false,"effortLevel":"low"}`
	if err := os.WriteFile(path, []byte(edited), 0600); err != nil {
		t.Fatal(err)
	}
	sf2, err := loadSettings(path)
	if err != nil {
		t.Fatalf("loadSettings (second): %v", err)
	}
	if err := sf2.save(); err != nil {
		t.Fatalf("second save: %v", err)
	}
	bak2, err := os.ReadFile(path + ".bak")
	if err != nil {
		t.Fatalf("read backup after second save: %v", err)
	}
	if string(bak2) != string(original) {
		t.Errorf("second save clobbered the original backup: got %q, want %q", bak2, original)
	}
	if string(bak2) == edited {
		t.Error("backup must not be ghost's own previous output")
	}
}

// TestWriteFileAtomic covers the contract the user-owned config writes share:
// a temp file in the same directory renamed over the target, no leftovers, and
// an existing file's permissions preserved (a 0600 config.toml must not
// become world-readable just because ghost rewrote it). The mode a *new* file
// receives depends on the process umask, so it is pinned separately in
// settings_unix_test.go.
func TestWriteFileAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")

	if err := writeFileAtomic(path, []byte("a = 1\n"), 0644); err != nil {
		t.Fatalf("writeFileAtomic (create): %v", err)
	}
	assertFileContent(t, path, "a = 1\n")

	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(path, []byte("b = 2\n"), 0644); err != nil {
		t.Fatalf("writeFileAtomic (replace): %v", err)
	}
	assertFileContent(t, path, "b = 2\n")
	assertFileMode(t, path, 0600)

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "config.toml" {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("atomic write left temp files behind: %v", names)
	}
}

// TestWriteFileAtomicKeepsSymlink covers a config.toml that is a symlink into a
// dotfiles repo: the rename must land on the link's target, not replace the
// link with a regular file (which is what a bare temp+rename does).
func TestWriteFileAtomicKeepsSymlink(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "dotfiles", "config.toml")
	link := filepath.Join(root, "config.toml")
	if err := os.MkdirAll(filepath.Dir(real), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(real, []byte("a = 1\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	// The rename lands in the target's directory, so that is the directory
	// whose entry must be flushed; flushing the link's directory would make
	// the new entry durable nowhere.
	var flushed []string
	recordSyncs(t, nil, func(dir string) error {
		flushed = append(flushed, dir)
		return nil
	})
	if err := writeFileAtomic(link, []byte("b = 2\n"), 0644); err != nil {
		t.Fatalf("writeFileAtomic through a symlink: %v", err)
	}
	wantDir, err := filepath.EvalSymlinks(filepath.Dir(real))
	if err != nil {
		t.Fatal(err)
	}
	if len(flushed) != 1 || flushed[0] != wantDir {
		t.Errorf("flushed directories = %q, want exactly the symlink target's directory %q", flushed, wantDir)
	}
	info, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Error("the symlink was replaced by a regular file; the user lost their dotfiles link")
	}
	assertFileContent(t, real, "b = 2\n")
	assertFileContent(t, link, "b = 2\n")
}

func assertFileContent(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Errorf("%s = %q, want %q", filepath.Base(path), got, want)
	}
}

// assertFileMode checks POSIX permission bits. Windows has no mode bits (every
// file reports 0666 whatever was asked for), so there is nothing to assert
// there; the mode contract is exercised on the Unix runners instead.
// filePerm returns a file's current permission bits, or 0 on Windows where
// there are none to read.
func filePerm(t *testing.T, path string) os.FileMode {
	t.Helper()
	if runtime.GOOS == "windows" {
		return 0
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode().Perm()
}

func assertFileMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	if runtime.GOOS == "windows" {
		return
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Errorf("%s mode = %v, want %v", filepath.Base(path), got, want)
	}
}

// TestWriteFileAtomicPrivateVersusPreserving pins the two helper flavours: one
// that keeps whatever mode the target already had, and one that additionally
// clamps the result so it can never be wider than the mode it was asked for.
func TestWriteFileAtomicPrivateVersusPreserving(t *testing.T) {
	dir := t.TempDir()

	// Seed, then read back the mode the umask actually granted, so the
	// assertion does not depend on the machine's umask.
	preserved := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(preserved, []byte("a = 1\n"), 0644); err != nil {
		t.Fatal(err)
	}
	before := filePerm(t, preserved)
	if err := writeFileAtomic(preserved, []byte("b = 2\n"), 0644); err != nil {
		t.Fatalf("writeFileAtomic: %v", err)
	}
	assertFileMode(t, preserved, before)

	clamped := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(clamped, []byte(`{}`), 0644); err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomicPrivate(clamped, []byte(`{"a":1}`), 0600); err != nil {
		t.Fatalf("writeFileAtomicPrivate: %v", err)
	}
	assertFileContent(t, clamped, `{"a":1}`)
	assertFileMode(t, clamped, filePerm(t, clamped)&0600)
}

// TestCreateTempWithModeNames pins the temp file's properties: a fresh name on
// every call, so a leftover or pre-planted file in the user's home cannot
// predict or collide with it, and the requested mode with the umask applied.
func TestCreateTempWithModeNames(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX mode bits; the umask contract is pinned in settings_unix_test.go")
	}
	dir := t.TempDir()

	first, err := createTempWithMode(dir, "config.toml", 0600)
	if err != nil {
		t.Fatalf("createTempWithMode (first): %v", err)
	}
	second, err := createTempWithMode(dir, "config.toml", 0600)
	if err != nil {
		t.Fatalf("createTempWithMode (second): %v", err)
	}
	firstName, secondName := first.Name(), second.Name()
	if firstName == secondName {
		t.Errorf("two temp files share the name %q", firstName)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
	// The name is derived from the target but carries a random suffix, and it is
	// hidden so it does not show up in a directory listing the user reads.
	base := filepath.Base(firstName)
	if !strings.HasPrefix(base, ".config.toml-") || !strings.HasSuffix(base, ".tmp") {
		t.Errorf("temp name %q should be a hidden .config.toml-<random>.tmp", base)
	}
	suffix := strings.TrimSuffix(strings.TrimPrefix(base, ".config.toml-"), ".tmp")
	if len(suffix) < 8 {
		t.Errorf("temp name %q should carry a random suffix of at least 8 hex characters, got %q", base, suffix)
	}
	if strings.Trim(suffix, "0123456789abcdef") != "" {
		t.Errorf("temp name %q should end in hex random bytes, got %q", base, suffix)
	}
	assertFileMode(t, firstName, 0600)
}

// errFlush is a stand-in for a filesystem that cannot flush: the durability
// step fails on demand, so what the write published is observable.
var errFlush = errors.New("flush refused by the test")

// recordSyncs replaces both durability steps with recorders that still call the
// real fsync, and restores them when the test ends. A write holds no lock of its
// own and these tests are not parallel, so a package var is the seam; nothing
// else in the package assigns them.
func recordSyncs(t *testing.T, onTemp func(*os.File) error, onDir func(string) error) *[]string {
	t.Helper()
	realTemp, realDir := syncTempFile, syncParentDir
	events := &[]string{}
	t.Cleanup(func() { syncTempFile, syncParentDir = realTemp, realDir })
	syncTempFile = func(f *os.File) error {
		*events = append(*events, "flush temp")
		if onTemp != nil {
			return onTemp(f)
		}
		return realTemp(f)
	}
	syncParentDir = func(dir string) error {
		*events = append(*events, "flush dir")
		if onDir != nil {
			return onDir(dir)
		}
		return realDir(dir)
	}
	return events
}

// readFileOr returns the content of path, or "<unreadable: err>" — a recorder
// runs inside the write, so it cannot Fatalf out of a nested call.
func readFileOr(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return "<unreadable: " + err.Error() + ">"
	}
	return string(data)
}

// TestWriteFileAtomicFlushesAroundTheRename pins the durability contract of
// every user-owned config write. Neither flush leaves any trace in the finished
// file — the content, the mode and the inode are the same with or without them
// — so the only thing that can be tested is WHEN each ran: the temp file's
// bytes have to reach the disk while the file is still a temp file, and the
// directory entry the rename created has to reach the disk after it. A write
// that syncs after the rename, or never syncs the directory, can still be
// interrupted into a config that is empty, truncated, or simply gone.
func TestWriteFileAtomicFlushesAroundTheRename(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte("a = 1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var tempPath, tempContent string
	events := recordSyncs(t, func(f *os.File) error {
		tempPath, tempContent = f.Name(), readFileOr(f.Name())
		if got := readFileOr(path); got != "a = 1\n" {
			t.Errorf("the temp file was flushed at %s, after the rename had already replaced the config with %q", f.Name(), got)
		}
		return nil
	}, func(d string) error {
		if got := readFileOr(path); got != "b = 2\n" {
			t.Errorf("the parent directory was flushed while %s still held %q; the rename is the edit that directory flush has to carry", path, got)
		}
		return nil
	})

	if err := writeFileAtomic(path, []byte("b = 2\n"), 0644); err != nil {
		t.Fatalf("writeFileAtomic: %v", err)
	}

	want := []string{"flush temp", "flush dir"}
	if !reflect.DeepEqual(*events, want) {
		t.Errorf("durability steps ran as %v, want %v", *events, want)
	}
	if tempPath == "" {
		t.Fatal("the temp file was never flushed, so a power cut before the rename could leave the config empty or truncated")
	}
	if tempContent != "b = 2\n" {
		t.Errorf("the flushed temp file %s held %q, want the new document: the flush happened before the bytes were written", tempPath, tempContent)
	}
	assertFileContent(t, path, "b = 2\n")
	assertFileMode(t, path, 0600)
}

// TestWriteFileAtomicFailedFlushPublishesNothing is the other half: a flush that
// fails is not a write that quietly happened anyway. Renaming a temp file whose
// bytes were never on the disk is the one way this path can still leave a
// truncated config, so the failure has to stop the write, report itself, and
// leave the user's config and their directory exactly as they were.
func TestWriteFileAtomicFailedFlushPublishesNothing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte("a = 1\n"), 0644); err != nil {
		t.Fatal(err)
	}
	recordSyncs(t, func(*os.File) error { return errFlush }, nil)

	err := writeFileAtomic(path, []byte("b = 2\n"), 0644)
	if !errors.Is(err, errFlush) {
		t.Fatalf("writeFileAtomic error = %v, want it to report the failed flush (errFlush)", err)
	}
	assertFileContent(t, path, "a = 1\n")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "config.toml" {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("a write that published nothing left files behind: %v", names)
	}
}

// TestWriteFileAtomicCreatesNothingDurableWhenTheFlushFails covers the create
// case, which takes a different branch through writeFileMode: there is no
// target whose mode to keep, so the flush failure is the only thing standing
// between an unflushed temp file and a rename that publishes it.
func TestWriteFileAtomicCreatesNothingDurableWhenTheFlushFails(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fresh.toml")
	recordSyncs(t, func(*os.File) error { return errFlush }, nil)

	if err := writeFileAtomic(path, []byte("a = 1\n"), 0644); !errors.Is(err, errFlush) {
		t.Fatalf("writeFileAtomic (create) error = %v, want it to report the failed flush (errFlush)", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("a create whose flush failed left %v behind, want nothing", names)
	}
}

// TestWriteFileAtomicPrivateAlsoFlushes pins that the 0600-clamping flavour gets
// the same durability: it is a different mode policy on the same write path, and
// a settings.json that is not flushed is the one config a crash leaves empty.
func TestWriteFileAtomicPrivateAlsoFlushes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	events := recordSyncs(t, nil, nil)

	if err := writeFileAtomicPrivate(path, []byte(`{"a":1}`), 0600); err != nil {
		t.Fatalf("writeFileAtomicPrivate: %v", err)
	}
	want := []string{"flush temp", "flush dir"}
	if !reflect.DeepEqual(*events, want) {
		t.Errorf("durability steps ran as %v, want %v", *events, want)
	}
	assertFileContent(t, path, `{"a":1}`)
}

// TestWriteFileAtomicReplacesADanglingSymlink pins the one symlink case that
// cannot be written through, so the doc comment above writeFileAtomic cannot
// drift into claiming every link survives. EvalSymlinks cannot resolve a link
// whose target is not there, so the write lands on the link's own name: the
// link is replaced by a regular file holding the new document, and nothing
// appears at the path it pointed at. A dotfiles repo whose checkout has not been
// cloned yet is exactly this.
func TestWriteFileAtomicReplacesADanglingSymlink(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "dotfiles", "config.toml")
	link := filepath.Join(root, "config.toml")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err := os.Lstat(link); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("the fixture's target exists, so the link is not dangling: %v", err)
	}

	if err := writeFileAtomic(link, []byte("a = 1\n"), 0644); err != nil {
		t.Fatalf("writeFileAtomic over a dangling symlink: %v", err)
	}

	info, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		t.Error("the dangling link was kept; the documented behaviour is that it is replaced by a regular file")
	}
	assertFileContent(t, link, "a = 1\n")
	if _, err := os.Lstat(target); !os.IsNotExist(err) {
		t.Errorf("a write replaced the dangling link with a file, so nothing should have been created at the path it pointed at: %v", err)
	}
}

// TestSettingsFileSaveWritesThroughASymlinkedConfig is the caller-level half of
// the same contract, through the write a user's `ghost mcp init` actually makes:
// a settings.json that is a symlink into a dotfiles checkout has to keep being a
// symlink, the merge has to land in the file it points at, and the file it
// points at has to come back clamped to 0600. Without the link surviving, the
// user gets a regular file in ~/.claude and an untracked copy in their dotfiles
// repo, and every later edit through the repo silently diverges.
func TestSettingsFileSaveWritesThroughASymlinkedConfig(t *testing.T) {
	home := t.TempDir()
	setHome(t, home)
	target := filepath.Join(home, "dotfiles", "claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte(`{"effortLevel":"high"}`), 0644); err != nil {
		t.Fatal(err)
	}
	link, err := settingsPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	sf, err := loadSettings(link)
	if err != nil {
		t.Fatalf("loadSettings: %v", err)
	}
	if _, err := sf.addPermissions(ghostPermissions); err != nil {
		t.Fatalf("addPermissions: %v", err)
	}
	if err := sf.save(); err != nil {
		t.Fatalf("save through the symlink: %v", err)
	}

	info, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("%s was replaced by a regular file (%v); the user's dotfiles link is gone and the merge went somewhere untracked", link, info.Mode())
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read the dotfiles target: %v", err)
	}
	if !strings.Contains(string(data), `"effortLevel": "high"`) {
		t.Errorf("the merge did not land in the file the link points at:\n%s", data)
	}
	if !strings.Contains(string(data), "mcp__ghost__ghost_memory_save") {
		t.Errorf("the ghost permissions are missing from the file the link points at:\n%s", data)
	}
	assertFileMode(t, target, 0600)

	// A second init is the shape that catches a link replaced by a regular file:
	// it would read and rewrite the regular file and the dotfiles copy would go
	// on being stale.
	if err := sf.save(); err != nil {
		t.Fatalf("second save through the symlink: %v", err)
	}
	info, err = os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Errorf("the second save replaced the symlink with a regular file: init is not idempotent for a linked config")
	}
}
