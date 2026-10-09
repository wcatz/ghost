package config

import (
	"strings"
	"testing"
)

// context.relevance_cutoff (#954) ships ON at the value the bench sweep chose.
func TestContextRelevanceCutoffDefault(t *testing.T) {
	isolateConfig(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if cfg.Context.RelevanceCutoff != 0.63 {
		t.Errorf("context.relevance_cutoff = %v, want the 0.63 default", cfg.Context.RelevanceCutoff)
	}
}

func TestContextRelevanceCutoffEnvOverride(t *testing.T) {
	isolateConfig(t)
	t.Setenv("GHOST_CONTEXT_RELEVANCE_CUTOFF", "0.4")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if cfg.Context.RelevanceCutoff != 0.4 {
		t.Errorf("context.relevance_cutoff = %v, want 0.4 from the environment", cfg.Context.RelevanceCutoff)
	}
}

// A cutoff outside [0,1] or NaN is a load error naming the key, from the file.
func TestContextRelevanceCutoffRefusesOutOfRange(t *testing.T) {
	for _, bad := range []string{"-0.1", "1.1", ".nan"} {
		isolateConfig(t)
		writeUserConfig(t, "context:\n  relevance_cutoff: "+bad+"\n")
		_, err := Load()
		if err == nil {
			t.Errorf("Load() accepted context.relevance_cutoff: %s", bad)
			continue
		}
		if !strings.Contains(err.Error(), "context.relevance_cutoff") {
			t.Errorf("the error for %s does not name the key: %v", bad, err)
		}
	}
}

func TestContextRelevanceCutoffRefusesOutOfRangeFromEnv(t *testing.T) {
	for _, bad := range []string{"-0.1", "1.1", "NaN"} {
		isolateConfig(t)
		t.Setenv("GHOST_CONTEXT_RELEVANCE_CUTOFF", bad)
		_, err := Load()
		if err == nil {
			t.Errorf("Load() accepted GHOST_CONTEXT_RELEVANCE_CUTOFF=%s", bad)
			continue
		}
		if !strings.Contains(err.Error(), "context.relevance_cutoff") {
			t.Errorf("the error for %s does not name the key: %v", bad, err)
		}
	}
}
