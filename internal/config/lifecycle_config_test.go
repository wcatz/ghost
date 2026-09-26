package config

import (
	"slices"
	"strings"
	"testing"
	"time"
)

// lifecycle.min_interval is the cooldown that stops the Stop hook spawning the
// reflect→resolve→supersede chain after every turn (#541). These pin the config
// surface: the compiled default, the file layer, the GHOST_* override, and how
// a value that cannot be read resolves.

func TestLifecycleMinInterval_CompiledDefault(t *testing.T) {
	isolateConfig(t)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	if got := cfg.Lifecycle.MinIntervalDuration(); got != DefaultLifecycleMinInterval {
		t.Errorf("lifecycle.min_interval = %s, want the compiled default %s",
			got, DefaultLifecycleMinInterval)
	}
	if got, want := cfg.Lifecycle.MinInterval, defaultLifecycleMinInterval; got != want {
		t.Errorf("lifecycle.min_interval = %q, want the compiled default %q", got, want)
	}
}

// TestDefaultLifecycleMinInterval_MatchesItsLiteral keeps the exported duration
// and the string the defaults map carries from drifting apart. The two are
// written separately (a duration constant and a YAML literal), and the hook
// reads one while the config file shows the other.
func TestDefaultLifecycleMinInterval_MatchesItsLiteral(t *testing.T) {
	d, err := time.ParseDuration(defaultLifecycleMinInterval)
	if err != nil {
		t.Fatalf("defaultLifecycleMinInterval = %q is not a duration: %v", defaultLifecycleMinInterval, err)
	}
	if d != DefaultLifecycleMinInterval {
		t.Errorf("defaultLifecycleMinInterval = %q parses to %s, want %s",
			defaultLifecycleMinInterval, d, DefaultLifecycleMinInterval)
	}
}

func TestLifecycleMinInterval_FromConfigFile(t *testing.T) {
	isolateConfig(t)
	writeUserConfig(t, "lifecycle:\n  min_interval: 45s\n")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	if got := cfg.Lifecycle.MinIntervalDuration(); got != 45*time.Second {
		t.Errorf("lifecycle.min_interval = %s, want 45s from the config file", got)
	}
}

// TestLifecycleMinInterval_ZeroInFileDisables pins the documented opt-out: 0
// means "no cooldown", which is a real choice (a project that wants every turn
// to consolidate) and must not be mistaken for "unset" and replaced by the
// default.
func TestLifecycleMinInterval_ZeroInFileDisables(t *testing.T) {
	isolateConfig(t)
	writeUserConfig(t, "lifecycle:\n  min_interval: 0\n")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	if got := cfg.Lifecycle.MinIntervalDuration(); got != 0 {
		t.Errorf("lifecycle.min_interval = %s, want 0 (the explicit opt-out)", got)
	}
}

// TestLifecycleMinIntervalEnvOverride pins GHOST_LIFECYCLE_MIN_INTERVAL. The
// key has no underscore in it, so the generic GHOST_ prefix + "_"→"."
// transformer already reaches lifecycle.min_interval; the explicit envOverrides
// entry is what makes the documented variable the one that WINS, and what the
// docs table and the env isolation helper are derived from.
func TestLifecycleMinIntervalEnvOverride(t *testing.T) {
	isolateConfig(t)
	t.Setenv("GHOST_LIFECYCLE_MIN_INTERVAL", "15m")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	if got := cfg.Lifecycle.MinIntervalDuration(); got != 15*time.Minute {
		t.Errorf("lifecycle.min_interval = %s, want 15m from GHOST_LIFECYCLE_MIN_INTERVAL", got)
	}
}

// TestLifecycleMinInterval_EnvBeatsConfigFile pins the precedence the env layer
// has over the file layer for this key, since a deployment that sets the
// cooldown in the environment must not be overridden by a config file.
func TestLifecycleMinInterval_EnvBeatsConfigFile(t *testing.T) {
	isolateConfig(t)
	writeUserConfig(t, "lifecycle:\n  min_interval: 45s\n")
	t.Setenv("GHOST_LIFECYCLE_MIN_INTERVAL", "0")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	if got := cfg.Lifecycle.MinIntervalDuration(); got != 0 {
		t.Errorf("lifecycle.min_interval = %s, want 0 (the env layer overrides the file)", got)
	}
}

