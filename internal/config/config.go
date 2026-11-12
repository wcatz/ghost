// Package config provides layered configuration for Ghost.
//
// Loading order (later layers override earlier):
package config

import (
	_ "embed"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/knadh/koanf/parsers/yaml"
	"github.com/knadh/koanf/providers/confmap"
	"github.com/knadh/koanf/v2"
)

//go:embed config.example.yaml
var exampleConfig []byte

// Config holds the global ghost configuration.
type Config struct {
	CLI        CLIConfig        `koanf:"cli"`
	Embedding  EmbeddingConfig  `koanf:"embedding"`
	Reflection ReflectionConfig `koanf:"reflection"`
	Lifecycle  LifecycleConfig  `koanf:"lifecycle"`
	Linking    LinkingConfig    `koanf:"linking"`
	Injection  InjectionConfig  `koanf:"injection"`
	Search     SearchConfig     `koanf:"search"`
	Context    ContextConfig    `koanf:"context"`
	Obsidian   ObsidianConfig   `koanf:"obsidian"`
	Routing    RoutingConfig    `koanf:"routing"`
	Scratch    ScratchConfig    `koanf:"scratch"`
}

const (
	defaultLifecycleMinInterval = "30m"
	DefaultLifecycleMinInterval = 30 * time.Minute
)

// LifecycleConfig controls when the Stop hook may spawn the auto-consolidation
// chain. It is a separate section from reflection.* on purpose: reflection.*
type LifecycleConfig struct {
	MinInterval string `koanf:"min_interval"`
}

// MinIntervalDuration resolves MinInterval to the cooldown to enforce, for a
// caller that has a Config value in hand.
func (c LifecycleConfig) MinIntervalDuration() time.Duration {
	raw := strings.TrimSpace(c.MinInterval)
	if raw == "" {
		return DefaultLifecycleMinInterval
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		warnf("lifecycle.min_interval: cannot read %q as a duration — using %s (set it to 0 to disable the cooldown)", raw, DefaultLifecycleMinInterval)
		return DefaultLifecycleMinInterval
	}
	if d < 0 {
		warnf("lifecycle.min_interval: %s is negative; treating it as 0 (no cooldown)", d)
		return 0
	}
	return d
}

// DefaultScratchMaxBytes is the compiled per-root scratch budget: 512 MiB.
// It is the value scratch.max_bytes takes when the key is unset, and the
const DefaultScratchMaxBytes int64 = 512 * 1024 * 1024

// DefaultRelevanceCutoff is the compiled value of context.relevance_cutoff, the
// assembler's relative-to-top cutoff on query-mode blocks (#954). It lives here
const DefaultRelevanceCutoff float64 = 0.63

// DefaultNoAnswerCosine is the compiled value of context.no_answer_cosine, the
// assembler's absolute no-answer bar on query-mode blocks (#955): a block whose
const DefaultNoAnswerCosine float64 = 0.62

// DefaultEmbeddingModel is the compiled value of embedding.model, and the model
// DefaultNoAnswerCosine was measured against.
const DefaultEmbeddingModel = "nomic-embed-text:v1.5"

const noAnswerKey = "context.no_answer_cosine"

// NoAnswerBarOffUnmeasured is the note carried when the model tie turned the bar
// off, so the retrieval trace can say why a bar that ships on is not applied.
const NoAnswerBarOffUnmeasured = "no_answer bar off: unmeasured embedding model"

func resolveNoAnswerBar(cfg *Config, explicit bool) {
	if explicit || cfg.Embedding.Model == DefaultEmbeddingModel {
		return
	}
	cfg.Context.NoAnswerCosine = 0
	cfg.Context.NoAnswerBarNote = NoAnswerBarOffUnmeasured
}

// ScratchConfig bounds the scratch root every harness spawn is confined to
// (internal/scratch: $GHOST_SCRATCH_DIR or <dataDir>/scratch).
type ScratchConfig struct {
	MaxBytes int64 `koanf:"max_bytes"`
}

// SearchConfig controls hybrid-search ranking behavior.
type SearchConfig struct {
	MinSimilarity float64 `koanf:"min_similarity"`
}

