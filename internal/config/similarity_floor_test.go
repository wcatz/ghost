package config

import (
	"strings"
	"testing"
)

// search.min_similarity and linking.threshold are cosines in [0, 1]. A NaN,
// infinite or out-of-range value silently empties the vector leg or the linking
// worker's edges, so both layers refuse it with an error naming the key, the
// fallback replaces it with the compiled default and warns, and every value the
// key is documented to take stays accepted. The file spells the non-finite
// values `.inf`/`.nan` and the environment `inf`/`nan`.
func TestUnitFloatKeysRefuseUnusableValues(t *testing.T) {
	cases := []struct {
		key, section, field, env string
		get                      func(*Config) float64
	}{
		{"search.min_similarity", "search", "min_similarity", "GHOST_SEARCH_MIN_SIMILARITY", func(c *Config) float64 { return c.Search.MinSimilarity }},
		{"linking.threshold", "linking", "threshold", "GHOST_LINKING_THRESHOLD", func(c *Config) float64 { return c.Linking.Threshold }},
	}
	for _, c := range cases {
		for _, bad := range []string{".inf", "-.inf", ".nan", "-0.5", "7"} {
			isolateConfig(t)
			writeUserConfig(t, c.section+":\n  "+c.field+": "+bad+"\n")
			if _, err := Load(); err == nil || !strings.Contains(err.Error(), c.key) {
				t.Errorf("file %s=%s: err %v, want a refusal naming the key", c.key, bad, err)
			}
		}
		for _, bad := range []string{"inf", "-inf", "nan", "-0.5", "7"} {
			isolateConfig(t)
			t.Setenv(c.env, bad)
			if _, err := Load(); err == nil || !strings.Contains(err.Error(), c.key) {
				t.Errorf("env %s=%s: err %v, want a refusal naming the key", c.env, bad, err)
			}

			isolateConfig(t)
			warnings := captureConfigWarnings(t)
			t.Setenv(c.env, bad)
			if got := c.get(FallbackConfig()); !inUnitRange(got) {
				t.Errorf("FallbackConfig with %s=%s: %v, want a value in [0,1]", c.env, bad, got)
			}
			if !strings.Contains(warnings.String(), c.key) {
				t.Errorf("FallbackConfig replaced %s=%s without a warning naming the key: %q", c.env, bad, warnings.String())
			}
		}
		for _, good := range []string{"0", "0.5", "1"} {
			isolateConfig(t)
			writeUserConfig(t, c.section+":\n  "+c.field+": "+good+"\n")
			cfg, err := Load()
			if err != nil || !inUnitRange(c.get(cfg)) {
				t.Errorf("file %s=%s: value %v err %v, want it accepted", c.key, good, c.get(cfg), err)
			}
		}
	}

	// Both keys bad at once: each is replaced, not only the first one reached.
	isolateConfig(t)
	captureConfigWarnings(t)
	t.Setenv("GHOST_SEARCH_MIN_SIMILARITY", "7")
	t.Setenv("GHOST_LINKING_THRESHOLD", "nan")
	cfg := FallbackConfig()
	if !inUnitRange(cfg.Search.MinSimilarity) || !inUnitRange(cfg.Linking.Threshold) {
		t.Errorf("FallbackConfig floors = %v, %v, want both in [0,1]", cfg.Search.MinSimilarity, cfg.Linking.Threshold)
	}
}
