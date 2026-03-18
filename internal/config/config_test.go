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
