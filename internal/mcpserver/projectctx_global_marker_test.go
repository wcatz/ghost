package mcpserver

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/wcatz/ghost/internal/memory"
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
// TestTheGlobalProjectContextRendersItsOwnWindow is the split's own degenerate
// case, and a review of #817 found it after the split shipped: `_global` IS a
// project, so `ResolveProject(ctx, "_global")` succeeds and two documented surfaces
// — `ghost://project/_global/context` and `recall_project` with
// `project_id: "_global"` — reach `buildProjectContext` with that id.
//
// `projectContextBudget` then sets `IncludeGlobal: false` because the bucket is
// already `_global`, and `projectContextSplit` puts every row in `globals` with an
// empty `own` half. The old guard then skipped the Global section on the reasoning
// that a bucket is not a project to count rows for, which is true and is the wrong
// question: it also discarded the `carried` half, so a store full of global memories
// was answered with the false census "No memories found for this project." The TOOL
// renders the same rows correctly, so two surfaces disagreed about one request.
//
// It is asserted through BOTH surfaces and it asserts the whole shape, because the
// census is a sentence a caller cannot act on and a heading-less block is the same
// failure wearing a different hat.
func TestTheGlobalProjectContextRendersItsOwnWindow(t *testing.T) {
	srv, session := newValiditySession(t)
	ctx := context.Background()

	// Enough globals to overrun the resource's 15-cap, so "the cap is not binding"
	// cannot be the reason a row is missing.
	const globals = 22
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

	assertWholeBlock := func(surface, out string) {
		t.Helper()
		// The false census, which is what the block answered instead.
		for _, census := range []string{"No memories found for this project.", "is not registered with Ghost yet"} {
			if strings.Contains(out, census) {
				t.Errorf("%s answered %q on a store holding %d global memories:\n%s", surface, census, globals, out)
			}
		}
		// And the rows are there, under the ONE heading that is true of them, with
		// no empty `## Memories` above a block that has none of that project's own.
		if strings.Contains(out, memorySectionHeading) {
			t.Errorf("%s rendered a %q section for a project whose window IS the globals, so it can only be "+
				"empty:\n%s", surface, memorySectionHeading, out)
		}
		if n := strings.Count(out, globalSectionHeading); n != 1 {
			t.Errorf("%s carries %d %q headings, want exactly one:\n%s", surface, n, globalSectionHeading, out)
		}
		// A COUNT over the population, never a containment of one member. The
		// fixture seeds 22 rows against a 20-row cap, and WHICH two are cut is a
		// function of `hex(randomblob(16))`: every row shares a category, an
		// importance and a pinned value, and `created_at` is the schema default at
		// second precision, so `passiveFetchSQL`'s final tie-break is the id and the
		// two rows with the largest ids are cut. A test naming one row therefore
		// fails on roughly 2 runs in 22, and the sibling test in this file already
		// states the rule it would break. The first version of this assertion did
		// exactly that, with a second version of its own on top: it counted an
		// `g`-shaped id prefix, which `ghost_save_global` never mints.
		if n := strings.Count(out, "a cross-project preference numbered"); n != projectContextMemoriesCap {
			t.Errorf("%s rendered %d global rows, want the %d its window cap admits:\n%s",
				surface, n, projectContextMemoriesCap, out)
		}
	}

	// The resource and the prompt body.
	text, err := srv.buildProjectContext(ctx, memory.GlobalProjectID)
	if err != nil {
		t.Fatalf("buildProjectContext(_global): %v", err)
	}
	assertWholeBlock("ghost://project/_global/context", text)

	// The tool, over the transport, so the two surfaces are compared as an agent
	// meets them rather than one of them through a function.
	tool := resultText(callTool(t, session, "ghost_project_context", map[string]any{
		"project_id": memory.GlobalProjectID,
	}))
	assertWholeBlock("ghost_project_context", tool)

	// And they say the SAME THING, which is the shape the defect had: two surfaces
	// on one request. The counts are equal because both cap the same window, and
	// the row sets are equal because both read the same bucket.
	if got, want := strings.Count(tool, "\n- ["), strings.Count(text, "\n- ["); got != want {
		t.Errorf("the tool carries %d rows and the resource %d for the same request, so the two surfaces "+
			"disagree about it:\n--- tool ---\n%s\n--- resource ---\n%s", got, want, tool, text)
	}
	// And the COUNT is the cap, which is the other half of the fix: the resource
	// caps the window at 20, so it admits exactly 20 of the 22 rows seeded. The
	// fixture seeds more than the cap ON PURPOSE, so "the rows are there" cannot
	// pass by showing everything.
	//
	// A note on what this assertion does NOT kill, because the honest answer is
	// interesting: letting the second read run for `_global` as well changes
	// nothing observable. Its rows are all in `alreadyShown`, so the `seen` filter
	// drops every one, and the block comes out byte-identical — the guard is there
	// for the wasted query, not for the output. Both surfaces pass with the guard
	// removed, and this test is not claiming otherwise.
	if n := strings.Count(text, "\n- ["); n != projectContextMemoriesCap {
		t.Errorf("the _global block carries %d rows, want the %d its window cap admits:\n%s",
			n, projectContextMemoriesCap, text)
	}
}

