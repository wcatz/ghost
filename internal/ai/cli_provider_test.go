package ai

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func writeFake(t *testing.T, dir, name string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell script fake binary requires a POSIX shell")
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write fake %s: %v", name, err)
	}
}

func TestCLIProvider_PrefersClaudeOverOpencode(t *testing.T) {
	dir := t.TempDir()
	writeFake(t, dir, "claude")
	writeFake(t, dir, "opencode")
	t.Setenv("PATH", dir)

	p := NewCLIProvider()
	if p.Name() != "cli" {
		t.Errorf("expected claude preference, got %q", p.Name())
	}
}

func TestCLIProvider_FallsBackToOpencode(t *testing.T) {
	dir := t.TempDir()
	writeFake(t, dir, "opencode")
	t.Setenv("PATH", dir)

	p := NewCLIProvider()
	if !p.Available() {
		t.Fatal("expected available via opencode")
	}
	if p.Name() != "opencode" {
		t.Errorf("expected opencode fallback, got %q", p.Name())
	}
}

func TestCLIProvider_UnavailableWhenNeither(t *testing.T) {
	t.Setenv("PATH", t.TempDir())

	p := NewCLIProvider()
	if p.Available() {
		t.Fatal("expected unavailable")
	}
	if p.Name() != "none" {
		t.Errorf("expected name %q, got %q", "none", p.Name())
	}
	if _, _, err := p.Reflect(context.Background(), "prompt"); err == nil {
		t.Fatal("expected error from Reflect when no backend")
	}
	if _, err := p.Classify(context.Background(), "sys", "user"); err == nil {
		t.Fatal("expected error from Classify when no backend")
	}
}

func TestCLIProvider_DelegatesReflect(t *testing.T) {
	dir := t.TempDir()
	writeFake(t, dir, "opencode")
	t.Setenv("PATH", dir)

	p := NewCLIProvider()
	if _, _, err := p.Reflect(context.Background(), "prompt"); err != nil {
		t.Fatalf("Reflect delegation: %v", err)
	}
}

func TestCLIProviderWithBinaries(t *testing.T) {
	t.Run("explicit opencode", func(t *testing.T) {
		dir := t.TempDir()
		writeFake(t, dir, "opencode")
		t.Setenv("PATH", t.TempDir())
		p := NewCLIProviderWithBinaries("", filepath.Join(dir, "opencode"), "", "")
		if !p.Available() || p.Name() != "opencode" {
			t.Fatalf("got available=%v name=%q, want true/opencode", p.Available(), p.Name())
		}
	})
	t.Run("explicit claude", func(t *testing.T) {
		dir := t.TempDir()
		writeFake(t, dir, "claude")
		t.Setenv("PATH", t.TempDir())
		p := NewCLIProviderWithBinaries(filepath.Join(dir, "claude"), "", "", "")
		if !p.Available() || p.Name() != "cli" {
			t.Fatalf("got available=%v name=%q, want true/cli", p.Available(), p.Name())
		}
	})
	t.Run("fallback to PATH when override missing", func(t *testing.T) {
		dir := t.TempDir()
		writeFake(t, dir, "opencode")
		t.Setenv("PATH", dir)
		p := NewCLIProviderWithBinaries(filepath.Join(dir, "no-such-claude"), "", "", "")
		if !p.Available() || p.Name() != "opencode" {
			t.Fatalf("got available=%v name=%q, want true/opencode", p.Available(), p.Name())
		}
	})
}