// ContextConfig controls how the assembler reports an assembled block's
// relevance verdict.
type ContextConfig struct {
	AbstainCosine   float32 `koanf:"abstain_cosine"`
	RelevanceCutoff float64 `koanf:"relevance_cutoff"`
	NoAnswerCosine  float64 `koanf:"no_answer_cosine"`
	NoAnswerBarNote string  `koanf:"-"`
}

// RoutingConfig steers sessions whose cwd matches no known project.
type RoutingConfig struct {
	DefaultProject string `koanf:"default_project"`
}

// CLIConfig holds explicit paths to the subprocess LLM binaries backing
// classification and consolidation. An empty value means "resolve from PATH";
type CLIConfig struct {
	ClaudeBinary   string `koanf:"claude_binary"`
	OpenCodeBinary string `koanf:"opencode_binary"`
	CodexBinary    string `koanf:"codex_binary"`
	GooseBinary    string `koanf:"goose_binary"`

	ModelReflect   string `koanf:"model_reflect"`
	ModelResolve   string `koanf:"model_resolve"`
	ModelSupersede string `koanf:"model_supersede"`
}

// ReflectionConfig holds memory consolidation settings.
type ReflectionConfig struct {
	AutoResolve                 bool `koanf:"auto_resolve"`
	AutoSupersede               bool `koanf:"auto_supersede"`
	AutoReflect                 bool `koanf:"auto_reflect"`
	SupersedeConsensus          int  `koanf:"supersede_consensus"`
	ConsolidationTimeoutMinutes int  `koanf:"consolidation_timeout_minutes"`

	LifecycleTimeoutMinutes int `koanf:"lifecycle_timeout_minutes"`
}

// ClearsConsolidatableSet reports whether any enabled lifecycle phase
// removes rows from the consolidatable set the overdue list counts.
func (c ReflectionConfig) ClearsConsolidatableSet() bool {
	return c.AutoReflect || c.AutoResolve
}

const (
	DefaultLifecycleTimeoutMinutes     = 60
	DefaultConsolidationTimeoutMinutes = 10
)

// LifecycleTimeout resolves LifecycleTimeoutMinutes to the bound on each
// lifecycle phase, with 0 meaning no bound. See timeoutMinutes.
func (c ReflectionConfig) LifecycleTimeout() time.Duration {
	return timeoutMinutes("reflection.lifecycle_timeout_minutes", c.LifecycleTimeoutMinutes, DefaultLifecycleTimeoutMinutes)
}

// ConsolidationTimeout resolves ConsolidationTimeoutMinutes to the bound on
// one consolidation call, with 0 meaning no bound. See timeoutMinutes.
func (c ReflectionConfig) ConsolidationTimeout() time.Duration {
	return timeoutMinutes("reflection.consolidation_timeout_minutes", c.ConsolidationTimeoutMinutes, DefaultConsolidationTimeoutMinutes)
}

const maxTimeoutMinutes = int64(math.MaxInt64 / int64(time.Minute))

func timeoutMinutes(key string, minutes, def int) time.Duration {
	if minutes < 0 {
		warnf("%s: %d is negative; using the default %d (set it to 0 to remove the bound)", key, minutes, def)
		return time.Duration(def) * time.Minute
	}
	if int64(minutes) > maxTimeoutMinutes {
		warnf("%s: %d is too large for a duration; using %d, the largest it can hold (set it to 0 to remove the bound)", key, minutes, maxTimeoutMinutes)
		return time.Duration(maxTimeoutMinutes) * time.Minute
	}
	return time.Duration(minutes) * time.Minute
}

// EmbeddingConfig holds local embedding settings.
type EmbeddingConfig struct {
	Enabled    bool   `koanf:"enabled"`
	OllamaURL  string `koanf:"ollama_url"`
	Model      string `koanf:"model"`
	Dimensions int    `koanf:"dimensions"`
}

// LinkingConfig controls the memory auto-linking worker. Linking requires
// embeddings, so it is only active when embedding is also enabled.
type LinkingConfig struct {
	Enabled           bool    `koanf:"enabled"`
	Threshold         float64 `koanf:"threshold"`
	DemotionThreshold float64 `koanf:"demotion_threshold"`
}

