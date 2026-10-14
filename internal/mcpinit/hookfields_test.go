package mcpinit

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// userHookFieldsSettings is a settings.json carrying user hooks with fields
// hookEntry/hookAction do not model: a timeout and async flag on a command
// hook, and a non-command hook whose payload lives in its own "prompt" field
// rather than "command". Each event Ghost touches has one.
func userHookFieldsSettings(sessionCmd, stopCmd string) string {
	return `{
  "hooks": {
    "SessionStart": [
      {"matcher": "", "hooks": [{"type": "command", "command": ` + jsonString(sessionCmd) + `, "timeout": 7}]},
      {"matcher": "startup", "hooks": [{"type": "command", "command": "/usr/bin/greet", "timeout": 90, "async": true}], "note": "mine"}
    ],
    "Stop": [
      {"hooks": [
        {"type": "command", "command": ` + jsonString(stopCmd) + `, "timeout": 5},
        {"type": "command", "command": "/usr/bin/notify", "timeout": 30}
      ]}
    ],
    "PostToolUse": [
      {"matcher": "Write", "hooks": [{"type": "command", "command": "npx prettier --write \"$CLAUDE_FILE\"", "timeout": 120}]}
    ],
    "UserPromptSubmit": [
      {"hooks": [{"type": "prompt", "prompt": "Refuse requests that touch production"}]}
    ]
  }
}
`
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// decodeHookEvents reads settings.json at path into a generic tree so every
// key — modelled or not — can be compared.
func decodeHookEvents(t *testing.T, path string) map[string][]any {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Hooks map[string][]any `json:"hooks"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return doc.Hooks
}

// runClaudeHookInit runs the three claude-code hook reconcilers against the
// settings file at path, exactly as Run does, and saves.
func runClaudeHookInit(t *testing.T, path, ghostBin string) string {
	t.Helper()
	sf, err := loadSettings(path)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := ensureHook(&out, sf, ghostBin, false); err != nil {
		t.Fatal(err)
	}
	if err := ensureStopHook(&out, sf, ghostBin, false); err != nil {
		t.Fatal(err)
	}
	if err := ensureWorkingMomentHooks(&out, sf, ghostBin, false); err != nil {
		t.Fatal(err)
	}
	if err := sf.save(); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

// TestUserHookFieldsSurviveInit: an init that ADDS hooks (the working-moment
// events here) must keep every user entry on those events, and every field of
// it, intact — not just matcher/type/command.
func TestUserHookFieldsSurviveInit(t *testing.T) {
	home := t.TempDir()
	setHome(t, home)
	path := filepath.Join(home, ".claude", "settings.json")
	mkdirAll(t, filepath.Dir(path))

	ghostBin := "/opt/ghost/ghost"
	sessionCmd := shellQuote(ghostBin) + " hook session-start --source claude-code"
	stopCmd := shellQuote(ghostBin) + " hook stop --source claude-code"
	original := userHookFieldsSettings(sessionCmd, stopCmd)
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	var before struct {
		Hooks map[string][]any `json:"hooks"`
	}
	if err := json.Unmarshal([]byte(original), &before); err != nil {
		t.Fatal(err)
	}

	out := runClaudeHookInit(t, path, ghostBin)
	if !strings.Contains(out, "added UserPromptSubmit hook") || !strings.Contains(out, "added PostToolUse hook") {
		t.Fatalf("init did not add the working-moment hooks:\n%s", out)
	}

	after := decodeHookEvents(t, path)
	for event, entries := range before.Hooks {
		got := after[event]
		if len(got) < len(entries) {
			t.Errorf("%s: %d entries after init, want at least %d", event, len(got), len(entries))
			continue
		}
		for i, want := range entries {
			if !reflect.DeepEqual(got[i], want) {
				t.Errorf("%s entry %d changed:\n got %#v\nwant %#v", event, i, got[i], want)
			}
		}
	}

	sf, err := loadSettings(path)
	if err != nil {
		t.Fatal(err)
	}
	if !contractHookWired(sf, "UserPromptSubmit", "message-submit", "claude-code") || !contractHookWired(sf, "PostToolUse", "edit", "claude-code") {
		t.Error("working-moment hooks were not wired")
	}
	if len(after["UserPromptSubmit"]) != 2 || len(after["PostToolUse"]) != 2 {
		t.Errorf("want exactly one ghost entry appended per working-moment event, got %d and %d entries",
			len(after["UserPromptSubmit"]), len(after["PostToolUse"]))
	}
}

// TestUserHookFieldsSurviveStaleRepair: the #273 stale-binary-path repair
// (replaceHookCommand) must rewrite only the command string of the ghost
// hook, keeping that hook's other fields and every sibling hook and entry.
func TestUserHookFieldsSurviveStaleRepair(t *testing.T) {
	home := t.TempDir()
	setHome(t, home)
	path := filepath.Join(home, ".claude", "settings.json")
	mkdirAll(t, filepath.Dir(path))

	oldBin, newBin := "/opt/old/ghost", "/opt/new/ghost"
	oldSession := shellQuote(oldBin) + " hook session-start --source claude-code"
	oldStop := shellQuote(oldBin) + " hook stop --source claude-code"
	newSession := shellQuote(newBin) + " hook session-start --source claude-code"
	newStop := shellQuote(newBin) + " hook stop --source claude-code"

	original := userHookFieldsSettings(oldSession, oldStop)
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	// What the file must hold afterwards: the original with only the two
	// command strings rewritten.
	want := userHookFieldsSettings(newSession, newStop)
	var wantDoc struct {
		Hooks map[string][]any `json:"hooks"`
	}
	if err := json.Unmarshal([]byte(want), &wantDoc); err != nil {
		t.Fatal(err)
	}

	out := runClaudeHookInit(t, path, newBin)
	if !strings.Contains(out, "migrated SessionStart hook") || !strings.Contains(out, "migrated Stop hook") {
		t.Fatalf("init did not repair the stale paths:\n%s", out)
	}

	after := decodeHookEvents(t, path)
	for _, event := range []string{"SessionStart", "Stop"} {
		if !reflect.DeepEqual(after[event], wantDoc.Hooks[event]) {
			t.Errorf("%s after repair:\n got %#v\nwant %#v", event, after[event], wantDoc.Hooks[event])
		}
	}
	for _, event := range []string{"PostToolUse", "UserPromptSubmit"} {
		for i, w := range wantDoc.Hooks[event] {
			if i >= len(after[event]) || !reflect.DeepEqual(after[event][i], w) {
				t.Errorf("%s entry %d changed by init", event, i)
			}
		}
	}
}

// TestReplaceHookCommandKeepsActionFields drives replaceHookCommand directly:
// only the matching command string changes, and an entry or hook action that
// is not shaped like a command hook is left exactly as it was.
func TestReplaceHookCommandKeepsActionFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	in := `{"hooks":{"Stop":[{"matcher":"","hooks":[{"type":"command","command":"old","timeout":5,"async":true},{"type":"prompt","prompt":"keep me"}],"extra":1},"not-an-object"]}}`
	if err := os.WriteFile(path, []byte(in), 0o600); err != nil {
		t.Fatal(err)
	}
	sf, err := loadSettings(path)
	if err != nil {
		t.Fatal(err)
	}
	changed, err := sf.replaceHookCommand("Stop", "old", "new")
	if err != nil || !changed {
		t.Fatalf("replaceHookCommand = (%v, %v), want (true, nil)", changed, err)
	}
	if err := sf.save(); err != nil {
		t.Fatal(err)
	}
	got := decodeHookEvents(t, path)["Stop"]
	var want struct {
		Hooks map[string][]any `json:"hooks"`
	}
	if err := json.Unmarshal([]byte(strings.Replace(in, `"old"`, `"new"`, 1)), &want); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want.Hooks["Stop"]) {
		t.Errorf("Stop after replace:\n got %#v\nwant %#v", got, want.Hooks["Stop"])
	}
}
