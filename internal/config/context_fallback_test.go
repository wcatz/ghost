package config

import (
	"math"
	"strings"
	"testing"
)

// The fallback keeps the GHOST_* environment, and the environment parsers for
// context.relevance_cutoff and context.no_answer_cosine read any float. Load
// refuses an out-of-range value through checkContextValues, so the MCP server
// falls back — and without the same check there, the fallback re-read the bad
// value and every ghost_memory_search failed in the assembler's request check.
// The fallback replaces each bad value with its compiled default and says so.

// badContextValues are the spellings floatValue accepts and a [0,1] value
// cannot be.
var badContextValues = []string{"5", "-0.5", "NaN", "Inf"}

func inUnitRange(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0) && v >= 0 && v <= 1
}

func TestFallbackConfigNeverCarriesAnOutOfRangeRelevanceCutoff(t *testing.T) {
	for _, bad := range badContextValues {
		isolateConfig(t)
		warnings := captureConfigWarnings(t)
		t.Setenv("GHOST_CONTEXT_RELEVANCE_CUTOFF", bad)

		if got := FallbackConfig().Context.RelevanceCutoff; got != DefaultRelevanceCutoff {
			t.Errorf("FallbackConfig with GHOST_CONTEXT_RELEVANCE_CUTOFF=%s: relevance_cutoff = %v, want the %v default", bad, got, DefaultRelevanceCutoff)
		}
		if !strings.Contains(warnings.String(), "context.relevance_cutoff") {
			t.Errorf("FallbackConfig replaced GHOST_CONTEXT_RELEVANCE_CUTOFF=%s without a warning naming the key: %q", bad, warnings.String())
		}
	}
}

func TestFallbackConfigNeverCarriesAnOutOfRangeNoAnswerCosine(t *testing.T) {
	for _, bad := range badContextValues {
		isolateConfig(t)
		warnings := captureConfigWarnings(t)
		t.Setenv("GHOST_CONTEXT_NO_ANSWER_COSINE", bad)

		if got := FallbackConfig().Context.NoAnswerCosine; got != DefaultNoAnswerCosine {
			t.Errorf("FallbackConfig with GHOST_CONTEXT_NO_ANSWER_COSINE=%s: no_answer_cosine = %v, want the %v default", bad, got, DefaultNoAnswerCosine)
		}
		if !strings.Contains(warnings.String(), "context.no_answer_cosine") {
			t.Errorf("FallbackConfig replaced GHOST_CONTEXT_NO_ANSWER_COSINE=%s without a warning naming the key: %q", bad, warnings.String())
		}
	}
}

// Both keys bad at once: each is replaced, not only the first one the check
// reaches.
func TestFallbackConfigReplacesEveryOutOfRangeContextValue(t *testing.T) {
	isolateConfig(t)
	captureConfigWarnings(t)
	t.Setenv("GHOST_CONTEXT_RELEVANCE_CUTOFF", "-0.5")
	t.Setenv("GHOST_CONTEXT_NO_ANSWER_COSINE", "5")

	cfg := FallbackConfig()
	if err := checkContextValues(cfg); err != nil {
		t.Errorf("FallbackConfig still carries an out-of-range context value: %v", err)
	}
	if cfg.Context.RelevanceCutoff != DefaultRelevanceCutoff || cfg.Context.NoAnswerCosine != DefaultNoAnswerCosine {
		t.Errorf("FallbackConfig context = %+v, want both compiled defaults", cfg.Context)
	}
}

// A rejected bar is not an explicit one: with an embedding model the default
// bar was not measured on, the compiled default is OFF, so the replacement is
// 0 with the model-tie note, not 0.62.
func TestFallbackConfigRejectedNoAnswerBarFollowsTheModelTie(t *testing.T) {
	isolateConfig(t)
	captureConfigWarnings(t)
	t.Setenv("GHOST_EMBEDDING_MODEL", "mxbai-embed-large")
	t.Setenv("GHOST_CONTEXT_NO_ANSWER_COSINE", "5")

	cfg := FallbackConfig()
	if cfg.Context.NoAnswerCosine != 0 {
		t.Errorf("no_answer_cosine = %v, want 0: a rejected bar under an unmeasured model must not apply 0.62", cfg.Context.NoAnswerCosine)
	}
	if cfg.Context.NoAnswerBarNote != NoAnswerBarOffUnmeasured {
		t.Errorf("NoAnswerBarNote = %q, want %q", cfg.Context.NoAnswerBarNote, NoAnswerBarOffUnmeasured)
	}
}

func TestLoadForHookNeverCarriesAnOutOfRangeContextValue(t *testing.T) {
	isolateConfig(t)
	captureConfigWarnings(t)
	t.Setenv("GHOST_CONTEXT_RELEVANCE_CUTOFF", "-0.5")
	t.Setenv("GHOST_CONTEXT_NO_ANSWER_COSINE", "5")

	cfg := LoadForHook()
	if v := cfg.Context.RelevanceCutoff; !inUnitRange(v) {
		t.Errorf("LoadForHook relevance_cutoff = %v, want a value in [0,1]", v)
	}
	if v := cfg.Context.NoAnswerCosine; !inUnitRange(v) {
		t.Errorf("LoadForHook no_answer_cosine = %v, want a value in [0,1]", v)
	}
}

// A valid environment value survives the fallback untouched.
func TestFallbackConfigKeepsInRangeContextValues(t *testing.T) {
	isolateConfig(t)
	t.Setenv("GHOST_CONTEXT_RELEVANCE_CUTOFF", "0.4")
	t.Setenv("GHOST_CONTEXT_NO_ANSWER_COSINE", "0.3")

	cfg := FallbackConfig()
	if cfg.Context.RelevanceCutoff != 0.4 {
		t.Errorf("relevance_cutoff = %v, want 0.4 from the environment", cfg.Context.RelevanceCutoff)
	}
	if cfg.Context.NoAnswerCosine != 0.3 {
		t.Errorf("no_answer_cosine = %v, want 0.3 from the environment", cfg.Context.NoAnswerCosine)
	}
}
