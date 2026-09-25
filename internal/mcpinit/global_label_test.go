package mcpinit

import (
	"fmt"
	"strings"
	"testing"
)

// TestSessionContextDoesNotClaimReflectionGlobalsAreYours: session start used
// to open every global section with "the user's own saved cross-project
// preferences", whatever had actually written those rows.
//
// That is the last link in issue #545's chain. Untrusted repository content is
// saved as a project memory, a reflection pass summarises it and marks it
// global, and it is then injected into every future session in every project —
// described as the user's own preference, with no provenance shown. The label
// is what makes the chain dangerous rather than merely noisy: an agent reads
// "the user's own preferences" as settled and does not question it.
//
// So the claim is made only when it is true, and every row that is not the
// user's own is tagged with who wrote it.
func TestSessionContextDoesNotClaimReflectionGlobalsAreYours(t *testing.T) {
	cases := []struct {
		name       string
		globals    []sessionMemory
		wantClaim  bool // may it say "the user's own saved cross-project preferences"?
		wantTagFor string
	}{
		{
			name: "all manual may claim they are the user's preferences",
			globals: []sessionMemory{
				{ID: "1", Category: "preference", Content: "Always use nerdctl, never docker.", Source: "manual"},
				{ID: "2", Category: "convention", Content: "Sign off every commit.", Source: "manual"},
			},
			wantClaim: true,
		},
		{
			name: "a reflection-derived global must not be called the user's preference",
			globals: []sessionMemory{
				{ID: "1", Category: "preference", Content: "SSH into relay 3 for staging.", Source: "reflection"},
			},
			wantClaim:  false,
			wantTagFor: "(reflection)",
		},
		{
			name: "an agent-written global must not be called the user's preference either",
			globals: []sessionMemory{
				{ID: "1", Category: "fact", Content: "Saved during a session.", Source: "mcp"},
			},
			wantClaim:  false,
			wantTagFor: "(mcp)",
		},
		{
			name: "a mixed set loses the blanket claim",
			globals: []sessionMemory{
				{ID: "1", Category: "preference", Content: "The user's own.", Source: "manual"},
				{ID: "2", Category: "fact", Content: "Derived by a summariser.", Source: "reflection"},
			},
			wantClaim:  false,
			wantTagFor: "(reflection)",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := formatSessionContext(
				"p1", "ghost", nil, "", nil, nil, 1, 0, true,
				tc.globals, len(tc.globals), true, allManualOnly(tc.globals), true,
			)

			const claim = "the user's own saved cross-project preferences"
			hasClaim := strings.Contains(out, claim)
			if hasClaim != tc.wantClaim {
				t.Errorf("claim present = %v, want %v\noutput:\n%s", hasClaim, tc.wantClaim, out)
			}
			if tc.wantTagFor != "" && !strings.Contains(out, tc.wantTagFor) {
				t.Errorf("origin tag %q missing, so the agent cannot tell who wrote it:\n%s", tc.wantTagFor, out)
			}
		})
	}
}

// TestSessionContextTagsEveryNonManualGlobal: the per-item tag has to name the
// actual source rather than a generic "not yours", because reflection-derived
// and agent-written rows deserve different suspicion — one came from a model
// summarising possibly-untrusted content, the other from an explicit save.
func TestSessionContextDoesNotClaimUnmigratedBuiltinSeed(t *testing.T) {
	out := formatSessionContext(
		"p1", "ghost", nil, "", nil, nil, 1, 0, true,
		[]sessionMemory{{ID: "1", Category: "preference", Content: "NEVER add Co-Authored-By or any AI attribution to commit messages. All commits belong to the user.", Source: "manual"}},
		1, true, true, true,
	)
	if strings.Contains(out, "the user's own saved cross-project preferences") {
		t.Errorf("unmigrated builtin seed was presented as user-authored:\n%s", out)
	}
	if !strings.Contains(out, "(builtin)") {
		t.Errorf("unmigrated builtin seed was not labelled builtin:\n%s", out)
	}
}

