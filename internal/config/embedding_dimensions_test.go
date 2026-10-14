package config

import (
	"strings"
	"testing"
)

// embedding.dimensions is the width every vector is checked against and the
// width stamped into the vector-space identity. A width of zero or less is one
// no model produces: the identity drops its width stamp, so every stored vector
// reads as stale, and the client refuses every new vector, so none of them can
// ever be rewritten. It is a load error naming the key, from the file and from
// the environment.
func TestEmbeddingDimensionsRefusesNonPositiveFromFile(t *testing.T) {
	for _, bad := range []string{"0", "-1"} {
		isolateConfig(t)
		writeUserConfig(t, "embedding:\n  dimensions: "+bad+"\n")
		_, err := Load()
		if err == nil {
			t.Errorf("Load() accepted embedding.dimensions: %s", bad)
			continue
		}
		if !strings.Contains(err.Error(), "embedding.dimensions") || !strings.Contains(err.Error(), bad) {
			t.Errorf("the error for %s does not name the key and the value: %v", bad, err)
		}
	}
}

func TestEmbeddingDimensionsRefusesNonPositiveFromEnv(t *testing.T) {
	for _, bad := range []string{"0", "-1"} {
		isolateConfig(t)
		t.Setenv("GHOST_EMBEDDING_DIMENSIONS", bad)
		_, err := Load()
		if err == nil {
			t.Errorf("Load() accepted GHOST_EMBEDDING_DIMENSIONS=%s", bad)
			continue
		}
		if !strings.Contains(err.Error(), "embedding.dimensions") {
			t.Errorf("the error for %s does not name the key: %v", bad, err)
		}
	}
}

// The refusal holds with embedding switched off too: the identity is stamped
// whether or not the worker runs, so a zero width still retires the corpus.
func TestEmbeddingDimensionsRefusedEvenWithEmbeddingDisabled(t *testing.T) {
	isolateConfig(t)
	writeUserConfig(t, "embedding:\n  enabled: false\n  dimensions: 0\n")
	if _, err := Load(); err == nil {
		t.Fatal("Load() accepted embedding.dimensions: 0 with embedding disabled")
	}
}

func TestEmbeddingDimensionsAcceptsAPositiveWidth(t *testing.T) {
	isolateConfig(t)
	t.Setenv("GHOST_EMBEDDING_DIMENSIONS", "1024")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if cfg.Embedding.Dimensions != 1024 {
		t.Errorf("embedding.dimensions = %d, want 1024 from the environment", cfg.Embedding.Dimensions)
	}
}

// The fallback keeps the environment, so a refusal at Load alone would leave
// the MCP server and every hook running at width zero: Load fails, the caller
// falls back, and the fallback re-reads GHOST_EMBEDDING_DIMENSIONS=0. The
// fallback replaces it with the compiled default and says so.
func TestFallbackConfigNeverCarriesANonPositiveDimension(t *testing.T) {
	want := defaultConfig().Embedding.Dimensions
	for _, bad := range []string{"0", "-1"} {
		isolateConfig(t)
		warnings := captureConfigWarnings(t)
		t.Setenv("GHOST_EMBEDDING_DIMENSIONS", bad)

		if got := FallbackConfig().Embedding.Dimensions; got != want {
			t.Errorf("FallbackConfig with GHOST_EMBEDDING_DIMENSIONS=%s: dimensions = %d, want the %d default", bad, got, want)
		}
		if !strings.Contains(warnings.String(), "embedding.dimensions") {
			t.Errorf("FallbackConfig replaced GHOST_EMBEDDING_DIMENSIONS=%s without a warning naming the key: %q", bad, warnings.String())
		}
	}
}

func TestLoadForHookNeverCarriesANonPositiveDimension(t *testing.T) {
	isolateConfig(t)
	captureConfigWarnings(t)
	t.Setenv("GHOST_EMBEDDING_DIMENSIONS", "0")

	want := defaultConfig().Embedding.Dimensions
	if got := LoadForHook().Embedding.Dimensions; got != want {
		t.Errorf("LoadForHook with GHOST_EMBEDDING_DIMENSIONS=0: dimensions = %d, want the %d default", got, want)
	}
}

// A valid environment width survives the fallback untouched.
func TestFallbackConfigKeepsAPositiveDimension(t *testing.T) {
	isolateConfig(t)
	t.Setenv("GHOST_EMBEDDING_DIMENSIONS", "1024")
	if got := FallbackConfig().Embedding.Dimensions; got != 1024 {
		t.Errorf("FallbackConfig dimensions = %d, want 1024 from the environment", got)
	}
}
