package mcpinit

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
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

// TestLoadSettings_BlankFileIsPopulated pins #1684: a 0-byte or
// whitespace-only settings.json carries nothing to merge, so it is populated as
// if it were absent, the way a blank codex hooks.json is written rather than
// refused (#1296). Refusing it would leave every re-run refusing the same file
// until the user deleted it, and the field hint names a field the file does not
// have.
func TestLoadSettings_BlankFileIsPopulated(t *testing.T) {
	cases := []struct {
		name string
		doc  string
	}{
		{"zero bytes", ""},
		{"whitespace only", " \n\t\r\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "settings.json")
			if err := os.WriteFile(path, []byte(tc.doc), 0o600); err != nil {
				t.Fatal(err)
			}

			sf, err := loadSettings(path)
			if err != nil {
				t.Fatalf("loadSettings refused a blank settings.json: %v", err)
			}
			if len(sf.raw) != 0 {
				t.Errorf("expected an empty document ready to populate, got %d keys", len(sf.raw))
			}
			added, err := sf.addPermissions([]string{"mcp__ghost__ghost_health"})
			if err != nil {
				t.Fatalf("addPermissions on a blank settings.json: %v", err)
			}
			if len(added) != 1 {
				t.Errorf("addPermissions added %d permissions, want 1", len(added))
			}
		})
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

// TestLoadSettings_NullDocumentReturnsError pins the first #1263 trigger: a
// settings.json whose whole document is the JSON literal null. Unmarshal
// decodes null into a nil map with no error, and addPermissions would then
// panic assigning into it. loadSettings refuses it, naming the file, instead
// of handing back a settingsFile whose raw map is nil.
func TestLoadSettings_NullDocumentReturnsError(t *testing.T) {
	path := tempSettings(t, `null`)
	_, err := loadSettings(path)
	if err == nil {
		t.Fatal("loadSettings must refuse a null settings.json, not decode it into a nil map")
	}
	if !strings.Contains(err.Error(), "settings.json") {
		t.Errorf("loadSettings error %q should name the file settings.json", err)
	}
}

// TestAddPermissions_NullPermissionsReturnsError pins the second #1263 trigger:
// "permissions": null is present-but-structurally-invalid, not absent, and both
// the allow-slice read and the permsMap rebuild decode it into nil with no
// error. addPermissions must refuse it — naming the field — rather than panic on
// permsMap["allow"] = ...
func TestAddPermissions_NullPermissionsReturnsError(t *testing.T) {
	path := tempSettings(t, `{"permissions":null}`)
	sf, err := loadSettings(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sf.addPermissions([]string{"mcp__ghost__ghost_health"}); err == nil {
		t.Fatal("addPermissions must refuse permissions:null, not panic on a nil map")
	} else if !strings.Contains(err.Error(), "permissions is null") {
		t.Errorf("addPermissions error %q should name the permissions field as null", err)
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

// TestReplaceEntryCommand_PreservesUserKeysAndText verifies that migrating a
// hook command keeps the entry's other keys (matcher, timeout, custom keys)
// in their original order and preserves their text (including <, >, &).
func TestReplaceEntryCommand_PreservesUserKeysAndText(t *testing.T) {
	// Seed an entry with custom key order and special characters in timeout/zz/aa
	entryRaw := json.RawMessage(`{"matcher":"Edit|Write","zz":1,"aa":2,"hooks":[{"type":"command","command":"OLD <>","timeout":5}]}`)

	rewritten, changed, err := replaceEntryCommand(entryRaw, "OLD <>", "NEW &&")
	if err != nil {
		t.Fatalf("replaceEntryCommand: %v", err)
	}
	if !changed {
		t.Fatal("expected changed=true")
	}

	var out map[string]json.RawMessage
	if err := json.Unmarshal(rewritten, &out); err != nil {
		t.Fatalf("unmarshal rewritten: %v", err)
	}

	// Check key order in the entry: matcher, zz, aa, hooks (by byte position)
	entryStr := string(rewritten)
	findKey := func(key string, start int) int {
		target := "\"" + key + "\""
		for i := start; i < len(entryStr); i++ {
			if strings.HasPrefix(entryStr[i:], target) {
				// Check if it's a key (preceded by { or ,)
				j := i - 1
				for j >= 0 && (entryStr[j] == ' ' || entryStr[j] == '\n' || entryStr[j] == '\t' || entryStr[j] == '\r') {
					j--
				}
				if j >= 0 && (entryStr[j] == '{' || entryStr[j] == ',') {
					return i
				}
			}
		}
		return -1
	}
	matcherPos := findKey("matcher", 0)
	zzPos := findKey("zz", matcherPos+1)
	aaPos := findKey("aa", zzPos+1)
	hooksPos := findKey("hooks", aaPos+1)
	if matcherPos < 0 || zzPos < 0 || aaPos < 0 || hooksPos < 0 || matcherPos >= zzPos || zzPos >= aaPos || aaPos >= hooksPos {
		t.Errorf("key order changed: want matcher < zz < aa < hooks (got matcher=%d, zz=%d, aa=%d, hooks=%d)", matcherPos, zzPos, aaPos, hooksPos)
	}

	// Check hooks array content
	var hooks []json.RawMessage
	if err := json.Unmarshal(out["hooks"], &hooks); err != nil {
		t.Fatalf("unmarshal hooks: %v", err)
	}
	if len(hooks) != 1 {
		t.Fatalf("expected 1 hook action, got %d", len(hooks))
	}
	var action map[string]json.RawMessage
	if err := json.Unmarshal(hooks[0], &action); err != nil {
		t.Fatalf("unmarshal action: %v", err)
	}
	// Command should be updated, with && preserved (not escaped)
	var cmd string
	if err := json.Unmarshal(action["command"], &cmd); err != nil {
		t.Fatalf("unmarshal command: %v", err)
	}
	if cmd != "NEW &&" {
		t.Errorf("command: got %q, want %q", cmd, "NEW &&")
	}
	// Timeout should be preserved
	var timeout int
	if err := json.Unmarshal(action["timeout"], &timeout); err != nil {
		t.Fatalf("unmarshal timeout: %v", err)
	}
	if timeout != 5 {
		t.Errorf("timeout: got %d, want 5", timeout)
	}
	// Type should be preserved
	var typ string
	if err := json.Unmarshal(action["type"], &typ); err != nil {
		t.Fatalf("unmarshal type: %v", err)
	}
	if typ != "command" {
		t.Errorf("type: got %q, want command", typ)
	}
}
