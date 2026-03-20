package tool

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestBash_BlockedPatterns(t *testing.T) {
	blocked := []string{
		"rm -rf /",
		"rm -rf ~/",
		"rm -rf /*",
		"rm -rf .",
		"mkfs /dev/sda",
		"dd if=/dev/zero of=/dev/sda",
		"dd if=/dev/urandom of=disk.img",
		":(){ :|:& };:",
		"> /dev/sda",
		"chmod -R 777 /",
		"> /etc/passwd",
		"> /etc/shadow",
		"> /etc/sudoers",
		"> /etc/hosts",
	}

	for _, cmd := range blocked {
		t.Run(cmd, func(t *testing.T) {
			input, _ := json.Marshal(bashInput{
				Command:     cmd,
				Description: "test blocked",
			})
			r := execBash(context.Background(), t.TempDir(), input)
			if !r.IsError {
				t.Errorf("expected block for %q", cmd)
			}
			if !strings.Contains(r.Content, "blocked") {
				t.Errorf("expected 'blocked' in error, got: %s", r.Content)
			}
		})
	}
}

func TestBash_BlockedPatternsCaseInsensitive(t *testing.T) {
	input, _ := json.Marshal(bashInput{
		Command:     "MKFS.ext4 /dev/sda1",
		Description: "format disk",
	})
	r := execBash(context.Background(), t.TempDir(), input)
	if !r.IsError {
		t.Fatal("expected block for uppercase MKFS")
	}
}

func TestBash_SimpleEcho(t *testing.T) {
	input, _ := json.Marshal(bashInput{
		Command:     "echo hello world",
		Description: "test echo",
	})
	r := execBash(context.Background(), t.TempDir(), input)
	if r.IsError {
		t.Fatalf("unexpected error: %s", r.Content)
	}
	if r.Content != "hello world" {
		t.Errorf("content = %q, want %q", r.Content, "hello world")
	}
}

func TestBash_NoOutput(t *testing.T) {
	input, _ := json.Marshal(bashInput{
		Command:     "true",
		Description: "no output command",
	})
	r := execBash(context.Background(), t.TempDir(), input)
	if r.IsError {
		t.Fatalf("unexpected error: %s", r.Content)
	}
	if r.Content != "(no output)" {
		t.Errorf("content = %q, want %q", r.Content, "(no output)")
	}
}

func TestBash_ExitError(t *testing.T) {
	input, _ := json.Marshal(bashInput{
		Command:     "exit 1",
		Description: "fail command",
	})
	r := execBash(context.Background(), t.TempDir(), input)
	if !r.IsError {
		t.Fatal("expected error for exit 1")
	}
	if !strings.Contains(r.Content, "exit error") {
		t.Errorf("expected 'exit error', got: %s", r.Content)
	}
}

func TestBash_StderrIncluded(t *testing.T) {
	input, _ := json.Marshal(bashInput{
		Command:     "echo out && echo err >&2",
		Description: "mixed output",
	})
	r := execBash(context.Background(), t.TempDir(), input)
	if r.IsError {
		t.Fatalf("unexpected error: %s", r.Content)
	}
	if !strings.Contains(r.Content, "out") || !strings.Contains(r.Content, "err") {
		t.Errorf("expected both stdout and stderr, got: %s", r.Content)
	}
}

func TestBash_TimeoutDefault(t *testing.T) {
	// Timeout of 0 should default to 60s (not block forever).
	// We just verify the command runs — not the actual timeout duration.
	input, _ := json.Marshal(bashInput{
		Command:        "echo ok",
		Description:    "default timeout",
		TimeoutSeconds: 0,
	})
	r := execBash(context.Background(), t.TempDir(), input)
	if r.IsError {
		t.Fatalf("unexpected error: %s", r.Content)
	}
	if r.Content != "ok" {
		t.Errorf("content = %q", r.Content)
	}
}

func TestBash_TimeoutClamped(t *testing.T) {
	// Timeout > 300 should be clamped to 300s.
	// Just test that it runs, not the actual timeout value.
	input, _ := json.Marshal(bashInput{
		Command:        "echo clamped",
		Description:    "clamped timeout",
		TimeoutSeconds: 999,
	})
	r := execBash(context.Background(), t.TempDir(), input)
	if r.IsError {
		t.Fatalf("unexpected error: %s", r.Content)
	}
	if r.Content != "clamped" {
		t.Errorf("content = %q", r.Content)
	}
}

func TestBash_Timeout(t *testing.T) {
	input, _ := json.Marshal(bashInput{
		Command:        "sleep 10",
		Description:    "should timeout",
		TimeoutSeconds: 1,
	})
	r := execBash(context.Background(), t.TempDir(), input)
	if !r.IsError {
		t.Fatal("expected timeout error")
	}
	if !strings.Contains(r.Content, "timed out") {
		t.Errorf("expected 'timed out', got: %s", r.Content)
	}
}

func TestBash_WorkingDirectory(t *testing.T) {
	dir := t.TempDir()
	input, _ := json.Marshal(bashInput{
		Command:     "pwd",
		Description: "check cwd",
	})
	r := execBash(context.Background(), dir, input)
	if r.IsError {
		t.Fatalf("unexpected error: %s", r.Content)
	}
	if r.Content != dir {
		t.Errorf("pwd = %q, want %q", r.Content, dir)
	}
}

func TestBash_InvalidJSON(t *testing.T) {
	r := execBash(context.Background(), "/tmp", json.RawMessage(`{bad`))
	if !r.IsError {
		t.Fatal("expected error for invalid JSON")
	}
}
