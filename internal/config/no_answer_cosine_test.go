package config

import (
	"strings"
	"testing"
)

// context.no_answer_cosine (#955) ships ON at the value the bench sweep chose,
// binds from the environment, and refuses a value outside [0,1] or NaN from
// either layer with an error naming the key.
func TestContextNoAnswerCosine(t *testing.T) {
	isolateConfig(t)
	cfg, err := Load()
	if err != nil || cfg.Context.NoAnswerCosine != DefaultNoAnswerCosine || cfg.Context.NoAnswerBarNote != "" {
		t.Fatalf("default: bar %v note %q err %v, want the default with no note", cfg.Context.NoAnswerCosine, cfg.Context.NoAnswerBarNote, err)
	}
	if cfg.Context.NoAnswerCosine != 0.62 {
		t.Errorf("default = %v, want 0.62", cfg.Context.NoAnswerCosine)
	}
	t.Setenv("GHOST_CONTEXT_NO_ANSWER_COSINE", "0.4")
	if cfg, err = Load(); err != nil || cfg.Context.NoAnswerCosine != 0.4 {
		t.Fatalf("env: bar %v err %v, want 0.4", cfg.Context.NoAnswerCosine, err)
	}
	for _, bad := range []string{"-0.1", "1.1", ".nan"} {
		isolateConfig(t)
		writeUserConfig(t, "context:\n  no_answer_cosine: "+bad+"\n")
		if _, err := Load(); err == nil || !strings.Contains(err.Error(), "context.no_answer_cosine") {
			t.Errorf("file %s: err %v, want a refusal naming the key", bad, err)
		}
	}
	for _, bad := range []string{"-0.1", "1.1", "NaN"} {
		isolateConfig(t)
		t.Setenv("GHOST_CONTEXT_NO_ANSWER_COSINE", bad)
		if _, err := Load(); err == nil || !strings.Contains(err.Error(), "context.no_answer_cosine") {
			t.Errorf("env %s: err %v, want a refusal naming the key", bad, err)
		}
	}
}

// The 0.62 default was measured on one embedding model, so it applies only while
// embedding.model is that model; an explicit bar is honoured for any model, and
// the fallback path applies the same tie.
func TestNoAnswerBarTiedToTheMeasuredModel(t *testing.T) {
	const other = "embedding:\n  model: some-other-model\n"
	isolateConfig(t)
	writeUserConfig(t, other)
	cfg, err := Load()
	if err != nil || cfg.Context.NoAnswerCosine != 0 || cfg.Context.NoAnswerBarNote != NoAnswerBarOffUnmeasured {
		t.Errorf("unmeasured: bar %v note %q err %v, want off with the note", cfg.Context.NoAnswerCosine, cfg.Context.NoAnswerBarNote, err)
	}

	isolateConfig(t)
	writeUserConfig(t, other+"context:\n  no_answer_cosine: 0.62\n")
	if cfg, err = Load(); err != nil || cfg.Context.NoAnswerCosine != 0.62 || cfg.Context.NoAnswerBarNote != "" {
		t.Errorf("file: bar %v note %q err %v, want an explicit 0.62 with no note", cfg.Context.NoAnswerCosine, cfg.Context.NoAnswerBarNote, err)
	}

	isolateConfig(t)
	writeUserConfig(t, other)
	t.Setenv("GHOST_CONTEXT_NO_ANSWER_COSINE", "0.55")
	if cfg, err = Load(); err != nil || cfg.Context.NoAnswerCosine != 0.55 || cfg.Context.NoAnswerBarNote != "" {
		t.Errorf("env: bar %v note %q err %v, want an explicit 0.55 with no note", cfg.Context.NoAnswerCosine, cfg.Context.NoAnswerBarNote, err)
	}

	isolateConfig(t)
	t.Setenv("GHOST_EMBEDDING_MODEL", "some-other-model")
	if fb := FallbackConfig(); fb.Context.NoAnswerCosine != 0 || fb.Context.NoAnswerBarNote != NoAnswerBarOffUnmeasured {
		t.Errorf("fallback: bar %v note %q, want off with the note", fb.Context.NoAnswerCosine, fb.Context.NoAnswerBarNote)
	}
}
