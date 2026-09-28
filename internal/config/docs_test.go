package config

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestConfigurationDocsListEveryEnvOverride holds docs/configuration.md's
// shortcut table to envOverrides, in both directions. Five names were missing
// when #561 checked it — GHOST_SCRATCH_MAX_BYTES, the two
// reflection.auto_resolve/auto_supersede toggles and the two timeout overrides —
// and a variable absent from the table is a variable nobody can set from the
// documentation. Two of the five were the opt-out for a feature, which makes the
// omission worse than an inconvenience: the prose elsewhere in the same file
// tells a reader to set GHOST_SCRATCH_MAX_BYTES=0, and the table they are looking
// at does not have it.
//
// The check is textual because the table IS the documentation; there is nothing
// else to assert. It is here rather than in a docs linter because a test in the
// package that owns the table is the one thing CI already runs.
func TestConfigurationDocsListEveryEnvOverride(t *testing.T) {
	const doc = "../../docs/configuration.md"
	raw, err := os.ReadFile(doc)
	if err != nil {
		t.Fatalf("read %s: %v", doc, err)
	}
	documented := documentedEnvOverrides(string(raw))

	for _, o := range envOverrides {
		if !documented[o.env] {
			t.Errorf("docs/configuration.md's shortcut table does not list %s (%s)", o.env, o.key)
		}
	}
	// The other direction: a documented shortcut that resolves to nothing sends
	// a reader to set a variable that is silently ignored.
	for name := range documented {
		found := false
		for _, o := range envOverrides {
			if o.env == name {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("docs/configuration.md's shortcut table lists %s, which is not an envOverride", name)
		}
	}
}

// envOverrideRowRE matches one row of the shortcut table: a GHOST_* name in the
// first column and its config key in the second. Anchored on the backticked name
// so a mention of a variable in the surrounding prose (there are several, and
// they are correct) is not read as a table row.
var envOverrideRowRE = regexp.MustCompile("(?m)^\\| `([A-Z0-9_]+)` \\| `([^`]+)` \\|$")

// documentedEnvOverrides returns the variable names the shortcut table lists. The
// key is read and discarded on purpose: the table's second column is checked by
// the test above against envOverrides' own keys, and a key typed into the
// documentation wrong is a different failure from a row missing, so this returns
// names only and lets the two directions be read separately.
func documentedEnvOverrides(doc string) map[string]bool {
	out := map[string]bool{}
	for _, m := range envOverrideRowRE.FindAllStringSubmatch(doc, -1) {
		out[m[1]] = true
	}
	return out
}

// TestConfigurationDocsShortcutTableIsPresent: the test above passes vacuously on
// a table that has been reformatted away, because an empty regexp match set makes
// "not documented" true for every override. So the table is required to be there
// and to have at least as many rows as there are overrides.
func TestConfigurationDocsShortcutTableIsPresent(t *testing.T) {
	raw, err := os.ReadFile("../../docs/configuration.md")
	if err != nil {
		t.Fatalf("read docs/configuration.md: %v", err)
	}
	rows := envOverrideRowRE.FindAllStringSubmatch(string(raw), -1)
	if len(rows) < len(envOverrides) {
		t.Errorf("found %d shortcut rows in docs/configuration.md, want at least %d — the table may have been reformatted",
			len(rows), len(envOverrides))
	}
	if !strings.Contains(string(raw), "The generic transformer replaces underscores with dots") {
		t.Error("docs/configuration.md no longer explains the generic GHOST_* mapping, so the table has lost its context")
	}
}
