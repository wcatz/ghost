package mcpinit

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wcatz/ghost/internal/embedding"
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

// TestStatus_HookMatchWithQuotedPath verifies that a SessionStart hook whose
// command is a quoted binary path (the form `ghost mcp init` writes on
// Windows, e.g. `"C:\Users\ghost\bin\ghost.exe" hook session-start`) is
// recognized as configured. Before this fix, Status checked for the literal
// substring "ghost hook session-start", which never appears once the binary
// path is quoted — so a fully healthy install was reported as missing the
// hook on every platform where ghostBin isn't literally "ghost".
func TestStatus_HookMatchWithQuotedPath(t *testing.T) {
	statusEnv(t)
	home := os.Getenv("HOME")
	settingsDir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(settingsDir, 0o700); err != nil {
		t.Fatalf("mkdir settings dir: %v", err)
	}
	settings := `{
  "hooks": {
    "SessionStart": [{"matcher": "", "hooks": [{"type": "command", "command": "\"/opt/ghost/bin/ghost\" hook session-start"}]}],
    "Stop": [{"matcher": "", "hooks": [{"type": "command", "command": "\"/opt/ghost/bin/ghost\" hook stop"}]}]
  }
}`
	if err := os.WriteFile(filepath.Join(settingsDir, "settings.json"), []byte(settings), 0o600); err != nil {
		t.Fatalf("write settings.json: %v", err)
	}

	var out bytes.Buffer
	if _, err := Status(&out); err != nil {
		t.Fatalf("Status: %v", err)
	}

	output := out.String()
	if !strings.Contains(output, "✓ SessionStart hook configured") {
		t.Errorf("expected SessionStart hook to be recognized as configured, got:\n%s", output)
	}
	if strings.Contains(output, "✗ SessionStart hook missing") {
		t.Errorf("a quoted-path hook command must not be reported missing, got:\n%s", output)
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

// TestReportConfigFile_Missing verifies the config file is reported
// informationally when absent, without failing the check (the config file is
// optional — defaults work without one).
func TestReportConfigFile_Missing(t *testing.T) {
	statusEnv(t)

	var out bytes.Buffer
	reportConfigFile(&out)

	if !strings.Contains(out.String(), "no config file") {
		t.Errorf("expected 'no config file' in output, got: %s", out.String())
	}
}

// TestReportConfigFile_Present verifies the config file's path is reported
// when it exists.
func TestReportConfigFile_Present(t *testing.T) {
	statusEnv(t)
	path := writeGhostConfigFile(t, "")

	var out bytes.Buffer
	reportConfigFile(&out)

	if !strings.Contains(out.String(), path) {
		t.Errorf("expected config file path %q in output, got: %s", path, out.String())
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

// TestStatus_OllamaDownDurationReported verifies that a down-since marker
// written by the embedding worker (internal/embedding.Worker) is surfaced as
// a duration alongside a currently-unreachable Ollama — the core behavior
// requested by issue #287.
func TestStatus_OllamaDownDurationReported(t *testing.T) {
	statusEnv(t)
	t.Setenv("PATH", writeStubGhost(t))

	srv := ollamaStub()
	url := srv.URL
	srv.Close() // unreachable — Status's own live check must see it down too

	writeGhostConfigFile(t, fmt.Sprintf(`embedding:
  ollama_url: %s
  model: nomic-embed-text:v1.5
  dimensions: 768
  enabled: true
`, url))
	t.Setenv("GHOST_EMBEDDING_ENABLED", "true")

	ghostDir := filepath.Join(os.Getenv("XDG_DATA_HOME"), "ghost")
	if err := os.MkdirAll(ghostDir, 0o700); err != nil {
		t.Fatalf("mkdir ghost dir: %v", err)
	}
	since := time.Now().Add(-(2*time.Hour + 3*time.Minute))
	marker := since.UTC().Format(time.RFC3339)
	if err := os.WriteFile(filepath.Join(ghostDir, embedding.OllamaDownMarkerFilename), []byte(marker), 0o600); err != nil {
		t.Fatalf("write marker: %v", err)
	}

	var out bytes.Buffer
	if _, err := Status(&out); err != nil {
		t.Fatalf("Status: %v", err)
	}

	want := fmt.Sprintf("Ollama down since %s (2h 3m)", since.UTC().Format("2006-01-02 15:04 UTC"))
	if !strings.Contains(out.String(), want) {
		t.Errorf("expected output to contain %q, got:\n%s", want, out.String())
	}
}

// TestStatus_OllamaDownDurationSuppressedWhenReachable is the regression test
// for the stale-marker failure mode: if the embedding worker's MCP server
// process exits while Ollama is down, nothing removes the marker file until
// a new worker instance later observes Ollama reachable again. `ghost mcp
// status` must not print a contradictory "Ollama down since ..." line next
// to a passing, live "Ollama model installed" check just because a stale
// marker happens to still be sitting on disk.
func TestStatus_OllamaDownDurationSuppressedWhenReachable(t *testing.T) {
	statusEnv(t)
	t.Setenv("PATH", writeStubGhost(t))

	model := "nomic-embed-text:v1.5"
	ollama := ollamaStub(model)
	defer ollama.Close()

	writeGhostConfigFile(t, fmt.Sprintf(`embedding:
  ollama_url: %s
  model: %s
  dimensions: 768
  enabled: true
`, ollama.URL, model))
	t.Setenv("GHOST_EMBEDDING_ENABLED", "true")

	ghostDir := filepath.Join(os.Getenv("XDG_DATA_HOME"), "ghost")
	if err := os.MkdirAll(ghostDir, 0o700); err != nil {
		t.Fatalf("mkdir ghost dir: %v", err)
	}
	stale := time.Now().Add(-5 * time.Hour).UTC().Format(time.RFC3339)
	if err := os.WriteFile(filepath.Join(ghostDir, embedding.OllamaDownMarkerFilename), []byte(stale), 0o600); err != nil {
		t.Fatalf("write stale marker: %v", err)
	}

	var out bytes.Buffer
	if _, err := Status(&out); err != nil {
		t.Fatalf("Status: %v", err)
	}

	if strings.Contains(out.String(), "Ollama down since") {
		t.Errorf("a currently-reachable Ollama must not report a stale down-duration, got:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "✓ Ollama model "+model+" installed") {
		t.Errorf("expected the live Ollama check to still pass, got:\n%s", out.String())
	}
}

// TestStatus_NoOllamaDownDurationWhenAbsent pins the no-marker baseline: a
// clean install with embedding disabled (statusEnv's default) never prints an
// Ollama-down duration line.
func TestStatus_NoOllamaDownDurationWhenAbsent(t *testing.T) {
	statusEnv(t)
	t.Setenv("PATH", writeStubGhost(t))

	var out bytes.Buffer
	if _, err := Status(&out); err != nil {
		t.Fatalf("Status: %v", err)
	}
	if strings.Contains(out.String(), "Ollama down since") {
		t.Errorf("expected no Ollama-down line without a marker file, got:\n%s", out.String())
	}
}
