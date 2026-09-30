package mcpserver

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestAResolvedProjectsGlobalRowsSitUnderTheGlobalHeading is #809.
//
// For a RESOLVED project, `ghost_project_context` and the context resource render
// one window — `projectContextBudget` sets `IncludeGlobal`, so the set is this
// project's rows PLUS `_global`'s — under a single `## Memories` heading. So a
// cross-project row was listed as one of this project's memories, with nothing on
// the line saying otherwise, and the server's own SessionStart instructions key
// their trust guidance on the heading:
//
//	Global memories under "Global (applies to all projects)" apply across every
//	project, but they are not all the user's own.
//
// The unresolved path already got this right, and `docs/mcp.md` documents the
// right answer. So the most common path — a project that resolves — was the one
// losing the heading the guidance depends on, and only the per-row `source=` label
// survived. Splitting the window by the row's OWN project is what the session-start
// block already does (`loadSessionPassive`), which is the pattern this follows.
func TestAResolvedProjectsGlobalRowsSitUnderTheGlobalHeading(t *testing.T) {
	srv, session := newValiditySession(t)
	ctx := context.Background()

	// A row of the requested project's own, and a cross-project row the store
	// holds, so "did the global row move?" is answerable on a store that has both.
	// The project row NAMES the project: ghost_memory_save keys a row on the
	// project_id argument, and an unnamed one lands wherever the harness resolves
	// the working directory to — which is not the project this block is read for,
	// and the test would then be asserting on a block with no project rows in it
	// for a reason that has nothing to do with the split.
	if _, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "ghost_memory_save",
		Arguments: map[string]any{
			"project_id": "vproj", "content": "a claim about this project only", "category": "preference",
		},
	}); err != nil {
		t.Fatalf("ghost_memory_save: %v", err)
	}
	if _, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "ghost_save_global",
		Arguments: map[string]any{"content": "a cross-project preference", "category": "preference"},
	}); err != nil {
		t.Fatalf("ghost_save_global: %v", err)
	}

	assertSplit := func(surface, out string) {
		t.Helper()
		memories, globals, ok := strings.Cut(out, globalSectionHeading)
		if !ok {
			t.Fatalf("%s rendered no %q section at all:\n%s", surface, globalSectionHeading, out)
		}
		if !strings.Contains(memories, memorySectionHeading) {
			t.Errorf("%s rendered no %q section:\n%s", surface, memorySectionHeading, out)
		}
		// The two directions, because a fix that moved the global row and took the
		// project's own row with it would pass one of them.
		if !strings.Contains(memories, "a claim about this project only") {
			t.Errorf("%s put no row of the project's own under %q:\n%s", surface, memorySectionHeading, out)
		}
		if strings.Contains(memories, "a cross-project preference") {
			t.Errorf("%s listed a cross-project row under %q, with nothing marking it as global — which is "+
				"the row the SessionStart trust guidance is written about:\n%s", surface, memorySectionHeading, out)
		}
		if !strings.Contains(globals, "a cross-project preference") {
			t.Errorf("%s did not deliver the cross-project row under %q:\n%s", surface, globalSectionHeading, out)
		}
		// EXACTLY ONE, on both surfaces and not just the resource. A COUNT rather
		// than the containment above, because `strings.Cut` takes the FIRST heading
		// and is blind to a second one — and a block with two `## Global` headings
		// is the specific way this refactor can go wrong: the tool renders the
		// window's `_global` half itself while the resource hands it to the section
		// that also runs the second read, so a tool that did both would list some
		// globals twice under two headings and the assertions above would all pass.
		// The first version of this test had the count on the resource only, and a
		// mutation putting a second Global section in the TOOL survived it.
		if n := strings.Count(out, globalSectionHeading); n != 1 {
			t.Errorf("%s carries %d %q headings, want exactly one:\n%s", surface, n, globalSectionHeading, out)
		}
		if n := strings.Count(out, memorySectionHeading); n != 1 {
			t.Errorf("%s carries %d %q headings, want exactly one:\n%s", surface, n, memorySectionHeading, out)
		}
	}

	// The tool, over the transport, because that is the surface an agent reads.
	assertSplit("ghost_project_context", resultText(callTool(t, session, "ghost_project_context",
		map[string]any{"project_id": "vproj"})))

	// The resource and the prompt body, which render the same rows under different
	// caps and carry two sections the tool does not.
	text, err := srv.buildProjectContext(ctx, "vproj")
	if err != nil {
		t.Fatalf("buildProjectContext: %v", err)
	}
	assertSplit("ghost://project/{id}/context", text)

	// And the `as_of` branch, which is the same defect reached a third way:
	// `MemoriesAsOf` reads `ProjectScoped`, and that is `project_id = ? OR
	// project_id = '_global'` — a union again, rendered through `formatMemories`
	// under `## Memories`. A future instant is past every row, so both rows are
	// live in it and both must be under the heading that is true of them.
	assertSplit("ghost_project_context as_of", resultText(callTool(t, session, "ghost_project_context",
		map[string]any{"project_id": "vproj", "as_of": "2099-01-01T00:00:00Z"})))
}

