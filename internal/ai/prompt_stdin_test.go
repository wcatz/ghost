package ai

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// issue #560: every harness client passed the prompt as ONE argv element. The

const stdinProbeMarker = "GHOST-STDIN-PROBE-MARKER"

const oversizedPromptBytes = 4 << 20

func oversizedPrompt() string {
	line := "memory line about the sqlite write lock\n"
	head := "consolidate these memories: " + stdinProbeMarker + "\n"
	return head + strings.Repeat(line, (oversizedPromptBytes-len(head))/len(line))
}

func fakeHarnessBinary(t *testing.T, name, probeArg string, probeLines []string, reportFmt string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell script fake binary requires a POSIX shell")
	}

	t.Setenv("GHOST_SCRATCH_DIR", t.TempDir())
	dir := t.TempDir()
	path := filepath.Join(dir, name)
	var script strings.Builder
	script.WriteString("#!/bin/sh\n")
	if probeArg != "" {
		fmt.Fprintf(&script, "if [ \"$1\" = %q ]; then\n", probeArg)
		for _, line := range probeLines {
			fmt.Fprintf(&script, "  printf '%%s\\n' %q\n", line)
		}
		script.WriteString("  exit 0\nfi\n")
	}
	script.WriteString(`for arg in "$@"; do
  case "$arg" in
    *` + stdinProbeMarker + `*) echo "prompt arrived as an argv element" >&2; exit 42 ;;
  esac
done
bytes=$(wc -c | tr -d ' ')
printf '` + reportFmt + `' "$bytes"
`)
	if err := os.WriteFile(path, []byte(script.String()), 0o755); err != nil {
		t.Fatalf("write fake %s: %v", name, err)
	}
	return path
}

func TestHarnessPromptArrivesOnStdin(t *testing.T) {
	prompt := oversizedPrompt()
	want := fmt.Sprintf("LEN:%d", len(prompt))

	claudeCaps := []string{"--safe-mode", "--restricted", "--strict-mcp-config", "--disable-slash-commands", "--tools", "--disallowedTools", "--setting-sources"}

	t.Run("claude", func(t *testing.T) {
		bin := fakeHarnessBinary(t, "claude", "--help", claudeCaps, "LEN:%s")
		text, _, err := NewCLIClientWithBinary(bin).Reflect(context.Background(), prompt)
		if err != nil {
			t.Fatalf("Reflect: %v", err)
		}
		if text != want {
			t.Errorf("claude saw %q, want %q — the prompt must arrive whole, on stdin", text, want)
		}
	})

	t.Run("opencode", func(t *testing.T) {
		bin := fakeHarnessBinary(t, "opencode", "--version", []string{"2.0.15"}, `{"type":"text","part":{"type":"text","text":"LEN:%s"}}`)
		text, _, err := NewOpenCodeClientWithBinary(bin).Reflect(context.Background(), prompt)
		if err != nil {
			t.Fatalf("Reflect: %v", err)
		}

		wantOpenCode := fmt.Sprintf("LEN:%d", len(openCodeNoToolsPreamble)+len(prompt))
		if text != wantOpenCode {
			t.Errorf("opencode saw %q, want %q — the prompt must arrive whole, on stdin", text, wantOpenCode)
		}
	})

	t.Run("codex", func(t *testing.T) {
		bin := fakeHarnessBinary(t, "codex", "", nil, "LEN:%s")
		text, _, err := NewCodexClientWithBinary(bin).Reflect(context.Background(), prompt)
		if err != nil {
			t.Fatalf("Reflect: %v", err)
		}
		if text != want {
			t.Errorf("codex saw %q, want %q — the prompt must arrive whole, on stdin", text, want)
		}
	})

	t.Run("goose", func(t *testing.T) {
		bin := fakeHarnessBinary(t, "goose", "", nil, "LEN:%s")
		text, _, err := NewGooseClientWithBinary(bin).Reflect(context.Background(), prompt)
		if err != nil {
			t.Fatalf("Reflect: %v", err)
		}
		if text != want {
			t.Errorf("goose saw %q, want %q — the prompt must arrive whole, on stdin", text, want)
		}
	})
}

func TestHarnessClassifyKeepsTheSystemPromptOffStdin(t *testing.T) {
	bin := fakeHarnessBinary(t, "claude", "--help",
		[]string{"--safe-mode", "--restricted", "--strict-mcp-config", "--tools", "--disallowedTools"},
		"LEN:%s")
	userContent := oversizedPrompt()

	text, err := NewCLIClientWithBinary(bin).Classify(context.Background(), "SYSTEM instructions", userContent)
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	if text != fmt.Sprintf("LEN:%d", len(userContent)) {
		t.Errorf("classify stdin carried %q, want the user content alone (LEN:%d)", text, len(userContent))
	}
}
