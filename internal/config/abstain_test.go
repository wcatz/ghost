package config

import (
	"strings"
	"testing"
)

// context.abstain_cosine is the assembler's Arm B (#580). It ships OFF (an
// unmeasured threshold presented as a default is a relevance verdict nobody
// derived from data) and binds from both layers, which decode by different paths.
func TestContextAbstainCosineDefaultAndBinding(t *testing.T) {
	isolateConfig(t)
	cfg, err := Load()
	if err != nil || cfg.Context.AbstainCosine != 0 {
		t.Fatalf("default: abstain_cosine %v err %v, want 0 (off)", cfg.Context.AbstainCosine, err)
	}

	isolateConfig(t)
	writeUserConfig(t, "context:\n  abstain_cosine: 0.62\n")
	if cfg, err = Load(); err != nil || cfg.Context.AbstainCosine != 0.62 {
		t.Errorf("file: abstain_cosine %v err %v, want 0.62", cfg.Context.AbstainCosine, err)
	}

	isolateConfig(t)
	t.Setenv("GHOST_CONTEXT_ABSTAIN_COSINE", "0.41")
	if cfg, err = Load(); err != nil || cfg.Context.AbstainCosine != 0.41 {
		t.Errorf("env: abstain_cosine %v err %v, want 0.41", cfg.Context.AbstainCosine, err)
	}
}

// strconv.ParseFloat accepts "inf" and "nan", and YAML resolves ".inf" and
// ".nan"; no cosine can take those values, so a typo has to be the load error
// every other unreadable GHOST_* value produces, from the parser, the
// environment and a file alike. Both ends of the range stay accepted.
func TestCosineValueRefusesValuesNoCosineCanTake(t *testing.T) {
	for _, bad := range []string{"inf", "-inf", "nan", "-0.4", "1.2", "not-a-number"} {
		if _, err := cosineValue(bad); err == nil {
			t.Errorf("cosineValue(%q) = nil error, want a refusal", bad)
		}
	}
	for _, good := range []string{"0", "0.62", "1", "0.0"} {
		if _, err := cosineValue(good); err != nil {
			t.Errorf("cosineValue(%q) = %v, want a value", good, err)
		}
	}

	isolateConfig(t)
	t.Setenv("GHOST_CONTEXT_ABSTAIN_COSINE", "inf")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "GHOST_CONTEXT_ABSTAIN_COSINE") {
		t.Errorf("env inf: err %v, want a refusal naming the variable", err)
	}
	for _, bad := range []string{".inf", "-.inf", ".nan", "-0.5", "2"} {
		isolateConfig(t)
		writeUserConfig(t, "context:\n  abstain_cosine: "+bad+"\n")
		if _, err := Load(); err == nil || !strings.Contains(err.Error(), "context.abstain_cosine") {
			t.Errorf("file %s: err %v, want a refusal naming the key", bad, err)
		}
	}
	for _, good := range []string{"0", "0.62", "1.0"} {
		isolateConfig(t)
		writeUserConfig(t, "context:\n  abstain_cosine: "+good+"\n")
		if _, err := Load(); err != nil {
			t.Errorf("file %s: refused: %v", good, err)
		}
	}
}
