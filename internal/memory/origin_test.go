package memory

import (
	"context"
	"testing"
)

// TestGlobalProjectIDIsThePersistedSentinel pins the exported sentinel to the
// value the database actually holds.
//
// CanonicalOriginSourceForProject compares a row's project_id against
// GlobalProjectID, so a constant that drifts from what the store writes or
// reads would make the helper quietly stop recognising the shipped seed — and
// every test of the helper would keep passing, because they would all be
// comparing against the same wrong constant. The checks below therefore read
// the persisted value from the store using a literal, not the constant, and
// only then compare it to the constant.
func TestGlobalProjectIDIsThePersistedSentinel(t *testing.T) {
	// Literal, deliberately not GlobalProjectID: this is the value the
	// pre-existing store SQL hardcodes.
	const persistedSentinel = "_global"

	if GlobalProjectID != persistedSentinel {
		t.Fatalf("GlobalProjectID = %q, want %q — the helper and the stored rows must agree", GlobalProjectID, persistedSentinel)
	}

	s := testStore(t)
	ctx := context.Background()
	if err := s.SeedGlobalMemories(ctx); err != nil {
		t.Fatalf("SeedGlobalMemories: %v", err)
	}

	// The shipped seed is stored under the persisted literal, read straight
	// from the column rather than through the constant.
	rows, err := s.db.QueryContext(ctx, `SELECT project_id FROM memories WHERE content = ?`, builtinSeedContent)
	if err != nil {
		t.Fatalf("query seed project_id: %v", err)
	}
	defer rows.Close() //nolint:errcheck
	var found bool
	for rows.Next() {
		var projectID string
		if err := rows.Scan(&projectID); err != nil {
			t.Fatalf("scan seed project_id: %v", err)
		}
		found = true
		if projectID != persistedSentinel {
			t.Errorf("seed stored under project_id %q, want the persisted sentinel %q", projectID, persistedSentinel)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate seed project_id: %v", err)
	}
	if !found {
		t.Fatalf("the shipped seed was not stored, so the sentinel has nothing to agree with")
	}

	// And the two spellings select the same rows, which is what every global
	// query in the codebase relies on.
	byLiteral, err := s.GetAll(ctx, persistedSentinel, 10)
	if err != nil {
		t.Fatalf("GetAll(%q): %v", persistedSentinel, err)
	}
	byConstant, err := s.GetAll(ctx, GlobalProjectID, 10)
	if err != nil {
		t.Fatalf("GetAll(GlobalProjectID): %v", err)
	}
	if len(byLiteral) == 0 {
		t.Fatalf("the persisted sentinel returned no rows, so it is not the value the store filters on")
	}
	if len(byConstant) != len(byLiteral) {
		t.Errorf("GetAll(GlobalProjectID) returned %d rows, GetAll(%q) returned %d — the constant and the stored value disagree",
			len(byConstant), persistedSentinel, len(byLiteral))
	}
}

// TestCanonicalOriginSourceForProjectScopesTheSeedRewriteToTheGlobalProject
// pins both halves of the compatibility rewrite's precondition. The v15
// migration that eventually makes this unnecessary needs both the global
// project and the frozen content, so a render of a row still in the legacy
// shape has to reach the same conclusion — and must not reach it for a project
// row.
//
// The project half is the one that matters. Relabelling a user's own project
// memory as builtin both misattributes it and strips the absence-of-a-tag that
// every surface uses as the marker for direct user material, which is a
// correctness loss rather than a cosmetic one.
func TestCanonicalOriginSourceForProjectScopesTheSeedRewriteToTheGlobalProject(t *testing.T) {
	cases := []struct {
		name      string
		projectID string
		source    string
		content   string
		want      string
	}{
		{
			name:      "the shipped seed in the global project is a builtin",
			projectID: GlobalProjectID,
			source:    "manual",
			content:   builtinSeedContent,
			want:      "builtin",
		},
		{
			name:      "the same words in a project are the user's own",
			projectID: "abc123",
			source:    "manual",
			content:   builtinSeedContent,
			want:      "manual",
		},
		{
			name:      "an unstated project is not the reserved one",
			projectID: "",
			source:    "manual",
			content:   builtinSeedContent,
			want:      "manual",
		},
		{
			name:      "another global preference keeps its source",
			projectID: GlobalProjectID,
			source:    "manual",
			content:   "a user's own preference",
			want:      "manual",
		},
		{
			name:      "an already-relabelled global seed is unchanged",
			projectID: GlobalProjectID,
			source:    "builtin",
			content:   builtinSeedContent,
			want:      "builtin",
		},
		{
			name:      "a non-manual source is never rewritten",
			projectID: GlobalProjectID,
			source:    "reflection",
			content:   builtinSeedContent,
			want:      "reflection",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := CanonicalOriginSourceForProject(tc.projectID, tc.source, tc.content); got != tc.want {
				t.Errorf("CanonicalOriginSourceForProject(%q, %q, seed?%v) = %q, want %q",
					tc.projectID, tc.source, tc.content == builtinSeedContent, got, tc.want)
			}
		})
	}
}

// TestOriginClassCoversEverySchemaSource keeps provenance interpretation in
// one place. The session banner and MCP listing both consume memories.source;
// a new legal source must not silently acquire a different trust rule in one
// surface than in the other.
func TestOriginClassCoversEverySchemaSource(t *testing.T) {
	cases := []struct {
		source    string
		wantOwn   bool
		wantLabel string
	}{
		{source: "manual", wantOwn: true, wantLabel: ""},
		{source: "reflection", wantOwn: false, wantLabel: "reflection"},
		{source: "chat", wantOwn: false, wantLabel: "chat"},
		{source: "tool", wantOwn: false, wantLabel: "tool"},
		{source: "mcp", wantOwn: false, wantLabel: "mcp"},
		{source: "onboarding", wantOwn: false, wantLabel: "onboarding"},
		{source: "decision_log", wantOwn: false, wantLabel: "decision_log"},
		{source: "builtin", wantOwn: false, wantLabel: "builtin"},
	}

	for _, tc := range cases {
		t.Run(tc.source, func(t *testing.T) {
			own, label := OriginClass(tc.source)
			if own != tc.wantOwn || label != tc.wantLabel {
				t.Errorf("OriginClass(%q) = (%v, %q), want (%v, %q)", tc.source, own, label, tc.wantOwn, tc.wantLabel)
			}
		})
	}
}
