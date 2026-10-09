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

// The lifecycle cooldown (#541). Kept as a duration STRING in Config — the key
// is written by a human in YAML — and resolved to a time.Duration at the point
// of use, exactly as obsidian.interval is.
const (
	// defaultLifecycleMinInterval is the compiled value of
	// lifecycle.min_interval. It is the string literal, because the defaults map
	// and defaultConfig's hand-written mirror are compared by
	// TestDefaultConfig_MatchesTheDefaultsMap and must carry the same text.
	defaultLifecycleMinInterval = "30m"
	// DefaultLifecycleMinInterval is that default as a duration, for callers
	// that need the number rather than the key. TestDefaultLifecycleMinInterval_
	// MatchesItsLiteral fails if the two ever disagree.
	DefaultLifecycleMinInterval = 30 * time.Minute
)

// LifecycleConfig controls when the Stop hook may spawn the auto-consolidation
// chain. It is a separate section from reflection.* on purpose: reflection.*
// says WHICH phases run, this says HOW OFTEN.
type LifecycleConfig struct {
	// MinInterval is the shortest gap between two lifecycle STARTS for one
	// project. The Stop hook fires after every turn, so without a cooldown each
	// turn paid for a full reflect→resolve→supersede chain — issue #541 counted
	// 543 runs in one lifecycle.log — even though reflect's input signature
	// skipped the unchanged set most of the time.
	//
	// A Go duration string ("30m", "1h30m"); "0" (or any non-positive value)
	// disables the cooldown and restores the old every-turn behavior. The
	// compiled default is 30m: long enough that a chatty session consolidates
	// about hourly, short enough that a project that saves memories in bursts
	// still consolidates while it is being worked on.
	MinInterval string `koanf:"min_interval"`
}

// MinIntervalDuration resolves MinInterval to the cooldown to enforce, for a
// caller that has a Config value in hand.
//
// A value that cannot be read falls back to the compiled default, NOT to zero.
// The distinction is the whole reason this lives in config rather than in the
// hook: zero means "no cooldown", so guessing it for a typo would silently switch
// off the guard the user asked for — the same near-zero-Config trap
// defaultConfig exists to prevent. A blank value is unset, not a mistake, and is
// silent. A non-positive interval means "off", matching scratch.max_bytes.
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
// fallback when config loading itself fails.
const DefaultScratchMaxBytes int64 = 512 * 1024 * 1024

// DefaultRelevanceCutoff is the compiled value of context.relevance_cutoff, the
// assembler's relative-to-top cutoff on query-mode blocks (#954). It lives here
// rather than as a bare literal in the defaults map so the bench that measures
// the cutoff (`ghost bench --context`, via ContextRequest) and the config layer
// that ships it read ONE number and cannot drift. A value of 0 is off; see
// ContextConfig.RelevanceCutoff.
//
// The number is chosen from the `ghost bench --cutoff-sweep` table in
// docs/benchmarks.md, and the trade it makes is explicit rather than tuned: on
// the graded corpus it admits 303 of the 304 baseline graded-relevant rows
// (the ship floor is 298), holds the answerable result rate at 1.000, raises
// context precision from 0.138 to 0.149 and lowers the estimated token cost per
// answer from about 297 to about 274. It is one step below the most aggressive
// share that still clears the relevant floor (0.64: 299 relevant, 0.156
// precision, 259 tokens), chosen for the five-row margin over the floor rather
// than for the last few points of precision — the floor is the hard constraint
// and a ship that clears it barely is a ship one corpus edit from failing it.
const DefaultRelevanceCutoff float64 = 0.63

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

