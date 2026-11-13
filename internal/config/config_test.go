package config

import (
	"bytes"
	"io"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
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

// chk is one named got/want pair, compared with reflect.DeepEqual.
type chk struct {
	name      string
	got, want any
}

func checkAll(t *testing.T, cs ...chk) {
	t.Helper()
	for _, c := range cs {
		if !reflect.DeepEqual(c.got, c.want) {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
}

func mustLoad(t *testing.T) *Config {
	t.Helper()
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	return cfg
}

func TestLoad_Defaults(t *testing.T) {
	isolateConfig(t)
	cfg := mustLoad(t)
	checkAll(t,
		chk{"embedding.enabled", cfg.Embedding.Enabled, true},
		chk{"embedding.dimensions", cfg.Embedding.Dimensions, 768},
		// The lifecycle bound must be finite by default: an unbounded hang would
		// wedge auto-consolidation for that project.
		chk{"reflection.lifecycle_timeout_minutes", cfg.Reflection.LifecycleTimeoutMinutes, 60},
		chk{"reflection.consolidation_timeout_minutes", cfg.Reflection.ConsolidationTimeoutMinutes, 10},
		chk{"linking.enabled", cfg.Linking.Enabled, true},
		chk{"linking.threshold", cfg.Linking.Threshold, 0.70},
		chk{"linking.demotion_threshold", cfg.Linking.DemotionThreshold, 0.90},
		chk{"search.min_similarity", cfg.Search.MinSimilarity, 0.0},
		chk{"obsidian.vault_dir", cfg.Obsidian.VaultDir, ""},
		chk{"obsidian.interval", cfg.Obsidian.Interval, "30s"},
		chk{"obsidian.auto_sync", cfg.Obsidian.AutoSync, false},
		chk{"routing.default_project", cfg.Routing.DefaultProject, ""},
		chk{"scratch.max_bytes", cfg.Scratch.MaxBytes, int64(512 * 1024 * 1024)},
		chk{"cli.model_reflect", cfg.CLI.ModelReflect, ""},
		chk{"cli.model_resolve", cfg.CLI.ModelResolve, ""},
		chk{"cli.model_supersede", cfg.CLI.ModelSupersede, ""},
		chk{"injection.behavior_floor", cfg.Injection.BehaviorFloor, 8},
		chk{"injection.behavior_categories", cfg.Injection.BehaviorCategories, []string{"gotcha", "convention", "preference", "decision"}},
		chk{"injection.category_weights", cfg.Injection.CategoryWeights == nil, true},
		chk{"injection.category_caps", cfg.Injection.CategoryCaps, map[string]int{"gotcha": 4}},
		// Unset by design: a non-nil default would be indistinguishable from a
		// scope somebody chose.
		chk{"injection.session_scope", len(cfg.Injection.SessionScope), 0},
	)
}

func TestLoad_YAMLFile(t *testing.T) {
	isolateConfig(t)
	writeUserConfig(t, `
embedding:
  model: "custom-embed-model"
linking:
  threshold: 0.85
  demotion_threshold: 0.95
reflection:
  auto_resolve: true
  lifecycle_timeout_minutes: 30
  consolidation_timeout_minutes: 7
routing:
  default_project: infrastructure
cli:
  model_reflect: opencode/big-pickle
  model_resolve: opencode/big-pickle
  model_supersede: opencode/big-pickle
scratch:
  max_bytes: 0
injection:
  session_scope:
    environment: development
    component: api
`)
	cfg := mustLoad(t)
	checkAll(t,
		chk{"embedding.model", cfg.Embedding.Model, "custom-embed-model"},
		chk{"linking.threshold", cfg.Linking.Threshold, 0.85},
		chk{"linking.demotion_threshold", cfg.Linking.DemotionThreshold, 0.95},
		chk{"reflection.auto_resolve", cfg.Reflection.AutoResolve, true},
		chk{"reflection.lifecycle_timeout_minutes", cfg.Reflection.LifecycleTimeoutMinutes, 30},
		chk{"reflection.consolidation_timeout_minutes", cfg.Reflection.ConsolidationTimeoutMinutes, 7},
		chk{"routing.default_project", cfg.Routing.DefaultProject, "infrastructure"},
		chk{"cli.model_reflect", cfg.CLI.ModelReflect, "opencode/big-pickle"},
		chk{"cli.model_resolve", cfg.CLI.ModelResolve, "opencode/big-pickle"},
		chk{"cli.model_supersede", cfg.CLI.ModelSupersede, "opencode/big-pickle"},
		// An explicit 0 is an opt-out that beats the 512 MiB default.
		chk{"scratch.max_bytes", cfg.Scratch.MaxBytes, int64(0)},
		// #577: the session scope is a setting a file can bind as a nested map.
		chk{"injection.session_scope", cfg.Injection.SessionScope, map[string]string{"environment": "development", "component": "api"}},
		chk{"embedding.enabled (untouched default)", cfg.Embedding.Enabled, true},
	)
}

// Every explicit env override, including the ones the generic _ → . transformer
// would map to the wrong key (obsidian.vault.dir, embedding.ollama.url,
// cli.model.resolve, scratch.max.bytes, ...), and the precedence over the file.
func TestLoad_EnvOverrides(t *testing.T) {
	isolateConfig(t)
	writeUserConfig(t, "embedding:\n  model: yaml-model\ncli:\n  model_resolve: yaml-model\n")
	for k, v := range map[string]string{
		"GHOST_EMBEDDING_MODEL":               "env-model",
		"GHOST_CLI_MODEL_RESOLVE":             "env-model",
		"GHOST_LINKING_ENABLED":               "false",
		"GHOST_LINKING_DEMOTION_THRESHOLD":    "0.42",
		"GHOST_OBSIDIAN_VAULT_DIR":            "/vaults/ghost",
		"GHOST_OBSIDIAN_AUTO_SYNC":            "true",
		"GHOST_OLLAMA_URL":                    "http://10.0.2.2:11434",
		"GHOST_ROUTING_DEFAULT_PROJECT":       "infrastructure",
		"GHOST_SCRATCH_MAX_BYTES":             "2048",
		"GHOST_INJECTION_BEHAVIOR_FLOOR":      "3",
		"GHOST_INJECTION_BEHAVIOR_CATEGORIES": "gotcha, decision",
		"GHOST_INJECTION_CATEGORY_WEIGHTS":    "gotcha=1.2,decision=1.5",
		"GHOST_INJECTION_CATEGORY_CAPS":       "gotcha=2,decision=1",
		"GHOST_INJECTION_SESSION_SCOPE":       "environment=development, component=api",
	} {
		t.Setenv(k, v)
	}
	cfg := mustLoad(t)
	checkAll(t,
		chk{"embedding.model", cfg.Embedding.Model, "env-model"},
		chk{"cli.model_resolve", cfg.CLI.ModelResolve, "env-model"},
		chk{"linking.enabled", cfg.Linking.Enabled, false},
		chk{"linking.demotion_threshold", cfg.Linking.DemotionThreshold, 0.42},
		chk{"obsidian.vault_dir", cfg.Obsidian.VaultDir, "/vaults/ghost"},
		chk{"obsidian.auto_sync", cfg.Obsidian.AutoSync, true},
		chk{"embedding.ollama_url", cfg.Embedding.OllamaURL, "http://10.0.2.2:11434"},
		chk{"routing.default_project", cfg.Routing.DefaultProject, "infrastructure"},
		chk{"scratch.max_bytes", cfg.Scratch.MaxBytes, int64(2048)},
		chk{"injection.behavior_floor", cfg.Injection.BehaviorFloor, 3},
		// A list and the maps cannot be decoded from a bare string.
		chk{"injection.behavior_categories", cfg.Injection.BehaviorCategories, []string{"gotcha", "decision"}},
		chk{"injection.category_weights", cfg.Injection.CategoryWeights, map[string]float64{"gotcha": 1.2, "decision": 1.5}},
		chk{"injection.category_caps", cfg.Injection.CategoryCaps, map[string]int{"gotcha": 2, "decision": 1}},
		chk{"injection.session_scope", cfg.Injection.SessionScope, map[string]string{"environment": "development", "component": "api"}},
	)
}

func TestConfigAndDataPaths(t *testing.T) {
	cfgHome, dataHome := t.TempDir(), t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfgHome)
	t.Setenv("XDG_DATA_HOME", dataHome)

	path, err := ConfigFilePath()
	if err != nil || path != filepath.Join(cfgHome, "ghost", "config.yaml") {
		t.Fatalf("ConfigFilePath = %q, %v", path, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("ConfigFilePath must not create the file, stat err = %v", err)
	}
	if _, created, err := EnsureConfigFile(); err != nil || !created {
		t.Fatalf("EnsureConfigFile first call: created %v err %v", created, err)
	}
	if info, err := os.Stat(path); err != nil || info.Size() == 0 {
		t.Errorf("config file not created or empty: %v", err)
	}
	if _, created, err := EnsureConfigFile(); err != nil || created {
		t.Errorf("EnsureConfigFile second call: created %v err %v, want no re-create", created, err)
	}

	// DataDirPath is the same location DataDir creates, without creating it, so
	// the no-LLM skip can leave no phantom directory.
	want := filepath.Join(dataHome, "ghost")
	if got, err := DataDirPath(); err != nil || got != want {
		t.Fatalf("DataDirPath = %q, %v, want %q", got, err, want)
	}
	if _, err := os.Stat(want); !os.IsNotExist(err) {
		t.Errorf("DataDirPath must not create the directory, stat err = %v", err)
	}
	if got, err := DataDir(); err != nil || got != want {
		t.Fatalf("DataDir = %q, %v, want %q", got, err, want)
	}
	if info, err := os.Stat(want); err != nil || !info.IsDir() {
		t.Errorf("DataDir did not create the directory: %v", err)
	}
}

// Without XDG_DATA_HOME the store falls back under HOME, and the dev-forbid
// guard applies to that fallback too.
func TestDataDir_DefaultFallback(t *testing.T) {
	withBuildVersion(t, devVersion)
	home := t.TempDir()
	t.Setenv("HOME", home)
	if runtime.GOOS == "windows" {
		t.Setenv("USERPROFILE", home)
	}
	t.Setenv("XDG_DATA_HOME", "")
	unsetEnvVars(t, []string{"XDG_DATA_HOME", DevForbidDataDirEnv})
	want := filepath.Join(home, ".local", "share", "ghost")

	if dir, err := DataDir(); err != nil || dir != want {
		t.Errorf("DataDir = %q, %v, want %q", dir, err, want)
	}
	t.Setenv(DevForbidDataDirEnv, want)
	if _, err := DataDir(); err == nil || !strings.Contains(err.Error(), DevForbidDataDirEnv) {
		t.Errorf("DataDir with the fallback forbidden = %v, want a refusal naming %s", err, DevForbidDataDirEnv)
	}
	if _, err := DataDirPath(); err == nil || !strings.Contains(err.Error(), DevForbidDataDirEnv) {
		t.Errorf("DataDirPath with the fallback forbidden = %v, want a refusal naming %s", err, DevForbidDataDirEnv)
	}
}

// An empty (or whitespace-only) session_scope value in the YAML form excludes
// every memory naming the key, so Load refuses the file; the hook path drops the
// file and runs unscoped, with a warning. A GHOST_INJECTION_SESSION_SCOPE
// replaces the file's map whole before the check, so that file is not refused
// and the env scope applies (pinning koanf's whole-value replace).
func TestSessionScopeEmptyYAMLValue(t *testing.T) {
	const file = "injection:\n  session_scope:\n    environment: %s\n    component: api\n"
	for name, value := range map[string]string{"empty": `""`, "whitespace": `" "`} {
		isolateConfig(t)
		writeUserConfig(t, strings.Replace(file, "%s", value, 1))
		if _, err := Load(); err == nil || !strings.Contains(err.Error(), "injection.session_scope: empty value for environment") {
			t.Errorf("%s: Load error = %v, want a refusal naming session_scope and environment", name, err)
		}
	}

	for name, tc := range map[string]struct {
		env      string
		want     map[string]string
		wantWarn bool
	}{
		"no env scope":  {wantWarn: true},
		"env scope set": {env: "environment=development", want: map[string]string{"environment": "development"}},
	} {
		isolateConfig(t)
		if tc.env != "" {
			t.Setenv("GHOST_INJECTION_SESSION_SCOPE", tc.env)
		}
		writeUserConfig(t, strings.Replace(file, "%s", `""`, 1))
		warnings := captureConfigWarnings(t)
		cfg := LoadForHook()
		if !maps.Equal(cfg.Injection.SessionScope, tc.want) {
			t.Errorf("%s: session_scope = %v, want %v", name, cfg.Injection.SessionScope, tc.want)
		}
		if got := strings.Contains(warnings.String(), "injection.session_scope: empty value for environment"); got != tc.wantWarn {
			t.Errorf("%s: refusal warning = %v, want %v", name, got, tc.wantWarn)
		}
	}
}

// A config file that exists but does not parse is an error naming the file, not
// a silent fall back to defaults. loadConfigFile covers the /etc layer Load
// cannot reach from a test: a malformed file is fatal, an absent one silent, and
// one that cannot be READ is a warning and skipped, never fatal.
func TestLoad_MalformedYAMLIsAnError(t *testing.T) {
	isolateConfig(t)
	path := writeUserConfig(t, malformedYAML)
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), path) {
		t.Errorf("Load() error = %v, want one naming %q", err, path)
	}

	parser := yaml.Parser()
	if err := loadConfigFile(koanf.New("."), path, parser); err == nil || !strings.Contains(err.Error(), path) {
		t.Errorf("loadConfigFile on a malformed file = %v, want an error naming it", err)
	}
	if err := loadConfigFile(koanf.New("."), filepath.Join(t.TempDir(), "config.yaml"), parser); err != nil {
		t.Errorf("loadConfigFile on an absent file = %v, want nil (the layer is optional)", err)
	}

	if os.Geteuid() == 0 {
		t.Skip("running as root: mode 0o000 is still readable")
	}
	unreadable := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(unreadable, []byte("embedding:\n  enabled: true\n"), 0o000); err != nil {
		t.Fatal(err)
	}
	warnings := captureConfigWarnings(t)
	if err := loadConfigFile(koanf.New("."), unreadable, parser); err != nil {
		t.Errorf("loadConfigFile on an unreadable file = %v, want nil (warned and skipped)", err)
	}
	if !strings.Contains(warnings.String(), unreadable) {
		t.Errorf("warning %q must name the unreadable file", warnings.String())
	}
}

// The hook path runs inside someone else's editor: a broken file must never fail
// their session, but is reported. The fallback is every layer that does not read
// a file (defaults AND the GHOST_* environment), so a deployment's opt-outs
// survive a broken file, an unreadable variable is reported and skipped without
// taking the rest down, and a valid config loads without a warning.
func TestLoadForHook(t *testing.T) {
	isolateConfig(t)
	path := writeUserConfig(t, malformedYAML)
	warnings := captureConfigWarnings(t)
	cfg := LoadForHook()
	checkAll(t,
		chk{"embedding.enabled", cfg.Embedding.Enabled, true},
		chk{"linking.demotion_threshold", cfg.Linking.DemotionThreshold, 0.90},
		chk{"injection.behavior_floor", cfg.Injection.BehaviorFloor, 8},
		chk{"warning names the file", strings.Contains(warnings.String(), path), true},
	)

	isolateConfig(t)
	t.Setenv("GHOST_EMBEDDING_ENABLED", "false")
	t.Setenv("GHOST_LINKING_DEMOTION_THRESHOLD", "0.42")
	t.Setenv("GHOST_SCRATCH_MAX_BYTES", "0")
	t.Setenv("GHOST_ROUTING_DEFAULT_PROJECT", "infrastructure")
	t.Setenv("GHOST_INJECTION_CATEGORY_WEIGHTS", "gotcha")
	path = writeUserConfig(t, malformedYAML)
	warnings = captureConfigWarnings(t)
	cfg = LoadForHook()
	checkAll(t,
		chk{"embedding.enabled", cfg.Embedding.Enabled, false},
		chk{"linking.demotion_threshold", cfg.Linking.DemotionThreshold, 0.42},
		chk{"scratch.max_bytes", cfg.Scratch.MaxBytes, int64(0)},
		chk{"routing.default_project", cfg.Routing.DefaultProject, "infrastructure"},
		chk{"warning names the file", strings.Contains(warnings.String(), path), true},
		chk{"warning names the bad variable", strings.Contains(warnings.String(), "GHOST_INJECTION_CATEGORY_WEIGHTS"), true},
	)

	isolateConfig(t)
	writeUserConfig(t, "linking:\n  demotion_threshold: 0.42\n")
	warnings = captureConfigWarnings(t)
	if got := LoadForHook().Linking.DemotionThreshold; got != 0.42 || warnings.String() != "" {
		t.Errorf("valid config: demotion_threshold %v, warnings %q, want 0.42 and none", got, warnings.String())
	}
}

// A GHOST_* value the generic provider cannot weakly convert yields the compiled
// defaults, never a near-zero Config (embeddings and linking off, an unbounded
// lifecycle), and the report names the variable. Only the bad variable is
// skipped: a good one beside it (a deliberate opt-out) survives.
func TestFallbackConfig_UndecodableGenericEnvValue(t *testing.T) {
	cases := []struct{ bad, badValue, good, goodValue string }{
		{"GHOST_LINKING_THRESHOLD", "high", "", ""},
		{"GHOST_EMBEDDING_DIMENSIONS", "abc", "GHOST_EMBEDDING_ENABLED", "false"},
		{"GHOST_EMBEDDING_ENABLED", "yes", "GHOST_SEARCH_MIN_SIMILARITY", "0.5"},
	}
	for _, tc := range cases {
		t.Run(tc.bad, func(t *testing.T) {
			isolateConfig(t)
			t.Setenv(tc.bad, tc.badValue)
			if tc.good != "" {
				t.Setenv(tc.good, tc.goodValue)
			}
			writeUserConfig(t, malformedYAML)
			warnings := captureConfigWarnings(t)

			cfg := LoadForHook()
			wantSim := 0.0
			if tc.good == "GHOST_SEARCH_MIN_SIMILARITY" {
				wantSim = 0.5
			}
			checkAll(t,
				chk{"embedding.enabled", cfg.Embedding.Enabled, tc.good != "GHOST_EMBEDDING_ENABLED"},
				chk{"embedding.dimensions", cfg.Embedding.Dimensions, 768},
				chk{"linking", [3]any{cfg.Linking.Enabled, cfg.Linking.Threshold, cfg.Linking.DemotionThreshold}, [3]any{true, 0.70, 0.90}},
				// 0 would mean unbounded.
				chk{"reflection.lifecycle_timeout_minutes", cfg.Reflection.LifecycleTimeoutMinutes, 60},
				chk{"scratch.max_bytes", cfg.Scratch.MaxBytes, DefaultScratchMaxBytes},
				chk{"search.min_similarity", cfg.Search.MinSimilarity, wantSim},
				chk{"warning names " + tc.bad, strings.Contains(warnings.String(), tc.bad), true},
			)
		})
	}
}

// The hand-written last-resort Config must match the compiled defaults map, or a
// key added to the map and forgotten there is only found on a machine that needed
// the fallback.
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

// FallbackConfig returns the compiled defaults, not Go zero values, when even the
// defaults layer will not decode (an unbindable entry makes every decode fail).
func TestFallbackConfig_NeverPanicsOnDecodeFailure(t *testing.T) {
	isolateConfig(t)
	writeUserConfig(t, malformedYAML)
	defaults["injection.category_caps"] = func() {}
	t.Cleanup(func() {
		defaults["injection.category_caps"] = map[string]interface{}{"gotcha": 4}
	})
	warnings := captureConfigWarnings(t)

	cfg := FallbackConfig()
	checkAll(t,
		chk{"embedding", [2]any{cfg.Embedding.Enabled, cfg.Embedding.Dimensions}, [2]any{true, 768}},
		chk{"linking", [3]any{cfg.Linking.Enabled, cfg.Linking.Threshold, cfg.Linking.DemotionThreshold}, [3]any{true, 0.70, 0.90}},
		chk{"reflection bounds", [2]int{cfg.Reflection.LifecycleTimeoutMinutes, cfg.Reflection.ConsolidationTimeoutMinutes}, [2]int{60, 10}},
		chk{"obsidian.interval", cfg.Obsidian.Interval, "30s"},
		chk{"scratch.max_bytes", cfg.Scratch.MaxBytes, DefaultScratchMaxBytes},
		chk{"warning says built-in defaults", strings.Contains(warnings.String(), "built-in defaults"), true},
	)
}

// isolateConfig must keep a host GHOST_* variable out of an assertion that a
// compiled default holds: the names derived from the env layers, and an ambient
// name this package has no mapping for (which still reaches koanf's generic
// provider).
func TestIsolateConfig_ClearsGhostVars(t *testing.T) {
	vars := map[string]string{
		"GHOST_NOT_A_CONFIG_KEY":           "1",
		"GHOST_LINKING_DEMOTION_THRESHOLD": "0.42",
		"GHOST_SCRATCH_MAX_BYTES":          "1024",
		"GHOST_EMBEDDING_ENABLED":          "false",
	}
	for k, v := range vars {
		t.Setenv(k, v)
	}
	isolateConfig(t)
	for k := range vars {
		if v, ok := os.LookupEnv(k); ok {
			t.Errorf("isolateConfig left %s=%q set", k, v)
		}
	}
}

// Warnings can be sent somewhere other than raw stderr (the MCP server's stderr
// belongs to the client protocol), a nil writer is ignored rather than panicking
// on the first warning, and the same warning prints once per process across
// loads while a different one still gets through.
func TestWarningWriter(t *testing.T) {
	isolateConfig(t)
	writeUserConfig(t, "linking:\n  thresholdd: 0.9\n")
	warnings := captureConfigWarnings(t)
	restore := SetWarningWriter(nil)
	defer restore()

	for range 2 {
		mustLoad(t)
	}
	if n := strings.Count(warnings.String(), "thresholdd"); n != 1 {
		t.Errorf("SetWarningWriter(nil) / dedup: unknown key warned %d times across two loads, want 1:\n%s", n, warnings.String())
	}
	warnf("a different problem")
	if !strings.Contains(warnings.String(), "a different problem") {
		t.Errorf("a distinct warning was swallowed: %q", warnings.String())
	}

	isolateConfig(t)
	path := writeUserConfig(t, malformedYAML)
	var injected bytes.Buffer
	defer SetWarningWriter(&injected)()
	LoadForHook()
	if !strings.Contains(injected.String(), path) {
		t.Errorf("injected sink got %q, want the parse warning naming %q", injected.String(), path)
	}
}

// The race the MCP server can hit: a tool handler inside warnf while the sink is
// replaced. Both sides are released from one barrier and run a fixed count, and
// the dedup set is cleared per iteration so every load reaches the sink read.
func TestSetWarningWriter_ConcurrentWithWarnings(t *testing.T) {
	isolateConfig(t)
	writeUserConfig(t, "linking:\n  thresholdd: 0.9\n")
	restore := SetWarningWriter(io.Discard)
	defer restore()

	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		for range 200 {
			resetWarned()
			if _, err := Load(); err != nil {
				t.Errorf("Load(): %v", err)
				return
			}
		}
	}()
	go func() {
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

// The unknown-key warning must not fire on Ghost's own keys: every compiled
// default is bound by a Config field, and a file naming one key per section
// (including the map and list kinds) is silent.
func TestLoad_AllKnownKeysDoNotWarn(t *testing.T) {
	isolateConfig(t)
	warnings := captureConfigWarnings(t)
	for key := range defaults {
		if _, ok := knownKeys[key]; !ok {
			t.Errorf("defaults key %q is not bound by any Config field", key)
		}
	}
	writeUserConfig(t, `embedding: {enabled: true, ollama_url: "http://localhost:11434", model: "nomic-embed-text:v1.5", dimensions: 768}
reflection: {auto_resolve: true, auto_supersede: true, auto_reflect: true, lifecycle_timeout_minutes: 60, consolidation_timeout_minutes: 10}
cli: {claude_binary: "", opencode_binary: "", codex_binary: "", goose_binary: "", model_reflect: "", model_resolve: "", model_supersede: ""}
linking: {enabled: true, threshold: 0.7, demotion_threshold: 0.9}
injection:
  behavior_floor: 8
  behavior_categories: [gotcha]
  category_weights: {gotcha: 1.2}
  category_caps: {gotcha: 4}
  session_scope: {environment: development}
search: {min_similarity: 0.0}
context: {abstain_cosine: 0.0}
obsidian: {vault_dir: "", interval: "30s", auto_sync: false}
routing: {default_project: ""}
scratch: {max_bytes: 536870912}
`)
	cfg := mustLoad(t)
	if cfg.Injection.CategoryWeights["gotcha"] != 1.2 {
		t.Errorf("injection.category_weights = %v, want gotcha:1.2 to load", cfg.Injection.CategoryWeights)
	}
	if got := warnings.String(); got != "" {
		t.Errorf("Load() warned for keys that are all bound: %q", got)
	}
}

// A GHOST_ value that cannot be read as the key's type is reported against the
// variable that carried it, and every bad variable is reported (returning at the
// first would hide the rest).
func TestLoad_MalformedEnvOverrideIsAnError(t *testing.T) {
	cases := []struct{ envKey, value string }{
		{"GHOST_INJECTION_BEHAVIOR_FLOOR", "eight"},
		{"GHOST_INJECTION_CATEGORY_WEIGHTS", "gotcha"},
		{"GHOST_INJECTION_CATEGORY_WEIGHTS", "gotcha=high"},
		{"GHOST_INJECTION_CATEGORY_CAPS", "gotcha=four"},
		{"GHOST_INJECTION_SESSION_SCOPE", "environment"},
		// An empty value parses as a string but is a filter, not a no-op.
		{"GHOST_INJECTION_SESSION_SCOPE", "environment="},
		{"GHOST_LINKING_DEMOTION_THRESHOLD", "high"},
	}
	for _, tc := range cases {
		isolateConfig(t)
		t.Setenv(tc.envKey, tc.value)
		if _, err := Load(); err == nil || !strings.Contains(err.Error(), tc.envKey) {
			t.Errorf("%s=%q: Load error = %v, want one naming the variable", tc.envKey, tc.value, err)
		}
	}

	isolateConfig(t)
	t.Setenv("GHOST_INJECTION_CATEGORY_WEIGHTS", "gotcha=high")
	t.Setenv("GHOST_INJECTION_CATEGORY_CAPS", "gotcha=four")
	_, err := Load()
	for _, name := range []string{"GHOST_INJECTION_CATEGORY_WEIGHTS", "GHOST_INJECTION_CATEGORY_CAPS"} {
		if err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("two bad variables: error %v must name %s", err, name)
		}
	}
}

// A section header with no children (`injection:` reads as YAML null) neither
// trips the unknown-key warning nor erases the defaults under it, and pruning it
// does not stop the warning for a genuine typo in the same file.
func TestLoad_NullSectionKeepsDefaults(t *testing.T) {
	cases := []struct {
		name, yaml string
		keep       func(*Config) bool
	}{
		{"injection", "injection:\n", func(c *Config) bool {
			return c.Injection.BehaviorFloor == 8 && c.Injection.CategoryCaps["gotcha"] == 4 && len(c.Injection.BehaviorCategories) == 4
		}},
		{"nested section", "injection:\n  category_weights:\n", func(c *Config) bool { return c.Injection.BehaviorFloor == 8 }},
	}
	for _, tc := range cases {
		isolateConfig(t)
		writeUserConfig(t, tc.yaml)
		warnings := captureConfigWarnings(t)
		if cfg := mustLoad(t); !tc.keep(cfg) {
			t.Errorf("%s: a bare section header wiped its defaults: %+v", tc.name, cfg)
		}
		if strings.Contains(warnings.String(), "unknown key") {
			t.Errorf("%s: a bare section header was reported as an unknown key: %q", tc.name, warnings.String())
		}
	}

	isolateConfig(t)
	writeUserConfig(t, "obsidian:\ninjection:\nobsidain:\n  vault_dir: /x\n")
	warnings := captureConfigWarnings(t)
	mustLoad(t)
	got := warnings.String()
	if !strings.Contains(got, "obsidain") || strings.Contains(got, "obsidian:") || strings.Contains(got, "injection:") {
		t.Errorf("warning %q must name the typo obsidain and not the bare headers", got)
	}
}
