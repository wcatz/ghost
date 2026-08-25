package ai

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSourceProviderForSource(t *testing.T) {
	dir := t.TempDir()
	claude := filepath.Join(dir, "claude")
	opencode := filepath.Join(dir, "opencode")
	if err := os.WriteFile(claude, []byte("#!/bin/sh\nexit 0"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(opencode, []byte("#!/bin/sh\nexit 0"), 0o755); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		source string
		name   string
		ok     bool
	}{
		{"claude-code", "cli", true},
		{"opencode", "opencode", true},
		{"codex", "cli", true},         // no codex CLI impl yet — falls back to claude
		{"goose", "cli", true},         // no goose CLI impl yet — falls back to claude
		{"unknown-source", "cli", true}, // falls back to claude
	}
	for _, tt := range tests {
		t.Run(tt.source, func(t *testing.T) {
			p := NewSourceProviderForSource(tt.source, claude, opencode)
			if p.Name() != tt.name {
				t.Errorf("Name() = %q, want %q", p.Name(), tt.name)
			}
			if !p.Available() {
				t.Error("expected available")
			}
		})
	}
}

func TestSourceProviderUnavailable(t *testing.T) {
	p := NewSourceProviderForSource("claude-code", "/nonexistent/claude")
	if p.Available() {
		t.Error("expected unavailable when binary not found")
	}
	if p.Name() != "none" {
		t.Errorf("Name() = %q, want %q", p.Name(), "none")
	}
	_, _, err := p.Reflect(t.Context(), "test")
	if err != errUnknownSource {
		t.Errorf("expected errUnknownSource from Reflect, got %v", err)
	}
	_, err = p.Classify(t.Context(), "sys", "user")
	if err != errUnknownSource {
		t.Errorf("expected errUnknownSource from Classify, got %v", err)
	}
}

func TestSourceProviderClassify(t *testing.T) {
	p := &SourceProvider{backend: nil, name: "none"}
	_, err := p.Classify(t.Context(), "sys", "user")
	if err != errUnknownSource {
		t.Errorf("expected errUnknownSource, got %v", err)
	}
}
