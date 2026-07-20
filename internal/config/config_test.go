package config

import (
	"os"
	"path/filepath"
	"testing"
)

// unsetEnvVars unsets the given env vars for the duration of the test,
// restoring original values on cleanup.
func unsetEnvVars(t *testing.T, keys []string) {
	t.Helper()
	for _, key := range keys {
		if old, ok := os.LookupEnv(key); ok {
			t.Setenv(key, old) // saves original, will restore on cleanup
			if err := os.Unsetenv(key); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestLoad_Defaults(t *testing.T) {
	// Isolate from real config files by pointing HOME/XDG to temp dir.
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	t.Setenv("XDG_CONFIG_HOME", tmpDir)

	// Unset any env vars that could interfere.
	unsetEnvVars(t, []string{"GHOST_API_KEY", "GHOST_EMBEDDING_ENABLED", "ANTHROPIC_API_KEY"})

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	// Verify compiled defaults are applied.
	if !cfg.Embedding.Enabled {
		t.Error("expected embedding.enabled=true")
	}
	if cfg.Embedding.Dimensions != 768 {
		t.Errorf("expected embedding.dimensions=768, got %d", cfg.Embedding.Dimensions)
	}
	// API key should be empty when no env or config files provide it.
	if cfg.API.Key != "" {
		t.Errorf("expected empty api.key, got %q", cfg.API.Key)
	}
}

func TestLoad_AnthropicAPIKey(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-test-key-12345")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	if cfg.API.Key != "sk-test-key-12345" {
		t.Errorf("expected api.key=sk-test-key-12345, got %q", cfg.API.Key)
	}
}

func TestDataDir_WithXDGDataHome(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", tmpDir)

	dir, err := DataDir()
	if err != nil {
		t.Fatalf("DataDir() error: %v", err)
	}

	expected := filepath.Join(tmpDir, "ghost")
	if dir != expected {
		t.Errorf("expected %q, got %q", expected, dir)
	}

	// Verify the directory was created.
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("directory was not created: %v", err)
	}
	if !info.IsDir() {
		t.Fatal("expected a directory")
	}
}

func TestLoad_GhostEnvOverrides(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	t.Setenv("XDG_CONFIG_HOME", tmpDir)

	// Clear any interfering env vars.
	unsetEnvVars(t, []string{"GHOST_API_KEY", "GHOST_REFLECTION_BACKEND", "ANTHROPIC_API_KEY"})

	// Set GHOST_* overrides.
	t.Setenv("GHOST_API_KEY", "sk-ghost-override")
	t.Setenv("GHOST_REFLECTION_BACKEND", "sqlite")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	if cfg.API.Key != "sk-ghost-override" {
		t.Errorf("expected api.key from GHOST_API_KEY, got %q", cfg.API.Key)
	}
	if cfg.Reflection.Backend != "sqlite" {
		t.Errorf("expected reflection.backend=sqlite from env, got %q", cfg.Reflection.Backend)
	}
}

func TestLoad_ExplicitEnvOverrides(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	t.Setenv("XDG_CONFIG_HOME", tmpDir)

	// Clear interfering env vars.
	unsetEnvVars(t, []string{"GHOST_API_KEY", "GHOST_LINKING_ENABLED", "ANTHROPIC_API_KEY"})

	t.Setenv("GHOST_LINKING_ENABLED", "false")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	if cfg.Linking.Enabled {
		t.Error("expected linking.enabled=false from env override")
	}
}

func TestLoad_ObsidianVaultDirEnvOverride(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	t.Setenv("XDG_CONFIG_HOME", tmpDir)

	// Clear interfering env vars.
	unsetEnvVars(t, []string{"GHOST_API_KEY", "ANTHROPIC_API_KEY"})

	// The generic _ → . transformer would map this to obsidian.vault.dir,
	// missing the obsidian.vault_dir key — the explicit override must catch it.
	t.Setenv("GHOST_OBSIDIAN_VAULT_DIR", "/vaults/ghost")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	if cfg.Obsidian.VaultDir != "/vaults/ghost" {
		t.Errorf("obsidian.vault_dir = %q, want %q (explicit env override)", cfg.Obsidian.VaultDir, "/vaults/ghost")
	}
}

func TestLoad_YAMLFileOverride(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	t.Setenv("XDG_CONFIG_HOME", tmpDir)

	// Clear interfering env vars.
	unsetEnvVars(t, []string{"GHOST_API_KEY", "GHOST_REFLECTION_BACKEND", "ANTHROPIC_API_KEY"})

	// Create a config file in the user config dir.
	configDir := filepath.Join(tmpDir, "ghost")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	configFile := filepath.Join(configDir, "config.yaml")
	yamlContent := `
api:
  key: "sk-from-yaml"
embedding:
  model: "custom-embed-model"
linking:
  threshold: 0.85
reflection:
  backend: "sqlite"
`
	if err := os.WriteFile(configFile, []byte(yamlContent), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	if cfg.API.Key != "sk-from-yaml" {
		t.Errorf("api.key = %q, want %q", cfg.API.Key, "sk-from-yaml")
	}
	if cfg.Embedding.Model != "custom-embed-model" {
		t.Errorf("embedding.model = %q, want %q", cfg.Embedding.Model, "custom-embed-model")
	}
	if cfg.Linking.Threshold != 0.85 {
		t.Errorf("linking.threshold = %f, want 0.85", cfg.Linking.Threshold)
	}
	if cfg.Reflection.Backend != "sqlite" {
		t.Errorf("reflection.backend = %q, want %q", cfg.Reflection.Backend, "sqlite")
	}

	// Unaffected defaults should remain.
	if !cfg.Embedding.Enabled {
		t.Error("embedding.enabled should still be true (default)")
	}
}

func TestLoad_EnvOverridesYAML(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	t.Setenv("XDG_CONFIG_HOME", tmpDir)

	// Clear interfering env vars.
	unsetEnvVars(t, []string{"GHOST_API_KEY", "GHOST_REFLECTION_BACKEND", "ANTHROPIC_API_KEY"})

	// YAML file sets backend to "haiku".
	configDir := filepath.Join(tmpDir, "ghost")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "config.yaml"), []byte("reflection:\n  backend: haiku\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Env var overrides to "sqlite".
	t.Setenv("GHOST_REFLECTION_BACKEND", "sqlite")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	// Env should take precedence over YAML.
	if cfg.Reflection.Backend != "sqlite" {
		t.Errorf("backend = %q, want %q (env should override yaml)", cfg.Reflection.Backend, "sqlite")
	}
}

func TestEnsureConfigFile(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmpDir)

	// First call should create.
	path, created, err := EnsureConfigFile()
	if err != nil {
		t.Fatalf("EnsureConfigFile: %v", err)
	}
	if !created {
		t.Error("expected file to be created on first call")
	}

	// File should exist.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("config file not created: %v", err)
	}
	if info.Size() == 0 {
		t.Error("config file should not be empty")
	}

	// Second call should not re-create.
	_, created2, err := EnsureConfigFile()
	if err != nil {
		t.Fatalf("EnsureConfigFile (2nd): %v", err)
	}
	if created2 {
		t.Error("should not create again on second call")
	}
}

