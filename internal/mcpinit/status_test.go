package mcpinit

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// TestStatus_ReportsOpenDBFailure verifies that a database which exists but
// fails to open (e.g. mid-migration foreign-key corruption) is surfaced as a
// failed check, not silently skipped. Before this fix, Status only inspected
// the database when memory.OpenDB succeeded, so a broken database looked
// identical to "All checks passed."
func TestStatus_ReportsOpenDBFailure(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dataDir)
	ghostDir := filepath.Join(dataDir, "ghost")
	if err := os.MkdirAll(ghostDir, 0o700); err != nil {
		t.Fatalf("mkdir ghost dir: %v", err)
	}
	dbPath := filepath.Join(ghostDir, "ghost.db")
	if err := os.WriteFile(dbPath, []byte("not a sqlite database"), 0o600); err != nil {
		t.Fatalf("write fake db: %v", err)
	}

	// Isolate PATH so Status can't shell out to a host-installed `claude`
	// binary — this test only exercises the database-open-failure check.
	t.Setenv("PATH", t.TempDir())

	var out bytes.Buffer
	healthy, err := Status(&out)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if healthy {
		t.Error("Status: healthy = true, want false for a broken database")
	}

	output := out.String()
	if !strings.Contains(output, "✗ database:") {
		t.Errorf("expected a failed database check, got:\n%s", output)
	}
	if strings.Contains(output, "All checks passed.") {
		t.Errorf("a broken database must not report \"All checks passed.\", got:\n%s", output)
	}
}

// TestStatus_ReportsInaccessibleDatabase verifies that a database which cannot
// be stat'd for a reason other than absence (e.g. a permission error) is
// surfaced as a failed check instead of being reported as a fresh install.
func TestStatus_ReportsInaccessibleDatabase(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission checks cannot fail as root")
	}
	statusEnv(t)
	dataHome := os.Getenv("XDG_DATA_HOME")
	ghostDir := filepath.Join(dataHome, "ghost")
	if err := os.MkdirAll(ghostDir, 0o700); err != nil {
		t.Fatalf("mkdir ghost dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ghostDir, "ghost.db"), nil, 0o600); err != nil {
		t.Fatalf("write db file: %v", err)
	}
	// Strip read+traverse so os.Stat on the database returns EACCES.
	if err := os.Chmod(ghostDir, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(ghostDir, 0o700) }) // let TempDir removal succeed

	var out bytes.Buffer
	healthy, err := Status(&out)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if healthy {
		t.Error("Status: healthy = true, want false for an inaccessible database")
	}

	output := out.String()
	if !strings.Contains(output, "✗ database:") {
		t.Errorf("expected a failed database check, got:\n%s", output)
	}
	if strings.Contains(output, "no Ghost database (run ghost first)") {
		t.Errorf("a permission error is not a fresh install, got:\n%s", output)
	}
	if strings.Contains(output, "All checks passed.") {
		t.Errorf("an inaccessible database must not report \"All checks passed.\", got:\n%s", output)
	}
}

// statusEnv isolates a status run from the host: no binaries on PATH or in the
// common install dirs, a clean XDG_CONFIG_HOME/XDG_DATA_HOME, and embeddings
// disabled so the Ollama check can't reach the network.
func statusEnv(t *testing.T) {
	t.Helper()
	t.Setenv("PATH", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("GHOST_EMBEDDING_ENABLED", "false")
	orig := systemBinDirs
	systemBinDirs = nil
	t.Cleanup(func() { systemBinDirs = orig })
}

// writeStubGhost creates an executable `ghost` stub in a temp dir and returns
// that dir, so exec.LookPath finds it without touching the host.
func writeStubGhost(t *testing.T) string {
	t.Helper()
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "ghost"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write stub ghost: %v", err)
	}
	return binDir
}