// InjectionConfig controls how the SessionStart hook selects which project
// memories to inject. It biases the limited slot budget toward high-signal,
type InjectionConfig struct {
	BehaviorFloor      int                `koanf:"behavior_floor"`
	BehaviorCategories []string           `koanf:"behavior_categories"`
	CategoryWeights    map[string]float64 `koanf:"category_weights"`
	CategoryCaps       map[string]int     `koanf:"category_caps"`
	SessionScope       map[string]string  `koanf:"session_scope"`
}

// DefaultInjectionConfig returns the compiled injection defaults. It mirrors the
// injection.* entries in the defaults map so callers that need a fallback when
func DefaultInjectionConfig() InjectionConfig {
	return InjectionConfig{
		BehaviorFloor:      8,
		BehaviorCategories: []string{"gotcha", "convention", "preference", "decision"},
		CategoryCaps:       map[string]int{"gotcha": 4},
	}
}

// ObsidianConfig controls the Obsidian vault mirror (ghost obsidian export|sync).
type ObsidianConfig struct {
	VaultDir string `koanf:"vault_dir"` // empty = ~/Documents/GhostVault, resolved by the CLI
	Interval string `koanf:"interval"`  // sync poll cadence, time.ParseDuration format
	AutoSync bool   `koanf:"auto_sync"` // if true, session-start spawns `ghost obsidian sync` automatically
}

var defaults = map[string]interface{}{
	"embedding.enabled":                        true,
	"embedding.ollama_url":                     "http://localhost:11434",
	"embedding.model":                          DefaultEmbeddingModel,
	"embedding.dimensions":                     768,
	"reflection.auto_resolve":                  false,
	"reflection.auto_supersede":                false,
	"reflection.auto_reflect":                  false,
	"reflection.supersede_consensus":           3,
	"reflection.lifecycle_timeout_minutes":     DefaultLifecycleTimeoutMinutes,
	"reflection.consolidation_timeout_minutes": DefaultConsolidationTimeoutMinutes,
	"lifecycle.min_interval":                   defaultLifecycleMinInterval,
	"cli.claude_binary":                        "",
	"cli.opencode_binary":                      "",
	"cli.codex_binary":                         "",
	"cli.goose_binary":                         "",
	"linking.enabled":                          true,
	"linking.threshold":                        0.70,
	"linking.demotion_threshold":               0.90,
	"injection.behavior_floor":                 8,
	"injection.behavior_categories":            []string{"gotcha", "convention", "preference", "decision"},
	"injection.category_caps":                  map[string]interface{}{"gotcha": 4},
	"search.min_similarity":                    0.0,
	"context.abstain_cosine":                   float32(0.0),
	"context.relevance_cutoff":                 DefaultRelevanceCutoff,
	"context.no_answer_cosine":                 DefaultNoAnswerCosine,
	"obsidian.vault_dir":                       "",
	"obsidian.interval":                        "30s",
	"obsidian.auto_sync":                       false,
	"routing.default_project":                  "",
	"scratch.max_bytes":                        DefaultScratchMaxBytes,
}

const systemConfigPath = "/etc/ghost/config.yaml"

