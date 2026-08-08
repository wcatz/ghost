package mcpinit

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// setupOpencodeTestEnv isolates a RunOpencode test: HOME and XDG_CONFIG_HOME
// point at temp dirs, PATH contains only a stub ghost binary (no claude, no
// opencode), and embeddings are disabled so the Ollama check stays offline.
func setupOpencodeTestEnv(t *testing.T) (home, xdg string) {
	t.Helper()
	home = t.TempDir()
	t.Setenv("HOME", home)
	xdg = t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	t.Setenv("GHOST_EMBEDDING_ENABLED", "false")

	binDir := filepath.Join(home, "bin")
	if err := os.MkdirAll(binDir, 0755); err != nil {
		t.Fatal(err)
	}
	writeStub(t, binDir, "ghost")
	t.Setenv("PATH", binDir)
	return home, xdg
}

// writeStub creates an executable stub script at binDir/name.
func writeStub(t *testing.T, binDir, name string) string {
	t.Helper()
	path := filepath.Join(binDir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRunOpencode_NoClaudeRequired(t *testing.T) {
	_, xdg := setupOpencodeTestEnv(t)

	var out bytes.Buffer
	if err := RunOpencode(&out, false); err != nil {
		t.Fatalf("RunOpencode: %v", err)
	}

	cfgPath := filepath.Join(xdg, "opencode", "opencode.json")
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("expected opencode config to be written: %v", err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("config should be valid JSON: %v", err)
	}
	mcp, ok := cfg["mcp"].(map[string]any)
	if !ok {
		t.Fatalf("expected mcp object in config, got: %v", cfg)
	}
	ghost, ok := mcp["ghost"].(map[string]any)
	if !ok {
		t.Fatalf("expected mcp.ghost in config, got: %v", mcp)
	}
	if ghost["type"] != "local" {
		t.Errorf("expected type local, got %v", ghost["type"])
	}
	cmd, ok := ghost["command"].([]any)
	if !ok || len(cmd) != 2 || cmd[1] != "mcp" {
		t.Errorf("expected command [<ghost> mcp], got %v", ghost["command"])
	}
	first, ok := cmd[0].(string)
	if !ok || filepath.Base(first) != "ghost" {
		t.Errorf("expected command[0] to be the ghost binary, got %v", cmd[0])
	}
	if ghost["enabled"] != true {
		t.Errorf("expected enabled true, got %v", ghost["enabled"])
	}

	output := out.String()
	if !strings.Contains(output, "registered ghost MCP server for opencode") {
		t.Errorf("expected registration message, got:\n%s", output)
	}
	if strings.Contains(output, "claude") {
		t.Errorf("opencode path must not mention claude, got:\n%s", output)
	}
	if !strings.Contains(output, "Restart opencode to activate.") {
		t.Errorf("expected 'Restart opencode to activate.', got:\n%s", output)
	}
}

func TestRunOpencode_DoesNotTouchClaudeSettings(t *testing.T) {
	home, _ := setupOpencodeTestEnv(t)

	var out bytes.Buffer
	if err := RunOpencode(&out, false); err != nil {
		t.Fatalf("RunOpencode: %v", err)
	}

	settings := filepath.Join(home, ".claude", "settings.json")
	if _, err := os.Stat(settings); err == nil {
		t.Error("RunOpencode must not create ~/.claude/settings.json")
	}
}

func TestRunOpencode_Idempotent(t *testing.T) {
	_, xdg := setupOpencodeTestEnv(t)

	var out1 bytes.Buffer
	if err := RunOpencode(&out1, false); err != nil {
		t.Fatalf("RunOpencode (first): %v", err)
	}
	cfgPath := filepath.Join(xdg, "opencode", "opencode.json")
	first, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}

	var out2 bytes.Buffer
	if err := RunOpencode(&out2, false); err != nil {
		t.Fatalf("RunOpencode (second): %v", err)
	}
	second, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Errorf("config changed on second run:\n--- first ---\n%s\n--- second ---\n%s", first, second)
	}
	if !strings.Contains(out2.String(), "already registered") {
		t.Errorf("second run should report 'already registered', got:\n%s", out2.String())
	}
}

