package config

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/knadh/koanf/parsers/yaml"
	"github.com/knadh/koanf/providers/confmap"
	"github.com/knadh/koanf/v2"
)

// malformedYAML is what a human typo produces: an unterminated quoted scalar.
const malformedYAML = "embedding:\n  model: \"nomic-embed-text\n"

// isolateConfig points HOME and XDG_CONFIG_HOME at a temp dir and clears every
// GHOST_* variable in the environment, so neither a real config file nor a
// host-exported variable can reach the assertions. Both sources are used: the
// names derived from the two env layers, so a variable added later is covered
// the day it is added, and the ambient environment, so a host that exports
// something this package has never heard of cannot skew a default either.
//
// The system-wide layer is the one input this does NOT isolate:
// /etc/ghost/config.yaml is a real file on a machine that installed Ghost
// system-wide, and the loader takes its path from a constant with no override,
// so no test can point it elsewhere. A machine with that file installed
// system-wide runs these tests against it.
func isolateConfig(t *testing.T) {
	t.Helper()
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	t.Setenv("XDG_CONFIG_HOME", tmpDir)
	unsetEnvVars(t, configEnvVarNames())
}

// configEnvVarNames lists every GHOST_* variable in the environment plus every
// name the two env layers can read: the explicit shortcuts, and the name koanf's
// generic GHOST_ prefix + "_"→"." transformer derives for each bound key.
func configEnvVarNames() []string {
	names := make([]string, 0, len(envOverrides)+len(knownKeys))
	for _, ov := range envOverrides {
		names = append(names, ov.env)
	}
	for key := range knownKeys {
		names = append(names, "GHOST_"+strings.ToUpper(strings.ReplaceAll(key, ".", "_")))
	}
	for _, kv := range os.Environ() {
		if name, _, ok := strings.Cut(kv, "="); ok && strings.HasPrefix(name, "GHOST_") {
			names = append(names, name)
		}
	}
	return names
}

// writeUserConfig writes content to the exact user config path Load() reads
// (XDG_CONFIG_HOME/ghost/config.yaml) and returns that path.
func writeUserConfig(t *testing.T, content string) string {
	t.Helper()
	dir := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "ghost")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// captureConfigWarnings redirects config warnings to a buffer for the duration
// of the test, so a test can assert on what a user would see. It also clears
// the per-process dedup set, which warnf consults: without that, a second test
// asserting the same warning would see nothing because the first one already
// spent it.
func captureConfigWarnings(t *testing.T) *bytes.Buffer {
	t.Helper()
	resetWarned()
	var buf bytes.Buffer
	restore := SetWarningWriter(&buf)
	t.Cleanup(func() {
		restore()
		resetWarned()
	})
	return &buf
}

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

// TestLoad_MalformedYAMLIsAnError pins that a config file which exists but does
// not parse is reported rather than swallowed. Before this, k.Load's error was
// discarded, so a typo left every key at its compiled default with nothing to
// explain why the user's settings were having no effect.
func TestLoad_MalformedYAMLIsAnError(t *testing.T) {
	isolateConfig(t)
	path := writeUserConfig(t, malformedYAML)

	cfg, err := Load()
	if err == nil {
		t.Fatalf("Load() = %+v, nil; a malformed config must be an error, not a silent fallback to defaults", cfg)
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error %q must name the offending file %q", err, path)
	}
}