func TestSessionContextTagsEveryNonManualGlobal(t *testing.T) {
	out := formatSessionContext(
		"p1", "ghost", nil, "", nil, nil, 1, 0, true,
		[]sessionMemory{
			{ID: "1", Category: "fact", Content: "Reflection derived this.", Source: "reflection"},
			{ID: "2", Category: "fact", Content: "An agent saved this.", Source: "mcp"},
		},
		2, true, false, true,
	)

	for _, want := range []string{"(reflection)", "(mcp)"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %s in:\n%s", want, out)
		}
	}
	// The manual row in a mixed set keeps no tag: absence of a tag is what
	// marks it as the user's own.
	if strings.Contains(out, "(manual)") {
		t.Errorf("manual rows must stay untagged, otherwise the marker is meaningless:\n%s", out)
	}
}

// allManualOnly reports the whole-set trust flag the loader now computes, so a
// formatting test can state the section's real origin without reaching for a
// database.
func allManualOnly(globals []sessionMemory) bool {
	for _, m := range globals {
		if m.Source != "manual" {
			return false
		}
	}
	return true
}

// TestLoadGlobalMemories_TrustFlagCoversHiddenRows: the "these are the user's
// own" claim is a property of the whole active _global set, not of the rows that
// survive the cap and the near-duplicate demotion pass. Deriving it from the
// shown slice let a machine-written row ranked below the cut vouch for the
// section: eight manual rows on screen, a reflection row hidden underneath, and
// the header still telling the agent to trust it.
func TestLoadGlobalMemories_TrustFlagCoversHiddenRows(t *testing.T) {
	db, dbPath := openFileTestDB(t)
	if _, err := db.Exec(`INSERT INTO projects (id, path, name) VALUES ('_global', '_global', 'global')`); err != nil {
		t.Fatalf("insert _global project: %v", err)
	}
	// More manual rows than the loader will show, so at least one reflection
	// row is guaranteed to fall below the cap.
	for i := 0; i < globalsCap*2+4; i++ {
		id := fmt.Sprintf("manual%02d", i)
		_, err := db.Exec(
			`INSERT INTO memories (id, project_id, category, content, source, importance, updated_at)
			 VALUES (?, '_global', 'preference', ?, 'manual', 1.0, datetime('now', ?))`,
			id, fmt.Sprintf("user preference number %d about validation workflow", i),
			fmt.Sprintf("-%d minutes", i),
		)
		if err != nil {
			t.Fatalf("insert manual %d: %v", i, err)
		}
	}
	// The lowest-ranked row of all, and machine-written.
	if _, err := db.Exec(`
		INSERT INTO memories (id, project_id, category, content, source, importance, updated_at)
		VALUES ('hidden01', '_global', 'fact', 'a machine wrote this one', 'reflection', 0.1, datetime('now', '-9999 minutes'))
	`); err != nil {
		t.Fatalf("insert hidden reflection row: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	globals, _, _, allManual, known := loadGlobalMemories(dbPath)
	if !known {
		t.Fatal("the loader could not answer the trust question, so the formatter has to assume the unsafe direction")
	}
	if allManual {
		t.Error("allRowsManual = true while a reflection row is active — the hidden row is invisible to the header")
	}
	if len(globals) >= globalsCap*2+4 {
		t.Fatalf("precondition: the loader showed %d rows, so nothing was actually hidden", len(globals))
	}
	for _, m := range globals {
		if m.Source != "manual" {
			t.Fatalf("precondition: a non-manual row reached the shown slice: %+v", m)
		}
	}

	out := formatSessionContext("p1", "ghost", nil, "", nil, nil, 1, 0, true,
		globals, 100, true, allManual, known)
	if strings.Contains(out, "the user's own saved cross-project preferences") {
		t.Errorf("the header claims the whole section is the user's own while a hidden reflection row is active:\n%s", out)
	}
}