// Load reads configuration with layered precedence.
// After Load returns, the caller may apply CLI flag overrides by mutating
func Load() (*Config, error) {
	k := koanf.New(".")

	if err := k.Load(confmap.Provider(defaults, "."), nil); err != nil {
		return nil, err
	}

	k.Delete(noAnswerKey)

	parser := yaml.Parser()

	if err := loadConfigFile(k, systemConfigPath, parser); err != nil {
		return nil, err
	}

	if configDir, err := userConfigDir(); err == nil {
		if err := loadConfigFile(k, filepath.Join(configDir, "ghost", "config.yaml"), parser); err != nil {
			return nil, err
		}
	}

	if err := loadEnvLayer(k); err != nil {
		return nil, err
	}

	cfg := &Config{}
	if err := k.Unmarshal("", cfg); err != nil {
		return nil, err
	}
	explicitBar := k.Exists(noAnswerKey)
	if !explicitBar {
		cfg.Context.NoAnswerCosine = DefaultNoAnswerCosine
	}
	resolveNoAnswerBar(cfg, explicitBar)
	if err := checkScopeValues(cfg); err != nil {
		return nil, err
	}
	if err := checkValueChecks(cfg); err != nil {
		return nil, err
	}
	if err := checkEmbeddingValues(cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

func checkEmbeddingValues(cfg *Config) error {
	if d := cfg.Embedding.Dimensions; d <= 0 {
		return fmt.Errorf("embedding.dimensions: the width of the model's vectors, a positive number (768 for %s), got %d", DefaultEmbeddingModel, d)
	}
	return nil
}

func checkValueChecks(cfg *Config) error {
	for _, check := range valueChecks {
		if err := check.check(cfg); err != nil {
			return err
		}
	}
	return nil
}

var valueChecks = []struct {
	key   string
	check func(*Config) error
	reset func(*Config) any
}{
	{"context.abstain_cosine", func(cfg *Config) error {
		if v := float64(cfg.Context.AbstainCosine); math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || v > 1 {
			return fmt.Errorf("context.abstain_cosine: a cosine is between 0 and 1, got %v", cfg.Context.AbstainCosine)
		}
		return nil
	}, func(cfg *Config) any {
		cfg.Context.AbstainCosine = defaultConfig().Context.AbstainCosine
		return cfg.Context.AbstainCosine
	}},
	{"context.relevance_cutoff", func(cfg *Config) error {
		if v := cfg.Context.RelevanceCutoff; math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || v > 1 {
			return fmt.Errorf("context.relevance_cutoff: a cutoff is a fraction in [0,1] where 0 is off, got %v", cfg.Context.RelevanceCutoff)
		}
		return nil
	}, func(cfg *Config) any {
		cfg.Context.RelevanceCutoff = defaultConfig().Context.RelevanceCutoff
		return cfg.Context.RelevanceCutoff
	}},
	{noAnswerKey, func(cfg *Config) error {
		if v := cfg.Context.NoAnswerCosine; math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || v > 1 {
			return fmt.Errorf("context.no_answer_cosine: a cosine is in [0,1] where 0 is off, got %v", cfg.Context.NoAnswerCosine)
		}
		return nil
	}, func(cfg *Config) any {
		cfg.Context.NoAnswerCosine = defaultConfig().Context.NoAnswerCosine
		return cfg.Context.NoAnswerCosine
	}},
	{"search.min_similarity", func(cfg *Config) error {
		if v := cfg.Search.MinSimilarity; math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || v > 1 {
			return fmt.Errorf("search.min_similarity: a cosine floor is in [0,1] where 0 is off, got %v", cfg.Search.MinSimilarity)
		}
		return nil
	}, func(cfg *Config) any {
		cfg.Search.MinSimilarity = defaultConfig().Search.MinSimilarity
		return cfg.Search.MinSimilarity
	}},
	{"linking.threshold", func(cfg *Config) error {
		if v := cfg.Linking.Threshold; math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || v > 1 {
			return fmt.Errorf("linking.threshold: a cosine is in [0,1], got %v", cfg.Linking.Threshold)
		}
		return nil
	}, func(cfg *Config) any {
		cfg.Linking.Threshold = defaultConfig().Linking.Threshold
		return cfg.Linking.Threshold
	}},
}

func checkScopeValues(cfg *Config) error {
	keys := make([]string, 0, len(cfg.Injection.SessionScope))
	for key, value := range cfg.Injection.SessionScope {
		if strings.TrimSpace(value) == "" {
			keys = append(keys, key)
		}
	}
	if len(keys) == 0 {
		return nil
	}
	slices.Sort(keys)
	return fmt.Errorf("injection.session_scope: empty value for %s", strings.Join(keys, ", "))
}

func loadEnvLayer(k *koanf.Koanf) error {
	var errs []error

	generic := make(map[string]interface{})
	for _, kv := range os.Environ() {
		name, value, ok := strings.Cut(kv, "=")
		if !ok || !strings.HasPrefix(name, envPrefix) {
			continue
		}
		key := strings.ToLower(strings.ReplaceAll(strings.TrimPrefix(name, envPrefix), "_", "."))
		if t, bound := boundKeyTypes[key]; bound && value != "" {
			parsed, err := parseEnvValue(t, value)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", name, err))
				continue
			}
			generic[key] = parsed
			continue
		}
		generic[key] = value
	}
	if len(generic) > 0 {
		if err := k.Load(confmap.Provider(generic, "."), nil); err != nil {
			errs = append(errs, err)
		}
	}

	for _, ov := range envOverrides {
		raw := os.Getenv(ov.env)
		if raw == "" {
			continue
		}
		val, err := ov.parse(raw)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", ov.env, err))
			continue
		}
		if err := k.Load(confmap.Provider(map[string]interface{}{
			ov.key: val,
		}, "."), nil); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", ov.env, err))
		}
	}
	return errors.Join(errs...)
}