// writeGhostConfigFile writes a ghost config.yaml into the isolated config dir
// (os.UserConfigDir under the test's HOME) and returns its path.
func writeGhostConfigFile(t *testing.T, content string) string {
	t.Helper()
	configDir, err := os.UserConfigDir()
	if err != nil {
		t.Fatalf("user config dir: %v", err)
	}
	dir := filepath.Join(configDir, "ghost")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir ghost config dir: %v", err)
	}
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write ghost config: %v", err)
	}
	return path
}

// writeOpencodeConfigFile writes an opencode config file into the given
// XDG_CONFIG_HOME and returns its path.
func writeOpencodeConfigFile(t *testing.T, configHome, content string) string {
	t.Helper()
	dir := filepath.Join(configHome, "opencode")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir opencode config dir: %v", err)
	}
	path := filepath.Join(dir, "opencode.json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write opencode config: %v", err)
	}
	return path
}

// TestStatusOpencode_GhostMissing verifies a missing ghost binary is reported
// as a failed check and blocks "All checks passed.".
func TestStatusOpencode_GhostMissing(t *testing.T) {
	statusEnv(t)
	writeOpencodeConfigFile(t, os.Getenv("XDG_CONFIG_HOME"), `{"mcp":{"ghost":{"type":"local","command":["ghost","mcp"],"enabled":true}}}`)

	var out bytes.Buffer
	healthy, err := StatusOpencode(&out)
	if err != nil {
		t.Fatalf("StatusOpencode: %v", err)
	}
	if healthy {
		t.Error("StatusOpencode: healthy = true, want false when the ghost binary is missing")
	}

	output := out.String()
	if !strings.Contains(output, "✗ ghost binary not found in PATH") {
		t.Errorf("expected a failed ghost binary check, got:\n%s", output)
	}
	if strings.Contains(output, "All checks passed.") {
		t.Errorf("missing ghost must not report \"All checks passed.\", got:\n%s", output)
	}
}

// TestStatusOpencode_RegisteredNoDatabase verifies a clean opencode setup — a
// ghost binary on PATH, a correct mcp.ghost config, and no database yet — is
// fully healthy and prints "All checks passed." without a Claude binary.
func TestStatusOpencode_RegisteredNoDatabase(t *testing.T) {
	statusEnv(t)
	binDir := writeStubGhost(t)
	t.Setenv("PATH", binDir)
	writeOpencodeConfigFile(t, os.Getenv("XDG_CONFIG_HOME"), `{"mcp":{"ghost":{"type":"local","command":["ghost","mcp"],"enabled":true}}}`)

	var out bytes.Buffer
	healthy, err := StatusOpencode(&out)
	if err != nil {
		t.Fatalf("StatusOpencode: %v", err)
	}
	if !healthy {
		t.Error("StatusOpencode: healthy = false, want true for a clean opencode setup")
	}

	output := out.String()
	for _, want := range []string{
		"✓ ghost binary: " + filepath.Join(binDir, "ghost"),
		"✓ opencode MCP config: ghost registered",
		"- no Ghost database (run ghost first)",
		"All checks passed.",
	} {
		if !strings.Contains(output, want) {
			t.Errorf("expected %q in output, got:\n%s", want, output)
		}
	}
	if strings.Contains(output, "✗") {
		t.Errorf("a clean opencode setup must have no failed checks, got:\n%s", output)
	}
}

// TestStatusOpencode_MCPConfigNotRegistered verifies a missing or wrongly
// commanded mcp.ghost entry fails the config check and blocks "All checks passed.".
func TestStatusOpencode_MCPConfigNotRegistered(t *testing.T) {
	cases := map[string]string{
		"wrong command": `{"mcp":{"ghost":{"type":"local","command":["ghost","server"],"enabled":true}}}`,
		"missing entry": `{"mcp":{}}`,
	}
	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			statusEnv(t)
			t.Setenv("PATH", writeStubGhost(t))
			writeOpencodeConfigFile(t, os.Getenv("XDG_CONFIG_HOME"), cfg)

			var out bytes.Buffer
			healthy, err := StatusOpencode(&out)
			if err != nil {
				t.Fatalf("StatusOpencode: %v", err)
			}
			if healthy {
				t.Error("StatusOpencode: healthy = true, want false for a bad mcp.ghost entry")
			}

			output := out.String()
			if !strings.Contains(output, "✗ opencode MCP config: ghost missing or wrong command") {
				t.Errorf("expected a failed opencode config check, got:\n%s", output)
			}
			if strings.Contains(output, "All checks passed.") {
				t.Errorf("a bad mcp.ghost entry must not report \"All checks passed.\", got:\n%s", output)
			}
		})
	}
}