func TestDataDir_DefaultFallback(t *testing.T) {
	// Unset XDG_DATA_HOME to test the fallback to ~/.local/share.
	t.Setenv("XDG_DATA_HOME", "")
	unsetEnvVars(t, []string{"XDG_DATA_HOME"})

	dir, err := DataDir()
	if err != nil {
		t.Fatalf("DataDir() error: %v", err)
	}

	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("UserHomeDir() error: %v", err)
	}
	expected := filepath.Join(home, ".local", "share", "ghost")
	if dir != expected {
		t.Errorf("expected %q, got %q", expected, dir)
	}
}

func TestLinkingDefaults(t *testing.T) {
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if !cfg.Linking.Enabled {
		t.Error("expected linking.enabled=true by default")
	}
	if cfg.Linking.Threshold != 0.70 {
		t.Errorf("expected linking.threshold=0.70, got %f", cfg.Linking.Threshold)
	}
}

func TestObsidianDefaults(t *testing.T) {
	// Isolate from the host: a real ~/.config/ghost/config.yaml or a
	// GHOST_OBSIDIAN_* var in the environment would otherwise skew defaults.
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	t.Setenv("XDG_CONFIG_HOME", tmpDir)
	// t.Setenv can't unset, and the env provider has no empty-value guard, so
	// clearing to "" would itself override the default. Save and restore.
	for _, key := range []string{"GHOST_OBSIDIAN_VAULT_DIR", "GHOST_OBSIDIAN_INTERVAL"} {
		if old, ok := os.LookupEnv(key); ok {
			_ = os.Unsetenv(key)
			t.Cleanup(func() { _ = os.Setenv(key, old) })
		}
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Obsidian.VaultDir != "" {
		t.Errorf("VaultDir default = %q, want empty (resolved at use time)", cfg.Obsidian.VaultDir)
	}
	if cfg.Obsidian.Interval != "30s" {
		t.Errorf("Interval default = %q, want 30s", cfg.Obsidian.Interval)
	}
	if cfg.Obsidian.AutoSync {
		t.Error("AutoSync default = true, want false (opt-in only)")
	}
}
