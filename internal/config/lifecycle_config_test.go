package config

import (
	"slices"
	"strings"
	"testing"
	"time"
)

// lifecycle.min_interval is the cooldown that stops the Stop hook spawning the
// reflect→resolve→supersede chain after every turn (#541).

// The compiled default, the file layer, the GHOST_LIFECYCLE_MIN_INTERVAL
// override and its precedence over the file. An explicit 0 means "no cooldown"
// and must not be mistaken for "unset".
func TestLifecycleMinInterval_Layers(t *testing.T) {
	isolateConfig(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	if got := cfg.Lifecycle.MinIntervalDuration(); got != DefaultLifecycleMinInterval {
		t.Errorf("default = %s, want %s", got, DefaultLifecycleMinInterval)
	}
	if cfg.Lifecycle.MinInterval != defaultLifecycleMinInterval {
		t.Errorf("default = %q, want %q", cfg.Lifecycle.MinInterval, defaultLifecycleMinInterval)
	}
	// The exported duration and the YAML literal are written separately.
	if d, err := time.ParseDuration(defaultLifecycleMinInterval); err != nil || d != DefaultLifecycleMinInterval {
		t.Errorf("defaultLifecycleMinInterval = %q parses to %s (%v), want %s", defaultLifecycleMinInterval, d, err, DefaultLifecycleMinInterval)
	}

	for _, tc := range []struct {
		name, file, env string
		want            time.Duration
	}{
		{"file", "45s", "", 45 * time.Second},
		{"zero in file disables", "0", "", 0},
		{"env", "", "15m", 15 * time.Minute},
		{"env beats file", "45s", "0", 0},
	} {
		isolateConfig(t)
		if tc.file != "" {
			writeUserConfig(t, "lifecycle:\n  min_interval: "+tc.file+"\n")
		}
		if tc.env != "" {
			t.Setenv("GHOST_LIFECYCLE_MIN_INTERVAL", tc.env)
		}
		cfg, err := Load()
		if err != nil {
			t.Fatalf("%s: Load(): %v", tc.name, err)
		}
		if got := cfg.Lifecycle.MinIntervalDuration(); got != tc.want {
			t.Errorf("%s: min_interval = %s, want %s", tc.name, got, tc.want)
		}
	}
}

// What a raw key value means as a cooldown. A value that cannot be read falls
// back to the compiled default, NOT to 0 (0 would silently switch the guard
// off); a negative value means off, matching scratch.max_bytes.
func TestLifecycleMinIntervalDuration(t *testing.T) {
	cases := []struct {
		name, raw string
		want      time.Duration
		warn      bool
	}{
		{name: "unset", raw: "", want: DefaultLifecycleMinInterval},
		{name: "blank", raw: "   ", want: DefaultLifecycleMinInterval},
		{name: "compound", raw: "1h30m", want: 90 * time.Minute},
		{name: "zero seconds disables", raw: "0s", want: 0},
		{name: "negative disables", raw: "-5m", want: 0, warn: true},
		{name: "unreadable keeps the default", raw: "soon", want: DefaultLifecycleMinInterval, warn: true},
		{name: "unitless keeps the default", raw: "30", want: DefaultLifecycleMinInterval, warn: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isolateConfig(t)
			warnings := captureConfigWarnings(t)
			if got := (LifecycleConfig{MinInterval: tc.raw}).MinIntervalDuration(); got != tc.want {
				t.Errorf("MinInterval(%q) = %s, want %s", tc.raw, got, tc.want)
			}
			if msg := warnings.String(); tc.warn != strings.Contains(msg, "lifecycle.min_interval") || (!tc.warn && msg != "") {
				t.Errorf("MinInterval(%q) warning = %q, want one naming the key only when warn=%v", tc.raw, msg, tc.warn)
			}
		})
	}
}

// A typo'd cooldown in a real file is reported by the same load the hook
// performs, a bare section keeps the compiled default, and the last-resort
// configs carry the cooldown.
func TestLifecycleMinInterval_ThroughLoad(t *testing.T) {
	isolateConfig(t)
	writeUserConfig(t, "lifecycle:\n  min_interval: every-turn\n")
	warnings := captureConfigWarnings(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	if got := cfg.Lifecycle.MinIntervalDuration(); got != DefaultLifecycleMinInterval {
		t.Errorf("unreadable: min_interval = %s, want the default %s", got, DefaultLifecycleMinInterval)
	}
	if !strings.Contains(warnings.String(), "lifecycle.min_interval") {
		t.Errorf("warning %q must name lifecycle.min_interval", warnings.String())
	}

	isolateConfig(t)
	writeUserConfig(t, "lifecycle:\n")
	warnings = captureConfigWarnings(t)
	if cfg, err = Load(); err != nil {
		t.Fatalf("Load(): %v", err)
	}
	if got := cfg.Lifecycle.MinIntervalDuration(); got != DefaultLifecycleMinInterval {
		t.Errorf("bare section wiped the default: %s, want %s", got, DefaultLifecycleMinInterval)
	}
	if strings.Contains(warnings.String(), "unknown key") {
		t.Errorf("a bare section header was reported as an unknown key: %q", warnings.String())
	}

	if got := FallbackConfig().Lifecycle.MinIntervalDuration(); got != DefaultLifecycleMinInterval {
		t.Errorf("FallbackConfig min_interval = %s, want %s", got, DefaultLifecycleMinInterval)
	}
	if got := defaultConfig().Lifecycle.MinIntervalDuration(); got != DefaultLifecycleMinInterval {
		t.Errorf("defaultConfig min_interval = %s, want %s", got, DefaultLifecycleMinInterval)
	}
}

// The #607 check: the key must NOT be reported as unknown while real typos
// beside it must be. The reported set is compared EXACTLY, because the natural
// typo "min_intervall" contains the real key and a substring check would pass.
func TestLifecycleMinInterval_KnownToTheUnknownKeyCheck(t *testing.T) {
	isolateConfig(t)
	path := writeUserConfig(t, "lifecycle:\n  min_interval: 10m\n  min_intervall: 99h\n  min_interva: 99h\n")
	warnings := captureConfigWarnings(t)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	if got := cfg.Lifecycle.MinIntervalDuration(); got != 10*time.Minute {
		t.Errorf("min_interval = %s, want 10m (the valid sibling key must still load)", got)
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
		t.Errorf("unknown keys reported = %v, want exactly %v", reported, want)
	}
	if !strings.Contains(got, path) {
		t.Errorf("warning %q must name the file it came from (%q)", got, path)
	}
}