// TestTheProjectContextSplitMovesRowsWithoutChangingTheBlock is the parity claim,
// and it is separate from the marker test because the marker test cannot see it: a
// split that rendered the same rows twice, or dropped one, would still put a
// cross-project row under the right heading.
//
// So it counts. Enough rows to overrun BOTH caps, because the interesting case is
// the one where the mixed window and the section's own second read disagree about
// what the block holds — that is where a row can be counted twice or not at all.
func TestTheProjectContextSplitMovesRowsWithoutChangingTheBlock(t *testing.T) {
	srv, session := newValiditySession(t)
	ctx := context.Background()

	// One row of the project's own, so the block has both populations and the
	// headings are both required to be present.
	if _, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "ghost_memory_save",
		Arguments: map[string]any{
			"project_id": "vproj", "content": "the only row of this project's own", "category": "preference",
		},
	}); err != nil {
		t.Fatalf("ghost_memory_save: %v", err)
	}
	// More globals than the resource's 20-cap admits, so the window cuts and the
	// second read has to be reconciled with what it already showed.
	const globals = 26
	for i := 0; i < globals; i++ {
		if _, err := session.CallTool(ctx, &mcp.CallToolParams{
			Name: "ghost_save_global",
			Arguments: map[string]any{
				"content":  fmt.Sprintf("a cross-project preference numbered %02d", i),
				"category": "preference",
			},
		}); err != nil {
			t.Fatalf("save_global %d: %v", i, err)
		}
	}

	text, err := srv.buildProjectContext(ctx, "vproj")
	if err != nil {
		t.Fatalf("buildProjectContext: %v", err)
	}

	// The headings are a COUNT, not a containment: `strings.Contains` cannot see a
	// duplicate, which is exactly the mistake `buildProjectContext`'s own `note()`
	// closure was written to avoid for the «...» explainer. The tool's counts are
	// asserted in the marker test above, which is where the fixture guarantees a
	// Global section exists on both surfaces.
	if n := strings.Count(text, globalSectionHeading); n != 1 {
		t.Errorf("the block carries %d %q headings, want exactly one:\n%s", n, globalSectionHeading, text)
	}

	memories, _, _ := strings.Cut(text, globalSectionHeading)
	// The project's own row is the only one under the Memories heading, and it is
	// there exactly once.
	if n := strings.Count(memories, "the only row of this project's own"); n != 1 {
		t.Errorf("the project's own row appears %d time(s) under %q:\n%s", n, memorySectionHeading, text)
	}
	// Every global row appears exactly once across the WHOLE block, and the block
	// holds the window's cap in total — the same rows the unsplit window admitted,
	// which is what makes this a move rather than a change.
	// Every global row appears AT MOST once across the whole block, and the block
	// holds the window's cap in total — the same rows the unsplit window admitted,
	// which is what makes this a move rather than a change. Rows the cap cut are
	// absent, and that is correct: the point is that none is present twice and
	// none is present that the unsplit window would not have admitted.
	//
	// WHICH rows are cut is not predictable and is not asserted: `ghost_save_global`
	// writes 26 rows of equal importance within the same second, so the order past
	// `created_at` is decided by the random id, and a test that named a cut row
	// would be asserting on the id generator. The count is asserted instead.
	//
	// The numbers are zero-padded because the assertion is a substring one and
	// "numbered 1" is a prefix of "numbered 12" — which is how the first version
	// of this test reported a row appearing eight times.
	cut := 0
	for i := 0; i < globals; i++ {
		switch n := strings.Count(text, fmt.Sprintf("numbered %02d»", i)); {
		case n > 1:
			t.Errorf("the global row numbered %02d appears %d time(s) in the block; the split moved rows between "+
				"sections and must not duplicate one:\n%s", i, n, text)
		case n == 0:
			cut++
		}
	}
	if n := strings.Count(text, "\n- ["); n != projectContextMemoriesCap {
		t.Errorf("the block holds %d memory rows, want the %d the mixed window's cap admits: the split moved "+
			"rows between sections, it did not add or drop any\n%s", n, projectContextMemoriesCap, text)
	}
	// And the cut is real, so "at most once" is not passing vacuously on a block
	// that admitted everything.
	if cut == 0 {
		t.Errorf("every one of the %d global rows reached the block, so the window did not cut and this test "+
			"proves nothing about the reconciliation:\n%s", globals, text)
	}
}
