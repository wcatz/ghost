package mcpinit

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// installEnv wires a sandbox for a whole `ghost mcp init` run: a temp home, an
// XDG data/config root inside it, a `claude` stub that records every invocation
// at binDir/calls, and a ghost stub on PATH. It returns the home and the bin
// directory holding the stubs.
func installEnv(t *testing.T) (home, binDir string) {
	t.Helper()
	home = t.TempDir()
	setHome(t, home)
	t.Setenv(pluginRootEnv, "")
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	orig := systemBinDirs
	systemBinDirs = nil
	t.Cleanup(func() { systemBinDirs = orig })

	binDir = filepath.Join(home, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFakeClaude(t, binDir, claudeFake{})
	writeStub(t, binDir, "ghost")
	t.Setenv("PATH", binDir)
	return home, binDir
}

// writeSettings writes doc to the sandbox's Claude settings.json and returns
// the path.
func writeSettings(t *testing.T, home, doc string) string {
	t.Helper()
	path := filepath.Join(home, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestRunRefusesMalformedSettingsBeforeRegistering pins #1267: every part of
// settings.json `ghost mcp init` parses is loaded and validated BEFORE step 4
// registers the MCP server, so a structurally invalid file is refused with the
// file and the field named, nothing is registered, and the file is left
// exactly as the user wrote it. A wrong-typed value must not surface a raw
// json.Unmarshal error either.
func TestRunRefusesMalformedSettingsBeforeRegistering(t *testing.T) {
	cases := []struct {
		name  string
		doc   string
		field string
	}{
		{"null document", "null", "settings.json is null"},
		{"array document", "[]", "settings.json must be a JSON object"},
		{"string document", `"x"`, "settings.json must be a JSON object"},
		{"number document", "5", "settings.json must be a JSON object"},
		{"invalid document", "{", "settings.json must be a JSON object"},
		{"permissions null", `{"permissions":null}`, "settings.json: permissions is null"},
		{"permissions array", `{"permissions":[]}`, "settings.json: permissions must be an object"},
		{"permissions string", `{"permissions":"x"}`, "settings.json: permissions must be an object"},
		{"allow string", `{"permissions":{"allow":"x"}}`, "settings.json: permissions.allow must be a list of strings"},
		{"hooks null", `{"hooks":null}`, "settings.json: hooks is null"},
		{"hooks array", `{"hooks":[]}`, "settings.json: hooks must be an object"},
		{"event null", `{"hooks":{"SessionStart":null}}`, `settings.json: hooks["SessionStart"] is null`},
		{"event string", `{"hooks":{"SessionStart":"x"}}`, `settings.json: hooks["SessionStart"] must be a list`},
		{"stop event string", `{"hooks":{"Stop":"x"}}`, `settings.json: hooks["Stop"] must be a list`},
		{"post-tool event string", `{"hooks":{"PostToolUse":[{"hooks":[{"type":"command","command":["x"]}]}]}}`, `settings.json: hooks["PostToolUse"] must be a list`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home, binDir := installEnv(t)
			path := writeSettings(t, home, tc.doc)

			var out bytes.Buffer
			err := Run(&out, false)
			if err == nil {
				t.Fatalf("Run accepted a malformed settings.json; output:\n%s", out.String())
			}
			if !strings.Contains(err.Error(), tc.field) {
				t.Errorf("refusal must name the file and the field %q, got: %v", tc.field, err)
			}
			// The refusal must carry the edit that clears it, not Run's
			// generic re-run hint: a re-run reproduces the same invalid file.
			if !strings.Contains(err.Error(), "Fix the named field") {
				t.Errorf("refusal must name the edit that clears it, got: %v", err)
			}
			if strings.Contains(err.Error(), "to retry") {
				t.Errorf("refusal must not carry the generic re-run hint, got: %v", err)
			}

			// The refusal happens BEFORE registration: the recording stub
			// writes a `calls` log on every invocation, so it must not exist.
			requireMissing(t, binDir, "calls")

			got, readErr := os.ReadFile(path)
			if readErr != nil {
				t.Fatalf("re-read settings.json: %v", readErr)
			}
			if string(got) != tc.doc {
				t.Errorf("the malformed file was modified: %q", got)
			}
		})
	}
}

// TestRunInstallsSettingsItCarriesThrough pins the shapes init must NOT
// refuse: it reads only the SessionStart, Stop and working-moment events, so a
// hook event it never reads is carried through as raw JSON and an unusual
// value there must not stop an install.
func TestRunInstallsSettingsItCarriesThrough(t *testing.T) {
	cases := []struct {
		name     string
		doc      string
		carryKey string
	}{
		{"empty", `{}`, ""},
		{"non-read string event", `{"hooks":{"Notification":"x"}}`, "Notification"},
		{"non-read list-command event", `{"hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":["echo","hi"]}]}]}}`, "PreToolUse"},
		{"invalid read-event entry beside a non-read oddity",
			`{"permissions":{"allow":["Bash"]},"hooks":{"UserPromptSubmit":[{"matcher":"","hooks":[{"type":"command","command":"ghost hook message-submit --source claude-code","timeout":30}]}],"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":["echo","hi"]}]}]},"effortLevel":"high"}`,
			"PreToolUse"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home, _ := installEnv(t)
			path := writeSettings(t, home, tc.doc)

			var out bytes.Buffer
			if err := Run(&out, false); err != nil {
				t.Fatalf("Run refused a settings.json it must carry through: %v\noutput:\n%s", err, out.String())
			}
			if tc.carryKey != "" {
				got, err := os.ReadFile(path)
				if err != nil {
					t.Fatalf("re-read settings.json: %v", err)
				}
				if !strings.Contains(string(got), tc.carryKey) {
					t.Errorf("init dropped the event it does not read: %s", got)
				}
			}
		})
	}
}

// TestRunInstallsWithNullAllow pins that an explicit null allow keeps the old
// behavior: it is empty, filled in with ghost's permissions, not refused. Only
// a shape init used to carry or fill in is left alone; a null permissions (a
// nil-map panic) and a malformed read event (a later refusal) are not.
func TestRunInstallsWithNullAllow(t *testing.T) {
	home, _ := installEnv(t)
	path := writeSettings(t, home, `{"permissions":{"allow":null}}`)

	var out bytes.Buffer
	if err := Run(&out, false); err != nil {
		t.Fatalf("Run refused a null allow, which it used to fill in: %v\noutput:\n%s", err, out.String())
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("re-read settings.json: %v", err)
	}
	if !strings.Contains(string(got), "mcp__ghost__ghost_health") {
		t.Errorf("init did not fill in a null allow: %s", got)
	}
}

// TestRunInstallsWithMissingSettings is the absent-file case: an absent
// settings.json is valid, and init populates it rather than refusing.
func TestRunInstallsWithMissingSettings(t *testing.T) {
	home, _ := installEnv(t)
	path := filepath.Join(home, ".claude", "settings.json")

	var out bytes.Buffer
	if err := Run(&out, false); err != nil {
		t.Fatalf("Run refused an absent settings.json: %v\noutput:\n%s", err, out.String())
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("init must create settings.json: %v", err)
	}
}

// TestRunInstallsWithBlankSettings is the blank-file case (#1684): a 0-byte or
// whitespace-only settings.json carries nothing to merge, so init populates it
// as if it were absent rather than refusing it with a hint that names no field.
// The blank file is not captured as the one-time .bak either: an empty backup
// would stand forever and block the user's real file, the reason a 0-byte
// config.toml is skipped rather than copied.
func TestRunInstallsWithBlankSettings(t *testing.T) {
	cases := []struct {
		name string
		doc  string
	}{
		{"zero bytes", ""},
		{"whitespace only", " \n\t\r\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home, _ := installEnv(t)
			path := writeSettings(t, home, tc.doc)

			var out bytes.Buffer
			if err := Run(&out, false); err != nil {
				t.Fatalf("Run refused a blank settings.json: %v\noutput:\n%s", err, out.String())
			}
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("re-read settings.json: %v", err)
			}
			var merged map[string]json.RawMessage
			if err := json.Unmarshal(got, &merged); err != nil {
				t.Fatalf("settings.json is not valid JSON after init: %v", err)
			}
			if !strings.Contains(string(got), "mcp__ghost__ghost_health") {
				t.Errorf("init did not add ghost permissions: %s", got)
			}
			if _, err := os.Stat(path + ".bak"); !os.IsNotExist(err) {
				t.Errorf("a blank settings.json was captured as the .bak (Stat err = %v)", err)
			}
		})
	}
}