const envPrefix = "GHOST_"

func parseEnvValue(t reflect.Type, s string) (interface{}, error) {
	switch t.Kind() {
	case reflect.String:
		return s, nil
	case reflect.Bool:
		return strconv.ParseBool(s)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return strconv.Atoi(s)
	case reflect.Float32, reflect.Float64:
		return strconv.ParseFloat(s, 64)
	default:
		return nil, fmt.Errorf("cannot be set from the environment (%s)", t.Kind())
	}
}

// LoadForHook loads configuration for a hook running inside someone else's
// editor session (SessionStart injection, obsidian auto-sync, stop-hook
func LoadForHook() *Config {
	cfg, err := Load()
	if err == nil {
		return cfg
	}
	warnf("%v — falling back to the environment and built-in defaults", err)
	return FallbackConfig()
}

// FallbackConfig returns the Config for callers that must not fail on a broken
// config file (LoadForHook, and the MCP server via cmd/ghost's bootstrap), so
func FallbackConfig() *Config {
	if cfg, ok := decodeFallback(loadEnvLayer); ok {
		return cfg
	}
	cfg, ok := decodeFallback(func(*koanf.Koanf) error { return nil })
	if !ok || cfg == nil {
		return defaultConfig()
	}
	return cfg
}

func defaultConfig() *Config {
	return &Config{
		Embedding: EmbeddingConfig{
			Enabled:    true,
			OllamaURL:  "http://localhost:11434",
			Model:      DefaultEmbeddingModel,
			Dimensions: 768,
		},
		Reflection: ReflectionConfig{
			ConsolidationTimeoutMinutes: DefaultConsolidationTimeoutMinutes,
			LifecycleTimeoutMinutes:     DefaultLifecycleTimeoutMinutes,
			SupersedeConsensus:          3,
		},
		Lifecycle: LifecycleConfig{MinInterval: defaultLifecycleMinInterval},
		Linking: LinkingConfig{
			Enabled:           true,
			Threshold:         0.70,
			DemotionThreshold: 0.90,
		},
		Injection: DefaultInjectionConfig(),
		Obsidian:  ObsidianConfig{Interval: "30s"},
		Scratch:   ScratchConfig{MaxBytes: DefaultScratchMaxBytes},
		Context:   ContextConfig{RelevanceCutoff: DefaultRelevanceCutoff, NoAnswerCosine: DefaultNoAnswerCosine},
	}
}

func decodeFallback(applyEnv func(*koanf.Koanf) error) (*Config, bool) {
	k := koanf.New(".")
	if err := k.Load(confmap.Provider(defaults, "."), nil); err != nil {
		warnf("compiled defaults are unusable: %v", err)
		return nil, false
	}
	k.Delete(noAnswerKey)
	if err := applyEnv(k); err != nil {
		warnf("%v — those variables were skipped", err)
	}
	cfg := &Config{}
	if err := k.Unmarshal("", cfg); err != nil {
		warnf("%v — using the built-in defaults", err)
		return nil, false
	}
	explicitBar := k.Exists(noAnswerKey)
	for _, c := range valueChecks {
		err := c.check(cfg)
		if err == nil {
			continue
		}
		warnf("%v — using the built-in default %v", err, c.reset(cfg))
		if c.key == noAnswerKey {
			explicitBar = false
		}
	}
	if !explicitBar {
		cfg.Context.NoAnswerCosine = DefaultNoAnswerCosine
	}
	resolveNoAnswerBar(cfg, explicitBar)
	if err := checkEmbeddingValues(cfg); err != nil {
		def := defaultConfig().Embedding.Dimensions
		warnf("%v — using the built-in default %d", err, def)
		cfg.Embedding.Dimensions = def
	}
	return cfg, true
}

