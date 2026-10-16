package config

import (
	"strings"
	"testing"
)

// badUnitFloats are the spellings both a config file and the environment accept
// for a key documented as a value in [0, 1], and that a value in [0, 1] cannot
// be. koanf's YAML parser resolves `.inf`/`-.inf`/`.nan` into the float field
// exactly as `strconv.ParseFloat` does, so the file path needs its own check
// rather than leaning on the environment parser.
var badUnitFloats = []string{".inf", "-.inf", ".nan", "-0.5", "2", "7"}

// badUnitFloatsEnv is badUnitFloats written the way the environment spells
// them: no leading dot on the non-finite values.
var badUnitFloatsEnv = []string{"inf", "-inf", "nan", "-0.5", "2", "7"}

// A non-finite or out-of-range search.min_similarity empties the vector leg of
// search: the leg's own filter drops every candidate at or below the floor, so
// an infinite or NaN floor keeps nothing, hybrid search answers from the
// keyword arm alone, and nothing says the vector leg is gone. A typo in a
// user's own config is the ordinary way this reaches a store.
func TestAnUnusableSearchFloorFromAFileIsALoadError(t *testing.T) {
	for _, bad := range badUnitFloats {
		isolateConfig(t)
		writeUserConfig(t, "search:\n  min_similarity: "+bad+"\n")

		cfg, err := Load()
		if err == nil {
			t.Errorf("Load() accepted search.min_similarity: %s (loaded %v), so the vector leg is silently gone", bad, cfg.Search.MinSimilarity)
			continue
		}
		if !strings.Contains(err.Error(), "search.min_similarity") {
			t.Errorf("the error for %s does not name the key: %v", bad, err)
		}
	}
}

// The same value from the environment override has to be the same load error:
// `floatValue` reads any float, so the refusal cannot come from the parser.
func TestAnUnusableSearchFloorFromTheEnvironmentIsALoadError(t *testing.T) {
	for _, bad := range badUnitFloatsEnv {
		isolateConfig(t)
		t.Setenv("GHOST_SEARCH_MIN_SIMILARITY", bad)

		_, err := Load()
		if err == nil {
			t.Errorf("Load() accepted GHOST_SEARCH_MIN_SIMILARITY=%s", bad)
			continue
		}
		if !strings.Contains(err.Error(), "search.min_similarity") {
			t.Errorf("the error for %s does not name the key: %v", bad, err)
		}
	}
}

// The refusal must not fire on a value the key is documented to take, including
// the compiled default and both ends of the range.
func TestAUsableSearchFloorIsAccepted(t *testing.T) {
	for _, good := range []string{"0", "0.0", "0.5", "1", "1.0"} {
		isolateConfig(t)
		writeUserConfig(t, "search:\n  min_similarity: "+good+"\n")

		cfg, err := Load()
		if err != nil {
			t.Errorf("Load() refused search.min_similarity: %s: %v", good, err)
			continue
		}
		if cfg.Search.MinSimilarity < 0 || cfg.Search.MinSimilarity > 1 {
			t.Errorf("search.min_similarity = %v, want a value inside [0, 1]", cfg.Search.MinSimilarity)
		}
	}
}

// linking.threshold is the same class of value — a cosine in [0, 1] — and gets
// the same refusal on both paths, even though nothing in production reads it
// today: the day a caller does read it, a NaN threshold must not be sitting in
// the config waiting.
func TestAnUnusableLinkingThresholdFromAFileIsALoadError(t *testing.T) {
	for _, bad := range badUnitFloats {
		isolateConfig(t)
		writeUserConfig(t, "linking:\n  threshold: "+bad+"\n")

		cfg, err := Load()
		if err == nil {
			t.Errorf("Load() accepted linking.threshold: %s (loaded %v)", bad, cfg.Linking.Threshold)
			continue
		}
		if !strings.Contains(err.Error(), "linking.threshold") {
			t.Errorf("the error for %s does not name the key: %v", bad, err)
		}
	}
}

