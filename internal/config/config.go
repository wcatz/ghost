// Package config provides layered configuration for Ghost.
//
// Loading order (later layers override earlier):
//  1. Compiled defaults
//  2. /etc/ghost/config.yaml          (system-wide)
//  3. ~/.config/ghost/config.yaml     (user-global)
//  4. .ghost/config.yaml              (project, checked in)
//  5. .ghost/config.local.yaml        (project, gitignored)
//  6. GHOST_* environment variables
//  7. CLI flag overrides (applied by caller after Load)
package config

import (
	_ "embed"
	"os"
	"path/filepath"
	"strings"

	"github.com/knadh/koanf/parsers/yaml"
	"github.com/knadh/koanf/providers/confmap"
	"github.com/knadh/koanf/providers/env"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/v2"
)

//go:embed config.example.yaml
var exampleConfig []byte

// Config holds the global ghost configuration.
type Config struct {
	API        APIConfig        `koanf:"api"`
	Defaults   DefaultsConfig   `koanf:"defaults"`
	Display    DisplayConfig    `koanf:"display"`
	Server     ServerConfig     `koanf:"server"`
	Embedding  EmbeddingConfig  `koanf:"embedding"`
	Reflection ReflectionConfig `koanf:"reflection"`
}

// APIConfig holds Claude API settings.
type APIConfig struct {
	Key          string `koanf:"key"`
	ModelQuality string `koanf:"model_quality"`
	ModelFast    string `koanf:"model_fast"`
}

// DefaultsConfig holds default behavior settings.
type DefaultsConfig struct {
	Mode               string `koanf:"mode"`
	ReflectionInterval int    `koanf:"reflection_interval"`
	MaxConvTurns       int    `koanf:"max_conversation_turns"`
	AutoMemory         bool   `koanf:"auto_memory"`
	ApprovalMode       string `koanf:"approval_mode"`
}

// DisplayConfig holds display preferences.
type DisplayConfig struct {
	ShowTokenUsage   bool   `koanf:"show_token_usage"`
	ShowCost         bool   `koanf:"show_cost"`
	StreamToolOutput bool   `koanf:"stream_tool_output"`
	Theme            string `koanf:"theme"`           // "dark", "light", "auto"
	ImageProtocol    string `koanf:"image_protocol"`  // "auto", "sixel", "kitty", "iterm2", "none"
	PlainMode        bool   `koanf:"plain_mode"`      // force legacy REPL (no bubbletea)
}

// ServerConfig holds ghost serve settings.
type ServerConfig struct {
	ListenAddr string `koanf:"listen_addr"`
	AuthToken  string `koanf:"auth_token"`
}

// ReflectionConfig holds memory consolidation settings.
type ReflectionConfig struct {
	Backend string `koanf:"backend"` // "auto", "haiku", "sqlite", "disabled"
}

// EmbeddingConfig holds local embedding settings.
type EmbeddingConfig struct {
	Enabled    bool   `koanf:"enabled"`
	OllamaURL  string `koanf:"ollama_url"`
	Model      string `koanf:"model"`
	Dimensions int    `koanf:"dimensions"`
}

// defaults is the base layer — always loaded first.
var defaults = map[string]interface{}{
	"api.model_quality":          "claude-opus-4-6-20250514",
	"api.model_fast":             "claude-sonnet-4-5-20250929",
	"defaults.mode":              "chat",
	"defaults.reflection_interval": 10,
	"defaults.max_conversation_turns": 50,
	"defaults.auto_memory":       true,
	"defaults.approval_mode":     "normal",
	"display.show_token_usage":   true,
	"display.show_cost":          true,
	"display.stream_tool_output": true,
	"display.theme":              "auto",
	"display.image_protocol":     "auto",
	"display.plain_mode":         false,
	"server.listen_addr":         "127.0.0.1:2187",
	"embedding.enabled":          true,
	"embedding.ollama_url":       "http://localhost:11434",
	"embedding.model":            "nomic-embed-text:v1.5",
	"embedding.dimensions":       768,
	"reflection.backend":         "auto",
}

// Load reads configuration with layered precedence.
// After Load returns, the caller may apply CLI flag overrides by mutating
// fields directly (e.g. cfg.API.ModelQuality = *modelFlag).
func Load() (*Config, error) {
	k := koanf.New(".")

	// Layer 1: compiled defaults.
	if err := k.Load(confmap.Provider(defaults, "."), nil); err != nil {
		return nil, err
	}

	parser := yaml.Parser()

	// Layer 2: /etc/ghost/config.yaml (system-wide).
	loadFileIfExists(k, "/etc/ghost/config.yaml", parser)

	// Layer 3: ~/.config/ghost/config.yaml (user-global).
	if configDir, err := os.UserConfigDir(); err == nil {
		loadFileIfExists(k, filepath.Join(configDir, "ghost", "config.yaml"), parser)
	}

	// Layer 6: GHOST_* environment variables.
	// e.g. GHOST_API_KEY → api.key, GHOST_DEFAULTS_MODE → defaults.mode
	if err := k.Load(env.Provider("GHOST_", ".", func(s string) string {
		return strings.ToLower(strings.Replace(
			strings.TrimPrefix(s, "GHOST_"), "_", ".", -1))
	}), nil); err != nil {
		return nil, err
	}

	// Also support the standard ANTHROPIC_API_KEY.
	if key := os.Getenv("ANTHROPIC_API_KEY"); key != "" {
		_ = k.Load(confmap.Provider(map[string]interface{}{
			"api.key": key,
		}, "."), nil)
	}

	// Explicit env overrides for keys with underscores in koanf tags.
	// koanf's _ → . transformer would map e.g. GHOST_SERVER_AUTH_TOKEN
	// to server.auth.token instead of server.auth_token.
	envOverrides := map[string]string{
		"GHOST_SERVER_AUTH_TOKEN": "server.auth_token",
	}
	for envKey, koanfKey := range envOverrides {
		if val := os.Getenv(envKey); val != "" {
			_ = k.Load(confmap.Provider(map[string]interface{}{
				koanfKey: val,
			}, "."), nil)
		}
	}

	cfg := &Config{}
	if err := k.Unmarshal("", cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

// DataDir returns the ghost data directory, creating it if needed.
func DataDir() (string, error) {
	dataHome := os.Getenv("XDG_DATA_HOME")
	if dataHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dataHome = filepath.Join(home, ".local", "share")
	}
	dir := filepath.Join(dataHome, "ghost")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

// EnsureConfigFile creates ~/.config/ghost/config.yaml from the embedded example
// if it doesn't already exist. Returns the path and whether a new file was created.
func EnsureConfigFile() (path string, created bool, err error) {
	configDir, err := os.UserConfigDir()
	if err != nil {
		return "", false, err
	}
	dir := filepath.Join(configDir, "ghost")
	path = filepath.Join(dir, "config.yaml")

	if _, err := os.Stat(path); err == nil {
		return path, false, nil
	}

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", false, err
	}
	if err := os.WriteFile(path, exampleConfig, 0o600); err != nil {
		return "", false, err
	}
	return path, true, nil
}

// loadFileIfExists loads a config file into koanf if it exists, silently skipping missing files.
func loadFileIfExists(k *koanf.Koanf, path string, parser koanf.Parser) {
	if _, err := os.Stat(path); err == nil {
		_ = k.Load(file.Provider(path), parser)
	}
}