var warnSink = newWarnSink(os.Stderr)

func newWarnSink(w io.Writer) *atomic.Pointer[io.Writer] {
	p := new(atomic.Pointer[io.Writer])
	p.Store(&w)
	return p
}

// SetWarningWriter redirects configuration warnings to w, and returns a function
// that restores the previous sink. A nil w keeps the current one.
func SetWarningWriter(w io.Writer) (restore func()) {
	if w == nil {
		return func() {}
	}
	prev := warnSink.Load()
	replacement := io.Writer(w)
	warnSink.Store(&replacement)
	return func() { warnSink.Store(prev) }
}

var warned = struct {
	sync.Mutex
	seen map[string]struct{}
}{seen: map[string]struct{}{}}

func resetWarned() {
	warned.Lock()
	defer warned.Unlock()
	warned.seen = map[string]struct{}{}
}

func warnf(format string, args ...interface{}) {
	msg := fmt.Sprintf("ghost: config: "+format, args...)

	warned.Lock()
	if _, dup := warned.seen[msg]; dup {
		warned.Unlock()
		return
	}
	warned.seen[msg] = struct{}{}
	warned.Unlock()

	_, _ = fmt.Fprintln(*warnSink.Load(), msg)
}

var knownKeys = collectKeys(reflect.TypeFor[Config]())

var boundKeyTypes = collectKeyTypes(reflect.TypeFor[Config]())

func collectKeyTypes(t reflect.Type) map[string]reflect.Type {
	out := make(map[string]reflect.Type)
	var walk func(reflect.Type, string)
	walk = func(t reflect.Type, prefix string) {
		for i := range t.NumField() {
			f := t.Field(i)
			name := f.Tag.Get("koanf")
			if name == "" {
				continue
			}
			key := name
			if prefix != "" {
				key = prefix + "." + name
			}
			if f.Type.Kind() == reflect.Struct {
				walk(f.Type, key)
				continue
			}
			out[key] = f.Type
		}
	}
	walk(t, "")
	return out
}

func collectKeys(t reflect.Type) map[string]struct{} {
	out := make(map[string]struct{})
	var walk func(reflect.Type, string)
	walk = func(t reflect.Type, prefix string) {
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			name := f.Tag.Get("koanf")
			if name == "" {
				continue
			}
			key := name
			if prefix != "" {
				key = prefix + "." + name
			}
			if f.Type.Kind() == reflect.Struct {
				walk(f.Type, key)
				continue
			}
			out[key] = struct{}{}
		}
	}
	walk(t, "")
	return out
}

func warnUnknownKeys(file map[string]interface{}, path string) {
	var unknown []string
	var walk func(map[string]interface{}, string)
	walk = func(m map[string]interface{}, prefix string) {
		for key, val := range m {
			full := key
			if prefix != "" {
				full = prefix + "." + key
			}
			if sub, ok := val.(map[string]interface{}); ok {
				walk(sub, full)
				continue
			}
			if !isKnownKey(full) {
				unknown = append(unknown, full)
			}
		}
	}
	walk(file, "")

	if len(unknown) == 0 {
		return
	}
	slices.Sort(unknown)
	warnf("%s: unknown key(s) ignored: %s", path, strings.Join(unknown, ", "))
}

func isKnownKey(key string) bool {
	for {
		if _, ok := knownKeys[key]; ok {
			return true
		}
		i := strings.LastIndex(key, ".")
		if i < 0 {
			return false
		}
		key = key[:i]
	}
}

type envOverride struct {
	env   string
	key   string
	parse func(string) (interface{}, error)
}

func stringValue(s string) (interface{}, error) { return s, nil }

func boolValue(s string) (interface{}, error) {
	v, err := strconv.ParseBool(s)
	if err != nil {
		return nil, err
	}
	return v, nil
}

