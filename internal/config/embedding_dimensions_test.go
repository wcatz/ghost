package config

import (
	"strings"
	"testing"
)

// embedding.dimensions is the width every vector is checked against. A width of
// zero or less is one no model produces, so it is a load error naming the key
// from the file, the environment, and even with embedding disabled (the
// identity is stamped whether or not the worker runs). The fallback replaces it
// with the compiled default and warns, and keeps a valid width.
func TestEmbeddingDimensions(t *testing.T) {
	for _, bad := range []string{"0", "-1"} {
		isolateConfig(t)
		writeUserConfig(t, "embedding:\n  dimensions: "+bad+"\n")
		if _, err := Load(); err == nil || !strings.Contains(err.Error(), "embedding.dimensions") || !strings.Contains(err.Error(), bad) {
			t.Errorf("file %s: err %v, want a refusal naming the key and value", bad, err)
		}

		isolateConfig(t)
		t.Setenv("GHOST_EMBEDDING_DIMENSIONS", bad)
		if _, err := Load(); err == nil || !strings.Contains(err.Error(), "embedding.dimensions") {
			t.Errorf("env %s: err %v, want a refusal naming the key", bad, err)
		}

		isolateConfig(t)
		warnings := captureConfigWarnings(t)
		t.Setenv("GHOST_EMBEDDING_DIMENSIONS", bad)
		if got, want := FallbackConfig().Embedding.Dimensions, defaultConfig().Embedding.Dimensions; got != want {
			t.Errorf("fallback %s: dimensions = %d, want the %d default", bad, got, want)
		}
		if !strings.Contains(warnings.String(), "embedding.dimensions") {
			t.Errorf("fallback %s: no warning naming the key: %q", bad, warnings.String())
		}
	}

	isolateConfig(t)
	writeUserConfig(t, "embedding:\n  enabled: false\n  dimensions: 0\n")
	if _, err := Load(); err == nil {
		t.Error("Load() accepted embedding.dimensions: 0 with embedding disabled")
	}

	isolateConfig(t)
	captureConfigWarnings(t)
	t.Setenv("GHOST_EMBEDDING_DIMENSIONS", "0")
	if got, want := LoadForHook().Embedding.Dimensions, defaultConfig().Embedding.Dimensions; got != want {
		t.Errorf("LoadForHook: dimensions = %d, want the %d default", got, want)
	}

	isolateConfig(t)
	t.Setenv("GHOST_EMBEDDING_DIMENSIONS", "1024")
	if cfg, err := Load(); err != nil || cfg.Embedding.Dimensions != 1024 {
		t.Errorf("Load: dimensions %v err %v, want 1024", cfg.Embedding.Dimensions, err)
	}
	if got := FallbackConfig().Embedding.Dimensions; got != 1024 {
		t.Errorf("FallbackConfig dimensions = %d, want 1024", got)
	}
}
