package ai

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fakeOpenCodeBinary writes a shell script standing in for `opencode` that
// emits the given lines to stdout, so run()'s plumbing can be verified without
// a real opencode install.
func fakeOpenCodeBinary(t *testing.T, script string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell script fake binary requires a POSIX shell")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "opencode")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatalf("write fake binary: %v", err)
	}
	return path
}

func TestParseOpenCodeOutput_ConcatenatesTextEvents(t *testing.T) {
	raw := `{"type":"step_start","timestamp":1,"part":{"type":"step-start"}}
{"type":"text","timestamp":2,"part":{"id":"a","type":"text","text":"{\"memories\":"}}
{"type":"text","timestamp":3,"part":{"id":"b","type":"text","text":"[]}"}}
{"type":"step_finish","timestamp":4,"part":{"id":"c","type":"step-finish","reason":"stop"}}
`
	got, err := parseOpenCodeOutput(raw)
	if err != nil {
		t.Fatalf("parseOpenCodeOutput: %v", err)
	}
	if got != `{"memories":[]}` {
		t.Errorf("got %q, want concatenated text events", got)
	}
}

func TestParseOpenCodeOutput_IgnoresNonText(t *testing.T) {
	raw := `{"type":"step_start","part":{"type":"step-start"}}
{"type":"reasoning","part":{"type":"reasoning","text":"thinking aloud"}}
{"type":"text","part":{"id":"a","type":"text","text":"OK"}}
`
	got, err := parseOpenCodeOutput(raw)
	if err != nil {
		t.Fatalf("parseOpenCodeOutput: %v", err)
	}
	if got != "OK" {
		t.Errorf("got %q, want only text events", got)
	}
}

func TestParseOpenCodeOutput_MalformedLineErrors(t *testing.T) {
	raw := "not json at all\n"
	if _, err := parseOpenCodeOutput(raw); err == nil {
		t.Fatal("expected error for malformed JSON line")
	}
}

func TestParseOpenCodeOutput_BlankLinesSkipped(t *testing.T) {
	raw := `{"type":"text","part":{"id":"a","type":"text","text":"OK"}}

{"type":"text","part":{"id":"b","type":"text","text":" fine"}}
`
	got, err := parseOpenCodeOutput(raw)
	if err != nil {
		t.Fatalf("parseOpenCodeOutput: %v", err)
	}
	if got != "OK fine" {
		t.Errorf("got %q, want blank lines skipped", got)
	}
}

func TestOpenCodeClient_Reflect_ReturnsConcatenatedText(t *testing.T) {
	bin := fakeOpenCodeBinary(t, `printf '%s\n' '{"type":"text","part":{"type":"text","text":"HELLO"}}' '{"type":"text","part":{"type":"text","text":" WORLD"}}'`)
	c := &OpenCodeClient{binary: bin}
	text, usage, err := c.Reflect(context.Background(), "prompt")
	if err != nil {
		t.Fatalf("Reflect: %v", err)
	}
	if text != "HELLO WORLD" {
		t.Errorf("got %q, want %q", text, "HELLO WORLD")
	}
	if usage != (TokenUsage{}) {
		t.Errorf("expected zero TokenUsage, got %+v", usage)
	}
}

func TestOpenCodeClient_Run_PropagatesStderrOnFailure(t *testing.T) {
	bin := fakeOpenCodeBinary(t, `echo "boom" >&2; exit 1`)
	c := &OpenCodeClient{binary: bin}
	_, _, err := c.Reflect(context.Background(), "prompt")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("expected error to include stderr, got %v", err)
	}
}

func TestOpenCodeClient_Classify_JoinsSystemAndUserIntoPrompt(t *testing.T) {
	bin := fakeOpenCodeBinary(t, `for last; do :; done
case "$last" in *SYSTEM*USER*) printf '%s\n' '{"type":"text","part":{"type":"text","text":"KEEP"}}';; *) echo "prompt not joined" >&2; exit 1;; esac`)
	c := &OpenCodeClient{binary: bin}
	text, err := c.Classify(context.Background(), "SYSTEM", "USER")
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	if text != "KEEP" {
		t.Errorf("got %q, want %q", text, "KEEP")
	}
}

func TestOpenCodeClient_ScrubsEnv(t *testing.T) {
	bin := fakeOpenCodeBinary(t, `
if [ -n "$ANTHROPIC_API_KEY" ]; then echo "LEAKED API KEY" >&2; exit 1; fi
case "$XDG_CONFIG_HOME" in
  *ghost-opencode-*) ;;
  *) echo "XDG_CONFIG_HOME not scrubbed: $XDG_CONFIG_HOME" >&2; exit 1;;
esac
printf '%s\n' '{"type":"text","part":{"type":"text","text":"OK"}}'
`)
	t.Setenv("ANTHROPIC_API_KEY", "sk-should-not-leak")
	t.Setenv("XDG_CONFIG_HOME", "/tmp/fake-real-config")
	c := &OpenCodeClient{binary: bin}
	text, _, err := c.Reflect(context.Background(), "prompt")
	if err != nil {
		t.Fatalf("Reflect: %v", err)
	}
	if text != "OK" {
		t.Errorf("got %q, want OK", text)
	}
}

// TestOpenCodeClient_ModelFlagFromEnv: when GHOST_OPENCODE_MODEL is set the
// subprocess invocation must carry `-m <model>` so callers can pin the model
// (e.g. deepseek) despite opencode's scrubbed config dir.
func TestOpenCodeClient_ModelFlagFromEnv(t *testing.T) {
	bin := fakeOpenCodeBinary(t, `printf '%s\n' '{"type":"text","part":{"type":"text","text":"'"$*"'"}}'`)
	t.Setenv("GHOST_OPENCODE_MODEL", "deepseek/deepseek-v4")
	c := &OpenCodeClient{binary: bin}
	text, _, err := c.Reflect(context.Background(), "prompt")
	if err != nil {
		t.Fatalf("Reflect: %v", err)
	}
	if !strings.Contains(text, "-m deepseek/deepseek-v4") || !strings.Contains(text, "--pure") {
		t.Fatalf("model flag not passed, got %q", text)
	}
}

// TestOpenCodeClient_NoEnvNoModelFlag: with GHOST_OPENCODE_MODEL unset or
// empty, no -m flag may appear (opencode keeps its default model selection).
func TestOpenCodeClient_NoEnvNoModelFlag(t *testing.T) {
	bin := fakeOpenCodeBinary(t, `printf '%s\n' '{"type":"text","part":{"type":"text","text":"'"$*"'"}}'`)
	t.Setenv("GHOST_OPENCODE_MODEL", "")
	c := &OpenCodeClient{binary: bin}
	text, _, err := c.Reflect(context.Background(), "prompt")
	if err != nil {
		t.Fatalf("Reflect: %v", err)
	}
	if strings.Contains(text, " -m ") || strings.HasSuffix(strings.TrimSpace(text), "-m") {
		t.Fatalf("unexpected -m flag: %q", text)
	}
}