func TestRunOpencode_DryRunWritesNothing(t *testing.T) {
	_, xdg := setupOpencodeTestEnv(t)

	var out bytes.Buffer
	if err := RunOpencode(&out, true); err != nil {
		t.Fatalf("RunOpencode dry run: %v", err)
	}

	output := out.String()
	if !strings.Contains(output, "would register ghost MCP server for opencode") {
		t.Errorf("dry run should say 'would register', got:\n%s", output)
	}
	if _, err := os.Stat(filepath.Join(xdg, "opencode", "opencode.json")); err == nil {
		t.Error("dry run must not write the opencode config")
	}
}

func TestRunOpencode_MergesIntoExistingJSONC(t *testing.T) {
	_, xdg := setupOpencodeTestEnv(t)
	dir := filepath.Join(xdg, "opencode")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	jsonc := filepath.Join(dir, "opencode.jsonc")
	existing := `{
  // theme preference
  "theme": "dark",
  "mcp": {
    "other": {"type": "local", "command": ["node", "server.js"]}
  }
}
`
	if err := os.WriteFile(jsonc, []byte(existing), 0644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := RunOpencode(&out, false); err != nil {
		t.Fatalf("RunOpencode: %v", err)
	}

	data, err := os.ReadFile(jsonc)
	if err != nil {
		t.Fatalf("read opencode.jsonc: %v", err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("merged config must be valid JSON (comments stripped): %v\n%s", err, data)
	}
	if cfg["theme"] != "dark" {
		t.Errorf("expected existing theme key preserved, got %v", cfg["theme"])
	}
	mcp, _ := cfg["mcp"].(map[string]any)
	if mcp == nil || mcp["other"] == nil {
		t.Errorf("expected existing mcp.other server preserved, got %v", mcp)
	}
	if mcp == nil || mcp["ghost"] == nil {
		t.Errorf("expected mcp.ghost merged in, got %v", mcp)
	}
	if _, err := os.Stat(filepath.Join(xdg, "opencode", "opencode.json")); err == nil {
		t.Error("must not create opencode.json alongside opencode.jsonc")
	}
}

// TestRunOpencode_MergesIntoJSONCWithComments covers the previously-missing
// JSONC forms: /* block comments */ and trailing commas, plus warning that the
// rewrite drops comments.
func TestRunOpencode_MergesJSONCWithBlockCommentsAndTrailingCommas(t *testing.T) {
	_, xdg := setupOpencodeTestEnv(t)
	dir := filepath.Join(xdg, "opencode")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	jsonc := filepath.Join(dir, "opencode.jsonc")
	existing := `{
  /* global theme */
  "theme": "dark",
  "mcp": {
    "other": {"type": "local", "command": ["node", "server.js"]},
  },
}
`
	if err := os.WriteFile(jsonc, []byte(existing), 0644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := RunOpencode(&out, false); err != nil {
		t.Fatalf("RunOpencode: %v", err)
	}

	output := out.String()
	if !strings.Contains(output, "warning: rewriting opencode.jsonc") {
		t.Errorf("expected comment-drop warning, got:\n%s", output)
	}

	data, err := os.ReadFile(jsonc)
	if err != nil {
		t.Fatalf("read opencode.jsonc: %v", err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("merged config must be valid JSON (block comments + trailing commas stripped): %v\n%s", err, data)
	}
	if cfg["theme"] != "dark" {
		t.Errorf("expected existing theme key preserved, got %v", cfg["theme"])
	}
	mcp, _ := cfg["mcp"].(map[string]any)
	if mcp == nil || mcp["ghost"] == nil {
		t.Errorf("expected mcp.ghost merged in, got %v", mcp)
	}
}

// TestWriteOpencodeConfig_PreservesMode pins the config file mode: an existing
// 0600 file stays 0600 after a rewrite (the previous 0644 chmod would have
// widened it), and a new file defaults to 0600.
func TestWriteOpencodeConfig_PreservesMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "opencode.json")

	t.Run("existing 0600 file preserves mode", func(t *testing.T) {
		if err := os.WriteFile(path, []byte(`{"mcp":{}}`), 0600); err != nil {
			t.Fatal(err)
		}
		if err := writeOpencodeConfig(path, map[string]any{"mcp": map[string]any{"ghost": map[string]any{"type": "local", "command": []string{"ghost", "mcp"}, "enabled": true}}}); err != nil {
			t.Fatalf("writeOpencodeConfig: %v", err)
		}
		st, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := st.Mode().Perm() & 0o777; got != 0600 {
			t.Errorf("mode = %o, want 600", got)
		}
	})

	t.Run("new file defaults to 0600", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "opencode.json")
		if err := writeOpencodeConfig(path, map[string]any{"mcp": map[string]any{}}); err != nil {
			t.Fatalf("writeOpencodeConfig: %v", err)
		}
		st, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := st.Mode().Perm() & 0o777; got != 0600 {
			t.Errorf("mode = %o, want 600", got)
		}
	})

	t.Run("symlinked config preserves the symlink", func(t *testing.T) {
		dir := t.TempDir()
		real := filepath.Join(dir, "real.json")
		link := filepath.Join(dir, "opencode.json")
		if err := os.WriteFile(real, []byte(`{"mcp":{}}`), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(real, link); err != nil {
			t.Fatal(err)
		}
		if err := writeOpencodeConfig(link, map[string]any{"mcp": map[string]any{"ghost": map[string]any{"type": "local", "command": []string{"ghost", "mcp"}, "enabled": true}}}); err != nil {
			t.Fatalf("writeOpencodeConfig: %v", err)
		}
		fi, err := os.Lstat(link)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode()&os.ModeSymlink == 0 {
			t.Fatal("writeOpencodeConfig replaced the symlink with a regular file")
		}
		target, err := os.Readlink(link)
		if err != nil {
			t.Fatal(err)
		}
		if target != real {
			t.Errorf("symlink target = %q, want %q", target, real)
		}
		data, err := os.ReadFile(real)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(data), "ghost") {
			t.Errorf("real file not updated: %s", data)
		}
	})
}