// TestLoadConfigFile covers the same failure on the layer Load cannot reach
// from a test (/etc/ghost/config.yaml is not writable), plus the case that must
// stay silent: a file layer that is simply absent.
func TestLoadFileIfExists(t *testing.T) {
	parser := yaml.Parser()

	present := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(present, []byte(malformedYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := loadConfigFile(koanf.New("."), present, parser); err == nil {
		t.Error("loadConfigFile accepted a malformed file; the parse error must propagate")
	} else if !strings.Contains(err.Error(), present) {
		t.Errorf("error %q must name %q", err, present)
	}

	absent := filepath.Join(t.TempDir(), "config.yaml")
	if err := loadConfigFile(koanf.New("."), absent, parser); err != nil {
		t.Errorf("loadFileIfExists on an absent file = %v, want nil (the layer is optional)", err)
	}
}

// TestLoadForHook_MalformedYAMLFallsBackWithWarning is the host-session half of
// the same problem: the SessionStart hook runs inside someone else's editor and
// must not fail their session over a typo, but must still say so.
func TestLoadForHook_MalformedYAMLFallsBackWithWarning(t *testing.T) {
	isolateConfig(t)
	path := writeUserConfig(t, malformedYAML)
	warnings := captureConfigWarnings(t)

	cfg := LoadForHook()
	if cfg == nil {
		t.Fatal("LoadForHook() = nil; the hook path must always be given a config")
	}
	if !cfg.Embedding.Enabled {
		t.Error("embedding.enabled = false, want the compiled default true")
	}
	if cfg.Linking.DemotionThreshold != 0.90 {
		t.Errorf("linking.demotion_threshold = %f, want the compiled default 0.90", cfg.Linking.DemotionThreshold)
	}
	if cfg.Injection.BehaviorFloor != 8 {
		t.Errorf("injection.behavior_floor = %d, want the compiled default 8", cfg.Injection.BehaviorFloor)
	}
	if got := warnings.String(); !strings.Contains(got, path) {
		t.Errorf("warning %q must name the file that failed to parse (%q)", got, path)
	}
}

// TestLoadForHook_ValidConfigLoadsWithoutWarning is the other half of the hook
// contract: the fallback must not fire (or warn) on a config that loads fine.
func TestLoadForHook_ValidConfigLoadsWithoutWarning(t *testing.T) {
	isolateConfig(t)
	writeUserConfig(t, "linking:\n  demotion_threshold: 0.42\n")
	warnings := captureConfigWarnings(t)

	cfg := LoadForHook()
	if cfg.Linking.DemotionThreshold != 0.42 {
		t.Errorf("linking.demotion_threshold = %f, want 0.42 (a valid config must still load)", cfg.Linking.DemotionThreshold)
	}
	if got := warnings.String(); got != "" {
		t.Errorf("LoadForHook() warned on a valid config: %q", got)
	}
}

// TestLoad_UnknownKeyWarns pins that a key no Config field binds is reported.
// A typo like linking.thresholdd otherwise does nothing at all, silently.
func TestLoad_UnknownKeyWarns(t *testing.T) {
	isolateConfig(t)
	path := writeUserConfig(t, "linking:\n  thresholdd: 0.9\n  threshold: 0.85\n")
	warnings := captureConfigWarnings(t)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if cfg.Linking.Threshold != 0.85 {
		t.Errorf("linking.threshold = %f, want 0.85 (the valid sibling key must still load)", cfg.Linking.Threshold)
	}
	got := warnings.String()
	if !strings.Contains(got, "linking.thresholdd") {
		t.Errorf("warning %q must name the unknown key linking.thresholdd", got)
	}
	if !strings.Contains(got, path) {
		t.Errorf("warning %q must name the file it came from (%q)", got, path)
	}
}

// TestFallbackConfig_KeepsEnvLayer pins that the fallback is every layer that
// does not read a config file — the compiled defaults AND the GHOST_*
// environment — not the compiled defaults alone. Dropping the env layer would
// be a regression, not a narrowing: before, a parse error was swallowed and the
// load carried on to the env layer, so a deployment that opts out with
// GHOST_EMBEDDING_ENABLED=false (or disables the scratch budget with
// GHOST_SCRATCH_MAX_BYTES=0) kept that opt-out even with a broken file beside
// it. Returning defaults alone would silently re-enable exactly what the
// operator turned off.
func TestFallbackConfig_KeepsEnvLayer(t *testing.T) {
	isolateConfig(t)
	t.Setenv("GHOST_EMBEDDING_ENABLED", "false")
	t.Setenv("GHOST_LINKING_DEMOTION_THRESHOLD", "0.42")
	t.Setenv("GHOST_INJECTION_BEHAVIOR_FLOOR", "3")
	t.Setenv("GHOST_SCRATCH_MAX_BYTES", "0")
	// A broken file is the whole point: these must still take effect.
	writeUserConfig(t, malformedYAML)
	warnings := captureConfigWarnings(t)

	cfg := LoadForHook()
	if cfg == nil {
		t.Fatal("LoadForHook() = nil")
	}
	if cfg.Embedding.Enabled {
		t.Error("embedding.enabled = true, want the GHOST_EMBEDDING_ENABLED=false opt-out to survive a broken file")
	}
	if cfg.Linking.DemotionThreshold != 0.42 {
		t.Errorf("linking.demotion_threshold = %f, want 0.42 (env layer must survive a broken file)", cfg.Linking.DemotionThreshold)
	}
	if cfg.Injection.BehaviorFloor != 3 {
		t.Errorf("injection.behavior_floor = %d, want 3 (env layer must survive a broken file)", cfg.Injection.BehaviorFloor)
	}
	if cfg.Scratch.MaxBytes != 0 {
		t.Errorf("scratch.max_bytes = %d, want 0 (the documented GHOST_SCRATCH_MAX_BYTES=0 opt-out)", cfg.Scratch.MaxBytes)
	}
	// Still the one fallback, still loud: the file problem is still reported.
	if !strings.Contains(warnings.String(), os.Getenv("XDG_CONFIG_HOME")) {
		t.Errorf("warning %q must still name the file that failed to parse", warnings.String())
	}
}

// TestFallbackConfig_BadEnvValueDegrades pins that an unreadable GHOST_* value
// does not take the fallback down with it. The hook path cannot return an
// error, so the bad variable is reported and the rest of the config stands.
func TestFallbackConfig_BadEnvValueDegrades(t *testing.T) {
	isolateConfig(t)
	t.Setenv("GHOST_INJECTION_CATEGORY_WEIGHTS", "gotcha")
	t.Setenv("GHOST_ROUTING_DEFAULT_PROJECT", "infrastructure")
	writeUserConfig(t, malformedYAML)
	warnings := captureConfigWarnings(t)

	cfg := LoadForHook()
	if cfg == nil {
		t.Fatal("LoadForHook() = nil; a bad env value must not take the fallback down")
	}
	if cfg.Routing.DefaultProject != "infrastructure" {
		t.Errorf("routing.default_project = %q, want the env value to survive a bad sibling", cfg.Routing.DefaultProject)
	}
	if !strings.Contains(warnings.String(), "GHOST_INJECTION_CATEGORY_WEIGHTS") {
		t.Errorf("warning %q must name the unreadable variable", warnings.String())
	}
}

// TestDefaultConfig_MatchesTheDefaultsMap pins the hand-written last-resort
// Config against the compiled defaults map. The literal exists for the case
// where even the defaults layer will not decode, which no ordinary run can
// reach — so without this, a key added to the map and forgotten here would only
// ever be discovered on a machine that needed the fallback.
func TestDefaultConfig_MatchesTheDefaultsMap(t *testing.T) {
	k := koanf.New(".")
	if err := k.Load(confmap.Provider(defaults, "."), nil); err != nil {
		t.Fatalf("load defaults: %v", err)
	}
	want := &Config{}
	if err := k.Unmarshal("", want); err != nil {
		t.Fatalf("unmarshal defaults: %v", err)
	}
	if got := defaultConfig(); !reflect.DeepEqual(got, want) {
		t.Errorf("defaultConfig() has drifted from the defaults map:\n got %+v\nwant %+v", got, want)
	}
}

// TestFallbackConfig_NeverPanicsOnDecodeFailure pins that FallbackConfig
// returns the compiled defaults — not a struct of Go zero values — on the paths
// where decodeFallback reports failure. It is the function that exists to absorb
// a broken config, so a panic here, or a server booting with embeddings and
// linking off and an unbounded lifecycle, is the outcome it must not have. The
// defaults map is a literal that normally always decodes, so the last resort is
// reached by breaking that assumption: an unbindable entry makes every decode
// fail, both the real one and the env-free retry.
func TestFallbackConfig_NeverPanicsOnDecodeFailure(t *testing.T) {
	isolateConfig(t)
	writeUserConfig(t, malformedYAML)
	// A value koanf cannot flatten into its key space: every decode of the
	// defaults layer now fails, not just the one carrying the environment.
	defaults["injection.category_caps"] = func() {}
	t.Cleanup(func() {
		defaults["injection.category_caps"] = map[string]interface{}{"gotcha": 4}
	})
	warnings := captureConfigWarnings(t)

	cfg := FallbackConfig() // must not panic
	if cfg == nil {
		t.Fatal("FallbackConfig() = nil")
	}
	// The full default set, not just the two fields a partial fill would set.
	// These are the fields cmd/ghost's own bootstrap test pins for this
	// fallback, because they are what decide whether the server comes up with
	// embeddings and linking on.
	if !cfg.Embedding.Enabled || cfg.Embedding.Dimensions != 768 {
		t.Errorf("embedding = %+v, want the compiled defaults (768 dimensions, enabled)", cfg.Embedding)
	}
	if !cfg.Linking.Enabled || cfg.Linking.Threshold != 0.70 || cfg.Linking.DemotionThreshold != 0.90 {
		t.Errorf("linking = %+v, want the compiled defaults", cfg.Linking)
	}
	if cfg.Reflection.LifecycleTimeoutMinutes != 60 || cfg.Reflection.ConsolidationTimeoutMinutes != 10 {
		t.Errorf("reflection = %+v, want the compiled defaults (60 and 10 minute bounds)", cfg.Reflection)
	}
	if cfg.Obsidian.Interval != "30s" {
		t.Errorf("obsidian.interval = %q, want the compiled default 30s", cfg.Obsidian.Interval)
	}
	if cfg.Scratch.MaxBytes != DefaultScratchMaxBytes {
		t.Errorf("scratch.max_bytes = %d, want %d", cfg.Scratch.MaxBytes, DefaultScratchMaxBytes)
	}
	if !strings.Contains(warnings.String(), "built-in defaults") {
		t.Errorf("warning %q must say the built-in defaults are in use", warnings.String())
	}
}

// TestFallbackConfig_UndecodableGenericEnvValue pins that a GHOST_* value the
// generic provider cannot weakly convert yields the compiled defaults, never a
// near-zero Config. These four keys have no envOverrides entry, so loadEnvLayer
// does not see the failure — it surfaces at the decode, and returning the
// partial Config would start the server with embeddings and linking off, a zero
// demotion threshold, and reflection.lifecycle_timeout_minutes=0: the unbounded
// lifecycle the defaults deliberately avoid.
func TestFallbackConfig_UndecodableGenericEnvValue(t *testing.T) {
	// Only the generic-mapped keys belong here. A key with an envOverrides entry
	// (GHOST_REFLECTION_LIFECYCLE_TIMEOUT_MINUTES, say) is caught earlier, by
	// loadEnvLayer's own parser, and is reported by variable name instead.
	cases := []struct{ envKey, value string }{
		{"GHOST_LINKING_THRESHOLD", "high"},
		{"GHOST_EMBEDDING_DIMENSIONS", "abc"},
		{"GHOST_EMBEDDING_ENABLED", "yes"},
	}
	for _, tc := range cases {
		t.Run(tc.envKey, func(t *testing.T) {
			isolateConfig(t)
			t.Setenv(tc.envKey, tc.value)
			writeUserConfig(t, malformedYAML)
			warnings := captureConfigWarnings(t)

			cfg := LoadForHook()
			if cfg == nil {
				t.Fatal("LoadForHook() = nil")
			}
			// Every value below is one the compiled defaults set. A near-zero
			// Config would fail each of them.
			if !cfg.Embedding.Enabled || cfg.Embedding.Dimensions != 768 {
				t.Errorf("embedding = %+v, want the compiled default (768 dimensions, enabled)", cfg.Embedding)
			}
			if !cfg.Linking.Enabled || cfg.Linking.Threshold != 0.70 || cfg.Linking.DemotionThreshold != 0.90 {
				t.Errorf("linking = %+v, want the compiled defaults", cfg.Linking)
			}
			// The one that matters most: 0 disables the bound entirely.
			if cfg.Reflection.LifecycleTimeoutMinutes != 60 {
				t.Errorf("reflection.lifecycle_timeout_minutes = %d, want the compiled default 60 (0 means unbounded)",
					cfg.Reflection.LifecycleTimeoutMinutes)
			}
			if cfg.Scratch.MaxBytes != 512*1024*1024 {
				t.Errorf("scratch.max_bytes = %d, want the compiled default 512 MiB", cfg.Scratch.MaxBytes)
			}
			// The bad value must be reported, not swallowed, and named by the
			// variable the user actually set.
			if !strings.Contains(warnings.String(), tc.envKey) {
				t.Errorf("warning %q must name the skipped variable %s", warnings.String(), tc.envKey)
			}
		})
	}
}

// TestIsolateConfig_ClearsConfigEnvVars makes the helper's claim testable: a
// host GHOST_* variable must not reach an assertion that a compiled default
// holds. The names are derived from the two env layers, so a variable added
// later is covered the day it is added.
// TestIsolateConfig_ClearsAmbientGhostVars covers the ambient half of the
// helper, which configEnvVarNames' derived list cannot: a name this package has
// no mapping for still reaches koanf's generic env provider, so it belongs in
// the cleared set even though it cannot change a Config field today. Pinning
// the helper's contract is the only way to keep that sweep from being dropped
// as apparently redundant.
func TestIsolateConfig_ClearsAmbientGhostVars(t *testing.T) {
	const unknown = "GHOST_NOT_A_CONFIG_KEY"
	t.Setenv(unknown, "1")

	isolateConfig(t)

	if v, ok := os.LookupEnv(unknown); ok {
		t.Errorf("isolateConfig left %s=%q set; a host GHOST_* export must not reach the load", unknown, v)
	}
}

func TestIsolateConfig_ClearsConfigEnvVars(t *testing.T) {
	for k, v := range map[string]string{
		"GHOST_LINKING_DEMOTION_THRESHOLD":           "0.42",
		"GHOST_INJECTION_BEHAVIOR_FLOOR":             "3",
		"GHOST_INJECTION_CATEGORY_WEIGHTS":           "gotcha=1.2",
		"GHOST_SCRATCH_MAX_BYTES":                    "1024",
		"GHOST_OBSIDIAN_AUTO_SYNC":                   "true",
		"GHOST_EMBEDDING_ENABLED":                    "false",
		"GHOST_ROUTING_DEFAULT_PROJECT":              "infrastructure",
		"GHOST_SEARCH_MIN_SIMILARITY":                "0.9",
		"GHOST_REFLECTION_AUTO_REFLECT":              "true",
		"GHOST_REFLECTION_LIFECYCLE_TIMEOUT_MINUTES": "5",
	} {
		t.Setenv(k, v)
	}

	isolateConfig(t)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	if cfg.Linking.DemotionThreshold != 0.90 {
		t.Errorf("linking.demotion_threshold = %f, want the compiled default 0.90", cfg.Linking.DemotionThreshold)
	}
	if cfg.Injection.BehaviorFloor != 8 {
		t.Errorf("injection.behavior_floor = %d, want the compiled default 8", cfg.Injection.BehaviorFloor)
	}
	if cfg.Injection.CategoryWeights != nil {
		t.Errorf("injection.category_weights = %v, want the compiled default nil", cfg.Injection.CategoryWeights)
	}
	if cfg.Scratch.MaxBytes != 512*1024*1024 {
		t.Errorf("scratch.max_bytes = %d, want the compiled default 512 MiB", cfg.Scratch.MaxBytes)
	}
	if cfg.Obsidian.AutoSync {
		t.Error("obsidian.auto_sync = true, want the compiled default false")
	}
	if !cfg.Embedding.Enabled {
		t.Error("embedding.enabled = false, want the compiled default true")
	}
	if cfg.Routing.DefaultProject != "" {
		t.Errorf("routing.default_project = %q, want the compiled default empty", cfg.Routing.DefaultProject)
	}
	if cfg.Search.MinSimilarity != 0 {
		t.Errorf("search.min_similarity = %f, want the compiled default 0", cfg.Search.MinSimilarity)
	}
	if cfg.Reflection.AutoReflect {
		t.Error("reflection.auto_reflect = true, want the compiled default false")
	}
	if cfg.Reflection.LifecycleTimeoutMinutes != 60 {
		t.Errorf("reflection.lifecycle_timeout_minutes = %d, want the compiled default 60", cfg.Reflection.LifecycleTimeoutMinutes)
	}
}

// TestSetWarningWriter_RedirectsWarnings pins that config warnings can be sent
// somewhere other than raw stderr. The MCP server needs that: its stderr belongs
// to the client protocol, so `ghost mcp` points the sink at the same writer
// mcpLogConfig returns and a GHOST_LOG_FILE redirect keeps config warnings out
// of the client's face. That matters because the hook-path warnings are not
// once-per-start — EnforceBudget calls LoadForHook before every harness spawn,
// inside the server process.
func TestSetWarningWriter_RedirectsWarnings(t *testing.T) {
	t.Run("unknown key", func(t *testing.T) {
		isolateConfig(t)
		writeUserConfig(t, "linking:\n  thresholdd: 0.9\n")

		var injected bytes.Buffer
		restore := SetWarningWriter(&injected)
		defer restore()

		if _, err := Load(); err != nil {
			t.Fatalf("Load(): %v", err)
		}
		if !strings.Contains(injected.String(), "linking.thresholdd") {
			t.Errorf("injected sink got %q, want the unknown-key warning", injected.String())
		}
	})

	t.Run("hook path", func(t *testing.T) {
		isolateConfig(t)
		path := writeUserConfig(t, malformedYAML)

		var injected bytes.Buffer
		restore := SetWarningWriter(&injected)
		defer restore()

		if cfg := LoadForHook(); cfg == nil {
			t.Fatal("LoadForHook() = nil")
		}
		if !strings.Contains(injected.String(), path) {
			t.Errorf("injected sink got %q, want the parse warning naming %q", injected.String(), path)
		}
	})
}

// TestSetWarningWriter_NilKeepsDefault pins that a nil writer is ignored rather
// than panicking later on the first warning.
func TestSetWarningWriter_NilKeepsDefault(t *testing.T) {
	isolateConfig(t)
	writeUserConfig(t, "linking:\n  thresholdd: 0.9\n")

	warnings := captureConfigWarnings(t)
	restore := SetWarningWriter(nil)
	defer restore()

	if _, err := Load(); err != nil {
		t.Fatalf("Load(): %v", err)
	}
	if !strings.Contains(warnings.String(), "linking.thresholdd") {
		t.Errorf("SetWarningWriter(nil) discarded the default sink; got %q", warnings.String())
	}
}

// TestSetWarningWriter_ConcurrentWithWarnings is the race the MCP server can
// hit: an in-flight tool handler sits inside a warnf — every harness spawn goes
// through scratch.EnforceBudget → LoadForHook, so ghost_resolve reaches it —
// while the sink is being replaced. A plain io.Writer variable fails this under
// -race, which CI runs (`go test -race -count=1 ./...`); a torn two-word
// interface read can fault, not just trip the detector.
func TestSetWarningWriter_ConcurrentWithWarnings(t *testing.T) {
	isolateConfig(t)
	writeUserConfig(t, "linking:\n  thresholdd: 0.9\n")
	restore := SetWarningWriter(io.Discard)
	defer restore()

	// Both sides are released from the same barrier and run a fixed number of
	// iterations, so neither can finish before the other starts — a loop that
	// exits early (say, on a `done` channel the main goroutine closes
	// immediately) can let the warning side run zero times and pass vacuously.
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { // the warning side: every spawn does this via LoadForHook
		defer wg.Done()
		<-start
		for range 200 {
			if _, err := Load(); err != nil {
				t.Errorf("Load(): %v", err)
				return
			}
		}
	}()
	go func() { // the replacing side, as runMCP does at server startup
		defer wg.Done()
		<-start
		for range 200 {
			restore := SetWarningWriter(io.Discard)
			restore()
		}
	}()
	close(start)
	wg.Wait()
}

// TestLoad_AllKnownKeysDoNotWarn guards the unknown-key warning against false
// positives: if it fired on Ghost's own keys it would mean nothing, so every
// key the compiled defaults set must be bound by a Config field, and a file
// naming one key per section (including the two map/list kinds) must be silent.
func TestLoad_AllKnownKeysDoNotWarn(t *testing.T) {
	isolateConfig(t)
	warnings := captureConfigWarnings(t)

	for key := range defaults {
		if _, ok := knownKeys[key]; !ok {
			t.Errorf("defaults key %q is not bound by any Config field", key)
		}
	}

	writeUserConfig(t, strings.Join([]string{
		"embedding:",
		"  enabled: true",
		"  ollama_url: http://localhost:11434",
		"  model: nomic-embed-text:v1.5",
		"  dimensions: 768",
		"reflection:",
		"  auto_resolve: true",
		"  auto_supersede: true",
		"  auto_reflect: true",
		"  lifecycle_timeout_minutes: 60",
		"  consolidation_timeout_minutes: 10",
		"cli:",
		`  claude_binary: ""`,
		`  opencode_binary: ""`,
		`  codex_binary: ""`,
		`  goose_binary: ""`,
		`  model_reflect: ""`,
		`  model_resolve: ""`,
		`  model_supersede: ""`,
		"linking:",
		"  enabled: true",
		"  threshold: 0.7",
		"  demotion_threshold: 0.9",
		"injection:",
		"  behavior_floor: 8",
		"  behavior_categories:",
		"    - gotcha",
		"  category_weights:",
		"    gotcha: 1.2",
		"  category_caps:",
		"    gotcha: 4",
		"search:",
		"  min_similarity: 0.0",
		"obsidian:",
		`  vault_dir: ""`,
		`  interval: "30s"`,
		"  auto_sync: false",
		"routing:",
		`  default_project: ""`,
		"scratch:",
		"  max_bytes: 536870912",
		"",
	}, "\n"))

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if cfg.Injection.CategoryWeights["gotcha"] != 1.2 {
		t.Errorf("injection.category_weights = %v, want gotcha:1.2 to load", cfg.Injection.CategoryWeights)
	}
	if got := warnings.String(); got != "" {
		t.Errorf("Load() warned for keys that are all bound: %q", got)
	}
}

// TestLoad_LinkingDemotionThresholdEnvOverride: GHOST_LINKING_DEMOTION_THRESHOLD
// must reach linking.demotion_threshold — the generic transformer produces
// linking.demotion.threshold and misses the key entirely.
func TestLoad_LinkingDemotionThresholdEnvOverride(t *testing.T) {
	isolateConfig(t)
	t.Setenv("GHOST_LINKING_DEMOTION_THRESHOLD", "0.42")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	if cfg.Linking.DemotionThreshold != 0.42 {
		t.Errorf("linking.demotion_threshold = %f, want 0.42 (env override)", cfg.Linking.DemotionThreshold)
	}
}

// TestLoad_ObsidianAutoSyncEnvOverride: GHOST_OBSIDIAN_AUTO_SYNC must reach
// obsidian.auto_sync; the generic transformer produces obsidian.auto.sync.
func TestLoad_ObsidianAutoSyncEnvOverride(t *testing.T) {
	isolateConfig(t)
	t.Setenv("GHOST_OBSIDIAN_AUTO_SYNC", "true")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	if !cfg.Obsidian.AutoSync {
		t.Error("obsidian.auto_sync = false, want true (env override)")
	}
}

// TestLoad_InjectionEnvOverrides gives every injection.* key a GHOST_ variable.
// The list and the two maps cannot be decoded by koanf's weakly-typed unmarshal
// from a bare string: "gotcha,decision" would arrive as the single element
// ["gotcha,decision"], and a string never becomes a map.
func TestLoad_InjectionEnvOverrides(t *testing.T) {
	isolateConfig(t)
	t.Setenv("GHOST_INJECTION_BEHAVIOR_FLOOR", "3")
	t.Setenv("GHOST_INJECTION_BEHAVIOR_CATEGORIES", "gotcha, decision")
	t.Setenv("GHOST_INJECTION_CATEGORY_WEIGHTS", "gotcha=1.2,decision=1.5")
	t.Setenv("GHOST_INJECTION_CATEGORY_CAPS", "gotcha=2,decision=1")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	if cfg.Injection.BehaviorFloor != 3 {
		t.Errorf("injection.behavior_floor = %d, want 3 (env override)", cfg.Injection.BehaviorFloor)
	}
	if want := []string{"gotcha", "decision"}; !slices.Equal(cfg.Injection.BehaviorCategories, want) {
		t.Errorf("injection.behavior_categories = %v, want %v (comma-separated env override)",
			cfg.Injection.BehaviorCategories, want)
	}
	if got := cfg.Injection.CategoryWeights; got["gotcha"] != 1.2 || got["decision"] != 1.5 {
		t.Errorf("injection.category_weights = %v, want gotcha:1.2 decision:1.5", got)
	}
	// gotcha:2 also proves the override beat the compiled default of 4.
	if got := cfg.Injection.CategoryCaps; got["gotcha"] != 2 || got["decision"] != 1 {
		t.Errorf("injection.category_caps = %v, want gotcha:2 decision:1", got)
	}
}

// TestLoad_MalformedEnvOverrideIsAnError pins that a GHOST_ value which cannot
// be read as the key's type is reported against the variable that carried it,
// naming the variable so the user knows which one to fix.
func TestLoad_MalformedEnvOverrideIsAnError(t *testing.T) {
	cases := []struct {
		name, envKey, value string
	}{
		{"behavior floor", "GHOST_INJECTION_BEHAVIOR_FLOOR", "eight"},
		{"category weights missing value", "GHOST_INJECTION_CATEGORY_WEIGHTS", "gotcha"},
		{"category weights bad value", "GHOST_INJECTION_CATEGORY_WEIGHTS", "gotcha=high"},
		{"category caps bad value", "GHOST_INJECTION_CATEGORY_CAPS", "gotcha=four"},
		{"demotion threshold", "GHOST_LINKING_DEMOTION_THRESHOLD", "high"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isolateConfig(t)
			t.Setenv(tc.envKey, tc.value)

			cfg, err := Load()
			if err == nil {
				t.Fatalf("Load() = %+v, nil; %s=%q cannot be read as its key's type",
					cfg, tc.envKey, tc.value)
			}
			if !strings.Contains(err.Error(), tc.envKey) {
				t.Errorf("error %q must name %s", err, tc.envKey)
			}
		})
	}
}

// TestLoad_NullSectionKeepsDefaults pins that a section header with no children
// — `injection:` on its own line, which YAML reads as null — neither trips the
// unknown-key warning nor erases the defaults under it. Both happened: the null
// key is not itself bound (only injection.behavior_floor and its siblings are),
// so it warned; and koanf merges the null over the defaults map, wiping the
// whole injection.* subtree. A user who opens their config to disable one
// setting and leaves the section header bare would silently lose every other
// value in it.
func TestLoad_NullSectionKeepsDefaults(t *testing.T) {
	cases := []struct {
		name, yaml string
		keep       func(*Config) bool
	}{
		{
			name: "injection",
			yaml: "injection:\n",
			keep: func(c *Config) bool {
				return c.Injection.BehaviorFloor == 8 &&
					c.Injection.CategoryCaps["gotcha"] == 4 &&
					len(c.Injection.BehaviorCategories) == 4
			},
		},
		{
			name: "obsidian",
			yaml: "obsidian:\n",
			keep: func(c *Config) bool { return c.Obsidian.Interval == "30s" },
		},
		{
			name: "nested section",
			yaml: "injection:\n  category_weights:\n",
			keep: func(c *Config) bool { return c.Injection.BehaviorFloor == 8 },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isolateConfig(t)
			writeUserConfig(t, tc.yaml)
			warnings := captureConfigWarnings(t)

			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load(): %v", err)
			}
			if !tc.keep(cfg) {
				t.Errorf("a bare section header wiped its defaults: %+v", cfg)
			}
			if got := warnings.String(); strings.Contains(got, "unknown key") {
				t.Errorf("a bare section header was reported as an unknown key: %q", got)
			}
		})
	}
}

