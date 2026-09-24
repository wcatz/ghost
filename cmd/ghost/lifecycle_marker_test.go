package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wcatz/ghost/internal/config"
)

// isolatedLifecycleEnv points HOME/USERPROFILE/XDG_DATA_HOME/XDG_CONFIG_HOME
// at fresh temp dirs, clears GHOST_* / ANTHROPIC_API_KEY overrides, pins the
// auto-phase config, and makes every LLM CLI binary unreachable — the exact
// mr-slave incident shape (auto_reflect on, no harness on the non-login PATH).
// Returns the data dir root (config.DataDir() = <root>/ghost).
func isolatedLifecycleEnv(t *testing.T) string {
	t.Helper()
	for _, e := range os.Environ() {
		if key, _, ok := strings.Cut(e, "="); ok && (strings.HasPrefix(key, "GHOST_") || key == "ANTHROPIC_API_KEY") {
			if old, ok := os.LookupEnv(key); ok {
				t.Setenv(key, old)
				_ = os.Unsetenv(key)
			}
		}
	}
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "config"))
	dataHome := filepath.Join(dir, "data")
	t.Setenv("XDG_DATA_HOME", dataHome)
	// No harness on PATH, and every explicit cli.*_binary override points
	// nowhere (they fall back to the same empty PATH), so llmOK is false
	// regardless of any /etc/ghost/config.yaml on the machine.
	t.Setenv("PATH", t.TempDir())
	t.Setenv("GHOST_CLI_CLAUDE_BINARY", filepath.Join(dir, "no-such-claude"))
	t.Setenv("GHOST_CLI_OPENCODE_BINARY", filepath.Join(dir, "no-such-opencode"))
	t.Setenv("GHOST_CLI_CODEX_BINARY", filepath.Join(dir, "no-such-codex"))
	t.Setenv("GHOST_CLI_GOOSE_BINARY", filepath.Join(dir, "no-such-goose"))

	// User-layer config wins over /etc for the keys it sets: exactly the
	// reflect-only auto-consolidation shape under test.
	path, err := config.ConfigFilePath()
	if err != nil {
		t.Fatalf("config file path: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir config dir: %v", err)
	}
	const yaml = "reflection:\n  auto_reflect: true\n  auto_resolve: false\n  auto_supersede: false\n"
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return dataHome
}

// TestRunLifecycle_ReflectSkippedWithoutLLMWritesMarker: with auto_reflect
// enabled and no LLM CLI reachable, runLifecycle skips reflect entirely
// today — silently, for the detached stop hook, forever. The run must leave
// a lifecycle-last-failure.json marker recording the skipped phase so the
// next session-start can surface it.
func TestRunLifecycle_ReflectSkippedWithoutLLMWritesMarker(t *testing.T) {
	dataHome := isolatedLifecycleEnv(t)

	// runLifecycle parses os.Args[2:]; point it at this invocation.
	origArgs := os.Args
	os.Args = []string{origArgs[0], "lifecycle", "--project", "projx"}
	defer func() { os.Args = origArgs }()

	runLifecycle()

	markerFile := filepath.Join(dataHome, "ghost", "lifecycle-last-failure.json")
	b, err := os.ReadFile(markerFile)
	if err != nil {
		t.Fatalf("expected a failure marker after reflect was skipped for no LLM backend: %v", err)
	}
	var m struct {
		Project      string   `json:"project"`
		PhasesFailed []string `json:"phases_failed"`
		Error        string   `json:"error"`
		At           string   `json:"at"`
		Version      int      `json:"version"`
	}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal marker %q: %v", b, err)
	}
	if m.Project != "projx" {
		t.Errorf("marker project = %q, want projx", m.Project)
	}
	if len(m.PhasesFailed) != 1 || m.PhasesFailed[0] != "reflect" {
		t.Errorf("marker phases_failed = %v, want [reflect]", m.PhasesFailed)
	}
	if !strings.Contains(m.Error, "no CLI LLM backend") {
		t.Errorf("marker error = %q, want the no-CLI-backend explanation", m.Error)
	}
	if _, err := time.Parse(time.RFC3339, m.At); err != nil {
		t.Errorf("marker at = %q, want RFC3339: %v", m.At, err)
	}
	if m.Version != 1 {
		t.Errorf("marker version = %d, want 1", m.Version)
	}
}
