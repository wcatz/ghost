package config

import (
	"os"
	"strings"
	"testing"
)

// context.abstain_cosine is the assembler's Arm B (#580). It ships OFF, and the
// default is the assertion that matters most: an unmeasured threshold presented
// as a default is a relevance verdict nobody derived from data, and the bench
// no-answer report is explicit that the two cosine distributions overlap.
func TestContextAbstainCosineIsOffByDefault(t *testing.T) {
	isolateConfig(t)
	for _, key := range []string{"GHOST_CONTEXT_ABSTAIN_COSINE"} {
		if old, ok := os.LookupEnv(key); ok {
			_ = os.Unsetenv(key)
			t.Cleanup(func() { _ = os.Setenv(key, old) })
		}
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if cfg.Context.AbstainCosine != 0 {
		t.Errorf("context.abstain_cosine = %v, want 0 — an unmeasured floor must ship off, and 0 "+
			"is the value the assembler reads as \"arm B disabled\"", cfg.Context.AbstainCosine)
	}
}

// The key has to bind, or a user who sets it would watch the assembler ignore a
// floor they believed was in force. Both layers are checked because they decode
// by different paths: the file through koanf's YAML provider and the variable
// through the generic GHOST_ prefix transform.
func TestContextAbstainCosineBindsFromFileAndEnv(t *testing.T) {
	t.Run("file", func(t *testing.T) {
		isolateConfig(t)
		writeUserConfig(t, "context:\n  abstain_cosine: 0.62\n")

		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load() error: %v", err)
		}
		if cfg.Context.AbstainCosine != 0.62 {
			t.Errorf("context.abstain_cosine = %v, want 0.62", cfg.Context.AbstainCosine)
		}
	})

	t.Run("env", func(t *testing.T) {
		isolateConfig(t)
		t.Setenv("GHOST_CONTEXT_ABSTAIN_COSINE", "0.41")

		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load() error: %v", err)
		}
		if cfg.Context.AbstainCosine != 0.41 {
			t.Errorf("context.abstain_cosine = %v, want 0.41", cfg.Context.AbstainCosine)
		}
	})
}

// TestTheCosineParserRefusesValuesNoCosineCanTake: `strconv.ParseFloat` accepts
// "inf" and "nan" without an error, and this key is documented as a value users
// set. An infinite floor is a relevance verdict no cosine can ever clear, so
// every result outside the keyword arm would come back weak with no way to tell
// that verdict from a measured one; NaN reads as OFF, so a user who set a floor
// is told there is none. Both are typos, and a typo has to be the load error
// every other unreadable GHOST_* value produces.
func TestTheCosineParserRefusesValuesNoCosineCanTake(t *testing.T) {
	for _, bad := range []string{"inf", "-inf", "nan", "-0.4", "1.2", "not-a-number"} {
		t.Run(bad, func(t *testing.T) {
			if _, err := cosineValue(bad); err == nil {
				t.Errorf("cosineValue(%q) = nil error, want a refusal: no cosine is %q", bad, bad)
			}
		})
	}
	for _, good := range []string{"0", "0.62", "1", "0.0"} {
		t.Run("valid "+good, func(t *testing.T) {
			if _, err := cosineValue(good); err != nil {
				t.Errorf("cosineValue(%q) = %v, want a value", good, err)
			}
		})
	}
}

// TestAnUnusableCosineFromTheEnvironmentIsALoadError: the parser refusing a
// value is only worth having if refusing it is VISIBLE. This is the whole point
// of the check — an infinite floor has to reach the operator as an error naming
// the variable they set, not as a silently absent floor the assembler renders as
// `abstain_cosine=off`.
func TestAnUnusableCosineFromTheEnvironmentIsALoadError(t *testing.T) {
	isolateConfig(t)
	t.Setenv("GHOST_CONTEXT_ABSTAIN_COSINE", "inf")

	_, err := Load()

	if err == nil {
		t.Fatal("Load() accepted GHOST_CONTEXT_ABSTAIN_COSINE=inf, so the floor reads as something the user did not ask for")
	}
	if !strings.Contains(err.Error(), "GHOST_CONTEXT_ABSTAIN_COSINE") {
		t.Errorf("the error does not name the variable that was refused: %v", err)
	}
}

// TestAnUnusableCosineFromAFileIsALoadErrorToo: the env parser is not the only
// way in, and a check on one path is not a property of the key. koanf's YAML
// parser resolves `.inf`, `-.inf` and `.nan` — the same values ParseFloat takes
// silently — and nothing downstream of a file decodes differently, so
// `context: {abstain_cosine: .inf}` would arm the floor at a value no cosine can
// reach and every answer outside the keyword arm would come back weak with
// nothing in it to distinguish that from a measured verdict.
func TestAnUnusableCosineFromAFileIsALoadErrorToo(t *testing.T) {
	for _, bad := range []string{".inf", "-.inf", ".nan", "-0.5", "2"} {
		isolateConfig(t)
		writeUserConfig(t, "context:\n  abstain_cosine: "+bad+"\n")

		cfg, err := Load()
		if err == nil {
			t.Errorf("Load() accepted context.abstain_cosine: %s, leaving the arm on at %v", bad, cfg.Context.AbstainCosine)
			continue
		}
		if !strings.Contains(err.Error(), "context.abstain_cosine") {
			t.Errorf("the error for %s does not name the key: %v", bad, err)
		}
	}
}

// The refusal must not fire on a value a user can legitimately set, including
// the compiled default and both ends of the range.
func TestAUsableCosineFromAFileIsAccepted(t *testing.T) {
	for _, good := range []string{"0", "0.0", "0.62", "1", "1.0"} {
		isolateConfig(t)
		writeUserConfig(t, "context:\n  abstain_cosine: "+good+"\n")

		cfg, err := Load()
		if err != nil {
			t.Errorf("Load() refused context.abstain_cosine: %s: %v", good, err)
			continue
		}
		if cfg.Context.AbstainCosine < 0 || cfg.Context.AbstainCosine > 1 {
			t.Errorf("abstain_cosine = %v, want a value inside [0, 1]", cfg.Context.AbstainCosine)
		}
	}
}