func TestAnUnusableLinkingThresholdFromTheEnvironmentIsALoadError(t *testing.T) {
	for _, bad := range badUnitFloatsEnv {
		isolateConfig(t)
		t.Setenv("GHOST_LINKING_THRESHOLD", bad)

		_, err := Load()
		if err == nil {
			t.Errorf("Load() accepted GHOST_LINKING_THRESHOLD=%s", bad)
			continue
		}
		if !strings.Contains(err.Error(), "linking.threshold") {
			t.Errorf("the error for %s does not name the key: %v", bad, err)
		}
	}
}

func TestAUsableLinkingThresholdIsAccepted(t *testing.T) {
	for _, good := range []string{"0", "0.7", "1"} {
		isolateConfig(t)
		writeUserConfig(t, "linking:\n  threshold: "+good+"\n")

		cfg, err := Load()
		if err != nil {
			t.Errorf("Load() refused linking.threshold: %s: %v", good, err)
			continue
		}
		if cfg.Linking.Threshold < 0 || cfg.Linking.Threshold > 1 {
			t.Errorf("linking.threshold = %v, want a value inside [0, 1]", cfg.Linking.Threshold)
		}
	}
}

// The MCP server and the session hooks fall back rather than fail, and the
// fallback keeps the environment. A NaN or infinite floor that reached the
// server would be handed to the store (SetVectorMinSimilarity), where it is the
// vector leg's only verdict — so the fallback replaces it with the compiled
// default and says so, one key at a time.
func TestFallbackConfigNeverCarriesAnUnusableSearchFloor(t *testing.T) {
	for _, bad := range badUnitFloatsEnv {
		isolateConfig(t)
		warnings := captureConfigWarnings(t)
		t.Setenv("GHOST_SEARCH_MIN_SIMILARITY", bad)

		if got := FallbackConfig().Search.MinSimilarity; !inUnitRange(got) {
			t.Errorf("FallbackConfig with GHOST_SEARCH_MIN_SIMILARITY=%s: min_similarity = %v, want a value in [0,1]", bad, got)
		}
		if !strings.Contains(warnings.String(), "search.min_similarity") {
			t.Errorf("FallbackConfig replaced GHOST_SEARCH_MIN_SIMILARITY=%s without a warning naming the key: %q", bad, warnings.String())
		}
	}
}

func TestFallbackConfigNeverCarriesAnUnusableLinkingThreshold(t *testing.T) {
	for _, bad := range badUnitFloatsEnv {
		isolateConfig(t)
		warnings := captureConfigWarnings(t)
		t.Setenv("GHOST_LINKING_THRESHOLD", bad)

		if got := FallbackConfig().Linking.Threshold; !inUnitRange(got) {
			t.Errorf("FallbackConfig with GHOST_LINKING_THRESHOLD=%s: threshold = %v, want a value in [0,1]", bad, got)
		}
		if !strings.Contains(warnings.String(), "linking.threshold") {
			t.Errorf("FallbackConfig replaced GHOST_LINKING_THRESHOLD=%s without a warning naming the key: %q", bad, warnings.String())
		}
	}
}

// Both keys bad at once: each is replaced, not only the first one the check
// reaches.
func TestFallbackConfigReplacesEveryUnusableFloor(t *testing.T) {
	isolateConfig(t)
	captureConfigWarnings(t)
	t.Setenv("GHOST_SEARCH_MIN_SIMILARITY", "7")
	t.Setenv("GHOST_LINKING_THRESHOLD", "nan")

	cfg := FallbackConfig()
	if !inUnitRange(cfg.Search.MinSimilarity) {
		t.Errorf("FallbackConfig search.min_similarity = %v, want a value in [0,1]", cfg.Search.MinSimilarity)
	}
	if !inUnitRange(cfg.Linking.Threshold) {
		t.Errorf("FallbackConfig linking.threshold = %v, want a value in [0,1]", cfg.Linking.Threshold)
	}
}
