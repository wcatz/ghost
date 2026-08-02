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
	in := []string{"FOO=bar", "ANTHROPIC_API_KEY=sk-secret", "PATH=/usr/bin"}
	out := stripAPIKey(in)
	for _, kv := range out {
		if strings.HasPrefix(kv, "ANTHROPIC_API_KEY=") {
			t.Fatalf("expected ANTHROPIC_API_KEY stripped, got %v", out)
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
echo -n '{"memories":[]}'
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

func TestCLIClient_Classify_JoinsPromptAndReturnsStdout(t *testing.T) {
	bin := fakeClaudeBinary(t, `
eval "last=\${$#}"
echo -n "$last" | grep -q "SYSTEM" || { echo "missing system prompt" >&2; exit 1; }
echo -n "$last" | grep -q "USERDATA" || { echo "missing user content" >&2; exit 1; }
echo -n "KEEP"
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