// TestTheGlobalProjectContextIsNotCountedAsAnotherProjectsRows keeps the half of
// the old guard that was RIGHT, and a review of #817 is right twice over about the
// first version: it seeded a memory in a DIFFERENT project, which nothing on the
// `_global` path counts, so deleting the guard left it green; and its fixture had a
// LIVE global in the block, which stops the note path being reached at all.
//
// The shape that reaches it, and the only one, is a `_global` window that admits
// NOTHING while `CountMemories` still counts what is in the store. A `valid_until`
// in the past is the cheapest such row: stage 2 drops a closed window, so the row
// is stored, counted, and not admitted. Then:
//
//   - WITH the guard, `projectContextOwnRowsNote` returns "" and the caller falls
//     through to its own answer;
//   - WITHOUT it, `n > 0` and `len(res.Items) == 0`, so it renders "Ghost holds N
//     memories for this project and none of it is in the block above" — a claim
//     about a PROJECT, over a block that is a listing of `_global`.
//
// The assertion is on the sentences rather than on which branch produced them, and
// the fixture is asserted first: a test that cannot tell whether its own shape is
// right is the exact failure this rewrite is fixing.
func TestTheGlobalProjectContextIsNotCountedAsAnotherProjectsRows(t *testing.T) {
	srv, session := newValiditySession(t)
	ctx := context.Background()

	// ONLY closed-window globals. A live one would be admitted into the block,
	// and the loop in `projectContextOwnRowsNote` that returns "" for a row of the
	// requested project would find it — so the note could not be reached and the
	// guard would never be consulted. That was the first version's second mistake.
	if _, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "ghost_save_global",
		Arguments: map[string]any{
			"content": "a cross-project preference whose window has closed", "category": "preference",
			"valid_until": "2020-01-01T00:00:00Z",
		},
	}); err != nil {
		t.Fatalf("save_global: %v", err)
	}

	// BOTH surfaces, and that is the second version of this test's lesson. The
	// first asserted `buildProjectContext` alone, so fixing the resource and
	// forgetting the tool shipped a defect that is this exact shape on the other
	// surface — which a review of #817 found, and which only an assertion over both
	// can catch.
	assertVerdict := func(surface, text string) {
		t.Helper()
		// The fixture must be the shape the test claims, or it proves nothing. The
		// row must be in the store and OUT of the block: a row the window admits
		// reaches the loop in `projectContextOwnRowsNote` that returns "" for a row
		// of the requested project, so the guard is never consulted.
		if n := countProjectMemories(t, srv, memory.GlobalProjectID); n != 1 {
			t.Fatalf("the store counts %d global rows, want 1 — this test is about a store that holds a row the "+
				"window will not admit", n)
		}
		if strings.Contains(text, "whose window has closed") {
			t.Fatalf("%s admitted the closed row, so the window did not exclude it and the note path is "+
				"unreachable:\n%s", surface, text)
		}
		// No sentence about "this project" and its rows: on a bucket that is not a
		// project to count rows for, every one of these is false.
		for _, note := range []string{
			"Ghost holds no memories for this project",
			"Ghost holds 1 memory for this project",
			"and none of it is in the block above",
			"the memory rows above are the cross-project ones",
			"are not all the user",
			"is registered but has no memories",
		} {
			if strings.Contains(text, note) {
				t.Errorf("%s carries %q, which is a claim about ANOTHER project and false here:\n%s",
					surface, note, text)
			}
		}
		// The CENSUS, named explicitly rather than passed over. A review of #817
		// caught that my first "the answer is not empty" assertion was satisfied BY
		// this very sentence, so a fixture that renders it passes a liveness check
		// and certifies the false claim.
		if strings.Contains(text, "No memories found for this project.") {
			t.Errorf("%s answers with the project census, which is a claim about a project:\n%s", surface, text)
		}
		// What DOES answer is the assembler's own verdict, which is a fact about
		// the WINDOW rather than about a project: rows were found and withheld.
		for _, want := range []string{"withheld as out of date", "not absent"} {
			if !strings.Contains(text, want) {
				t.Errorf("%s does not carry the assembler's verdict (%q), so it says nothing true about the "+
					"withheld cross-project rows:\n%s", surface, want, text)
			}
		}
		// The `ghost_memories_list` pointer is CORRECT here and is asserted, because
		// a first version of the docs said a `_global` request never emits one and
		// that was false: the tool resolves `_global` and lists the global rows
		// still marked with their window, so the advice is actionable. The clause
		// that would be false is the "Ghost holds N memories for this project" one,
		// and that is what is forbidden above.
		if !strings.Contains(text, "Call ghost_memories_list") {
			t.Errorf("%s does not say where the withheld cross-project rows are, so a caller has no route to "+
				"them:\n%s", surface, text)
		}
	}

	resource, err := srv.buildProjectContext(ctx, memory.GlobalProjectID)
	if err != nil {
		t.Fatalf("buildProjectContext(_global): %v", err)
	}
	assertVerdict("ghost://project/_global/context", resource)

	tool := resultText(callTool(t, session, "ghost_project_context", map[string]any{
		"project_id": memory.GlobalProjectID,
	}))
	assertVerdict("ghost_project_context", tool)
}

