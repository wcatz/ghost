package config

import (
	"math"
	"strings"
	"testing"
	"time"
)

// reflection.lifecycle_timeout_minutes and reflection.consolidation_timeout_minutes
// bound a lifecycle phase and one consolidation call. Only an explicit 0
// removes the bound: a negative value used to remove it too, with no warning,
// and a value too large for a time.Duration wrapped into a negative duration
// that removed it the same way.

// largestMinutes is the largest minute count a time.Duration can hold,
// computed here rather than read from the package so a wrong constant there
// cannot agree with itself.
const largestMinutes = int(math.MaxInt64 / int64(time.Minute))

func TestReflectionTimeouts(t *testing.T) {
	keys := []struct {
		key     string
		def     time.Duration
		resolve func(int) time.Duration
	}{
		{
			key: "reflection.lifecycle_timeout_minutes",
			def: DefaultLifecycleTimeoutMinutes * time.Minute,
			resolve: func(m int) time.Duration {
				return ReflectionConfig{LifecycleTimeoutMinutes: m}.LifecycleTimeout()
			},
		},
		{
			key: "reflection.consolidation_timeout_minutes",
			def: DefaultConsolidationTimeoutMinutes * time.Minute,
			resolve: func(m int) time.Duration {
				return ReflectionConfig{ConsolidationTimeoutMinutes: m}.ConsolidationTimeout()
			},
		},
	}
	largest := time.Duration(largestMinutes) * time.Minute
	for _, k := range keys {
		cases := []struct {
			name    string
			minutes int
			want    time.Duration
			warn    bool
		}{
			{name: "zero removes the bound", minutes: 0, want: 0},
			{name: "ordinary", minutes: 45, want: 45 * time.Minute},
			{name: "largest representable", minutes: largestMinutes, want: largest},
			{name: "negative keeps the default", minutes: -5, want: k.def, warn: true},
			{name: "minus one keeps the default", minutes: -1, want: k.def, warn: true},
			{name: "one past the largest is clamped", minutes: largestMinutes + 1, want: largest, warn: true},
			{name: "overflow is clamped", minutes: 200000000, want: largest, warn: true},
		}
		for _, tc := range cases {
			t.Run(k.key+"/"+tc.name, func(t *testing.T) {
				isolateConfig(t)
				warnings := captureConfigWarnings(t)

				got := k.resolve(tc.minutes)
				if got != tc.want {
					t.Errorf("%s=%d resolves to %s, want %s", k.key, tc.minutes, got, tc.want)
				}
				msg := warnings.String()
				if !tc.warn {
					if msg != "" {
						t.Errorf("%s=%d warned on a valid value: %q", k.key, tc.minutes, msg)
					}
					return
				}
				if !strings.Contains(msg, k.key) {
					t.Errorf("warning %q must name %s", msg, k.key)
				}
			})
		}
	}
}

// TestReflectionTimeouts_NegativeWarnsThroughLoad pins that a negative value
// in a real config file is reported and resolved to the default by the same
// load the lifecycle performs, rather than accepted as "no bound".
func TestReflectionTimeouts_NegativeWarnsThroughLoad(t *testing.T) {
	isolateConfig(t)
	writeUserConfig(t, "reflection:\n  lifecycle_timeout_minutes: -5\n  consolidation_timeout_minutes: -5\n")
	warnings := captureConfigWarnings(t)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	if got, want := cfg.Reflection.LifecycleTimeout(), DefaultLifecycleTimeoutMinutes*time.Minute; got != want {
		t.Errorf("lifecycle timeout = %s, want the compiled default %s", got, want)
	}
	if got, want := cfg.Reflection.ConsolidationTimeout(), DefaultConsolidationTimeoutMinutes*time.Minute; got != want {
		t.Errorf("consolidation timeout = %s, want the compiled default %s", got, want)
	}
	msg := warnings.String()
	for _, key := range []string{"reflection.lifecycle_timeout_minutes", "reflection.consolidation_timeout_minutes"} {
		if !strings.Contains(msg, key) {
			t.Errorf("warning %q must name %s", msg, key)
		}
	}
}

// TestReflectionTimeouts_CompiledDefaults pins that the resolved defaults are
// the constants a negative value falls back to.
func TestReflectionTimeouts_CompiledDefaults(t *testing.T) {
	isolateConfig(t)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	if got := cfg.Reflection.LifecycleTimeout(); got != 60*time.Minute {
		t.Errorf("lifecycle timeout = %s, want 60m", got)
	}
	if got := cfg.Reflection.ConsolidationTimeout(); got != 10*time.Minute {
		t.Errorf("consolidation timeout = %s, want 10m", got)
	}
}
