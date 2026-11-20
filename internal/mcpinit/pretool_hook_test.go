package mcpinit

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestEnsurePreToolUseHookAddsHook(t *testing.T) {
	sf := &settingsFile{raw: map[string]json.RawMessage{}}
	var out bytes.Buffer
	if err := ensurePreToolUseHook(&out, sf, "/opt/ghost", false); err != nil {
		t.Fatal(err)
	}
	cmds, err := sf.hookCommands("PreToolUse")
	if err != nil || len(cmds) != 1 {
		t.Fatalf("expected one PreToolUse hook, cmds=%v err=%v", cmds, err)
	}
	if !strings.Contains(cmds[0], "hook pre-tool-use --source claude-code") {
		t.Fatalf("unexpected hook command: %s", cmds[0])
	}
	if !strings.Contains(out.String(), "added PreToolUse hook") {
		t.Fatalf("unexpected output: %s", out.String())
	}
	var out2 bytes.Buffer
	if err := ensurePreToolUseHook(&out2, sf, "/opt/ghost", false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out2.String(), "already configured") {
		t.Fatalf("expected idempotent output: %s", out2.String())
	}
}

func TestEnsurePreToolUseHookRefusesListCommand(t *testing.T) {
	raw := `{"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":["echo","hi"]}]}]}`
	sf := &settingsFile{raw: map[string]json.RawMessage{"hooks": json.RawMessage(raw)}}
	var out bytes.Buffer
	if err := ensurePreToolUseHook(&out, sf, "/opt/ghost", false); err == nil {
		t.Fatalf("a list-typed PreToolUse command must be refused, output: %s", out.String())
	}
}
