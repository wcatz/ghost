package mcpinit

import (
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// builtinSeedText is the frozen shipped rule Ghost writes into the global
// project. memory keeps its own unexported copy; this one is spelled out here
// so a change to the shipped text fails these tests instead of silently
// redefining "correct" alongside the code.
const builtinSeedText = "NEVER add Co-Authored-By or any AI attribution to commit messages. All commits belong to the user."

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
				tc.globals, len(tc.globals), true,
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

// TestSessionContextDoesNotClaimLegacyShapedBuiltinSeed: a row still in the
// shape a pre-v15 build wrote — the shipped seed recorded as source='manual' —
// must render as the Ghost-shipped rule it is, whichever build reads it. The
// row is global by construction, so it carries the global project as its own.
func TestSessionContextDoesNotClaimLegacyShapedBuiltinSeed(t *testing.T) {
	out := formatSessionContext(
		"p1", "ghost", nil, "", nil, nil, 1, 0, true,
		[]sessionMemory{{ID: "1", ProjectID: memory.GlobalProjectID, Category: "preference", Content: builtinSeedText, Source: "manual"}},
		1, true,
	)
	if strings.Contains(out, "the user's own saved cross-project preferences") {
		t.Errorf("legacy-shaped builtin seed was presented as user-authored:\n%s", out)
	}
	if !strings.Contains(out, "(builtin)") {
		t.Errorf("legacy-shaped builtin seed was not labelled builtin:\n%s", out)
	}
}

// TestSessionContextGuidanceNamesTheLegacyShapedBuiltinSeed: the guidance
// sentence enumerates the origin labels the rows above it actually carry, so
// it has to read the same canonicalized source the renderer does.
//
// It used to read sessionMemory.Source directly. On a store holding only the
// manual-shaped shipped seed that produced a self-contradicting block: the row
// rendered "(builtin)" while the guidance said no row had "any recorded
// automated origin" — telling the agent to trust a label it was also told
// does not exist.
func TestSessionContextGuidanceNamesTheLegacyShapedBuiltinSeed(t *testing.T) {
	cases := []struct {
		name    string
		globals []sessionMemory
		want    string
	}{
		{
			name:    "a lone legacy-shaped seed is named as builtin",
			globals: []sessionMemory{{ID: "1", ProjectID: memory.GlobalProjectID, Category: "preference", Content: builtinSeedText, Source: "manual"}},
			want:    "parenthesized tags (builtin)",
		},
		{
			name: "a legacy-shaped seed beside a reflected row names both",
			globals: []sessionMemory{
				{ID: "1", ProjectID: memory.GlobalProjectID, Category: "preference", Content: builtinSeedText, Source: "manual"},
				{ID: "2", ProjectID: memory.GlobalProjectID, Category: "fact", Content: "Derived by a summariser.", Source: "reflection"},
			},
			want: "parenthesized tags (builtin, reflection)",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := formatSessionContext("p1", "ghost", nil, "", nil, nil, 1, 0, true, tc.globals, len(tc.globals), true)
			if !strings.Contains(out, tc.want) {
				t.Errorf("guidance must name %q:\n%s", tc.want, out)
			}
			if strings.Contains(out, "no recorded automated origin") {
				t.Errorf("guidance denies a recorded origin the rows above it carry:\n%s", out)
			}
		})
	}
}

// TestSessionContextTagsEveryNonManualGlobal: the per-item tag has to name the
// actual source rather than a generic "not yours", because reflection-derived
// and agent-written rows deserve different suspicion — one came from a model
// summarising possibly-untrusted content, the other from an explicit save.
func TestSessionContextTagsEveryNonManualGlobal(t *testing.T) {
	out := formatSessionContext(
		"p1", "ghost", nil, "", nil, nil, 1, 0, true,
		[]sessionMemory{
			{ID: "1", Category: "fact", Content: "Reflection derived this.", Source: "reflection"},
			{ID: "2", Category: "fact", Content: "An agent saved this.", Source: "mcp"},
		},
		2, true,
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

// TestSessionContextGuidanceNamesTheOriginsActuallyPresent prevents the
// banner from explaining only reflection and MCP while rendering the other
// legal source values without context. In particular, onboarding and
// decision_log are not interchangeable with an agent write.
func TestSessionContextGuidanceNamesTheOriginsActuallyPresent(t *testing.T) {
	sources := []string{"reflection", "chat", "tool", "mcp", "onboarding", "decision_log", "builtin"}
	globals := make([]sessionMemory, 0, len(sources)+1)
	for i, source := range sources {
		globals = append(globals, sessionMemory{
			ID:       string(rune('a' + i)),
			Category: "fact",
			Content:  source,
			Source:   source,
		})
	}
	globals = append(globals, sessionMemory{ID: "z", Category: "preference", Content: "user row", Source: "manual"})

	out := formatSessionContext("p1", "ghost", nil, "", nil, nil, 1, 0, true, globals, len(globals), true)
	for _, source := range sources {
		if !strings.Contains(out, "("+source+")") {
			t.Errorf("source %q is rendered without its origin tag:\n%s", source, out)
		}
		if !strings.Contains(out, source) {
			t.Errorf("source %q is not named in the guidance:\n%s", source, out)
		}
	}
	if strings.Contains(out, "manual") {
		t.Errorf("guidance must describe the absence of an origin tag, not tell readers to look for a manual marker:\n%s", out)
	}
}