// TestLoad_RealSiblingStillWarns is the other half: pruning bare section
// headers must not stop the warning for a genuine typo in the same file.
func TestLoad_RealSiblingStillWarns(t *testing.T) {
	isolateConfig(t)
	writeUserConfig(t, "obsidian:\ninjection:\nobsidain:\n  vault_dir: /x\n")
	warnings := captureConfigWarnings(t)

	if _, err := Load(); err != nil {
		t.Fatalf("Load(): %v", err)
	}
	got := warnings.String()
	if !strings.Contains(got, "obsidain") {
		t.Errorf("warning %q must still name the genuine typo obsidain", got)
	}
	if strings.Contains(got, "obsidian:") || strings.Contains(got, "injection:") {
		t.Errorf("warning %q must not name the bare section headers", got)
	}
}

// TestLoadEnvLayer_ReportsEveryBadOverride pins that one unreadable variable
// does not hide the ones after it: the bad value here comes FIRST in the
// override table, so returning early would report only it and silently drop the
// good variable behind it.
func TestLoadEnvLayer_ReportsEveryBadOverride(t *testing.T) {
	isolateConfig(t)
	// Table order: ..._CATEGORY_WEIGHTS precedes ..._CATEGORY_CAPS.
	t.Setenv("GHOST_INJECTION_CATEGORY_WEIGHTS", "gotcha=high")
	t.Setenv("GHOST_INJECTION_CATEGORY_CAPS", "gotcha=four")

	_, err := Load()
	if err == nil {
		t.Fatal("Load() accepted two unreadable GHOST_* values")
	}
	for _, name := range []string{"GHOST_INJECTION_CATEGORY_WEIGHTS", "GHOST_INJECTION_CATEGORY_CAPS"} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("error %q must name %s; returning at the first bad value hides the rest", err, name)
		}
	}
}

