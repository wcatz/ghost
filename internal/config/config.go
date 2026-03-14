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
	API       APIConfig       `koanf:"api"`
	Defaults  DefaultsConfig  `koanf:"defaults"`
	Display   DisplayConfig   `koanf:"display"`
	Server    ServerConfig    `koanf:"server"`
	Embedding EmbeddingConfig `koanf:"embedding"`
	GitHub    GitHubConfig    `koanf:"github"`
	Telegram  TelegramConfig  `koanf:"telegram"`
	Calendar  CalendarConfig  `koanf:"calendar"`
	Briefing  BriefingConfig  `koanf:"briefing"`
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
	ShowTokenUsage   bool `koanf:"show_token_usage"`
	ShowCost         bool `koanf:"show_cost"`
	StreamToolOutput bool `koanf:"stream_tool_output"`
}

// ServerConfig holds ghost serve settings.
type ServerConfig struct {
	ListenAddr string `koanf:"listen_addr"`
}

// EmbeddingConfig holds local embedding settings.
type EmbeddingConfig struct {
	Enabled    bool   `koanf:"enabled"`
	OllamaURL  string `koanf:"ollama_url"`
	Model      string `koanf:"model"`
	Dimensions int    `koanf:"dimensions"`
}

// GitHubConfig holds GitHub notification monitor settings.
type GitHubConfig struct {
	Token    string `koanf:"token"`
	Interval int    `koanf:"interval"` // poll interval in seconds
}

// TelegramConfig holds Telegram bot settings.
type TelegramConfig struct {
	Token      string `koanf:"token"`
	AllowedIDs string `koanf:"allowed_ids"` // comma-separated user IDs
}

// CalendarConfig holds CalDAV calendar settings.
type CalendarConfig struct {
	URL      string `koanf:"url"`
	Username string `koanf:"username"`
	Password string `koanf:"password"`
}

// BriefingConfig holds morning briefing schedule settings.
type BriefingConfig struct {
	Enabled  bool   `koanf:"enabled"`
	Schedule string `koanf:"schedule"` // cron expression, e.g. "0 8 * * *"
}

// ProjectConfig holds per-project configuration from .ghost/config.yaml.
type ProjectConfig struct {
	Project     ProjectInfo     `koanf:"project"`
	Conventions ConventionsInfo `koanf:"conventions"`
	Context     ContextInfo     `koanf:"context"`
	Git         GitInfo         `koanf:"git"`
}

// ProjectInfo holds project identity.
type ProjectInfo struct {
	Name string `koanf:"name"`
}

// ConventionsInfo holds project coding conventions.
type ConventionsInfo struct {
	Indent       string `koanf:"indent"`
	TestCommand  string `koanf:"test_command"`
	LintCommand  string `koanf:"lint_command"`
	BuildCommand string `koanf:"build_command"`
}

// ContextInfo holds project context configuration.
type ContextInfo struct {
	IncludeFiles   []string `koanf:"include_files"`
	IgnorePatterns []string `koanf:"ignore_patterns"`
}

// GitInfo holds git workflow preferences.
type GitInfo struct {
	BranchPrefix string `koanf:"branch_prefix"`
	CommitStyle  string `koanf:"commit_style"`
}

// defaults is the base layer — always loaded first.
var defaults = map[string]interface{}{
	"api.model_quality":          "claude-sonnet-4-5-20250929",
	"api.model_fast":             "claude-haiku-4-5-20251001",
	"defaults.mode":              "code",
	"defaults.reflection_interval": 10,
	"defaults.max_conversation_turns": 50,
	"defaults.auto_memory":       true,
	"defaults.approval_mode":     "normal",
	"display.show_token_usage":   true,
	"display.show_cost":          true,
	"display.stream_tool_output": true,
	"server.listen_addr":         "127.0.0.1:2187",
	"embedding.enabled":          true,
	"embedding.ollama_url":       "http://localhost:11434",
	"embedding.model":            "nomic-embed-text:v1.5",
	"embedding.dimensions":       768,
	"github.interval":            60,
	"briefing.enabled":           false,
	"briefing.schedule":          "0 8 * * 1-5",
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

	cfg := &Config{}
	if err := k.Unmarshal("", cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

// LoadProject reads per-project config from .ghost/config.yaml in the given directory.
func LoadProject(projectPath string) (*ProjectConfig, error) {
	k := koanf.New(".")

	// Defaults.
	if err := k.Load(confmap.Provider(map[string]interface{}{
		"conventions.indent":       "tab",
		"context.include_files":    []string{"CLAUDE.md", "README.md"},
		"context.ignore_patterns":  []string{"vendor/", "node_modules/", ".git/", "dist/", "build/"},
		"git.branch_prefix":        "feat/",
		"git.commit_style":         "conventional",
	}, "."), nil); err != nil {
		return nil, err
	}

	parser := yaml.Parser()

	// .ghost/config.yaml (checked in)
	loadFileIfExists(k, filepath.Join(projectPath, ".ghost", "config.yaml"), parser)

	// .ghost/config.local.yaml (gitignored)
	loadFileIfExists(k, filepath.Join(projectPath, ".ghost", "config.local.yaml"), parser)

	cfg := &ProjectConfig{}
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
	if err := os.MkdirAll(dir, 0o755); err != nil {
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

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", false, err
	}
	if err := os.WriteFile(path, exampleConfig, 0o644); err != nil {
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