// TestRunDoesNotBackUpItsOwnSettings pins that the one-time settings.json.bak
// stays the user's pre-ghost file and never becomes ghost's own output. On a
// fresh install the first run creates settings.json from nothing, so it has
// nothing to back up; the SECOND run is the one whose save sees a file on disk
// — and that file is ghost's own output, which the .bak slot must not take.
// Because the .bak is never rolled forward, capturing ghost's bytes there
// would leave a later user edit with nowhere to be kept, so the test also
// shows a genuine edit is still captured once the slot has been left free.
func TestRunDoesNotBackUpItsOwnSettings(t *testing.T) {
	home, _ := installEnv(t)
	path := filepath.Join(home, ".claude", "settings.json")

	var first bytes.Buffer
	if err := Run(&first, false); err != nil {
		t.Fatalf("first Run: %v\n%s", err, first.String())
	}
	if _, err := os.Stat(path + ".bak"); !os.IsNotExist(err) {
		t.Errorf("the first init backed something up (Stat err = %v), but settings.json did not exist before it", err)
	}

	var second bytes.Buffer
	if err := Run(&second, false); err != nil {
		t.Fatalf("second Run: %v\n%s", err, second.String())
	}
	if _, err := os.Stat(path + ".bak"); !os.IsNotExist(err) {
		bak, _ := os.ReadFile(path + ".bak")
		t.Errorf("the second init backed up ITS OWN output:\n%q", bak)
	}

	// The slot was left free for the user: an edit of their own is captured.
	doc, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read settings.json: %v", err)
	}
	var merged map[string]json.RawMessage
	if err := json.Unmarshal(doc, &merged); err != nil {
		t.Fatalf("parse settings.json: %v", err)
	}
	merged["effortLevel"] = json.RawMessage(`"high"`)
	edited, err := json.MarshalIndent(merged, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	edited = append(edited, '\n')
	if err := os.WriteFile(path, edited, 0o600); err != nil {
		t.Fatal(err)
	}

	var third bytes.Buffer
	if err := Run(&third, false); err != nil {
		t.Fatalf("third Run: %v\n%s", err, third.String())
	}
	bak, err := os.ReadFile(path + ".bak")
	if err != nil {
		t.Fatalf("a user edit was not backed up: %v", err)
	}
	if !bytes.Equal(bak, edited) {
		t.Errorf(".bak = %q, want the user's file %q", bak, edited)
	}
}

