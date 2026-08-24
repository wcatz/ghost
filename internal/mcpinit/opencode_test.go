package mcpinit

import (
	"bytes"

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
	home, xdg := setupOpencodeTestEnv(t)
	want := renderOpencodeGhostPlugin(filepath.Join(home, "bin", "ghost"))

	var out bytes.Buffer
	if err := RunOpencode(&out, false); err != nil {
		t.Fatalf("RunOpencode: %v", err)
	}

	// Plugin-only integration: the single artifact registers MCP (config
	// hook) and bridges stop events. opencode's own config file is never
	// touched — no jsonc rewriting, no comment loss.
	cfgPath := filepath.Join(xdg, "opencode", "opencode.json")
	if _, err := os.Stat(cfgPath); !os.IsNotExist(err) {
		t.Error("RunOpencode must not write opencode.json")
	}
	if _, err := os.Stat(filepath.Join(xdg, "opencode", "opencode.jsonc")); !os.IsNotExist(err) {
		t.Error("RunOpencode must not write opencode.jsonc")
	}
	pluginPath := filepath.Join(xdg, "opencode", "plugins", "ghost-opencode.ts")
	data, err := os.ReadFile(pluginPath)
	if err != nil {
		t.Fatalf("expected plugin installed at %s: %v", pluginPath, err)
	}
	if string(data) != want {
		t.Error("installed plugin should match the rendered embedded source (binary path baked)")
	}
	if !strings.Contains(string(data), `const GHOST_BIN_DEFAULT = "`+filepath.Join(home, "bin", "ghost")+`"`) {
		t.Error("plugin default must be the resolved absolute binary path")
	}

	output := out.String()
	if !strings.Contains(output, "+ installed lifecycle plugin") {
		t.Errorf("expected installation message, got:\n%s", output)
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

// TestRunOpencode_Idempotent verifies a second run changes nothing on disk
// beyond what the first wrote.
func TestRunOpencode_Idempotent(t *testing.T) {
	_, xdg := setupOpencodeTestEnv(t)
	pluginPath := filepath.Join(xdg, "opencode", "plugins", "ghost-opencode.ts")

	var out1 bytes.Buffer
	if err := RunOpencode(&out1, false); err != nil {
		t.Fatalf("RunOpencode (first): %v", err)
	}
	first, err := os.ReadFile(pluginPath)
	if err != nil {
		t.Fatalf("read plugin: %v", err)
	}

	var out2 bytes.Buffer
	if err := RunOpencode(&out2, false); err != nil {
		t.Fatalf("RunOpencode (second): %v", err)
	}
	if !strings.Contains(out2.String(), "already installed") {
		t.Errorf("second run should report 'already installed', got:\n%s", out2.String())
	}
	second, err := os.ReadFile(pluginPath)
	if err != nil {
		t.Fatalf("re-read plugin: %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Error("second run must not alter an identical plugin file")
	}
	if _, err := os.Stat(filepath.Join(xdg, "opencode", "opencode.json")); !os.IsNotExist(err) {
		t.Error("no opencode.json should exist after idempotent runs")
	}
}

func TestRunOpencode_DryRunWritesNothing(t *testing.T) {
	_, xdg := setupOpencodeTestEnv(t)

	var out bytes.Buffer
	if err := RunOpencode(&out, true); err != nil {
		t.Fatalf("RunOpencode dry run: %v", err)
	}

	output := out.String()
	if !strings.Contains(output, "would install lifecycle plugin") {
		t.Errorf("dry run should say 'would install lifecycle plugin', got:\n%s", output)
	}
	if _, err := os.Stat(filepath.Join(xdg, "opencode", "opencode.json")); err == nil {
		t.Error("dry run must not write the opencode config")
	}
	if _, err := os.Stat(filepath.Join(xdg, "opencode", "plugins", "ghost-opencode.ts")); err == nil {
		t.Error("dry run must not write the lifecycle plugin")
	}
}

// TestRunOpencode_InstallsLifecyclePlugin verifies init writes the embedded
// adapter to <config>/opencode/plugins/ and that a second run is a no-op
// reporting "already installed".
func TestRunOpencode_InstallsLifecyclePlugin(t *testing.T) {
	home, xdg := setupOpencodeTestEnv(t)
	want := renderOpencodeGhostPlugin(filepath.Join(home, "bin", "ghost"))
	pluginPath := filepath.Join(xdg, "opencode", "plugins", "ghost-opencode.ts")

	var out bytes.Buffer
	if err := RunOpencode(&out, false); err != nil {
		t.Fatalf("RunOpencode (first): %v", err)
	}
	data, err := os.ReadFile(pluginPath)
	if err != nil {
		t.Fatalf("expected plugin installed at %s: %v", pluginPath, err)
	}
	if string(data) != want {
		t.Error("installed plugin should match the rendered embedded source")
	}
	if !strings.Contains(out.String(), "+ installed lifecycle plugin") {
		t.Errorf("first run should report installation, got:\n%s", out.String())
	}

	var out2 bytes.Buffer
	if err := RunOpencode(&out2, false); err != nil {
		t.Fatalf("RunOpencode (second): %v", err)
	}
	if !strings.Contains(out2.String(), "already installed") {
		t.Errorf("second run should report 'already installed', got:\n%s", out2.String())
	}
	again, err := os.ReadFile(pluginPath)
	if err != nil {
		t.Fatalf("re-read plugin: %v", err)
	}
	if string(again) != want {
		t.Error("second run must not alter an identical plugin file")
	}
}

// TestRunOpencode_PluginDriftRestored verifies a drifted or outdated plugin
// file is overwritten with the embedded source by the next init.
func TestRunOpencode_PluginDriftRestored(t *testing.T) {
	home, xdg := setupOpencodeTestEnv(t)
	want := renderOpencodeGhostPlugin(filepath.Join(home, "bin", "ghost"))
	pluginPath := filepath.Join(xdg, "opencode", "plugins", "ghost-opencode.ts")

	var first bytes.Buffer
	if err := RunOpencode(&first, false); err != nil {
		t.Fatalf("RunOpencode (install): %v", err)
	}
	if err := os.WriteFile(pluginPath, []byte("// ghost-opencode v0 — stale"), 0644); err != nil {
		t.Fatalf("clobber plugin: %v", err)
	}

	var second bytes.Buffer
	if err := RunOpencode(&second, false); err != nil {
		t.Fatalf("RunOpencode (repair): %v", err)
	}
	data, err := os.ReadFile(pluginPath)
	if err != nil {
		t.Fatalf("re-read plugin: %v", err)
	}
	if string(data) != want {
		t.Error("drifted plugin should be restored to the rendered embedded source")
	}
	if !strings.Contains(second.String(), "+ installed lifecycle plugin") {
		t.Errorf("drift repair should report reinstallation, got:\n%s", second.String())
	}
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
