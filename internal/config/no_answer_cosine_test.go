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

// The 0.62 default was measured on one embedding model, so it applies only while
// embedding.model is that model.
func TestNoAnswerBarIsOffForAnUnmeasuredModel(t *testing.T) {
	isolateConfig(t)
	writeUserConfig(t, "embedding:\n  model: some-other-model\n")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if cfg.Context.NoAnswerCosine != 0 {
		t.Errorf("no_answer_cosine = %v, want 0 (off) for an unmeasured model with no explicit bar", cfg.Context.NoAnswerCosine)
	}
	if cfg.Context.NoAnswerBarNote != NoAnswerBarOffUnmeasured {
		t.Errorf("note = %q, want %q", cfg.Context.NoAnswerBarNote, NoAnswerBarOffUnmeasured)
	}
}

func TestNoAnswerBarExplicitValueIsHonouredForAnyModel(t *testing.T) {
	isolateConfig(t)
	writeUserConfig(t, "embedding:\n  model: some-other-model\ncontext:\n  no_answer_cosine: 0.62\n")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if cfg.Context.NoAnswerCosine != 0.62 || cfg.Context.NoAnswerBarNote != "" {
		t.Errorf("file: bar %v note %q, want an explicit 0.62 honoured with no note", cfg.Context.NoAnswerCosine, cfg.Context.NoAnswerBarNote)
	}

	isolateConfig(t)
	writeUserConfig(t, "embedding:\n  model: some-other-model\n")
	t.Setenv("GHOST_CONTEXT_NO_ANSWER_COSINE", "0.55")
	cfg, err = Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if cfg.Context.NoAnswerCosine != 0.55 || cfg.Context.NoAnswerBarNote != "" {
		t.Errorf("env: bar %v note %q, want an explicit 0.55 honoured with no note", cfg.Context.NoAnswerCosine, cfg.Context.NoAnswerBarNote)
	}
}

func TestNoAnswerBarDefaultModelKeepsTheDefaultWithNoNote(t *testing.T) {
	isolateConfig(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if cfg.Context.NoAnswerCosine != DefaultNoAnswerCosine || cfg.Context.NoAnswerBarNote != "" {
		t.Errorf("bar %v note %q, want the default with no note", cfg.Context.NoAnswerCosine, cfg.Context.NoAnswerBarNote)
	}
}

// The fallback path (a broken config file) applies the same tie.
func TestNoAnswerBarTieAppliesOnTheFallbackPath(t *testing.T) {
	isolateConfig(t)
	t.Setenv("GHOST_EMBEDDING_MODEL", "some-other-model")
	cfg := FallbackConfig()
	if cfg.Context.NoAnswerCosine != 0 || cfg.Context.NoAnswerBarNote != NoAnswerBarOffUnmeasured {
		t.Errorf("fallback: bar %v note %q, want off with the note", cfg.Context.NoAnswerCosine, cfg.Context.NoAnswerBarNote)
	}
}