func intValue(s string) (interface{}, error) {
	v, err := strconv.Atoi(s)
	if err != nil {
		return nil, err
	}
	return v, nil
}

func floatValue(s string) (interface{}, error) {
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return nil, err
	}
	return v, nil
}

func cosineValue(s string) (interface{}, error) {
	v, err := strconv.ParseFloat(s, 32)
	if err != nil {
		return nil, err
	}
	switch {
	case math.IsNaN(v) || math.IsInf(v, 0):
		return nil, fmt.Errorf("not a cosine: %s", s)
	case v < 0 || v > 1:
		return nil, fmt.Errorf("a cosine is between 0 and 1, got %s", s)
	}
	return float32(v), nil
}

func commaList(s string) (interface{}, error) {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out, nil
}

func commaPairs(s string) ([][2]string, error) {
	var out [][2]string
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		k, v, ok := strings.Cut(p, "=")
		if k, v = strings.TrimSpace(k), strings.TrimSpace(v); !ok || k == "" {
			return nil, fmt.Errorf("expected key=value, got %q", p)
		}
		out = append(out, [2]string{k, v})
	}
	return out, nil
}

func floatMap(s string) (interface{}, error) {
	pairs, err := commaPairs(s)
	if err != nil {
		return nil, err
	}
	out := make(map[string]float64, len(pairs))
	for _, p := range pairs {
		f, err := strconv.ParseFloat(p[1], 64)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", p[0], err)
		}
		out[p[0]] = f
	}
	return out, nil
}

func intMap(s string) (interface{}, error) {
	pairs, err := commaPairs(s)
	if err != nil {
		return nil, err
	}
	out := make(map[string]int, len(pairs))
	for _, p := range pairs {
		n, err := strconv.Atoi(p[1])
		if err != nil {
			return nil, fmt.Errorf("%s: %w", p[0], err)
		}
		out[p[0]] = n
	}
	return out, nil
}

func stringMap(s string) (interface{}, error) {
	pairs, err := commaPairs(s)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(pairs))
	for _, p := range pairs {
		if p[1] == "" {
			return nil, fmt.Errorf("%s: empty value", p[0])
		}
		out[p[0]] = p[1]
	}
	return out, nil
}

var envOverrides = []envOverride{
	{"GHOST_OBSIDIAN_VAULT_DIR", "obsidian.vault_dir", stringValue},
	{"GHOST_CLI_CLAUDE_BINARY", "cli.claude_binary", stringValue},
	{"GHOST_CLI_OPENCODE_BINARY", "cli.opencode_binary", stringValue},
	{"GHOST_CLI_CODEX_BINARY", "cli.codex_binary", stringValue},
	{"GHOST_CLI_GOOSE_BINARY", "cli.goose_binary", stringValue},
	{"GHOST_CLI_MODEL_REFLECT", "cli.model_reflect", stringValue},
	{"GHOST_CLI_MODEL_RESOLVE", "cli.model_resolve", stringValue},
	{"GHOST_CLI_MODEL_SUPERSEDE", "cli.model_supersede", stringValue},
	{"GHOST_REFLECTION_AUTO_REFLECT", "reflection.auto_reflect", boolValue},
	{"GHOST_REFLECTION_AUTO_RESOLVE", "reflection.auto_resolve", boolValue},
	{"GHOST_REFLECTION_AUTO_SUPERSEDE", "reflection.auto_supersede", boolValue},
	{"GHOST_REFLECTION_SUPERSEDE_CONSENSUS", "reflection.supersede_consensus", intValue},
	{"GHOST_REFLECTION_LIFECYCLE_TIMEOUT_MINUTES", "reflection.lifecycle_timeout_minutes", intValue},
	{"GHOST_REFLECTION_CONSOLIDATION_TIMEOUT_MINUTES", "reflection.consolidation_timeout_minutes", intValue},
	{"GHOST_LIFECYCLE_MIN_INTERVAL", "lifecycle.min_interval", stringValue},
	{"GHOST_OLLAMA_URL", "embedding.ollama_url", stringValue},
	{"GHOST_ROUTING_DEFAULT_PROJECT", "routing.default_project", stringValue},
	{"GHOST_SEARCH_MIN_SIMILARITY", "search.min_similarity", floatValue},
	{"GHOST_CONTEXT_ABSTAIN_COSINE", "context.abstain_cosine", cosineValue},
	{"GHOST_CONTEXT_RELEVANCE_CUTOFF", "context.relevance_cutoff", floatValue},
	{"GHOST_CONTEXT_NO_ANSWER_COSINE", "context.no_answer_cosine", floatValue},
	{"GHOST_SCRATCH_MAX_BYTES", "scratch.max_bytes", intValue},
	{"GHOST_LINKING_DEMOTION_THRESHOLD", "linking.demotion_threshold", floatValue},
	{"GHOST_OBSIDIAN_AUTO_SYNC", "obsidian.auto_sync", boolValue},
	{"GHOST_INJECTION_BEHAVIOR_FLOOR", "injection.behavior_floor", intValue},
	{"GHOST_INJECTION_BEHAVIOR_CATEGORIES", "injection.behavior_categories", commaList},
	{"GHOST_INJECTION_CATEGORY_WEIGHTS", "injection.category_weights", floatMap},
	{"GHOST_INJECTION_CATEGORY_CAPS", "injection.category_caps", intMap},
	{"GHOST_INJECTION_SESSION_SCOPE", "injection.session_scope", stringMap},
}

