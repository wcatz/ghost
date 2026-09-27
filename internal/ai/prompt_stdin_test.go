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
// kernel caps a single argument at 32 pages (128 KiB on a 4 KiB-page x86,
// 512 KiB on a 16 KiB-page one) and caps argv+env together, so a prompt past
// that ceiling failed the spawn with E2BIG — and reflect builds prompts out of
// up to 2000 memories of 8000 bytes, so a large project produced a prompt no
// harness could be given. The prompt belongs on stdin, which every one of the
// four harnesses reads when the positional message is absent (codex and goose
// also accept an explicit `-` sentinel).

// stdinProbeMarker is a token the prompt carries and the fake harness looks
// for in argv. It is what makes "the prompt did not travel as an argument"
// observable rather than assumed.
const stdinProbeMarker = "GHOST-STDIN-PROBE-MARKER"

// oversizedPrompt is far past any platform's argv ceiling (Linux caps one
// argument at 32 pages; macOS caps argv+env at 1 MB) and comfortably inside
// what a reflect prompt can legitimately reach, so the test states the size
// the bug actually broke at.
const oversizedPromptBytes = 4 << 20

func oversizedPrompt() string {
	line := "memory line about the sqlite write lock\n"
	head := "consolidate these memories: " + stdinProbeMarker + "\n"
	return head + strings.Repeat(line, (oversizedPromptBytes-len(head))/len(line))
}

// fakeHarnessBinary writes a shell script standing in for a harness. It answers
// the client's own capability/version probe from probeLines, then reports how
// many bytes arrived on stdin using reportFmt (a printf format taking the byte
// count) — and fails loudly if the prompt turned up in argv instead.
func fakeHarnessBinary(t *testing.T, name, probeArg string, probeLines []string, reportFmt string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell script fake binary requires a POSIX shell")
	}
	// Every run() opens a scratch dir; pin the root to the test's temp dir so
	// the real data dir is never touched.
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

// TestHarnessPromptArrivesOnStdin covers all four clients: a prompt far too
// large for argv reaches the harness intact, and reaches it on stdin. opencode
// needs its own report format because run() parses a JSON event stream instead
// of returning raw stdout.
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
		// The no-tools preamble is prepended to the prompt, so it is part of
		// what stdin must carry.
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

// TestHarnessClassifyKeepsTheSystemPromptOffStdin is the split the classify
// path needs: the system prompt stays a flag argument (small, fixed, and never
// confusable with user content) while the untrusted user content moves to
// stdin. The oversized payload proves the user content is not in argv, where
// the kernel would refuse it.
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
