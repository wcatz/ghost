// Package config provides layered configuration for Ghost.
//
// Loading order (later layers override earlier):
//  1. Compiled defaults
//  2. /etc/ghost/config.yaml          (system-wide)
//  3. user config path (platform default; for example
//     ~/.config/ghost/config.yaml on Linux)
//  4. GHOST_* environment variables
//  5. CLI flag overrides (applied by caller after Load)
package config

import (
	_ "embed"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

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
	Linking    LinkingConfig    `koanf:"linking"`
	Injection  InjectionConfig  `koanf:"injection"`
	Search     SearchConfig     `koanf:"search"`
	Obsidian   ObsidianConfig   `koanf:"obsidian"`
	Routing    RoutingConfig    `koanf:"routing"`
	Scratch    ScratchConfig    `koanf:"scratch"`
}

// DefaultScratchMaxBytes is the compiled per-root scratch budget: 512 MiB.
// It is the value scratch.max_bytes takes when the key is unset, and the
// fallback when config loading itself fails.
const DefaultScratchMaxBytes int64 = 512 * 1024 * 1024

// ScratchConfig bounds the scratch root every harness spawn is confined to
// (internal/scratch: $GHOST_SCRATCH_DIR or <dataDir>/scratch).
type ScratchConfig struct {
	// MaxBytes is the per-root size budget enforced before each harness
	// spawn: over budget → stale entries are reaped first; still over budget
	// after the reap → a loud warning naming the root and the bytes-over, and
	// the spawn proceeds anyway (hygiene never blocks maintenance, but is
	// never quiet).
	//
	// 0 disables enforcement entirely — the explicit opt-out, distinct from
	// leaving the key unset, which keeps the 512 MiB default. A negative
	// value behaves as 0 (the check treats any non-positive budget as off).
	MaxBytes int64 `koanf:"max_bytes"`
}

// SearchConfig controls hybrid-search ranking behavior.
type SearchConfig struct {
	// MinSimilarity is the cosine floor applied to vector-leg candidates
	// before RRF fusion. 0 (default) preserves historical behavior of only
	// dropping non-positive cosines; raise to stop weak semantic matches from
	// padding the fused window. FTS candidates are exempt.
	MinSimilarity float64 `koanf:"min_similarity"`
}

// RoutingConfig steers sessions whose cwd matches no known project.
type RoutingConfig struct {
	// DefaultProject is the project (name or id, resolved at use time) that
	// home-dir and filesystem-root sessions fall back to for memory context.
	// Empty disables the fallback entirely — the historical default.
	DefaultProject string `koanf:"default_project"`
}

// CLIConfig holds explicit paths to the subprocess LLM binaries backing
// classification and consolidation. An empty value means "resolve from PATH";
// a set path overrides PATH lookup. This matters for auto_reflect: the Stop
// hook process's PATH is often not the interactive shell's, so a binary
// installed under ~/.opencode/bin (or similar) is invisible to exec.LookPath
// unless its path is configured explicitly here.
type CLIConfig struct {
	ClaudeBinary   string `koanf:"claude_binary"`
	OpenCodeBinary string `koanf:"opencode_binary"`
	CodexBinary    string `koanf:"codex_binary"`
	GooseBinary    string `koanf:"goose_binary"`

	// Per-phase harness model pins for the opencode backend (e.g.
	// "opencode/big-pickle"). Empty means "use the explicit Big Pickle default
	// in ai.OpenCodeClient". Each lifecycle phase runs as its own process, so a
	// pin cannot leak between phases. Ignored by the claude/codex/goose clients,
	// which have no model flag.
	ModelReflect   string `koanf:"model_reflect"`
	ModelResolve   string `koanf:"model_resolve"`
	ModelSupersede string `koanf:"model_supersede"`
}

