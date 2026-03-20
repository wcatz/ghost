package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoad_Defaults(t *testing.T) {
	// Isolate from real config files by pointing HOME/XDG to temp dir.
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	t.Setenv("XDG_CONFIG_HOME", tmpDir)

	// Unset any env vars that could interfere. We save/restore manually
	// because t.Setenv("X","") would set it to empty (not unset), and
	// koanf's env provider treats empty as an override.
	for _, key := range []string{
		"GHOST_API_KEY",
		"GHOST_DEFAULTS_MODE",
		"GHOST_SERVER_LISTEN_ADDR",
		"ANTHROPIC_API_KEY",
	} {
		if old, ok := os.LookupEnv(key); ok {
			os.Unsetenv(key)
			t.Cleanup(func() { os.Setenv(key, old) })
		}
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	// Verify compiled defaults are applied.
	if cfg.API.ModelQuality != "claude-opus-4-6-20250514" {
		t.Errorf("expected model_quality default, got %q", cfg.API.ModelQuality)
	}
	if cfg.API.ModelFast != "claude-sonnet-4-5-20250929" {
		t.Errorf("expected model_fast default, got %q", cfg.API.ModelFast)
	}
	if cfg.Defaults.Mode != "chat" {
		t.Errorf("expected defaults.mode=chat, got %q", cfg.Defaults.Mode)
	}
	if cfg.Defaults.ReflectionInterval != 10 {
		t.Errorf("expected reflection_interval=10, got %d", cfg.Defaults.ReflectionInterval)
	}
	if cfg.Defaults.MaxConvTurns != 50 {
		t.Errorf("expected max_conversation_turns=50, got %d", cfg.Defaults.MaxConvTurns)
	}
	if !cfg.Defaults.AutoMemory {
		t.Error("expected auto_memory=true")
	}
	if cfg.Defaults.ApprovalMode != "normal" {
		t.Errorf("expected approval_mode=normal, got %q", cfg.Defaults.ApprovalMode)
	}
	if cfg.Server.ListenAddr != "127.0.0.1:2187" {
		t.Errorf("expected listen_addr=127.0.0.1:2187, got %q", cfg.Server.ListenAddr)
	}
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
	for _, key := range []string{"GHOST_API_KEY", "GHOST_DEFAULTS_MODE", "ANTHROPIC_API_KEY"} {
		if old, ok := os.LookupEnv(key); ok {
			os.Unsetenv(key)
			t.Cleanup(func() { os.Setenv(key, old) })
		}
	}

	// Set GHOST_* overrides.
	t.Setenv("GHOST_API_KEY", "sk-ghost-override")
	t.Setenv("GHOST_DEFAULTS_MODE", "code")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	if cfg.API.Key != "sk-ghost-override" {
		t.Errorf("expected api.key from GHOST_API_KEY, got %q", cfg.API.Key)
	}
	if cfg.Defaults.Mode != "code" {
		t.Errorf("expected defaults.mode=code from env, got %q", cfg.Defaults.Mode)
	}
}

func TestLoad_ExplicitEnvOverrides(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	t.Setenv("XDG_CONFIG_HOME", tmpDir)

	// Clear interfering env vars.
	for _, key := range []string{"GHOST_API_KEY", "GHOST_SERVER_AUTH_TOKEN", "ANTHROPIC_API_KEY"} {
		if old, ok := os.LookupEnv(key); ok {
			os.Unsetenv(key)
			t.Cleanup(func() { os.Setenv(key, old) })
		}
	}

	t.Setenv("GHOST_SERVER_AUTH_TOKEN", "my-secret-token")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	if cfg.Server.AuthToken != "my-secret-token" {
		t.Errorf("expected server.auth_token from env, got %q", cfg.Server.AuthToken)
	}
}

func TestLoad_YAMLFileOverride(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	t.Setenv("XDG_CONFIG_HOME", tmpDir)

	// Clear interfering env vars.
	for _, key := range []string{
		"GHOST_API_KEY", "GHOST_DEFAULTS_MODE", "GHOST_SERVER_LISTEN_ADDR",
		"ANTHROPIC_API_KEY",
	} {
		if old, ok := os.LookupEnv(key); ok {
			os.Unsetenv(key)
			t.Cleanup(func() { os.Setenv(key, old) })
		}
	}

	// Create a config file in the user config dir.
	configDir := filepath.Join(tmpDir, "ghost")
	os.MkdirAll(configDir, 0o700)
	configFile := filepath.Join(configDir, "config.yaml")
	yamlContent := `
api:
  model_quality: "custom-model-quality"
  model_fast: "custom-model-fast"
defaults:
  mode: "review"
  reflection_interval: 20
server:
  listen_addr: "0.0.0.0:9999"
`
	os.WriteFile(configFile, []byte(yamlContent), 0o600)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	if cfg.API.ModelQuality != "custom-model-quality" {
		t.Errorf("model_quality = %q, want %q", cfg.API.ModelQuality, "custom-model-quality")
	}
	if cfg.API.ModelFast != "custom-model-fast" {
		t.Errorf("model_fast = %q, want %q", cfg.API.ModelFast, "custom-model-fast")
	}
	if cfg.Defaults.Mode != "review" {
		t.Errorf("mode = %q, want %q", cfg.Defaults.Mode, "review")
	}
	if cfg.Defaults.ReflectionInterval != 20 {
		t.Errorf("reflection_interval = %d, want 20", cfg.Defaults.ReflectionInterval)
	}
	if cfg.Server.ListenAddr != "0.0.0.0:9999" {
		t.Errorf("listen_addr = %q, want %q", cfg.Server.ListenAddr, "0.0.0.0:9999")
	}

	// Unaffected defaults should remain.
	if !cfg.Defaults.AutoMemory {
		t.Error("auto_memory should still be true (default)")
	}
}

func TestLoad_EnvOverridesYAML(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	t.Setenv("XDG_CONFIG_HOME", tmpDir)

	// Clear interfering env vars.
	for _, key := range []string{"GHOST_API_KEY", "GHOST_DEFAULTS_MODE", "ANTHROPIC_API_KEY"} {
		if old, ok := os.LookupEnv(key); ok {
			os.Unsetenv(key)
			t.Cleanup(func() { os.Setenv(key, old) })
		}
	}

	// YAML file sets mode to "review".
	configDir := filepath.Join(tmpDir, "ghost")
	os.MkdirAll(configDir, 0o700)
	os.WriteFile(filepath.Join(configDir, "config.yaml"), []byte("defaults:\n  mode: review\n"), 0o600)

	// Env var overrides to "debug".
	t.Setenv("GHOST_DEFAULTS_MODE", "debug")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	// Env should take precedence over YAML.
	if cfg.Defaults.Mode != "debug" {
		t.Errorf("mode = %q, want %q (env should override yaml)", cfg.Defaults.Mode, "debug")
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

func TestLoad_DisplayDefaults(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	t.Setenv("XDG_CONFIG_HOME", tmpDir)

	for _, key := range []string{"GHOST_API_KEY", "ANTHROPIC_API_KEY"} {
		if old, ok := os.LookupEnv(key); ok {
			os.Unsetenv(key)
			t.Cleanup(func() { os.Setenv(key, old) })
		}
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	if !cfg.Display.ShowTokenUsage {
		t.Error("expected show_token_usage=true")
	}
	if !cfg.Display.ShowCost {
		t.Error("expected show_cost=true")
	}
	if !cfg.Display.StreamToolOutput {
		t.Error("expected stream_tool_output=true")
	}
	if cfg.Display.Theme != "auto" {
		t.Errorf("expected theme=auto, got %q", cfg.Display.Theme)
	}
	if cfg.Display.ImageProtocol != "auto" {
		t.Errorf("expected image_protocol=auto, got %q", cfg.Display.ImageProtocol)
	}
	if cfg.Display.PlainMode {
		t.Error("expected plain_mode=false")
	}
}

func TestLoad_VoiceDefaults(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	t.Setenv("XDG_CONFIG_HOME", tmpDir)

	for _, key := range []string{"GHOST_API_KEY", "ANTHROPIC_API_KEY"} {
		if old, ok := os.LookupEnv(key); ok {
			os.Unsetenv(key)
			t.Cleanup(func() { os.Setenv(key, old) })
		}
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	if cfg.Voice.Enabled {
		t.Error("expected voice.enabled=false")
	}
	if cfg.Voice.STTBackend != "whisper" {
		t.Errorf("expected stt_backend=whisper, got %q", cfg.Voice.STTBackend)
	}
	if cfg.Voice.TTSBackend != "piper" {
		t.Errorf("expected tts_backend=piper, got %q", cfg.Voice.TTSBackend)
	}
	if cfg.Voice.SilenceMs != 800 {
		t.Errorf("expected silence_ms=800, got %d", cfg.Voice.SilenceMs)
	}
	if cfg.Voice.SampleRate != 16000 {
		t.Errorf("expected sample_rate=16000, got %d", cfg.Voice.SampleRate)
	}
	if cfg.Voice.InputDevice != "default" {
		t.Errorf("expected input_device=default, got %q", cfg.Voice.InputDevice)
	}
}

func TestDataDir_DefaultFallback(t *testing.T) {
	// Unset XDG_DATA_HOME to test the fallback to ~/.local/share.
	t.Setenv("XDG_DATA_HOME", "")
	os.Unsetenv("XDG_DATA_HOME")

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