// TestLifecycleMinIntervalDuration is the resolution table: what a raw key value
// means as a cooldown. The two failure cases matter most. A value that cannot be
// read falls back to the compiled default, NOT to 0 — 0 means "no cooldown", so
// guessing it would silently switch off the very guard the user asked for, the
// same near-zero-Config trap the rest of this package refuses. A negative value
// means the same thing 0 does (the check treats any non-positive interval as
// off), matching scratch.max_bytes.
func TestLifecycleMinIntervalDuration(t *testing.T) {
	cases := []struct {
		name, raw string
		want      time.Duration
		warn      string // substring the warning must carry; "" = must stay silent
	}{
		{name: "unset", raw: "", want: DefaultLifecycleMinInterval},
		{name: "blank", raw: "   ", want: DefaultLifecycleMinInterval},
		{name: "default literal", raw: "30m", want: 30 * time.Minute},
		{name: "compound", raw: "1h30m", want: 90 * time.Minute},
		{name: "sub-minute", raw: "90s", want: 90 * time.Second},
		{name: "zero disables", raw: "0", want: 0},
		{name: "zero seconds disables", raw: "0s", want: 0},
		{name: "negative disables", raw: "-5m", want: 0, warn: "lifecycle.min_interval"},
		{name: "unreadable keeps the default", raw: "soon", want: DefaultLifecycleMinInterval, warn: "lifecycle.min_interval"},
		{name: "unitless keeps the default", raw: "30", want: DefaultLifecycleMinInterval, warn: "lifecycle.min_interval"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isolateConfig(t)
			warnings := captureConfigWarnings(t)

			got := LifecycleConfig{MinInterval: tc.raw}.MinIntervalDuration()
			if got != tc.want {
				t.Errorf("MinInterval(%q) = %s, want %s", tc.raw, got, tc.want)
			}
			msg := warnings.String()
			if tc.warn == "" {
				// A value the user got right must not produce a line on the
				// stop hook's stderr, once per turn.
				if msg != "" {
					t.Errorf("MinInterval(%q) warned on a valid value: %q", tc.raw, msg)
				}
				return
			}
			if !strings.Contains(msg, tc.warn) {
				t.Errorf("warning %q must name %s (a value the user cannot act on silently is a guess)", msg, tc.warn)
			}
		})
	}
}

// TestLifecycleMinInterval_UnreadableValueWarnsThroughLoad pins that the
// warning reaches the user through a real config file, not just the resolver:
// a typo'd cooldown must be reported by the same load the hook performs.
func TestLifecycleMinInterval_UnreadableValueWarnsThroughLoad(t *testing.T) {
	isolateConfig(t)
	writeUserConfig(t, "lifecycle:\n  min_interval: every-turn\n")
	warnings := captureConfigWarnings(t)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	if got := cfg.Lifecycle.MinIntervalDuration(); got != DefaultLifecycleMinInterval {
		t.Errorf("lifecycle.min_interval = %s, want the compiled default %s", got, DefaultLifecycleMinInterval)
	}
	if msg := warnings.String(); !strings.Contains(msg, "lifecycle.min_interval") {
		t.Errorf("warning %q must name lifecycle.min_interval", msg)
	}
}

// TestLifecycleMinInterval_KnownToTheUnknownKeyCheck pins the #607 check: the
// new key must NOT be reported as an unknown key, while real typos beside it
// must be. The check is derived from the struct tags, so a key added without one
// would be silently reported on every load.
//
// The reported set is compared EXACTLY rather than by substring, because the
// natural typo here contains the real key: "min_intervall" is "min_interval"
// plus an l, so a Contains check for the real key matches it too and this test
// would pass while the check regressed to reporting both.
func TestLifecycleMinInterval_KnownToTheUnknownKeyCheck(t *testing.T) {
	isolateConfig(t)
	path := writeUserConfig(t, "lifecycle:\n  min_interval: 10m\n  min_intervall: 99h\n  min_interva: 99h\n")
	warnings := captureConfigWarnings(t)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	if got := cfg.Lifecycle.MinIntervalDuration(); got != 10*time.Minute {
		t.Errorf("lifecycle.min_interval = %s, want 10m (the valid sibling key must still load)", got)
	}
	got := warnings.String()
	const marker = "unknown key(s) ignored: "
	i := strings.Index(got, marker)
	if i < 0 {
		t.Fatalf("no unknown-key warning at all: %q", got)
	}
	reported := strings.Split(strings.TrimSpace(got[i+len(marker):]), ", ")
	want := []string{"lifecycle.min_interva", "lifecycle.min_intervall"} // warnUnknownKeys sorts
	if !slices.Equal(reported, want) {
		t.Errorf("unknown keys reported = %v, want exactly %v (lifecycle.min_interval must never be among them)", reported, want)
	}
	if !strings.Contains(got, path) {
		t.Errorf("warning %q must name the file it came from (%q)", got, path)
	}
}

// TestLifecycleMinInterval_BareSectionKeepsDefaults: a user who opens their
// config to change one lifecycle setting and leaves the header bare must not
// lose the compiled cooldown (pruneNulls exists for exactly this).
func TestLifecycleMinInterval_BareSectionKeepsDefaults(t *testing.T) {
	isolateConfig(t)
	writeUserConfig(t, "lifecycle:\n")
	warnings := captureConfigWarnings(t)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	if got := cfg.Lifecycle.MinIntervalDuration(); got != DefaultLifecycleMinInterval {
		t.Errorf("a bare section header wiped the default: lifecycle.min_interval = %s, want %s", got, DefaultLifecycleMinInterval)
	}
	if msg := warnings.String(); strings.Contains(msg, "unknown key") {
		t.Errorf("a bare section header was reported as an unknown key: %q", msg)
	}
}

// TestFallbackConfig_KeepsTheLifecycleCooldown pins the last-resort Config
// (defaultConfig) for the new key, through the load path that is supposed to
// mirror the defaults map.
func TestFallbackConfig_KeepsTheLifecycleCooldown(t *testing.T) {
	isolateConfig(t)

	if got := FallbackConfig().Lifecycle.MinIntervalDuration(); got != DefaultLifecycleMinInterval {
		t.Errorf("FallbackConfig lifecycle.min_interval = %s, want %s", got, DefaultLifecycleMinInterval)
	}
	if got := defaultConfig().Lifecycle.MinIntervalDuration(); got != DefaultLifecycleMinInterval {
		t.Errorf("defaultConfig lifecycle.min_interval = %s, want %s", got, DefaultLifecycleMinInterval)
	}
}
