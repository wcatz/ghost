package config

import (
	"strings"
	"testing"
)

// Real search queries are recorded by default so a retrieval change has a
// regression gate before it ships. The opt-out is one key, so a caller that
// prefers not to keep query text can turn it off in the file or the environment
// and a typo cannot silently become "off".
func TestSearchRecordQueriesDefaultsOnAndCanBeOptedOut(t *testing.T) {
	isolateConfig(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Search.RecordQueries {
		t.Error("search.record_queries defaults off; real queries need a default-on gate")
	}
	if !FallbackConfig().Search.RecordQueries {
		t.Error("FallbackConfig turns search recording off")
	}

	for _, tc := range []struct {
		name  string
		file  string
		env   string
		value string
	}{
		{"file", "search:\n  record_queries: false\n", "", ""},
		{"env", "", "GHOST_SEARCH_RECORD_QUERIES", "false"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateConfig(t)
			if tc.file != "" {
				writeUserConfig(t, tc.file)
			}
			if tc.env != "" {
				t.Setenv(tc.env, tc.value)
			}
			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.Search.RecordQueries {
				t.Errorf("%s opt-out did not take effect: %v", tc.name, cfg.Search.RecordQueries)
			}
		})
	}

	isolateConfig(t)
	warnings := captureConfigWarnings(t)
	writeUserConfig(t, "search:\n  record_queries: false\n")
	if _, err := Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if strings.Contains(warnings.String(), "record_queries") {
		t.Errorf("a known key was reported as unknown: %q", warnings.String())
	}
}