// ContextConfig controls how the assembler reports an assembled block's
// relevance verdict.
type ContextConfig struct {
	// AbstainCosine is Arm B of the assembler's relevance floor (#580): a row
	// satisfies it when the vector leg's cosine is at least this value. 0
	// (default) leaves the arm OFF, which is the only honest default today —
	// the bench no-answer report shows the answerable and no-answer cosine
	// distributions overlap, so no constant separates them and a threshold set
	// here is a decision a user makes, not one Ghost infers. search.
	// min_similarity is a different floor and does not feed this: it is applied
	// inside the vector leg before fusion and therefore never sees a keyword-only
	// result, which is exactly the case arm B is here to judge.
	AbstainCosine float32 `koanf:"abstain_cosine"`
	// RelevanceCutoff is the relative cutoff applied in the assembler to
	// QUERY-mode blocks only (#954): once a row's fused score falls below this
	// fraction of the top row's, the answer stops there, so a caller gets fewer,
	// better rows instead of a window that always fills. It is a share of the
	// top row's score, so 1.0 keeps only rows that tie the top and a smaller
	// value keeps a longer tail; 0 (default) leaves the cutoff OFF, which is the
	// state every caller runs in until a measured default is chosen.
	//
	// It can only ever SHORTEN an answer: `limit` remains the maximum, the top
	// row is always kept (a result rate below 1.000 would be a regression) and a
	// pinned row is never cut. PASSIVE surfaces are a digest and keep their
	// slices, so the cutoff never touches them whatever this is set to.
	//
	// It is one rule and its parameter is this share, chosen from the
	// `ghost bench --context` sweep in docs/benchmarks.md. search.min_similarity
	// and AbstainCosine are different floors and neither feeds this: both judge a
	// row's absolute score, and this judges it against the best row in the same
	// block.
	RelevanceCutoff float64 `koanf:"relevance_cutoff"`
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
	// SupersedeConsensus is how many classification passes the AUTOMATIC
	// supersede phase must agree on before it writes an edge (#779). It is read
	// ONLY when auto_supersede is true, because the phase that would use it does
	// not otherwise run — a key that silently affected a hand-run
	// `ghost supersede` would make the automatic and manual paths differ for a
	// reason no command line shows.
	//
	// 1 means no gate, which is the ungated historical pass. The default is 3,
	// the number #779's measurement used: edges the classifier proposed in all
	// three of three dry runs scored 0.79 precision against 0.55 over distinct
	// proposals and 0.33 for one proposed in a single run, so unanimity across
	// three is the only subset that cleared a bar worth automating. A lower
	// default would be a gate chosen for costing less than the evidence supports,
	// and 2 is accepted because an operator who wants two is entitled to it.
	//
	// A value below supersede.MinConsensus is CLAMPED to the ungated pass rather
	// than refused, and that is a deliberate difference from the CLI flag. A typo
	// in a config file should not fail a lifecycle phase at 2am with a message
	// about quorum arithmetic; it should run the pass the operator already had.
	//
	// The clamp is NOT visible on the phase's own report — an ungated run prints
	// no gate line at all, so a clamped phase's stdout is byte-identical to one
	// with no key. It is visible on `ghost lifecycle`'s own stderr, beside the
	// reflect-skip notice and in the same voice, which on the unattended path is
	// the phase tail and the lifecycle.log beneath it. The boundary is
	// MinConsensus rather than 1: below it there is no gate, not a smaller one.
	//
	// It does NOT scale LifecycleTimeoutMinutes, and that is a decision rather
	// than an oversight. That bound catches a HUNG phase, so multiplying it by
	// the work factor makes it N times longer to notice a model that never
	// answers — which is how a hang detector stops being one — and there is no
	// principled factor to multiply by anyway, since the per-pass cost depends
	// on the corpus and the model. So N lands on the operator: a gated phase
	// multiplies its wall time against an unchanged deadline, and an expired one
	// can SIGTERM inside the apply block, whose per-pair writes are separate
	// transactions, leaving a PARTIAL WRITE, no report at all, and a
	// lifecycle-last-failure marker. The cost is documented in
	// config.example.yaml, docs/configuration.md and docs/cli.md rather than
	// absorbed silently.
	SupersedeConsensus int `koanf:"supersede_consensus"`
	// ConsolidationTimeoutMinutes bounds a single `ghost reflect`
	// consolidation call. It was hardcoded at 3 minutes, which the LLM tier
	// hits on a large project: a ~190-memory prompt takes about that long on
	// the opencode backend, so runs were killed mid-flight ("opencode run:
	// signal: killed") and — because the autonomous path passes --require-llm —
	// the whole reflect then failed with no fallback. 0 disables the bound.
	// It covers the whole consolidation, harness calls included: the LLM tier's
	// one repair turn (#689) spends what is left rather than getting a budget of
	// its own. Keep it below reflection.lifecycle_timeout_minutes, which bounds
	// the outer lifecycle phase that runs this command.
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
	// SessionScope is the scope the injected block is selected under
	// (injection.session_scope). It is empty by default and deliberately has no
	// compiled default: unset means the session-start surface applies no scope
	// predicate and shows every scoped row it would otherwise have shown. A
	// non-empty value is a request scope, and is matched with memory.ScopeMatches
	// — the rule search applies, through assemble.ScopeContradicts — so a memory
	// that does not mention a requested key still applies. The linker and the
	// dedup folds ask the two-row question instead, memory.ScopesConflict, which
	// is the same rule read the other way round.
	SessionScope map[string]string `koanf:"session_scope"`
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
	"reflection.supersede_consensus":           3,
	"reflection.lifecycle_timeout_minutes":     60,
	"reflection.consolidation_timeout_minutes": 10,
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
	if err := checkScopeValues(cfg); err != nil {
		return nil, err
	}
	if err := checkContextValues(cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

// checkContextValues refuses a context.abstain_cosine no cosine can be, on the
// FILE path as well as the environment one.
//
// cosineValue already refuses these for the environment, and koanf's YAML parser
// resolves the same spellings — `.inf`, `-.inf`, `.nan` — into the float32 field
// without complaint, so the env check alone would leave the worst of it unguarded
// on the path a hand-edited config actually takes. They are worth refusing for
// what they do downstream rather than for being unreadable: an infinite floor is
// a relevance verdict no row's cosine can clear, so every answer outside the
// keyword arm would come back `weak` with nothing in it to tell that from a
// measured verdict; NaN compares false against 0 and so reads as no threshold at
// all, telling a user who set a floor that there is none; and a negative value is
// satisfied by the worst row in the corpus, which is the opposite of a floor.
func checkContextValues(cfg *Config) error {
	if v := float64(cfg.Context.AbstainCosine); math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || v > 1 {
		return fmt.Errorf("context.abstain_cosine: a cosine is between 0 and 1, got %v", cfg.Context.AbstainCosine)
	}
	// The cutoff is a share of the top row's fused score, so it is the same
	// (0,1] range a fraction can be in: 0 leaves it off, and a value above 1
	// would keep every row (a threshold above the top score admits nothing it
	// would not already have). NaN compares false against both bounds and reads
	// as off, which would tell a user who set a cutoff that there is none — the
	// same reason abstain_cosine refuses it.
	if v := cfg.Context.RelevanceCutoff; math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || v > 1 {
		return fmt.Errorf("context.relevance_cutoff: a cutoff is a fraction in [0,1] where 0 is off, got %v", cfg.Context.RelevanceCutoff)
	}
	return nil
}

// checkScopeValues refuses an injection.session_scope key whose value is empty
// or only whitespace, the way the env form does (stringMap, which trims through
// commaPairs). An empty value is not a no-op: it is a filter that excludes every
// row naming that key with any other value.
//
// Refusing is chosen for being loud and consistent with the env form, not for
// keeping rows out: it is not a narrowing guarantee. The CLI subcommands fail on
// it, but LoadForHook falls back to the compiled defaults plus GHOST_*. The
// defaults carry no session_scope, so the session-start block for that session
// is not scope-filtered at all. A GHOST_INJECTION_SESSION_SCOPE replaces the
// file's whole session_scope map before this check, so that file is not refused
// and the env scope applies (both cases pinned by TestLoadForHook_EmptySessionScopeValueFallsBackUnscoped). Dropping
// only the bad key would have kept the others filtering, but silently, and left
// the file and env forms disagreeing about the same input.
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
		Context:   ContextConfig{RelevanceCutoff: DefaultRelevanceCutoff},
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
//   - a non-scalar field (injection.behavior_categories and the injection maps),
//     which koanf's weakly-typed decode cannot build from a bare string:
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

// cosineValue parses a 32-bit cosine in [0, 1], for the one key that holds one
// (context.abstain_cosine). Its own parser because the parse has to land on the
// field's own type: a float64 handed to a float32 field through confmap decodes as
// zero, so a cosine set in the environment would read as no threshold at all
// while the key looked like it had been accepted.
//
// It also refuses what ParseFloat accepts and a cosine cannot be, which is the
// whole reason this is not the generic float parser. An infinite floor is a
// relevance verdict no row's cosine can clear, so every result outside the
// keyword arm would come back `weak` with nothing in the answer to distinguish
// that verdict from a measured one; NaN compares false against 0, so it reads as
// OFF and a user who set a floor is told there is none; a negative value is
// satisfied by every row including the worst, which is the opposite of a floor.
// All three are typos, and a typo has to be the load error every other unreadable
// GHOST_* value produces rather than a silently accepted setting.
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

// stringMap parses "key=value,..." into the map[string]string a config key
// expects. A scope is a set of strings, so nothing here can fail to convert and
// the only checks available are the pair syntax commaPairs names and the value.
//
// An empty value is an error, as it is in the two numeric maps above — where
// ParseFloat and Atoi reject it by failing — and for the same reason: nothing
// else here can reject it, and a request key whose value is the empty string is
// a filter rather than a no-op. It excludes every row that names the key with
// any other value, so a trailing "=" would quietly leave a session with an
// unexplained subset of its store. The YAML form decodes an empty value rather
// than failing, so Load refuses it after decoding (checkScopeValues).
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
	{"GHOST_REFLECTION_SUPERSEDE_CONSENSUS", "reflection.supersede_consensus", intValue},
	{"GHOST_REFLECTION_LIFECYCLE_TIMEOUT_MINUTES", "reflection.lifecycle_timeout_minutes", intValue},
	{"GHOST_REFLECTION_CONSOLIDATION_TIMEOUT_MINUTES", "reflection.consolidation_timeout_minutes", intValue},
	// GHOST_LIFECYCLE_MIN_INTERVAL: the generic transformer replaces every
	// underscore with a dot, so it would produce lifecycle.min.interval and miss
	// the key entirely.
	{"GHOST_LIFECYCLE_MIN_INTERVAL", "lifecycle.min_interval", stringValue},
	{"GHOST_OLLAMA_URL", "embedding.ollama_url", stringValue},
	{"GHOST_ROUTING_DEFAULT_PROJECT", "routing.default_project", stringValue},
	{"GHOST_SEARCH_MIN_SIMILARITY", "search.min_similarity", floatValue},
	// GHOST_CONTEXT_ABSTAIN_COSINE: the generic _→. transformer would produce
	// context.abstain.cosine, missing the abstain_cosine key entirely.
	{"GHOST_CONTEXT_ABSTAIN_COSINE", "context.abstain_cosine", cosineValue},
	// GHOST_CONTEXT_RELEVANCE_CUTOFF: the same reason — the generic transformer
	// would produce context.relevance.cutoff, missing the relevance_cutoff key.
	{"GHOST_CONTEXT_RELEVANCE_CUTOFF", "context.relevance_cutoff", floatValue},
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
	{"GHOST_INJECTION_SESSION_SCOPE", "injection.session_scope", stringMap},
}

// DataDirPath returns the ghost data directory path WITHOUT creating it, so
// callers that must not leave a phantom directory behind (the stop hook's
// no-LLM skip, marker reads) can still locate the store.
//
// It is also where the GHOST_DEV_FORBID_DATA_DIR refusal happens (#721), and
// that placement is the whole mechanism rather than a convenience: EVERY path
// into the data directory goes through this function or through DataDir, so
// putting the check here is what makes "no path can reach the forbidden
// directory" a property of the tree instead of a rule each caller has to
// remember. A first version guarded the store-opening entry points and missed
// four paths that resolve the directory without opening a store — scratch
// Reap's root, the lifecycle failure marker's write and clear, and the marker
// read on every session start — and three of those wrote or deleted a file named
// after a project the code had been told it cannot resolve.
//
// So the callers' obligation is the ordinary one: handle the error. The
// fail-open paths (the hook and the marker bookkeeping) already returned on a
// data-dir error of any kind, which is why a refusal needs no new handling
// there at all.
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
	// Before any creation: DataDir MkdirAll's this path, and a refusal that
	// arrived afterwards would leave the phantom directory the variable exists
	// to protect.
	if err := CheckDevDataDir(buildVersion, dir); err != nil {
		return "", err
	}
	return dir, nil
}

// buildVersion is this binary's version — the string the release ldflags stamp
// into main.version, and "dev" for a plain `go build`. DataDirPath needs it and
// config cannot import cmd, so cmd/ghost hands it over once from its dispatch
// (SetBuildVersion), the same route memory.SetDetectRemote takes.
//
// The default is "dev" because that is the guarded side: a caller that has not
// wired the version yet is a development build, and is refused rather than
// allowed. It is read from a single goroutine (the process start) and by the
// hook paths, which are one process per fire, so it is a plain var rather than
// an atomic.
var buildVersion = "dev"

// SetBuildVersion tells the config package which build it is part of, so the
// GHOST_DEV_FORBID_DATA_DIR refusal applies to development builds only.
func SetBuildVersion(v string) { buildVersion = v }

// BuildVersion returns the version the data-directory check judges this build
// by. It exists for the tests that need the guarded side without going through
// a dispatch, and for a caller that wants to report which build answered.
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
