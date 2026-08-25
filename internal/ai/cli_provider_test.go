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
	t.Setenv("PATH", t.TempDir()) // empty dir: no claude, no opencode

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

	// Note: the fake opencode above exits 0 with no output, so Reflect returns
	// an empty string, not an error — this asserts delegation plumbing only.
	p := NewCLIProvider()
	if _, _, err := p.Reflect(context.Background(), "prompt"); err != nil {
		t.Fatalf("Reflect delegation: %v", err)
	}
}

func TestCLIProviderWithBinaries_UsesExplicitOpenCodePath(t *testing.T) {
	dir := t.TempDir()
	writeFake(t, dir, "opencode")
	t.Setenv("PATH", t.TempDir()) // empty PATH: nothing resolvable by name

	p := NewCLIProviderWithBinaries("", filepath.Join(dir, "opencode"), "", "")
	if !p.Available() {
		t.Fatal("expected available via explicit opencode path")
	}
	if p.Name() != "opencode" {
		t.Errorf("expected opencode, got %q", p.Name())
	}
}

func TestCLIProviderWithBinaries_UsesExplicitClaudePath(t *testing.T) {
	dir := t.TempDir()
	writeFake(t, dir, "claude")
	t.Setenv("PATH", t.TempDir())

	p := NewCLIProviderWithBinaries(filepath.Join(dir, "claude"), "", "", "")
	if !p.Available() {
		t.Fatal("expected available via explicit claude path")
	}
	if p.Name() != "cli" {
		t.Errorf("expected cli, got %q", p.Name())
	}
}

func TestCLIProviderWithBinaries_FallsBackToPathWhenOverrideMissing(t *testing.T) {
	dir := t.TempDir()
	writeFake(t, dir, "opencode")
	t.Setenv("PATH", dir)

	// Explicit claude path is missing, so fall back to the PATH lookup, which
	// finds opencode.
	p := NewCLIProviderWithBinaries(filepath.Join(dir, "no-such-claude"), "", "", "")
	if !p.Available() {
		t.Fatal("expected available via PATH fallback")
	}
	if p.Name() != "opencode" {
		t.Errorf("expected opencode via PATH fallback, got %q", p.Name())
	}
}
