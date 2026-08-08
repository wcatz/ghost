package ai

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestStripAPIKey(t *testing.T) {
	in := []string{"FOO=bar", "ANTHROPIC_API_KEY=sk-secret", "anthropic_api_key=sk-secret2", "PATH=/usr/bin"}
	out := stripAPIKey(in)
	for _, kv := range out {
		if strings.HasPrefix(strings.ToUpper(kv), "ANTHROPIC_API_KEY=") {
			t.Fatalf("expected ANTHROPIC_API_KEY stripped (any case), got %v", out)
		}
	}
	if len(out) != 2 {
		t.Fatalf("expected 2 remaining vars, got %v", out)
	}
}

// fakeClaudeBinary writes a shell script standing in for `claude` that echoes
// stdin/env visibility back so run()'s plumbing (args, env stripping, stdout
// capture) can be verified without a real subprocess call.
func fakeClaudeBinary(t *testing.T, script string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell script fake binary requires a POSIX shell")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "claude")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatalf("write fake binary: %v", err)
	}
	return path
}

func TestCLIClient_Reflect_StripsAPIKeyAndReturnsStdout(t *testing.T) {
	bin := fakeClaudeBinary(t, `
if [ -n "$ANTHROPIC_API_KEY" ]; then
  echo "LEAKED" >&2
  exit 1
fi
printf '%s' '{"memories":[]}'
`)
	t.Setenv("ANTHROPIC_API_KEY", "sk-should-not-leak")

	c := &CLIClient{binary: bin}
	text, usage, err := c.Reflect(context.Background(), "prompt")
	if err != nil {
		t.Fatalf("Reflect: %v", err)
	}
	if text != `{"memories":[]}` {
		t.Errorf("unexpected stdout: %q", text)
	}
	if usage != (TokenUsage{}) {
		t.Errorf("expected zero TokenUsage, got %+v", usage)
	}
}

func TestCLIClient_Classify_PassesSystemPromptAndUserContentAsDistinctArgs(t *testing.T) {
	bin := fakeClaudeBinary(t, `
found_flag=0
found_system=0
found_user=0
for arg in "$@"; do
  if [ "$arg" = "--system-prompt" ]; then found_flag=1; fi
  if [ "$arg" = "SYSTEM instructions" ]; then found_system=1; fi
  if [ "$arg" = "USERDATA content" ]; then found_user=1; fi
done
if [ "$found_flag" -ne 1 ]; then echo "missing --system-prompt flag" >&2; exit 1; fi
if [ "$found_system" -ne 1 ]; then echo "system prompt not passed as distinct arg" >&2; exit 1; fi
if [ "$found_user" -ne 1 ]; then echo "user content not passed as distinct arg" >&2; exit 1; fi
printf '%s' "KEEP"
`)
	c := &CLIClient{binary: bin}
	text, err := c.Classify(context.Background(), "SYSTEM instructions", "USERDATA content")
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	if text != "KEEP" {
		t.Errorf("expected %q, got %q", "KEEP", text)
	}
}

func TestCLIClient_Run_PropagatesStderrOnFailure(t *testing.T) {
	bin := fakeClaudeBinary(t, `echo "boom" >&2; exit 1`)
	c := &CLIClient{binary: bin}
	_, _, err := c.Reflect(context.Background(), "prompt")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("expected error to include stderr, got %v", err)
	}
}
