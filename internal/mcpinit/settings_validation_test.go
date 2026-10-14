package mcpinit

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRunRefusesMalformedSettingsBeforeRegistering pins #1267: every field
// `ghost mcp init` parses in steps 5-8 is loaded and validated BEFORE step 4
// registers the MCP server, so a structurally invalid settings.json is refused
// with the file and the field named, nothing is registered, and the file is
// left exactly as the user wrote it. A wrong-typed value must not surface a raw
// json.Unmarshal error either.
func TestRunRefusesMalformedSettingsBeforeRegistering(t *testing.T) {
	cases := []struct {
		name  string
		doc   string
		field string
	}{
		{"null document", "null", "settings.json is null"},
		{"permissions null", `{"permissions":null}`, "settings.json: permissions is null"},
		{"permissions array", `{"permissions":[]}`, "settings.json: permissions must be an object"},
		{"permissions string", `{"permissions":"x"}`, "settings.json: permissions must be an object"},
		{"allow string", `{"permissions":{"allow":"x"}}`, "settings.json: permissions.allow must be a list of strings"},
		{"hooks null", `{"hooks":null}`, "settings.json: hooks is null"},
		{"hooks array", `{"hooks":[]}`, "settings.json: hooks must be an object"},
		{"event null", `{"hooks":{"SessionStart":null}}`, `settings.json: hooks["SessionStart"] is null`},
		{"event string", `{"hooks":{"SessionStart":"x"}}`, `settings.json: hooks["SessionStart"] must be a list`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			setHome(t, home)
			t.Setenv(pluginRootEnv, "")
			orig := systemBinDirs
			systemBinDirs = nil
			t.Cleanup(func() { systemBinDirs = orig })

			// A `claude` stub that records every invocation, so a case proves
			// the refusal landed before registration rather than assuming it.
			binDir := filepath.Join(home, "bin")
			if err := os.MkdirAll(binDir, 0o755); err != nil {
				t.Fatal(err)
			}
			writeFakeClaude(t, binDir, claudeFake{})
			writeStub(t, binDir, "ghost")
			t.Setenv("PATH", binDir)

			path := filepath.Join(home, ".claude", "settings.json")
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(tc.doc), 0o600); err != nil {
				t.Fatal(err)
			}

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

// TestValidateAcceptsWellFormedSettings is the other side: the preflight must
// not refuse a settings.json that carries the fields steps 5-8 parse — every
// event uses the host's array shape, including events init does not itself
// reconcile.
func TestValidateAcceptsWellFormedSettings(t *testing.T) {
	path := tempSettings(t, `{
		"permissions": {"allow": ["Bash"], "deny": []},
		"hooks": {
			"SessionStart": [{"matcher": "", "hooks": [{"type": "command", "command": "ghost hook session-start --source claude-code", "timeout": 30}]}],
			"Stop": [{"hooks": [{"type": "command", "command": "ghost hook stop --source claude-code"}]}],
			"UserPromptSubmit": [],
			"PostToolUse": [{"matcher": "Edit|Write|MultiEdit", "hooks": [{"type": "command", "command": "ghost hook edit --source claude-code"}]}],
			"PreToolUse": [{"matcher": "Bash", "hooks": [{"type": "command", "command": "echo hi"}]}]
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
	for _, doc := range []string{`{}`, `{"effortLevel":"high"}`} {
		path := tempSettings(t, doc)
		sf, err := loadSettings(path)
		if err != nil {
			t.Fatalf("loadSettings(%s): %v", doc, err)
		}
		if err := sf.validate(); err != nil {
			t.Errorf("settings without permissions/hooks were refused: %v (doc %s)", err, doc)
		}
	}
}
