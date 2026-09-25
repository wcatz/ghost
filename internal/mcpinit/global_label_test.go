package mcpinit

import (
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

// TestSessionContextTagsEveryNonManualGlobal: the per-item tag has to name the
// actual source rather than a generic "not yours", because reflection-derived
// and agent-written rows deserve different suspicion — one came from a model
// summarising possibly-untrusted content, the other from an explicit save.
func TestSessionContextDoesNotClaimUnmigratedBuiltinSeed(t *testing.T) {
	out := formatSessionContext(
		"p1", "ghost", nil, "", nil, nil, 1, 0, true,
		[]sessionMemory{{ID: "1", Category: "preference", Content: "NEVER add Co-Authored-By or any AI attribution to commit messages. All commits belong to the user.", Source: "manual"}},
		1, true,
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