// TestRunBacksUpSettingsThatAreNotPurelyGhosts pins the other side of the skip:
// a settings.json that differs from ghost's own output by anything at all keeps
// its one-time .bak. Each case starts from the file a clean run writes, changes
// one thing, and runs init again; the .bak must hold exactly that edited file.
func TestRunBacksUpSettingsThatAreNotPurelyGhosts(t *testing.T) {
	cases := []struct {
		name string
		edit func(t *testing.T, doc map[string]any)
	}{
		{"user top-level key", func(t *testing.T, doc map[string]any) {
			doc["effortLevel"] = "high"
		}},
		{"foreign hook entry", func(t *testing.T, doc map[string]any) {
			hooks := doc["hooks"].(map[string]any)
			hooks["SessionStart"] = append(hooks["SessionStart"].([]any), map[string]any{
				"matcher": "",
				"hooks":   []any{map[string]any{"type": "command", "command": "other-tool start"}},
			})
		}},
		{"extra permission", func(t *testing.T, doc map[string]any) {
			perms := doc["permissions"].(map[string]any)
			perms["allow"] = append(perms["allow"].([]any), "Bash(ls:*)")
		}},
		{"deny key", func(t *testing.T, doc map[string]any) {
			doc["permissions"].(map[string]any)["deny"] = []any{"Bash(rm:*)"}
		}},
		{"autoMemoryEnabled true", func(t *testing.T, doc map[string]any) {
			doc["autoMemoryEnabled"] = true
		}},
		{"ghost hook entry with a timeout", func(t *testing.T, doc map[string]any) {
			entry := doc["hooks"].(map[string]any)["Stop"].([]any)[0].(map[string]any)
			entry["hooks"].([]any)[0].(map[string]any)["timeout"] = 30
		}},
		{"ghost hook command with a tool tail", func(t *testing.T, doc map[string]any) {
			entry := doc["hooks"].(map[string]any)["Stop"].([]any)[0].(map[string]any)
			action := entry["hooks"].([]any)[0].(map[string]any)
			action["command"] = action["command"].(string) + " && other-tool"
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home, _ := installEnv(t)
			path := filepath.Join(home, ".claude", "settings.json")

			var first bytes.Buffer
			if err := Run(&first, false); err != nil {
				t.Fatalf("first Run: %v\n%s", err, first.String())
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var doc map[string]any
			if err := json.Unmarshal(data, &doc); err != nil {
				t.Fatal(err)
			}
			tc.edit(t, doc)
			edited, err := json.MarshalIndent(doc, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			edited = append(edited, '\n')
			if err := os.WriteFile(path, edited, 0o600); err != nil {
				t.Fatal(err)
			}

			var second bytes.Buffer
			if err := Run(&second, false); err != nil {
				t.Fatalf("second Run: %v\n%s", err, second.String())
			}
			bak, err := os.ReadFile(path + ".bak")
			if err != nil {
				t.Fatalf("no .bak was written for a file that is not purely ghost's: %v", err)
			}
			if !bytes.Equal(bak, edited) {
				t.Errorf(".bak = %q, want the edited file %q", bak, edited)
			}
		})
	}
}

// TestRunInstallsWithValidSettings is the whole-command happy path: a
// well-formed file is installed into, keeping the user's entries and adding
// ghost's.
func TestRunInstallsWithValidSettings(t *testing.T) {
	home, _ := installEnv(t)
	doc := `{"permissions":{"allow":["Bash"]},"hooks":{"SessionStart":[{"matcher":"","hooks":[{"type":"command","command":"ghost hook session-start --source claude-code"}]}]},"autoMemoryEnabled":false}`
	path := writeSettings(t, home, doc)

	var out bytes.Buffer
	if err := Run(&out, false); err != nil {
		t.Fatalf("Run refused a well-formed settings.json: %v\noutput:\n%s", err, out.String())
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("re-read settings.json: %v", err)
	}
	if !strings.Contains(string(got), "mcp__ghost__ghost_health") {
		t.Errorf("init did not add ghost permissions: %s", got)
	}
	if !strings.Contains(string(got), "Bash") {
		t.Errorf("init dropped the user's existing permission: %s", got)
	}
}

// TestValidateAcceptsWellFormedSettings is the direct validator: it must not
// refuse a settings.json carrying the fields steps 5-8 parse, including a
// non-read event whose shape it would carry through.
func TestValidateAcceptsWellFormedSettings(t *testing.T) {
	path := tempSettings(t, `{
		"permissions": {"allow": ["Bash"], "deny": []},
		"hooks": {
			"SessionStart": [{"matcher": "", "hooks": [{"type": "command", "command": "ghost hook session-start --source claude-code", "timeout": 30}]}],
			"Stop": [{"hooks": [{"type": "command", "command": "ghost hook stop --source claude-code"}]}],
			"UserPromptSubmit": [],
			"PostToolUse": [{"matcher": "Edit|Write|MultiEdit", "hooks": [{"type": "command", "command": "ghost hook edit --source claude-code"}]}],
			"Notification": "x",
			"PreToolUse": [{"matcher": "Bash", "hooks": [{"type": "command", "command": ["echo", "hi"]}]}]
		},
		"autoMemoryEnabled": false,
		"effortLevel": "high"
	}`)
	sf, err := loadSettings(path)
	if err != nil {
		t.Fatalf("loadSettings: %v", err)
	}
	if err := sf.validate(); err != nil {
		t.Errorf("a well-formed settings.json was refused: %v", err)
	}
}

// TestValidateAcceptsAbsentFields pins that an absent permissions or hooks is
// absent and not invalid: init fills a missing file in, and it must keep doing
// so.
func TestValidateAcceptsAbsentFields(t *testing.T) {
	for _, doc := range []string{`{}`, `{"effortLevel":"high"}`, `{"hooks":{"Notification":"x"}}`} {
		path := tempSettings(t, doc)
		sf, err := loadSettings(path)
		if err != nil {
			t.Fatalf("loadSettings(%s): %v", doc, err)
		}
		if err := sf.validate(); err != nil {
			t.Errorf("settings without a read event were refused: %v (doc %s)", err, doc)
		}
	}
}

// TestRunMergesBOMSettings pins #1379: Claude Code reads a settings.json that
// starts with a UTF-8 byte-order mark, so init merges into it instead of
// refusing, keeps the mark at the start of the file and the user's keys, and
// still takes the one-time .bak of the user's pre-ghost bytes.
func TestRunMergesBOMSettings(t *testing.T) {
	home, _ := installEnv(t)
	seed := "\ufeff{\"model\":\"opus\"}\n"
	path := writeSettings(t, home, seed)

	var out bytes.Buffer
	if err := Run(&out, false); err != nil {
		t.Fatalf("Run refused a BOM-prefixed settings.json: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(got), "\ufeff{") {
		t.Errorf("the byte-order mark must stay at the start of the file, got:\n%q", got)
	}
	if strings.Count(string(got), "\ufeff") != 1 {
		t.Errorf("exactly one byte-order mark expected, got:\n%q", got)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(bytes.TrimPrefix(got, []byte("\ufeff")), &doc); err != nil {
		t.Fatalf("merged file is not valid JSON after the mark: %v", err)
	}
	if string(doc["model"]) != `"opus"` {
		t.Errorf("the user's key must survive the merge, got %s", doc["model"])
	}
	if _, ok := doc["hooks"]; !ok {
		t.Error("ghost's hooks were not merged")
	}
	bak, err := os.ReadFile(path + ".bak")
	if err != nil {
		t.Fatalf("the pre-ghost file must be backed up: %v", err)
	}
	if string(bak) != seed {
		t.Errorf(".bak must hold the user's original bytes, got %q", bak)
	}
}

// TestRunBOMSettingsRoundTrip pins the other half of #1379: the mark is
// preserved only where the file had one. A BOM-free file stays BOM-free, a
// fresh install writes no mark, and a second run over a BOM file is a fixed
// point that still holds exactly one mark.
func TestRunBOMSettingsRoundTrip(t *testing.T) {
	const bom = "\ufeff"

	t.Run("existing file without a mark", func(t *testing.T) {
		home, _ := installEnv(t)
		path := writeSettings(t, home, "{\"model\":\"opus\"}\n")
		var out bytes.Buffer
		if err := Run(&out, false); err != nil {
			t.Fatalf("Run: %v", err)
		}
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(got), bom) {
			t.Errorf("a BOM-free file must stay BOM-free, got:\n%q", got)
		}
	})

	t.Run("fresh install", func(t *testing.T) {
		home, _ := installEnv(t)
		path := filepath.Join(home, ".claude", "settings.json")
		var out bytes.Buffer
		if err := Run(&out, false); err != nil {
			t.Fatalf("Run: %v", err)
		}
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(got), bom) {
			t.Errorf("a fresh install must write no mark, got:\n%q", got)
		}
	})

	t.Run("second run on a marked file", func(t *testing.T) {
		home, _ := installEnv(t)
		path := writeSettings(t, home, bom+"{\"model\":\"opus\"}\n")
		var out bytes.Buffer
		if err := Run(&out, false); err != nil {
			t.Fatalf("first Run: %v", err)
		}
		first, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		out.Reset()
		if err := Run(&out, false); err != nil {
			t.Fatalf("second Run: %v", err)
		}
		second, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(first) != string(second) {
			t.Errorf("second run must be a fixed point:\nfirst:\n%q\nsecond:\n%q", first, second)
		}
		if n := strings.Count(string(second), bom); n != 1 || !strings.HasPrefix(string(second), bom) {
			t.Errorf("want exactly one leading mark, got %d in:\n%q", n, second)
		}
	})
}

// TestRun_SettingsMergePreservesUserTextAndKeyOrder verifies that the
// settings.json merge preserves user text with &&, <, > and non-alphabetical
// key order, and that a second run is a byte-for-byte no-op.
func TestRun_SettingsMergePreservesUserTextAndKeyOrder(t *testing.T) {
	home, _ := installEnv(t)
	path := writeSettings(t, home,
		`{"Zeta":1,"hooks":{"PreToolUse":[{"hooks":[{"type":"command","command":"echo a && cat <x >y"}]}]},"permissions":{"allow":["Bash(a && b)"],"deny":["Bash(rm -rf /)"]},"Alpha":2}`)

	var out bytes.Buffer
	if err := Run(&out, false); err != nil {
		t.Fatalf("Run first: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	first := string(data)
	t.Logf("first run output:\n%s", first)

	// Verify user content preserved exactly
	if !strings.Contains(first, "echo a && cat <x >y") {
		t.Errorf("user hook command escaped/lost")
	}
	if !strings.Contains(first, "Bash(a && b)") {
		t.Errorf("user permission escaped/lost")
	}
	if !strings.Contains(first, "Bash(rm -rf /)") {
		t.Errorf("user deny escaped/lost")
	}
	// Verify top-level key order: Zeta, hooks, permissions, Alpha
	iZeta := strings.Index(first, "\"Zeta\"")
	iHooks := strings.Index(first, "\"hooks\"")
	iPerms := strings.Index(first, "\"permissions\"")
	iAlpha := strings.Index(first, "\"Alpha\"")
	if iZeta >= iHooks || iHooks >= iPerms || iPerms >= iAlpha {
		t.Errorf("top-level key order changed: want Zeta < hooks < permissions < Alpha")
	}
	// Verify permissions key order: allow, deny
	iAllow := strings.Index(first, "\"allow\"")
	iDeny := strings.Index(first, "\"deny\"")
	if iAllow >= iDeny {
		t.Errorf("permissions key order changed: want allow < deny")
	}
	// Verify hooks event order: PreToolUse (ghost's SessionStart added after)
	iPreToolUse := strings.Index(first, "\"PreToolUse\"")
	iSessionStart := strings.Index(first, "\"SessionStart\"")
	if iPreToolUse >= iSessionStart {
		t.Errorf("hooks event order changed: want PreToolUse < SessionStart")
	}

	// Second run - must be byte-for-byte identical
	var secondOut bytes.Buffer
	if err := Run(&secondOut, false); err != nil {
		t.Fatalf("Run second: %v", err)
	}
	data2, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	second := string(data2)
	if first != second {
		t.Errorf("second run changed bytes:\nfirst:\n%s\nsecond:\n%s", first, second)
	}
}

// TestRun_SettingsMergePreservesEscapedPermission verifies that a permission
// the user wrote with escapes (\u0026, \u0041) keeps those exact bytes.
func TestRun_SettingsMergePreservesEscapedPermission(t *testing.T) {
	home, _ := installEnv(t)
	// Seed with the escaped form of & in the permission value.
	path := writeSettings(t, home,
		`{"Zeta":1,"permissions":{"allow":["Bash(a \u0026 b)","Bash(\u0041)"]},"Alpha":2}`)

	var out bytes.Buffer
	if err := Run(&out, false); err != nil {
		t.Fatalf("Run first: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	first := string(data)
	t.Logf("first run output:\n%s", first)

	// The escape is kept as written, not decoded and not double-escaped.
	// \u0041 is an escape the JSON encoder never writes, so a decode and
	// re-encode of the entry would show up as a plain A.
	if !strings.Contains(first, `Bash(a \u0026 b)`) || !strings.Contains(first, `Bash(\u0041)`) {
		t.Errorf("escaped permission lost or double-escaped: %s", first)
	}

	// Second run - must be byte-for-byte identical
	var secondOut bytes.Buffer
	if err := Run(&secondOut, false); err != nil {
		t.Fatalf("Run second: %v", err)
	}
	data2, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	second := string(data2)
	if first != second {
		t.Errorf("second run changed bytes:\nfirst:\n%s\nsecond:\n%s", first, second)
	}
}

// TestRun_SettingsMergeNoAllowKeyOrder verifies that when permissions has no
// allow key (or allow is null), the other keys keep their order and allow
// is appended at the end.
func TestRun_SettingsMergeNoAllowKeyOrder(t *testing.T) {
	home, _ := installEnv(t)
	// Seed with permissions having zeta, deny, but no allow.
	path := writeSettings(t, home,
		`{"Zeta":1,"permissions":{"zeta":[1],"deny":["Bash(rm -rf /)"]},"Alpha":2}`)

	var out bytes.Buffer
	if err := Run(&out, false); err != nil {
		t.Fatalf("Run first: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	first := string(data)
	t.Logf("first run output:\n%s", first)

	// Verify top-level key order: Zeta, permissions, Alpha
	iZeta := strings.Index(first, "\"Zeta\"")
	iPerms := strings.Index(first, "\"permissions\"")
	iAlpha := strings.Index(first, "\"Alpha\"")
	if iZeta >= iPerms || iPerms >= iAlpha {
		t.Errorf("top-level key order changed: want Zeta < permissions < Alpha")
	}

	// Verify permissions key order: zeta, deny, allow (allow appended at end)
	iZetaP := strings.Index(first, "\"zeta\"")
	iDeny := strings.Index(first, "\"deny\"")
	iAllow := strings.Index(first, "\"allow\"")
	if iZetaP >= iDeny || iDeny >= iAllow {
		t.Errorf("permissions key order changed: want zeta < deny < allow")
	}

	// Second run - must be byte-for-byte identical
	var secondOut bytes.Buffer
	if err := Run(&secondOut, false); err != nil {
		t.Fatalf("Run second: %v", err)
	}
	data2, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	second := string(data2)
	if first != second {
		t.Errorf("second run changed bytes:\nfirst:\n%s\nsecond:\n%s", first, second)
	}
}

// TestRun_SettingsMergeNullAllowKeyOrder verifies that when permissions.allow
// is explicitly null, the other keys keep their order and allow is appended.
func TestRun_SettingsMergeNullAllowKeyOrder(t *testing.T) {
	home, _ := installEnv(t)
	// Seed with permissions having zeta, deny, and allow: null.
	path := writeSettings(t, home,
		`{"Zeta":1,"permissions":{"zeta":[1],"deny":["Bash(rm -rf /)"],"allow":null},"Alpha":2}`)

	var out bytes.Buffer
	if err := Run(&out, false); err != nil {
		t.Fatalf("Run first: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	first := string(data)
	t.Logf("first run output:\n%s", first)

	// Verify top-level key order: Zeta, permissions, Alpha
	iZeta := strings.Index(first, "\"Zeta\"")
	iPerms := strings.Index(first, "\"permissions\"")
	iAlpha := strings.Index(first, "\"Alpha\"")
	if iZeta >= iPerms || iPerms >= iAlpha {
		t.Errorf("top-level key order changed: want Zeta < permissions < Alpha")
	}

	// Verify permissions key order: zeta, deny, allow (allow appended at end)
	iZetaP := strings.Index(first, "\"zeta\"")
	iDeny := strings.Index(first, "\"deny\"")
	iAllow := strings.Index(first, "\"allow\"")
	if iZetaP >= iDeny || iDeny >= iAllow {
		t.Errorf("permissions key order changed: want zeta < deny < allow")
	}

	// Second run - must be byte-for-byte identical
	var secondOut bytes.Buffer
	if err := Run(&secondOut, false); err != nil {
		t.Fatalf("Run second: %v", err)
	}
	data2, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	second := string(data2)
	if first != second {
		t.Errorf("second run changed bytes:\nfirst:\n%s\nsecond:\n%s", first, second)
	}
}

// TestRun_SettingsMergePreservesSpecialCharsInKeys verifies that top-level
// and nested keys containing < or & are preserved without escaping.
func TestRun_SettingsMergePreservesSpecialCharsInKeys(t *testing.T) {
	home, _ := installEnv(t)
	// Seed with keys containing < and & at top level, inside hooks, and in permissions.
	path := writeSettings(t, home,
		`{"k<":1,"hooks":{"Pre<&":[{"hooks":[{"type":"command","command":"echo hi"}]}]},"permissions":{"allow&":["Bash(a && b)"]},"Alpha":2}`)

	var out bytes.Buffer
	if err := Run(&out, false); err != nil {
		t.Fatalf("Run first: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	first := string(data)
	t.Logf("first run output:\n%s", first)

	// Verify keys with < and & are preserved without escaping
	if !strings.Contains(first, "\"k<\"") {
		t.Errorf("top-level key with < escaped: %s", first)
	}
	if !strings.Contains(first, "\"Pre<&\"") {
		t.Errorf("key inside hooks with < or & escaped: %s", first)
	}
	if !strings.Contains(first, "\"allow&\"") {
		t.Errorf("permissions key with & escaped: %s", first)
	}

	// Second run - must be byte-for-byte identical
	var secondOut bytes.Buffer
	if err := Run(&secondOut, false); err != nil {
		t.Fatalf("Run second: %v", err)
	}
	data2, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	second := string(data2)
	if first != second {
		t.Errorf("second run changed bytes:\nfirst:\n%s\nsecond:\n%s", first, second)
	}
}