// ReflectionConfig holds memory consolidation settings.
type ReflectionConfig struct {
	AutoResolve   bool `koanf:"auto_resolve"`
	AutoSupersede bool `koanf:"auto_supersede"`
	AutoReflect   bool `koanf:"auto_reflect"`
	// ConsolidationTimeoutMinutes bounds a single `ghost reflect`
	// consolidation call. It was hardcoded at 3 minutes, which the LLM tier
	// hits on a large project: a ~190-memory prompt takes about that long on
	// the opencode backend, so runs were killed mid-flight ("opencode run:
	// signal: killed") and — because the autonomous path passes --require-llm —
	// the whole reflect then failed with no fallback. 0 disables the bound.
	// Keep it below reflection.lifecycle_timeout_minutes, which bounds the
	// outer lifecycle phase that runs this command.
	ConsolidationTimeoutMinutes int `koanf:"consolidation_timeout_minutes"`

	// LifecycleTimeoutMinutes bounds each phase of the auto-consolidation
	// chain (reflect, resolve, supersede). The default is generous but finite
	// (see defaults): the stop hook guards the chain with one per-project PID
	// file keyed to the detached parent's liveness, so a phase that hung with
	// no bound would wedge auto-consolidation for that project until the
	// process was killed by hand. A bound lets a stuck run expire and be
	// retried next session. Set 0 to disable the bound entirely; the timeout
	// is a graceful SIGTERM to the phase's process group first, escalating
	// only if the grace period expires.
	LifecycleTimeoutMinutes int `koanf:"lifecycle_timeout_minutes"`
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
// hard-to-derive categories (gotcha/convention/preference/decision) without
// growing the total footprint. behavior_floor of 0 disables the bias entirely
// (pure DecayRankingSQL selection, the historical behavior). category_caps
// bounds how many pass-1 reserved slots a single behavioral category may take,
// so a gotcha-heavy corpus cannot fill every guaranteed slot with gotchas
// (a 46% gotcha share would otherwise dominate the floor).
type InjectionConfig struct {
	BehaviorFloor      int                `koanf:"behavior_floor"`
	BehaviorCategories []string           `koanf:"behavior_categories"`
	CategoryWeights    map[string]float64 `koanf:"category_weights"`
	CategoryCaps       map[string]int     `koanf:"category_caps"`
}

// DefaultInjectionConfig returns the compiled injection defaults. It mirrors the
// injection.* entries in the defaults map so callers that need a fallback when
// Load() fails (e.g. the SessionStart hook's read-only path) never diverge from
// the layered defaults.
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

// defaults is the base layer — always loaded first.
var defaults = map[string]interface{}{
	"embedding.enabled":                        true,
	"embedding.ollama_url":                     "http://localhost:11434",
	"embedding.model":                          "nomic-embed-text:v1.5",
	"embedding.dimensions":                     768,
	"reflection.auto_resolve":                  false,
	"reflection.auto_supersede":                false,
	"reflection.auto_reflect":                  false,
	"reflection.lifecycle_timeout_minutes":     60,
	"reflection.consolidation_timeout_minutes": 10,
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
	"obsidian.vault_dir":                       "",
	"obsidian.interval":                        "30s",
	"obsidian.auto_sync":                       false,
	"routing.default_project":                  "",
	"scratch.max_bytes":                        DefaultScratchMaxBytes,
}

// systemConfigPath is the system-wide config layer. Named so the load path and
// the errors it produces say the same file.
const systemConfigPath = "/etc/ghost/config.yaml"

// Load reads configuration with layered precedence.
// After Load returns, the caller may apply CLI flag overrides by mutating
// fields directly.
//
// A config file that exists but does not parse is an error, not a silent
// fallback to the compiled defaults: one typo used to leave every key at its
// default with nothing to explain why. Callers that must not fail — the
// host-session hooks — use LoadForHook instead, which reports the same problem
// as a warning and keeps the defaults. A file that exists but cannot be read is
// only a warning; see loadFileIfExists for why the two differ.
//
// Unknown keys in a config file are reported the same way but do not fail the
// load, because a key Ghost does not bind is harmless to everything that does.
func Load() (*Config, error) {
	k := koanf.New(".")

	// Layer 1: compiled defaults.
	if err := k.Load(confmap.Provider(defaults, "."), nil); err != nil {
		return nil, err
	}

	parser := yaml.Parser()

	// Layer 2: /etc/ghost/config.yaml (system-wide).
	if err := loadConfigFile(k, systemConfigPath, parser); err != nil {
		return nil, err
	}

	// Layer 3: the platform's user config path (user-global).
	if configDir, err := userConfigDir(); err == nil {
		if err := loadConfigFile(k, filepath.Join(configDir, "ghost", "config.yaml"), parser); err != nil {
			return nil, err
		}
	}

	// Layer 4: GHOST_* environment variables.
	// e.g. GHOST_CLI_CLAUDE_BINARY → cli.claude_binary (see envOverrides below).
	if err := loadEnvLayer(k); err != nil {
		return nil, err
	}

	cfg := &Config{}
	if err := k.Unmarshal("", cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

// loadEnvLayer applies the GHOST_* environment variables to k: the generic
// GHOST_ prefix + "_"→"." mapping first, then the explicit envOverrides
// shortcuts for the keys that transformer cannot reach.
//
// Both passes skip an unreadable value and keep going, collecting every failure
// into one error. Returning at the first bad value hid the rest behind it — the
// one behind it was a good value, silently dropped — and, in the generic pass,
// discarding the whole layer: the bulk env provider loads every variable as a
// string, so a single value koanf cannot convert surfaces much later as a decode
// error naming no variable at all, and took GHOST_EMBEDDING_ENABLED=false down
// with it. Each error names the variable that carried it.
func loadEnvLayer(k *koanf.Koanf) error {
	var errs []error

	// Pass 1: the generic mapping, built one variable at a time. A value bound
	// to a Config field is converted here, so an unconvertible one can be
	// skipped and named rather than failing the decode of everything else.
	generic := make(map[string]interface{})
	for _, kv := range os.Environ() {
		name, value, ok := strings.Cut(kv, "=")
		if !ok || !strings.HasPrefix(name, envPrefix) {
			continue
		}
		key := strings.ToLower(strings.ReplaceAll(strings.TrimPrefix(name, envPrefix), "_", "."))
		// An empty value is left as the string it has always been: the generic
		// mapping has no empty-value guard, and adding one would silently change
		// what `GHOST_EMBEDDING_ENABLED=` means.
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

	// Pass 2: the explicit shortcuts, which win over the generic mapping.
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

// envPrefix is the namespace the generic GHOST_ mapping claims.
const envPrefix = "GHOST_"

// parseEnvValue converts one GHOST_* value to the type its Config field decodes
// into. A field that is not a scalar (a slice or a map) cannot come from the
// environment at all — that is what the envOverrides parsers are for — so it is
// reported rather than guessed at.
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
// reflection, session routing). A broken config must not fail the host's
// session, so the error is reported on stderr and FallbackConfig is returned:
// the session still gets its context, and the user still finds out why their
// settings are not taking effect.
//
// CLI subcommands use Load instead, and fail with the same error.
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
// one broken file can never change which values those paths use.
//
// It is every layer that does not read a config file: the compiled defaults
// plus the GHOST_* environment. Keeping the env layer is not a nicety — before
// the parse error was surfaced at all, a malformed file was skipped and the load
// carried on to the environment, so returning the defaults alone would
// silently undo an operator's opt-out (GHOST_EMBEDDING_ENABLED=false,
// GHOST_SCRATCH_MAX_BYTES=0) and hand it back to them as the opposite.
//
// A GHOST_* value that cannot be read is reported and skipped rather than
// returned as an error, because this path has no way to fail. Every such
// variable is skipped, not only the first: loadEnvLayer collects them all, so
// one bad value no longer costs the operator the rest of their environment.
//
// It never returns a near-zero Config. Merging the environment in makes the
// unmarshal step reachable for a reason the defaults-only version did not have:
// koanf's generic env provider stores every value as a string, so one that
// cannot be weakly converted to its field's type (GHOST_EMBEDDING_DIMENSIONS=abc)
// fails the whole decode. Returning that partial Config would start the server
// with embeddings and linking off, a zero demotion threshold, and — worst —
// reflection.lifecycle_timeout_minutes=0, the unbounded lifecycle the compiled
// defaults exist to avoid. So a decode failure drops the environment and keeps
// the defaults, loudly, down to the last resort: even if the defaults layer
// itself cannot be decoded, the result is defaultConfig() — the same values,
// written out — never a struct of Go zero values.
func FallbackConfig() *Config {
	if cfg, ok := decodeFallback(loadEnvLayer); ok {
		return cfg
	}
	// The defaults map is a literal that always decodes, so this retry cannot
	// fail in practice. It is still checked: decodeFallback reports failure with
	// a nil *Config*, and a panic in the one function that exists to absorb a
	// broken config is precisely the failure mode that must not be reachable.
	cfg, ok := decodeFallback(func(*koanf.Koanf) error { return nil })
	if !ok || cfg == nil {
		return defaultConfig()
	}
	return cfg
}

// defaultConfig is the last resort: the compiled defaults written out, for the
// case where even the defaults layer will not decode. It mirrors the defaults
// map the way DefaultInjectionConfig mirrors its injection.* entries, and
// TestDefaultConfig_MatchesTheDefaultsMap fails if the two drift, so adding a
// key to one without the other cannot reach a machine that needs this.
func defaultConfig() *Config {
	return &Config{
		Embedding: EmbeddingConfig{
			Enabled:    true,
			OllamaURL:  "http://localhost:11434",
			Model:      "nomic-embed-text:v1.5",
			Dimensions: 768,
		},
		Reflection: ReflectionConfig{
			ConsolidationTimeoutMinutes: 10,
			LifecycleTimeoutMinutes:     60,
		},
		Linking: LinkingConfig{
			Enabled:           true,
			Threshold:         0.70,
			DemotionThreshold: 0.90,
		},
		Injection: DefaultInjectionConfig(),
		Obsidian:  ObsidianConfig{Interval: "30s"},
		Scratch:   ScratchConfig{MaxBytes: DefaultScratchMaxBytes},
	}
}

// decodeFallback builds the Config from the compiled defaults with env applied
// through applyEnv, reporting whether the result decoded. applyEnv is a
// parameter so the env-free retry does not have to undo anything koanf already
// merged.
func decodeFallback(applyEnv func(*koanf.Koanf) error) (*Config, bool) {
	k := koanf.New(".")
	if err := k.Load(confmap.Provider(defaults, "."), nil); err != nil {
		warnf("compiled defaults are unusable: %v", err)
		return nil, false
	}
	if err := applyEnv(k); err != nil {
		warnf("%v — those variables were skipped", err)
	}
	cfg := &Config{}
	if err := k.Unmarshal("", cfg); err != nil {
		warnf("%v — using the built-in defaults", err)
		return nil, false
	}
	return cfg, true
}

// warnSink is where configuration warnings go. It is atomic because the sink is
// replaced at MCP-server startup while other goroutines are already running, and
// a two-word io.Writer read racing a write can tear.
var warnSink = newWarnSink(os.Stderr)

func newWarnSink(w io.Writer) *atomic.Pointer[io.Writer] {
	p := new(atomic.Pointer[io.Writer])
	p.Store(&w)
	return p
}

// SetWarningWriter redirects configuration warnings to w, and returns a function
// that restores the previous sink. A nil w keeps the current one.
//
// It exists for the MCP server, whose stderr belongs to the client protocol:
// cmd/ghost points the sink at the same writer mcpLogConfig returns, so setting
// GHOST_LOG_FILE keeps config warnings out of the client's face. That is not
// cosmetic here — the hook-path warnings are not once per process. Every
// harness spawn goes through scratch.EnforceBudget → LoadForHook inside the
// server, so an unredirected sink would emit one protocol-noise line per spawn.
//
// Safe to call concurrently with in-flight warnings; nothing here needs a
// "call it before starting goroutines" precondition.
func SetWarningWriter(w io.Writer) (restore func()) {
	if w == nil {
		return func() {}
	}
	prev := warnSink.Load()
	replacement := io.Writer(w)
	warnSink.Store(&replacement)
	return func() { warnSink.Store(prev) }
}

// warned records the messages already reported. Configuration is loaded more
// than once per process — a SessionStart hook and a status check in one session,
// and once per harness spawn on the spawn path — so an un-deduped warning
// repeats a line the user has already read, which trains them to skip it.
var warned = struct {
	sync.Mutex
	seen map[string]struct{}
}{seen: map[string]struct{}{}}

// resetWarned clears the dedup set. For tests: one process asserting the same
// warning twice would otherwise see it once.
func resetWarned() {
	warned.Lock()
	defer warned.Unlock()
	warned.seen = map[string]struct{}{}
}

// warnf reports a non-fatal configuration problem, once per distinct message per
// process. Layered config is loaded from inside host-session hooks that must
// not fail, so the problems that must not stop the caller are reported here
// rather than returned. A different problem is still reported, so the dedup
// cannot hide a second problem behind the first.
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

// knownKeys is every koanf key the Config struct binds, derived once from its
// struct tags so it cannot drift as fields are added.
var knownKeys = collectKeys(reflect.TypeFor[Config]())

// boundKeyTypes is the type each bound key decodes into, so a GHOST_* value can
// be converted — and an unconvertible one caught and named — before it reaches
// the decode of the whole config.
var boundKeyTypes = collectKeyTypes(reflect.TypeFor[Config]())

// collectKeyTypes is collectKeys keeping the field type, for the leaves.
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

// collectKeys walks a config struct's koanf tags into the flat key set koanf
// itself uses. Only a nested struct keeps descending; maps, slices and scalars
// are leaves as far as the key set is concerned, because koanf addresses them
// by the key that holds the collection.
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

// warnUnknownKeys reports the leaf keys a config file sets that no Config field
// binds, so a typo is never a silent no-op.
//
// It walks the file's own tree rather than koanf's flat key set, because a
// section is not a candidate: `injection:` binds no field of its own — only its
// children do — so reporting one would call every ordinary section header a
// typo. Walking through sections is also what makes an ancestor of a known key
// known, which is the rule that stops a bare `injection:` (pruned as null before
// it got here, but reachable through a header that also has a child) from being
// reported, while leaving a genuine typo under the same section visible.
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

// isKnownKey reports whether key is one Config binds, or a child of one. The
// walk is what makes a map value's entries count as known: koanf flattens
// injection.category_weights into one key per entry
// (injection.category_weights.gotcha), and a typo in the parent
// (…category_weight.gotcha) still walks up to nothing that is bound.
//
// Note what it deliberately does NOT do: walk DOWN to a known section. Treating
// every ancestor as known would make `linking.thresholdd` a non-typo for the
// same reason `linking` is, which is the whole job of the check.
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

// envOverride names a GHOST_* variable the generic GHOST_ prefix plus "_"→"."
// transformer cannot map onto a config key, together with the parser that turns
// its raw string into the Go type the Config field needs.
//
// Two distinct gaps are covered:
//
//   - a koanf tag that itself contains "_" (obsidian.vault_dir), which the
//     transformer would split into obsidian.vault.dir; and
//   - a non-scalar field (injection.behavior_categories and the two injection
//     maps), which koanf's weakly-typed decode cannot build from a bare string:
//     "gotcha,decision" would arrive as the single element ["gotcha,decision"],
//     and a string never becomes a map at all.
type envOverride struct {
	env   string
	key   string
	parse func(string) (interface{}, error)
}

// stringValue passes a string through untouched. Every parser below names the
// type it produces, so a value that cannot be read as its key's type is
// reported against the variable that carried it instead of surfacing much later
// as an unmarshal error that only names the key.
func stringValue(s string) (interface{}, error) { return s, nil }

// boolValue parses a Go boolean ("true", "1", "off").
func boolValue(s string) (interface{}, error) {
	v, err := strconv.ParseBool(s)
	if err != nil {
		return nil, err
	}
	return v, nil
}

// intValue parses a base-10 integer.
func intValue(s string) (interface{}, error) {
	v, err := strconv.Atoi(s)
	if err != nil {
		return nil, err
	}
	return v, nil
}

// floatValue parses a 64-bit float.
func floatValue(s string) (interface{}, error) {
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return nil, err
	}
	return v, nil
}

// commaList parses "a,b,c" into the []string a config key expects.
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

// commaPairs splits "key=value,key=value" on commas, then on the first "=" of
// each pair.
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

// floatMap parses "key=value,..." into the map[string]float64 a config key
// expects, naming the offending key when a value is not a number.
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

// intMap parses "key=value,..." into the map[string]int a config key expects.
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

// envOverrides is a slice rather than a map so the order the variables are
// applied in is fixed instead of randomized by map iteration.
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
	{"GHOST_REFLECTION_LIFECYCLE_TIMEOUT_MINUTES", "reflection.lifecycle_timeout_minutes", intValue},
	{"GHOST_REFLECTION_CONSOLIDATION_TIMEOUT_MINUTES", "reflection.consolidation_timeout_minutes", intValue},
	{"GHOST_OLLAMA_URL", "embedding.ollama_url", stringValue},
	{"GHOST_ROUTING_DEFAULT_PROJECT", "routing.default_project", stringValue},
	{"GHOST_SEARCH_MIN_SIMILARITY", "search.min_similarity", floatValue},
	// GHOST_SCRATCH_MAX_BYTES: the generic _→. transformer would produce
	// scratch.max.bytes, missing the max_bytes key entirely.
	{"GHOST_SCRATCH_MAX_BYTES", "scratch.max_bytes", intValue},
	{"GHOST_LINKING_DEMOTION_THRESHOLD", "linking.demotion_threshold", floatValue},
	{"GHOST_OBSIDIAN_AUTO_SYNC", "obsidian.auto_sync", boolValue},
	{"GHOST_INJECTION_BEHAVIOR_FLOOR", "injection.behavior_floor", intValue},
	// The list and the two maps need parsing, not just remapping — see
	// envOverride's doc comment.
	{"GHOST_INJECTION_BEHAVIOR_CATEGORIES", "injection.behavior_categories", commaList},
	{"GHOST_INJECTION_CATEGORY_WEIGHTS", "injection.category_weights", floatMap},
	{"GHOST_INJECTION_CATEGORY_CAPS", "injection.category_caps", intMap},
}

// DataDirPath returns the ghost data directory path WITHOUT creating it, so
// callers that must not leave a phantom directory behind (the stop hook's
// no-LLM skip, marker reads) can still locate the store.
func DataDirPath() (string, error) {
	dataHome := os.Getenv("XDG_DATA_HOME")
	if dataHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dataHome = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(dataHome, "ghost"), nil
}

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
// was created.
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

// loadConfigFile loads a config file into koanf and reports the keys in it
// that Config does not bind. A file that is not there is
// not an error — the layer is optional — and neither is one that cannot be
// READ: a root-owned /etc/ghost/config.yaml, or a user file with the wrong
// mode, is not a configuration mistake worth failing every command over, and
// the main branch behaved that way. It is warned about and skipped.
//
// A file that is readable but does not PARSE is an error naming the path, and
// that is the distinction worth stopping for: the user wrote a typo, and will
// keep not getting the settings they asked for until someone says so.
func loadConfigFile(k *koanf.Koanf, path string, parser koanf.Parser) error {
	raw, err := os.ReadFile(path)
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

// pruneNulls drops the keys whose YAML value is null, in place. A section
// header with no children — `injection:` on a line of its own — parses to null,
// and koanf merges that null over the defaults map, erasing the whole subtree:
// a user who opened their config to change one setting and left the header bare
// would silently lose every other value under it. Dropping the key leaves the
// defaults standing, which is what an absent section has always meant.
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

// userConfigDir returns the base user config directory, honoring XDG_CONFIG_HOME
// when set. Unlike os.UserConfigDir — which ignores XDG_CONFIG_HOME on macOS —
// this keeps the config path consistent with DataDir's XDG_DATA_HOME handling
// and lets tests point XDG_CONFIG_HOME at a temp dir on every platform.
func userConfigDir() (string, error) {
	if dir := os.Getenv("XDG_CONFIG_HOME"); dir != "" {
		return dir, nil
	}
	return os.UserConfigDir()
}
