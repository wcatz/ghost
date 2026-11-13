package config

import (
	"strings"
	"testing"
)

// context.relevance_cutoff (#954) ships ON at the value the bench sweep chose,
// binds from the environment, and refuses a value outside [0,1] or NaN from
// either layer with an error naming the key.
func TestContextRelevanceCutoff(t *testing.T) {
	isolateConfig(t)
	cfg, err := Load()
	if err != nil || cfg.Context.RelevanceCutoff != 0.63 {
		t.Fatalf("default: cutoff %v err %v, want 0.63", cfg.Context.RelevanceCutoff, err)
	}
	t.Setenv("GHOST_CONTEXT_RELEVANCE_CUTOFF", "0.4")
	if cfg, err = Load(); err != nil || cfg.Context.RelevanceCutoff != 0.4 {
		t.Fatalf("env: cutoff %v err %v, want 0.4", cfg.Context.RelevanceCutoff, err)
	}
	for _, bad := range []string{"-0.1", "1.1", ".nan"} {
		isolateConfig(t)
		writeUserConfig(t, "context:\n  relevance_cutoff: "+bad+"\n")
		if _, err := Load(); err == nil || !strings.Contains(err.Error(), "context.relevance_cutoff") {
			t.Errorf("file %s: err %v, want a refusal naming the key", bad, err)
		}
	}
	for _, bad := range []string{"-0.1", "1.1", "NaN"} {
		isolateConfig(t)
		t.Setenv("GHOST_CONTEXT_RELEVANCE_CUTOFF", bad)
		if _, err := Load(); err == nil || !strings.Contains(err.Error(), "context.relevance_cutoff") {
			t.Errorf("env %s: err %v, want a refusal naming the key", bad, err)
		}
	}
}