func TestCheckPrereqs_ProbesCommonDirs(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	binDir := filepath.Join(home, "bin")
	if err := os.MkdirAll(binDir, 0755); err != nil {
		t.Fatal(err)
	}
	ghostStub := writeStub(t, binDir, "ghost")
	t.Setenv("PATH", binDir)

	// Place claude only in a common install dir that is not on PATH.
	localBin := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(localBin, 0755); err != nil {
		t.Fatal(err)
	}
	claudeStub := writeStub(t, localBin, "claude")

	var out bytes.Buffer
	ghostBin, claudeBin, err := checkPrereqs(&out, "claude")
	if err != nil {
		t.Fatalf("checkPrereqs: %v", err)
	}
	if ghostBin != ghostStub {
		t.Errorf("ghost = %q, want %q", ghostBin, ghostStub)
	}
	if claudeBin != claudeStub {
		t.Errorf("claude = %q, want %q", claudeBin, claudeStub)
	}
}

func TestCheckPrereqs_ClaudeMissing(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("GHOST_EMBEDDING_ENABLED", "false")
	orig := systemBinDirs
	systemBinDirs = nil
	t.Cleanup(func() { systemBinDirs = orig })
	binDir := filepath.Join(home, "bin")
	if err := os.MkdirAll(binDir, 0755); err != nil {
		t.Fatal(err)
	}
	writeStub(t, binDir, "ghost")
	t.Setenv("PATH", binDir)

	var out bytes.Buffer
	if _, _, err := checkPrereqs(&out, "claude"); err == nil {
		t.Error("expected error when claude is missing from PATH and common dirs")
	}
	if _, _, err := checkPrereqs(&out, "opencode"); err != nil {
		t.Errorf("opencode target should require only ghost: %v", err)
	}
}
