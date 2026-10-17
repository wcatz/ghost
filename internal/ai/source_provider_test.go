package ai

import (
	"os"
	"testing"
)

func sourceProviderTestBinary(t *testing.T) string {
	t.Helper()
	binary, err := os.Executable()
	if err != nil {
		t.Fatalf("resolve test executable: %v", err)
	}
	return binary
}

func TestSourceProviderForSource(t *testing.T) {
	binary := sourceProviderTestBinary(t)
	claude, opencode, codex, goose := binary, binary, binary, binary

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

// TestKnownSourcesMatchTheProviderSwitch ties the written-down source list to
// newSourceProvider's switch: every listed token resolves to an available
// provider when its binary exists, and a few tokens outside the list do not.
// A source dropped from the list fails here; a case added to the switch alone
// does not, because the switch cannot be enumerated, so a new source still has
// to be added to knownSources by hand.
func TestKnownSourcesMatchTheProviderSwitch(t *testing.T) {
	binary := sourceProviderTestBinary(t)
	for _, source := range KnownSources() {
		if !IsKnownSource(source) {
			t.Errorf("IsKnownSource(%q) = false for a listed source", source)
		}
		if p := NewSourceProviderForSource(source, binary, binary, binary, binary); !p.Available() {
			t.Errorf("listed source %q is not resolved by the provider switch", source)
		}
	}
	// Every token this package itself produces — from an MCP client name or
	// a harness process name — must be one the list names, or a detected
	// source would be refused as unknown.
	for _, c := range clientSourceNames {
		if !IsKnownSource(c.source) {
			t.Errorf("clientSourceNames maps %q to %q, which KnownSources omits", c.name, c.source)
		}
	}
	for _, name := range []string{"claude", "claude-code", "opencode", "codex", "goose"} {
		if s := sourceFromProcessName(name); !IsKnownSource(s) {
			t.Errorf("sourceFromProcessName(%q) = %q, which KnownSources omits", name, s)
		}
	}
	for _, env := range []string{"OPENCODE", "CLAUDECODE", "CLAUDE_CODE_ENTRYPOINT"} {
		lookup := func(k string) string {
			if k == env {
				return "1"
			}
			return ""
		}
		if s := detectSourceFromEnv(lookup); !IsKnownSource(s) {
			t.Errorf("detectSourceFromEnv with %s set = %q, which KnownSources omits", env, s)
		}
	}
	for _, source := range []string{"", "claude", "Claude-Code", "bogus"} {
		if IsKnownSource(source) {
			t.Errorf("IsKnownSource(%q) = true, want false", source)
		}
		if p := NewSourceProviderForSource(source, binary, binary, binary, binary); p.Available() {
			t.Errorf("unlisted source %q is resolved by the provider switch; add it to knownSources", source)
		}
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
	codex := sourceProviderTestBinary(t)
	p := NewSourceProviderForSource("codex", "", "", codex, "")
	if p.Name() != "codex" {
		t.Errorf("Name() = %q, want %q", p.Name(), "codex")
	}
	if !p.Available() {
		t.Error("expected available")
	}
}

func TestSourceProviderGooseRoutesToRealBackend(t *testing.T) {
	goose := sourceProviderTestBinary(t)
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
	binary := sourceProviderTestBinary(t)
	claude, opencode := binary, binary

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