// TestLoadEnvLayer_GenericBadValueKeepsGoodOne is the generic-layer half, and
// the case that was previously all-or-nothing: one variable koanf cannot weakly
// convert failed the whole decode, so GHOST_EMBEDDING_ENABLED=false — a
// deliberate opt-out — was lost along with everything else. Only the bad
// variable may be skipped, and the report must name it.
func TestLoadEnvLayer_GenericBadValueKeepsGoodOne(t *testing.T) {
	// The bad variable is set first in both cases, so it is the earlier of the
	// two as the layer walks the environment.
	cases := []struct{ name, badKey, badValue, goodKey, goodValue string }{
		{"dimensions then enabled", "GHOST_EMBEDDING_DIMENSIONS", "abc", "GHOST_EMBEDDING_ENABLED", "false"},
		{"threshold then min_similarity", "GHOST_LINKING_THRESHOLD", "high", "GHOST_SEARCH_MIN_SIMILARITY", "0.5"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isolateConfig(t)
			t.Setenv(tc.badKey, tc.badValue)
			t.Setenv(tc.goodKey, tc.goodValue)
			warnings := captureConfigWarnings(t)

			// The fallback is where a skipped variable is survivable: Load
			// reports the error and yields nothing.
			writeUserConfig(t, malformedYAML)
			cfg := LoadForHook()
			if cfg == nil {
				t.Fatal("LoadForHook() = nil")
			}
			if got := warnings.String(); !strings.Contains(got, tc.badKey) {
				t.Errorf("warning %q must name the skipped variable %s", got, tc.badKey)
			}
			if tc.goodKey == "GHOST_EMBEDDING_ENABLED" && cfg.Embedding.Enabled {
				t.Error("embedding.enabled = true, want the GHOST_EMBEDDING_ENABLED=false opt-out to survive a bad sibling")
			}
			if tc.goodKey == "GHOST_SEARCH_MIN_SIMILARITY" && cfg.Search.MinSimilarity != 0.5 {
				t.Errorf("search.min_similarity = %f, want 0.5 (the good variable must survive its bad sibling)",
					cfg.Search.MinSimilarity)
			}
			// A skipped variable must not take the rest of the environment with
			// it, and the defaults must still stand behind what is left.
			if cfg.Linking.DemotionThreshold != 0.90 {
				t.Errorf("linking.demotion_threshold = %f, want the default 0.90", cfg.Linking.DemotionThreshold)
			}
		})
	}
}