// TestStatusOpencode_EmptyStoreHealthy exercises the full DB path against a
// fresh (empty) database with a stubbed Ollama, verifying the total==0
// embeddings check passes and the whole run stays healthy.
func TestStatusOpencode_EmptyStoreHealthy(t *testing.T) {
	ollama := ollamaStub("nomic-embed-text:v1.5")
	defer ollama.Close()

	statusEnv(t)
	writeGhostConfigFile(t, fmt.Sprintf(`embedding:
  ollama_url: %s
  model: nomic-embed-text:v1.5
  dimensions: 768
  enabled: true
`, ollama.URL))
	t.Setenv("GHOST_EMBEDDING_ENABLED", "true")
	t.Setenv("PATH", writeStubGhost(t))

	ghostDir := filepath.Join(os.Getenv("XDG_DATA_HOME"), "ghost")
	if err := os.MkdirAll(ghostDir, 0o700); err != nil {
		t.Fatalf("mkdir ghost dir: %v", err)
	}
	db, err := memory.OpenDB(filepath.Join(ghostDir, "ghost.db"))
	if err != nil {
		t.Fatalf("open fresh db: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close fresh db: %v", err)
	}

	writeOpencodeConfigFile(t, os.Getenv("XDG_CONFIG_HOME"), `{"mcp":{"ghost":{"type":"local","command":["ghost","mcp"],"enabled":true}}}`)

	var out bytes.Buffer
	healthy, err := StatusOpencode(&out)
	if err != nil {
		t.Fatalf("StatusOpencode: %v", err)
	}
	if !healthy {
		t.Error("StatusOpencode: healthy = false, want true for an empty but valid store")
	}

	output := out.String()
	for _, want := range []string{
		"✓ Ollama model nomic-embed-text:v1.5 installed",
		"✓ embeddings: 0 memories (store empty)",
		"All checks passed.",
	} {
		if !strings.Contains(output, want) {
			t.Errorf("expected %q in output, got:\n%s", want, output)
		}
	}
}

// TestCheckEmbeddingStats pins the embedding-stats classification: an empty
// store passes with "(store empty)", a populated store passes only when some
// memories are embedded, and a populated store with zero embeddings fails.
func TestCheckEmbeddingStats(t *testing.T) {
	cases := []struct {
		embedded, total int
		wantPass        bool
		wantText        string
	}{
		{0, 0, true, "embeddings: 0 memories (store empty)"},
		{0, 5, false, "embeddings: 0/5 memories — vector search and linking inactive"},
		{5, 5, true, "embeddings: 5/5 memories"},
		{3, 5, true, "embeddings: 3/5 memories"},
	}
	for _, c := range cases {
		var passed bool
		var passText, failText string
		check := func(ok bool, pass, fail string) {
			passed = ok
			passText = pass
			failText = fail
		}
		checkEmbeddingStats(check, c.embedded, c.total)
		if passed != c.wantPass {
			t.Errorf("embedded=%d total=%d: want ok=%v, got %v", c.embedded, c.total, c.wantPass, passed)
		}
		text := passText
		if !passed {
			text = failText
		}
		if text != c.wantText {
			t.Errorf("embedded=%d total=%d: want text %q, got %q", c.embedded, c.total, c.wantText, text)
		}
	}
}
