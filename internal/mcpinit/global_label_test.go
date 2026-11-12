package mcpinit

import (
	"strings"
	"testing"
)

// builtinSeedText is the frozen shipped rule Ghost writes into the global
// project. memory keeps its own unexported copy; this one is spelled out here
// so a change to the shipped text fails these tests instead of silently
// redefining "correct" alongside the code.
const builtinSeedText = "NEVER add Co-Authored-By or any AI attribution to commit messages. All commits belong to the user."

// rowLines returns the memory ROW lines of a rendered block — the lines that
// start with the row bullet. The origin guards below assert through these lines
// rather than over the whole output on purpose: globalOriginGuidance prints the
// same labels in its own parentheses ("parenthesized tags (reflection, mcp)"),
// so a bare strings.Contains on the block is satisfied by the guidance sentence
// alone. Drop the label from the row renderer and the guidance still says it —
// which is exactly the #545 failure mode, an agent told to trust a label no row
// actually carries. A label found here is a label on the row itself.
func rowLines(out string) []string {
	var rows []string
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "- [") {
			rows = append(rows, line)
		}
	}
	return rows
}

// rowCarries reports whether some row line carries fragment, e.g. "source=reflection".
func rowCarries(out, fragment string) bool {
	for _, line := range rowLines(out) {
		if strings.Contains(line, fragment) {
			return true
		}
	}
	return false
}

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
		name         string
		globals      []sessionMemory
		wantClaim    bool   // may it say "the user's own saved cross-project preferences"?
		wantRowLabel string // fragment the ROW line must carry, "" for none
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
			wantClaim:    false,
			wantRowLabel: "source=reflection",
		},
		{
			name: "an agent-written global must not be called the user's preference either",
			globals: []sessionMemory{
				{ID: "1", Category: "fact", Content: "Saved during a session.", Source: "mcp"},
			},
			wantClaim:    false,
			wantRowLabel: "source=mcp",
		},
		{
			name: "a mixed set loses the blanket claim",
			globals: []sessionMemory{
				{ID: "1", Category: "preference", Content: "The user's own.", Source: "manual"},
				{ID: "2", Category: "fact", Content: "Derived by a summariser.", Source: "reflection"},
			},
			wantClaim:    false,
			wantRowLabel: "source=reflection",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := formatSessionContext(
				"p1", "ghost", nil, nil, "", nil, nil, 1,
				tc.globals, shownOnly(0, len(tc.globals)),
			)

			const claim = "the user's own saved cross-project preferences"
			hasClaim := strings.Contains(out, claim)
			if hasClaim != tc.wantClaim {
				t.Errorf("claim present = %v, want %v\noutput:\n%s", hasClaim, tc.wantClaim, out)
			}
			if tc.wantRowLabel != "" && !rowCarries(out, tc.wantRowLabel) {
				t.Errorf("origin label %q missing from the ROW line, so the agent cannot tell who wrote it:\n%s", tc.wantRowLabel, out)
			}
		})
	}
}
