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
	codex := filepath.Join(dir, "codex")
	goose := filepath.Join(dir, "goose")
	if err := os.WriteFile(claude, []byte("#!/bin/sh\nexit 0"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(opencode, []byte("#!/bin/sh\nexit 0"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(codex, []byte("#!/bin/sh\nexit 0"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(goose, []byte("#!/bin/sh\nexit 0"), 0o755); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		source string
		name   string
		ok     bool
	}{
		{"claude-code", "cli", true},
		{"opencode", "opencode", true},
		{"codex", "codex", true},
		{"goose", "goose", true},
		// Empty/unknown source must NOT cascade to claude (or anything else):
		// callers resolve the source or fail, so the provider is unavailable.
		{"unknown-source", "none", false},
		{"", "none", false},
	}
	for _, tt := range tests {
		t.Run(tt.source, func(t *testing.T) {
			p := NewSourceProviderForSource(tt.source, claude, opencode, codex, goose)
			if p.Name() != tt.name {
				t.Errorf("Name() = %q, want %q", p.Name(), tt.name)
			}
			if p.Available() != tt.ok {
				t.Errorf("Available() = %v, want %v", p.Available(), tt.ok)
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

func TestSourceProviderCodexRoutesToRealBackend(t *testing.T) {
	dir := t.TempDir()
	codex := filepath.Join(dir, "codex")
	if err := os.WriteFile(codex, []byte("#!/bin/sh\nexit 0"), 0o755); err != nil {
		t.Fatal(err)
	}
	p := NewSourceProviderForSource("codex", "", "", codex, "")
	if p.Name() != "codex" {
		t.Errorf("Name() = %q, want %q", p.Name(), "codex")
	}
	if !p.Available() {
		t.Error("expected available")
	}
}

func TestSourceProviderGooseRoutesToRealBackend(t *testing.T) {
	dir := t.TempDir()
	goose := filepath.Join(dir, "goose")
	if err := os.WriteFile(goose, []byte("#!/bin/sh\nexit 0"), 0o755); err != nil {
		t.Fatal(err)
	}
	p := NewSourceProviderForSource("goose", "", "", "", goose)
	if p.Name() != "goose" {
		t.Errorf("Name() = %q, want %q", p.Name(), "goose")
	}
	if !p.Available() {
		t.Error("expected available")
	}
}

func TestSourceForClientName(t *testing.T) {
	tests := []struct {
		clientName string
		want       string
	}{
		{"opencode", "opencode"},
		{"OpenCode", "opencode"},
		{"claude-code", "claude-code"},
		{"Claude Desktop", "claude-code"},
		{"codex", "codex"},
		{"goose", "goose"},
		{"some-unknown-client", ""},
		{"", ""},
	}
	for _, tt := range tests {
		if got := SourceForClientName(tt.clientName); got != tt.want {
			t.Errorf("SourceForClientName(%q) = %q, want %q", tt.clientName, got, tt.want)
		}
	}
}

// TestNewSourceProviderForSourceWithModel_ThreadsModel pins that the
// constructor-level model reaches the opencode backend (and only that backend).
// The other harnesses have no model flag, so their backends must be constructed
// as before. This is the MCP server's path for applying cli.model_resolve
// without mutating process env.
func TestNewSourceProviderForSourceWithModel_ThreadsModel(t *testing.T) {
	dir := t.TempDir()
	claude := filepath.Join(dir, "claude")
	opencode := filepath.Join(dir, "opencode")
	if err := os.WriteFile(claude, []byte("#!/bin/sh\nexit 0"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(opencode, []byte("#!/bin/sh\nexit 0"), 0o755); err != nil {
		t.Fatal(err)
	}

	// opencode backend: constructor model must be non-empty.
	p := NewSourceProviderForSourceWithModel("opencode", "opencode/big-pickle", claude, opencode, "", "")
	oc, ok := p.backend.(*OpenCodeClient)
	if !ok {
		t.Fatalf("opencode backend = %T, want *OpenCodeClient", p.backend)
	}
	if oc.model != "opencode/big-pickle" {
		t.Errorf("opencode backend model = %q, want opencode/big-pickle", oc.model)
	}

	// claude backend: no model field — it must still construct as *CLIClient
	// (the model is simply not threaded to harnesses that have no model flag).
	p = NewSourceProviderForSourceWithModel("claude-code", "opencode/big-pickle", claude, opencode, "", "")
	if _, ok := p.backend.(*CLIClient); !ok {
		t.Errorf("claude backend = %T, want *CLIClient", p.backend)
	}

	// Empty model: constructor behaves like the plain constructor.
	p = NewSourceProviderForSourceWithModel("opencode", "", claude, opencode, "", "")
	oc, ok = p.backend.(*OpenCodeClient)
	if !ok {
		t.Fatalf("opencode backend = %T, want *OpenCodeClient", p.backend)
	}
	if oc.model != "" {
		t.Errorf("opencode backend model = %q, want empty", oc.model)
	}
}
