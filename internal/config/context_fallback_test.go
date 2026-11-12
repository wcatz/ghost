package config

import (
	"math"
	"strings"
	"testing"
)

// The fallback keeps the GHOST_* environment, and the environment parsers for
// context.relevance_cutoff and context.no_answer_cosine read any float. Load
// refuses an out-of-range value, so the fallback must replace each bad value
// with its compiled default and say so, or every search would fail in the
// assembler's request check.

func inUnitRange(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0) && v >= 0 && v <= 1
}

func TestFallbackConfigContextValues(t *testing.T) {
	cases := []struct {
		key, env string
		get      func(*Config) float64
		def      float64
	}{
		{"context.relevance_cutoff", "GHOST_CONTEXT_RELEVANCE_CUTOFF", func(c *Config) float64 { return c.Context.RelevanceCutoff }, DefaultRelevanceCutoff},
		{"context.no_answer_cosine", "GHOST_CONTEXT_NO_ANSWER_COSINE", func(c *Config) float64 { return c.Context.NoAnswerCosine }, DefaultNoAnswerCosine},
	}
	for _, c := range cases {
		for _, bad := range []string{"5", "-0.5", "NaN", "Inf"} {
			isolateConfig(t)
			warnings := captureConfigWarnings(t)
			t.Setenv(c.env, bad)
			if got := c.get(FallbackConfig()); got != c.def {
				t.Errorf("%s=%s: %v, want the %v default", c.env, bad, got, c.def)
			}
			if !strings.Contains(warnings.String(), c.key) {
				t.Errorf("%s=%s replaced without a warning naming the key: %q", c.env, bad, warnings.String())
			}
		}
		isolateConfig(t)
		t.Setenv(c.env, "0.3")
		if got := c.get(FallbackConfig()); got != 0.3 {
			t.Errorf("%s=0.3 (valid): %v, want it kept", c.env, got)
		}
	}

	// Both keys bad at once: each is replaced, on the hook path too.
	isolateConfig(t)
	captureConfigWarnings(t)
	t.Setenv("GHOST_CONTEXT_RELEVANCE_CUTOFF", "-0.5")
	t.Setenv("GHOST_CONTEXT_NO_ANSWER_COSINE", "5")
	cfg := FallbackConfig()
	if err := checkValueChecks(cfg); err != nil {
		t.Errorf("FallbackConfig still carries an out-of-range context value: %v", err)
	}
	if cfg.Context.RelevanceCutoff != DefaultRelevanceCutoff || cfg.Context.NoAnswerCosine != DefaultNoAnswerCosine {
		t.Errorf("FallbackConfig context = %+v, want both compiled defaults", cfg.Context)
	}
	hook := LoadForHook()
	if !inUnitRange(hook.Context.RelevanceCutoff) || !inUnitRange(hook.Context.NoAnswerCosine) {
		t.Errorf("LoadForHook context = %+v, want values in [0,1]", hook.Context)
	}

	// A rejected bar is not an explicit one: under an unmeasured model the
	// compiled default is OFF, with the model-tie note, not 0.62.
	isolateConfig(t)
	captureConfigWarnings(t)
	t.Setenv("GHOST_EMBEDDING_MODEL", "mxbai-embed-large")
	t.Setenv("GHOST_CONTEXT_NO_ANSWER_COSINE", "5")
	cfg = FallbackConfig()
	if cfg.Context.NoAnswerCosine != 0 || cfg.Context.NoAnswerBarNote != NoAnswerBarOffUnmeasured {
		t.Errorf("rejected bar under an unmeasured model: %v note %q, want 0 with the note", cfg.Context.NoAnswerCosine, cfg.Context.NoAnswerBarNote)
	}
}

// Every key decodeFallback checks has a reset, and the reset writes a value the
// same check accepts. A key added to valueChecks without one fails here rather
// than leaving the fallback to carry the bad value.
func TestValueChecksEachResetToAValueTheirCheckAccepts(t *testing.T) {
	for _, c := range valueChecks {
		if c.reset == nil {
			t.Errorf("%s has no reset: the fallback would detect a bad value and keep it", c.key)
			continue
		}
		cfg := defaultConfig()
		cfg.Context = ContextConfig{
			AbstainCosine:   float32(math.NaN()),
			RelevanceCutoff: math.NaN(),
			NoAnswerCosine:  math.NaN(),
		}
		cfg.Search.MinSimilarity = math.NaN()
		cfg.Linking.Threshold = math.NaN()
		if err := c.check(cfg); err == nil {
			t.Errorf("%s: the check accepted NaN, so this test cannot see its reset", c.key)
			continue
		}
		c.reset(cfg)
		if err := c.check(cfg); err != nil {
			t.Errorf("%s: after its reset the check still refuses: %v", c.key, err)
		}
	}
}