// TestTheGlobalProjectContextOnAStoreWithNoGlobalsAtAll is the other half of the
// empty path, and it is separated because the two answers are different for a
// reason that is easy to state wrongly.
//
// A store holding NO global rows has `ReasonNoMemories` — a census of the WINDOW,
// and the one case where "nothing to show" is a fact about the REQUEST rather than
// a claim about a project. That half is kept. What must not appear is the project
// census, and what must not appear either is the abstention: no rows were found, so
// saying they were withheld would be its own false statement, which is the mirror of
// the one the sibling test catches.
func TestTheGlobalProjectContextOnAStoreWithNoGlobalsAtAll(t *testing.T) {
	srv, session := newValiditySession(t)
	ctx := context.Background()

	resource, err := srv.buildProjectContext(ctx, memory.GlobalProjectID)
	if err != nil {
		t.Fatalf("buildProjectContext(_global): %v", err)
	}
	tool := resultText(callTool(t, session, "ghost_project_context", map[string]any{
		"project_id": memory.GlobalProjectID,
	}))
	for surface, text := range map[string]string{
		"ghost://project/_global/context": resource,
		"ghost_project_context":           tool,
	} {
		if strings.TrimSpace(text) == "" {
			t.Errorf("%s is empty, so a caller is told nothing at all", surface)
		}
		for _, forbidden := range []string{
			"No memories found for this project.",
			"withheld as out of date",
			"Ghost holds",
			"is registered but has no memories",
		} {
			if strings.Contains(text, forbidden) {
				t.Errorf("%s carries %q on a store with no globals at all:\n%s", surface, forbidden, text)
			}
		}
		// The census is a fact about the CROSS-PROJECT rows, which is the request.
		if !strings.Contains(text, "cross-project") {
			t.Errorf("%s does not say what the census is a census OF:\n%s", surface, text)
		}
	}
}

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