// TestWarnf_OncePerDistinctMessage pins the dedup: a SessionStart hook and
// `ghost mcp status` in one process must not print the same line twice, while a
// genuinely different problem still gets through. Without it a broken config
// reported once per harness spawn turns a single typo into a stream of identical
// lines nobody reads past the first.
func TestWarnf_OncePerDistinctMessage(t *testing.T) {
	isolateConfig(t)
	warnings := captureConfigWarnings(t)

	warnf("the same problem")
	warnf("the same problem")
	warnf("a different problem")

	got := warnings.String()
	if n := strings.Count(got, "the same problem"); n != 1 {
		t.Errorf("printed the identical warning %d times, want 1:\n%s", n, got)
	}
	if n := strings.Count(got, "a different problem"); n != 1 {
		t.Errorf("printed the second distinct warning %d times, want 1:\n%s", n, got)
	}
}

// TestLoad_UnknownKeyWarnedOnceAcrossLoads is the dedup where it earns its
// keep: two loads in one process, as a hook and a status check do, must produce
// one line.
func TestLoad_UnknownKeyWarnedOnceAcrossLoads(t *testing.T) {
	isolateConfig(t)
	writeUserConfig(t, "linking:\n  thresholdd: 0.9\n")
	warnings := captureConfigWarnings(t)

	for i := range 2 {
		if _, err := Load(); err != nil {
			t.Fatalf("Load() %d: %v", i, err)
		}
	}
	if n := strings.Count(warnings.String(), "thresholdd"); n != 1 {
		t.Errorf("warned about the same unknown key %d times across two loads, want 1:\n%s",
			n, warnings.String())
	}
}

