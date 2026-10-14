package mcpinit

import (
	"bytes"
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
