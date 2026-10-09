package config

import (
	"strings"
	"testing"
)

// context.no_answer_cosine (#955) ships ON at the value the bench sweep chose.
func TestContextNoAnswerCosineDefault(t *testing.T) {
	isolateConfig(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if cfg.Context.NoAnswerCosine != 0.62 {
		t.Errorf("context.no_answer_cosine = %v, want the 0.62 default", cfg.Context.NoAnswerCosine)
	}
}

func TestContextNoAnswerCosineEnvOverride(t *testing.T) {
	isolateConfig(t)
	t.Setenv("GHOST_CONTEXT_NO_ANSWER_COSINE", "0.4")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if cfg.Context.NoAnswerCosine != 0.4 {
		t.Errorf("context.no_answer_cosine = %v, want 0.4 from the environment", cfg.Context.NoAnswerCosine)
	}
}

// A cutoff outside [0,1] or NaN is a load error naming the key, from the file.
func TestContextNoAnswerCosineRefusesOutOfRange(t *testing.T) {
	for _, bad := range []string{"-0.1", "1.1", ".nan"} {
		isolateConfig(t)
		writeUserConfig(t, "context:\n  no_answer_cosine: "+bad+"\n")
		_, err := Load()
		if err == nil {
			t.Errorf("Load() accepted context.no_answer_cosine: %s", bad)
			continue
		}
		if !strings.Contains(err.Error(), "context.no_answer_cosine") {
			t.Errorf("the error for %s does not name the key: %v", bad, err)
		}
	}
}

func TestContextNoAnswerCosineRefusesOutOfRangeFromEnv(t *testing.T) {
	for _, bad := range []string{"-0.1", "1.1", "NaN"} {
		isolateConfig(t)
		t.Setenv("GHOST_CONTEXT_NO_ANSWER_COSINE", bad)
		_, err := Load()
		if err == nil {
			t.Errorf("Load() accepted GHOST_CONTEXT_NO_ANSWER_COSINE=%s", bad)
			continue
		}
		if !strings.Contains(err.Error(), "context.no_answer_cosine") {
			t.Errorf("the error for %s does not name the key: %v", bad, err)
		}
	}
}