// TestLoadConfigFile_UnreadableFileIsSkipped pins the architect's ruling: a
// config file that exists but cannot be READ is a warning, never a fatal error.
// The main branch behaved this way, and a permission problem in a file the user
// did not write must not stop `ghost reflect` with an error about YAML it never
// got to parse. A file that is readable but does not parse stays fatal.
func TestLoadConfigFile_UnreadableFileIsSkipped(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: mode 0o000 is still readable")
	}
	parser := yaml.Parser()

	unreadable := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(unreadable, []byte("embedding:\n  enabled: true\n"), 0o000); err != nil {
		t.Fatal(err)
	}
	warnings := captureConfigWarnings(t)
	if err := loadConfigFile(koanf.New("."), unreadable, parser); err != nil {
		t.Errorf("loadConfigFile on an unreadable file = %v, want nil (warned and skipped)", err)
	}
	if got := warnings.String(); !strings.Contains(got, unreadable) {
		t.Errorf("warning %q must name the unreadable file %q", got, unreadable)
	}

	// Readable but malformed stays an error: that is the case this PR exists for.
	malformed := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(malformed, []byte(malformedYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := loadConfigFile(koanf.New("."), malformed, parser); err == nil {
		t.Error("loadConfigFile accepted a readable but malformed file; a parse error must stay fatal")
	}
}