// DataDirPath returns the ghost data directory path WITHOUT creating it, so
// callers that must not leave a phantom directory behind (the stop hook's
func DataDirPath() (string, error) {
	dataHome := os.Getenv("XDG_DATA_HOME")
	if dataHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dataHome = filepath.Join(home, ".local", "share")
	}
	dir := filepath.Join(dataHome, "ghost")
	if err := CheckDevDataDir(buildVersion, dir); err != nil {
		return "", err
	}
	return dir, nil
}

var buildVersion = "dev"

// SetBuildVersion tells the config package which build it is part of, so the
// GHOST_DEV_FORBID_DATA_DIR refusal applies to development builds only.
func SetBuildVersion(v string) { buildVersion = v }

// BuildVersion returns the version the data-directory check judges this build
// by. It exists for the tests that need the guarded side without going through
func BuildVersion() string { return buildVersion }

// DataDir returns the ghost data directory, creating it if needed.
func DataDir() (string, error) {
	dir, err := DataDirPath()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

// ConfigFilePath returns the platform's user config path, without checking
// whether it exists or creating it.
func ConfigFilePath() (string, error) {
	configDir, err := userConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(configDir, "ghost", "config.yaml"), nil
}

// EnsureConfigFile creates the platform's user config path from the embedded
// example if it doesn't already exist. Returns the path and whether a new file
func EnsureConfigFile() (path string, created bool, err error) {
	path, err = ConfigFilePath()
	if err != nil {
		return "", false, err
	}

	if _, err := os.Stat(path); err == nil {
		return path, false, nil
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", false, err
	}
	if err := os.WriteFile(path, exampleConfig, 0o600); err != nil {
		return "", false, err
	}
	return path, true, nil
}

func loadConfigFile(k *koanf.Koanf, path string, parser koanf.Parser) error {
	raw, err := readConfigLayer(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		warnf("cannot read %s: %v — skipping it", path, err)
		return nil
	}
	parsed, err := parser.Unmarshal(raw)
	if err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	pruneNulls(parsed)
	warnUnknownKeys(parsed, path)
	if err := k.Load(confmap.Provider(parsed, "."), nil); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	return nil
}

func readConfigLayer(path string) ([]byte, error) {
	f, err := os.OpenFile(path, configOpenFlags, 0) // #nosec G304 -- the system and user config paths, not caller input
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("not a regular file (%s)", info.Mode().Type())
	}
	return io.ReadAll(f)
}

func pruneNulls(m map[string]interface{}) map[string]interface{} {
	for key, val := range m {
		if val == nil {
			delete(m, key)
			continue
		}
		if sub, ok := val.(map[string]interface{}); ok {
			pruneNulls(sub)
		}
	}
	return m
}

func userConfigDir() (string, error) {
	if dir := os.Getenv("XDG_CONFIG_HOME"); dir != "" {
		return dir, nil
	}
	return os.UserConfigDir()
}
