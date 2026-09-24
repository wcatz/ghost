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
	unsetEnvVars(t, []string{"GHOST_EMBEDDING_ENABLED"})

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
	// The lifecycle phase bound must be finite by default: the stop hook keeps
	// one per-project PID file keyed to the parent's liveness, so an unbounded
	// hang would wedge auto-consolidation for that project until manual
	// intervention.
	if cfg.Reflection.LifecycleTimeoutMinutes != 60 {
		t.Errorf("expected reflection.lifecycle_timeout_minutes=60, got %d", cfg.Reflection.LifecycleTimeoutMinutes)
	}
	// A single consolidation call gets its own, longer bound: the hardcoded 3
	// minutes it replaced was hit by large projects, and a kill there is fatal
	// on the autonomous --require-llm path.
	if cfg.Reflection.ConsolidationTimeoutMinutes != 10 {
		t.Errorf("expected reflection.consolidation_timeout_minutes=10, got %d", cfg.Reflection.ConsolidationTimeoutMinutes)
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

func TestLoad_RoutingDefaultProjectEnv(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	t.Setenv("XDG_CONFIG_HOME", tmpDir)
	unsetEnvVars(t, []string{"GHOST_ROUTING_DEFAULT_PROJECT"})

	t.Setenv("GHOST_ROUTING_DEFAULT_PROJECT", "infrastructure")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	if cfg.Routing.DefaultProject != "infrastructure" {
		t.Errorf("expected routing.default_project=infrastructure from env, got %q", cfg.Routing.DefaultProject)
	}
}

func TestLoad_DefaultProjectFromYAML(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	t.Setenv("XDG_CONFIG_HOME", tmpDir)
	unsetEnvVars(t, []string{"GHOST_ROUTING_DEFAULT_PROJECT"})

	cfgDir := filepath.Join(tmpDir, "ghost")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	yamlCfg := "routing:\n  default_project: infrastructure\n"
	if err := os.WriteFile(filepath.Join(cfgDir, "config.yaml"), []byte(yamlCfg), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	if cfg.Routing.DefaultProject != "infrastructure" {
		t.Errorf("expected routing.default_project=infrastructure from yaml, got %q", cfg.Routing.DefaultProject)
	}
}

func TestLoad_DefaultProjectEmptyByDefault(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	t.Setenv("XDG_CONFIG_HOME", tmpDir)
	unsetEnvVars(t, []string{"GHOST_ROUTING_DEFAULT_PROJECT"})

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	if cfg.Routing.DefaultProject != "" {
		t.Errorf("expected routing.default_project empty by default, got %q", cfg.Routing.DefaultProject)
	}
}

func TestLoad_GhostEnvOverrides(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	t.Setenv("XDG_CONFIG_HOME", tmpDir)

	// Clear any interfering env vars.
	unsetEnvVars(t, []string{"GHOST_EMBEDDING_MODEL"})

	// Set GHOST_* overrides.
	t.Setenv("GHOST_EMBEDDING_MODEL", "custom-embed-model")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	if cfg.Embedding.Model != "custom-embed-model" {
		t.Errorf("expected embedding.model=custom-embed-model from env, got %q", cfg.Embedding.Model)
	}
}

func TestLoad_ExplicitEnvOverrides(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	t.Setenv("XDG_CONFIG_HOME", tmpDir)

	// Clear interfering env vars.
	unsetEnvVars(t, []string{"GHOST_LINKING_ENABLED"})

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

func TestLoad_OllamaURLEnvOverride(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	t.Setenv("XDG_CONFIG_HOME", tmpDir)

	// The generic _ → . transformer would map this to embedding.ollama.url,
	// missing the embedding.ollama_url key — the explicit override must catch it.
	t.Setenv("GHOST_OLLAMA_URL", "http://10.0.2.2:11434")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	if cfg.Embedding.OllamaURL != "http://10.0.2.2:11434" {
		t.Errorf("embedding.ollama_url = %q, want %q (explicit env override)", cfg.Embedding.OllamaURL, "http://10.0.2.2:11434")
	}
}

func TestLoad_YAMLFileOverride(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	t.Setenv("XDG_CONFIG_HOME", tmpDir)

	// Clear interfering env vars.
	unsetEnvVars(t, []string{"GHOST_EMBEDDING_MODEL"})

	// Create a config file in the user config dir.
	configDir := filepath.Join(tmpDir, "ghost")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	configFile := filepath.Join(configDir, "config.yaml")
	yamlContent := `
embedding:
  model: "custom-embed-model"
linking:
  threshold: 0.85
  demotion_threshold: 0.95
reflection:
  auto_resolve: true
  lifecycle_timeout_minutes: 30
  consolidation_timeout_minutes: 7
`
	if err := os.WriteFile(configFile, []byte(yamlContent), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	if cfg.Embedding.Model != "custom-embed-model" {
		t.Errorf("embedding.model = %q, want %q", cfg.Embedding.Model, "custom-embed-model")
	}
	if cfg.Linking.Threshold != 0.85 {
		t.Errorf("linking.threshold = %f, want 0.85", cfg.Linking.Threshold)
	}
	if cfg.Linking.DemotionThreshold != 0.95 {
		t.Errorf("linking.demotion_threshold = %f, want 0.95", cfg.Linking.DemotionThreshold)
	}
	if !cfg.Reflection.AutoResolve {
		t.Errorf("reflection.auto_resolve = %v, want true", cfg.Reflection.AutoResolve)
	}
	if cfg.Reflection.LifecycleTimeoutMinutes != 30 {
		t.Errorf("reflection.lifecycle_timeout_minutes = %d, want 30", cfg.Reflection.LifecycleTimeoutMinutes)
	}
	if cfg.Reflection.ConsolidationTimeoutMinutes != 7 {
		t.Errorf("reflection.consolidation_timeout_minutes = %d, want 7", cfg.Reflection.ConsolidationTimeoutMinutes)
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
	unsetEnvVars(t, []string{"GHOST_EMBEDDING_MODEL", "GHOST_CLI_MODEL_REFLECT", "GHOST_CLI_MODEL_RESOLVE", "GHOST_CLI_MODEL_SUPERSEDE"})

	// YAML file sets embedding.model to "yaml-model" and the phase pin to
	// "yaml-model".
	configDir := filepath.Join(tmpDir, "ghost")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "config.yaml"), []byte("embedding:\n  model: yaml-model\ncli:\n  model_resolve: yaml-model\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Env vars override to "env-model".
	t.Setenv("GHOST_EMBEDDING_MODEL", "env-model")
	t.Setenv("GHOST_CLI_MODEL_RESOLVE", "env-model")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	// Env should take precedence over YAML.
	if cfg.Embedding.Model != "env-model" {
		t.Errorf("embedding.model = %q, want %q (env should override yaml)", cfg.Embedding.Model, "env-model")
	}
	// The generic _ → . transformer would map this to cli.model.resolve,
	// missing the cli.model_resolve key — the explicit override must catch it.
	if cfg.CLI.ModelResolve != "env-model" {
		t.Errorf("cli.model_resolve = %q, want %q (explicit env override should override yaml)", cfg.CLI.ModelResolve, "env-model")
	}
}

func TestLoad_CLIPhaseModelsFromYAML(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	t.Setenv("XDG_CONFIG_HOME", tmpDir)
	// Phase model pins now have explicit GHOST_CLI_MODEL_* env overrides, so a
	// host-exported value would outrank the YAML under test.
	unsetEnvVars(t, []string{"GHOST_CLI_MODEL_REFLECT", "GHOST_CLI_MODEL_RESOLVE", "GHOST_CLI_MODEL_SUPERSEDE"})

	cfgDir := filepath.Join(tmpDir, "ghost")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	yamlCfg := "cli:\n  model_reflect: opencode/big-pickle\n  model_resolve: opencode/big-pickle\n  model_supersede: opencode/big-pickle\n"
	if err := os.WriteFile(filepath.Join(cfgDir, "config.yaml"), []byte(yamlCfg), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	if cfg.CLI.ModelReflect != "opencode/big-pickle" {
		t.Errorf("cli.model_reflect = %q, want %q", cfg.CLI.ModelReflect, "opencode/big-pickle")
	}
	if cfg.CLI.ModelResolve != "opencode/big-pickle" {
		t.Errorf("cli.model_resolve = %q, want %q", cfg.CLI.ModelResolve, "opencode/big-pickle")
	}
	if cfg.CLI.ModelSupersede != "opencode/big-pickle" {
		t.Errorf("cli.model_supersede = %q, want %q", cfg.CLI.ModelSupersede, "opencode/big-pickle")
	}
}

func TestLoad_CLIPhaseModelsEmptyByDefault(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	t.Setenv("XDG_CONFIG_HOME", tmpDir)
	unsetEnvVars(t, []string{"GHOST_CLI_MODEL_REFLECT", "GHOST_CLI_MODEL_RESOLVE", "GHOST_CLI_MODEL_SUPERSEDE"})

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	if cfg.CLI.ModelReflect != "" {
		t.Errorf("cli.model_reflect default = %q, want empty (client default)", cfg.CLI.ModelReflect)
	}
	if cfg.CLI.ModelResolve != "" {
		t.Errorf("cli.model_resolve default = %q, want empty (client default)", cfg.CLI.ModelResolve)
	}
	if cfg.CLI.ModelSupersede != "" {
		t.Errorf("cli.model_supersede default = %q, want empty (client default)", cfg.CLI.ModelSupersede)
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

func TestConfigFilePath_DoesNotCreateFile(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmpDir)

	path, err := ConfigFilePath()
	if err != nil {
		t.Fatalf("ConfigFilePath: %v", err)
	}
	if want := filepath.Join(tmpDir, "ghost", "config.yaml"); path != want {
		t.Errorf("ConfigFilePath = %q, want %q", path, want)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("ConfigFilePath must not create the file, stat err = %v", err)
	}
}

// TestDataDirPath_NoCreate pins the split the failure marker relies on:
// DataDirPath returns the same location DataDir creates, but without ever
// creating it — so the stop hook's no-LLM skip can check for an existing
// store and leave no phantom directory when there is none.
func TestDataDirPath_NoCreate(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", tmpDir)

	path, err := DataDirPath()
	if err != nil {
		t.Fatalf("DataDirPath() error: %v", err)
	}
	expected := filepath.Join(tmpDir, "ghost")
	if path != expected {
		t.Errorf("expected %q, got %q", expected, path)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("DataDirPath must not create the directory, stat err = %v", err)
	}
	// DataDir creates the same path it returned.
	if _, err := DataDir(); err != nil {
		t.Fatalf("DataDir() error: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("DataDir must create the DataDirPath path: %v", err)
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
	if cfg.Linking.DemotionThreshold != 0.90 {
		t.Errorf("expected linking.demotion_threshold=0.90, got %f", cfg.Linking.DemotionThreshold)
	}
}

func TestSearchDefaults(t *testing.T) {
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if cfg.Search.MinSimilarity != 0.0 {
		t.Errorf("expected search.min_similarity=0.0, got %f", cfg.Search.MinSimilarity)
	}
}

func TestObsidianDefaults(t *testing.T) {
	// Isolate from the host: a real platform Ghost user config or a
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

func TestInjectionConfigDefaults(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	t.Setenv("XDG_CONFIG_HOME", tmpDir)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Injection.BehaviorFloor != 8 {
		t.Errorf("injection.behavior_floor = %d, want 8", cfg.Injection.BehaviorFloor)
	}
	want := map[string]bool{"gotcha": true, "convention": true, "preference": true, "decision": true}
	if len(cfg.Injection.BehaviorCategories) != len(want) {
		t.Errorf("behavior_categories = %v, want 4 entries", cfg.Injection.BehaviorCategories)
		return
	}
	for _, c := range cfg.Injection.BehaviorCategories {
		if !want[c] {
			t.Errorf("unexpected behavior category %q", c)
		}
	}
	if cfg.Injection.CategoryWeights != nil {
		t.Errorf("category_weights default should be nil, got %v", cfg.Injection.CategoryWeights)
	}
	if cfg.Injection.CategoryCaps["gotcha"] != 4 {
		t.Errorf("category_caps[gotcha] = %d, want 4", cfg.Injection.CategoryCaps["gotcha"])
	}
}

func TestScratchDefaults(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	t.Setenv("XDG_CONFIG_HOME", tmpDir)
	unsetEnvVars(t, []string{"GHOST_SCRATCH_MAX_BYTES"})

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Scratch.MaxBytes != 512*1024*1024 {
		t.Errorf("scratch.max_bytes = %d, want 536870912 (512 MiB default)", cfg.Scratch.MaxBytes)
	}
}

func TestScratchMaxBytesFromYAML(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	t.Setenv("XDG_CONFIG_HOME", tmpDir)
	unsetEnvVars(t, []string{"GHOST_SCRATCH_MAX_BYTES"})

	cfgDir := filepath.Join(tmpDir, "ghost")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "config.yaml"), []byte("scratch:\n  max_bytes: 1024\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Scratch.MaxBytes != 1024 {
		t.Errorf("scratch.max_bytes = %d, want 1024 (yaml)", cfg.Scratch.MaxBytes)
	}
}

// TestScratchMaxBytesZeroDisables pins the 0-semantics: an explicit 0 in the
// config file must override the compiled 512 MiB default (opt-out), distinct
// from "unset" which keeps the default.
func TestScratchMaxBytesZeroDisables(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	t.Setenv("XDG_CONFIG_HOME", tmpDir)
	unsetEnvVars(t, []string{"GHOST_SCRATCH_MAX_BYTES"})

	cfgDir := filepath.Join(tmpDir, "ghost")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "config.yaml"), []byte("scratch:\n  max_bytes: 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Scratch.MaxBytes != 0 {
		t.Errorf("scratch.max_bytes = %d, want 0 (explicit opt-out must beat the default)", cfg.Scratch.MaxBytes)
	}
}

// TestScratchMaxBytesEnvOverride: GHOST_SCRATCH_MAX_BYTES must map to
// scratch.max_bytes — the generic GHOST_* _→. transformer would produce
// scratch.max.bytes and miss, so the explicit envOverrides entry is required.
func TestScratchMaxBytesEnvOverride(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	t.Setenv("XDG_CONFIG_HOME", tmpDir)
	t.Setenv("GHOST_SCRATCH_MAX_BYTES", "2048")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Scratch.MaxBytes != 2048 {
		t.Errorf("scratch.max_bytes = %d, want 2048 (env override)", cfg.Scratch.MaxBytes)
	}
}
