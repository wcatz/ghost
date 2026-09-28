package memory

import (
	"context"
	"strings"
	"testing"
)

// stampPtr is a stored-layout validity stamp as a caller would hand it over.
func stampPtr(v string) *string { return &v }

// A fold writes two rows, and the one search keeps answering is neither of them
// individually: the target is what stays in the corpus and what a later
// consolidation absorbs, while the copy the response names is a near-identical
// paraphrase nothing is guaranteed to retrieve. So a save that states a window
// has to reach the target as well, or the caller is told the claim was stored and
// the row that answers questions still has none.
//
// The pin-on-fold path (TestUpsertPin_FoldPinsTheExistingRowToo) is the same
// argument about a different column, and it is why the fix is not "the caller
// can always follow duplicateOf and update it themselves".
//
// Both tests here drive the fold through FoldOnly rather than through
// near-identical wording. The default fold is chosen by a 0.5 Jaccard bar on
// token overlap, so a test that depends on it depends on a constant this PR does
// not own: a threshold change would turn a skip — two green tests asserting
// nothing, and the only coverage of the new merge — into a silent hole. FoldOnly
// folds on foldOnlyEquivalent, which is case, whitespace and terminal-punctuation
// normalization, so the fold is a property of the text rather than of a bar. It
// is the same UPDATE statement in the same transaction, which is the code under
// test; the default fold adds an INSERT and a link on top of it.
func TestUpsertFoldAppliesTheCallersValidityToTheTarget(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	const text = "the vector identity is compared as an opaque string, never parsed"
	target, dup, _, err := s.UpsertWithOptions(ctx, testProject, "gotcha",
		text, "mcp", 0.6, nil, UpsertOptions{
			Validity: Validity{ValidUntil: stampPtr("2026-10-01 23:59:59")},
		})
	if err != nil {
		t.Fatalf("UpsertWithOptions (target): %v", err)
	}
	if dup != "" {
		t.Fatalf("the first save folded into %q; a fresh memory is not a fold target", dup)
	}

	got, dup, _, err := s.UpsertWithOptions(ctx, testProject, "gotcha",
		"The vector identity is compared as an opaque string, never parsed",
		"mcp", 0.6, nil, UpsertOptions{
			Validity:   Validity{ValidUntil: stampPtr("2027-03-31 23:59:59")},
			Provenance: Provenance{Agent: "codex", SourceRef: "internal/memory/vector.go"},
			FoldOnly:   true,
		})
	if err != nil {
		t.Fatalf("UpsertWithOptions (fold): %v", err)
	}
	if got != target || dup != target {
		t.Fatalf("fold returned (%q, duplicateOf %q), want the target %q twice — the fold branch was not reached", got, dup, target)
	}

	mems, err := s.GetByIDs(ctx, []string{target})
	if err != nil || len(mems) != 1 {
		t.Fatalf("GetByIDs(%s): %v (n=%d)", target, err, len(mems))
	}
	if mems[0].ValidUntil == nil {
		t.Fatalf("fold target %s has no valid_until: the caller's claim reached no row", target)
	}
	if *mems[0].ValidUntil != "2027-03-31 23:59:59" {
		t.Errorf("fold target ValidUntil = %q, want the caller's 2027-03-31 23:59:59", *mems[0].ValidUntil)
	}
	if mems[0].Agent != "codex" {
		t.Errorf("fold target Agent = %q, want the caller's codex", mems[0].Agent)
	}
	if mems[0].SourceRef != "internal/memory/vector.go" {
		t.Errorf("fold target SourceRef = %q, want the caller's reference", mems[0].SourceRef)
	}
}

// The other half, and the reason the fold uses COALESCE rather than a plain
// assignment: a re-statement of a fact is not a retraction of a boundary somebody
// else recorded. A caller who says nothing about the window must leave what the
// target holds alone, or every unrelated near-duplicate save would silently
// expire a dated memory.
func TestUpsertFoldLeavesAnUnstatedWindowAlone(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	const text = "the vector identity is compared as an opaque string, never parsed"
	target, _, _, err := s.UpsertWithOptions(ctx, testProject, "gotcha",
		text, "mcp", 0.6, nil, UpsertOptions{
			Validity: Validity{ValidUntil: stampPtr("2026-10-01 23:59:59")},
		})
	if err != nil {
		t.Fatalf("UpsertWithOptions (target): %v", err)
	}

	_, dup, _, err := s.UpsertWithOptions(ctx, testProject, "gotcha",
		"The vector identity is compared as an opaque string, never parsed",
		"mcp", 0.6, nil, UpsertOptions{FoldOnly: true})
	if err != nil {
		t.Fatalf("UpsertWithOptions (fold): %v", err)
	}
	if dup != target {
		t.Fatalf("fold returned duplicateOf %q, want the target %q — the fold branch was not reached", dup, target)
	}

	mems, err := s.GetByIDs(ctx, []string{target})
	if err != nil || len(mems) != 1 {
		t.Fatalf("GetByIDs(%s): %v (n=%d)", target, err, len(mems))
	}
	if mems[0].ValidUntil == nil || *mems[0].ValidUntil != "2026-10-01 23:59:59" {
		t.Errorf("fold target ValidUntil = %v, want the 2026-10-01 23:59:59 it already held", mems[0].ValidUntil)
	}
}

// The merge joins two halves that were stated independently — the target's
// boundary and the caller's — so it is the one writer that can build a window no
// caller ever wrote. Before this the target kept its own consistent pair and a
// contradictory claim landed only on the copy the save inserted; readValidity
// tests expiry first, so the merged row would read as `future` and then
// `expired` and drop out of ranked retrieval with no error at the tool or the
// store.
func TestUpsertFoldRefusesAWindowThatEndsBeforeTheTargetsWindowStarts(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	const text = "the vector identity is compared as an opaque string, never parsed"
	target, _, _, err := s.UpsertWithOptions(ctx, testProject, "gotcha",
		text, "mcp", 0.6, nil, UpsertOptions{
			Validity: Validity{ValidFrom: stampPtr("2026-12-01 00:00:00")},
		})
	if err != nil {
		t.Fatalf("UpsertWithOptions (target): %v", err)
	}

	// A perfectly legal claim on its own: nothing says valid_until has to be
	// later than some date it never heard of.
	_, _, _, err = s.UpsertWithOptions(ctx, testProject, "gotcha",
		"The vector identity is compared as an opaque string, never parsed",
		"mcp", 0.6, nil, UpsertOptions{
			Validity: Validity{ValidUntil: stampPtr("2026-10-01 00:00:00")},
			FoldOnly: true,
		})
	if err == nil {
		t.Fatal("fold merged a valid_until onto a target's valid_from and stored a window that ends two months before it starts")
	}
	if !strings.Contains(err.Error(), "not after valid_from") {
		t.Errorf("error does not name the contradiction: %v", err)
	}

	// The refusal is the whole save's: the target keeps the window it had rather
	// than taking half of a pair that was refused.
	mems, err := s.GetByIDs(ctx, []string{target})
	if err != nil || len(mems) != 1 {
		t.Fatalf("GetByIDs(%s): %v (n=%d)", target, err, len(mems))
	}
	if mems[0].ValidUntil != nil {
		t.Errorf("a refused fold still wrote valid_until = %q to the target", *mems[0].ValidUntil)
	}
}

// The gate that keeps the check above off unrelated saves: a fold that states no
// boundary cannot make the target's window inconsistent, so it is not judged
// against the target's own stored pair. Store.Create, ImportMemory and
// RestoreSnapshot all write the stamps through with no order check, and that is
// deliberate, so a row whose window is out of order is reachable — and an
// ordinary re-save of its text has to keep working. The target is the out-of-order
// row itself, because a target with no window has nothing to mis-judge.
func TestUpsertFoldDoesNotJudgeTheTargetsOwnWindow(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	const text = "the vector identity is compared as an opaque string, never parsed"
	target, err := s.Create(ctx, testProject, Memory{
		Category:   "gotcha",
		Content:    text,
		Source:     "mcp",
		ValidFrom:  stampPtr("2026-12-01 00:00:00"),
		ValidUntil: stampPtr("2026-10-01 00:00:00"),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, dup, _, err := s.UpsertWithOptions(ctx, testProject, "gotcha",
		text, "mcp", 0.6, nil, UpsertOptions{FoldOnly: true})
	if err != nil {
		t.Fatalf("a fold that stated no window was refused over the target's own stored window: %v", err)
	}
	if got != target || dup != target {
		t.Fatalf("fold returned (%q, duplicateOf %q), want the target %q twice — the fold branch was not reached", got, dup, target)
	}

	// And the pair is untouched: the check read it and did nothing with it.
	mems, err := s.GetByIDs(ctx, []string{target})
	if err != nil || len(mems) != 1 {
		t.Fatalf("GetByIDs(%s): %v (n=%d)", target, err, len(mems))
	}
	if mems[0].ValidUntil == nil || *mems[0].ValidUntil != "2026-10-01 00:00:00" {
		t.Errorf("a fold that stated no window changed the target's own: ValidUntil = %v", mems[0].ValidUntil)
	}
}

// The bound is on the column, not on the MCP argument resolver, so the two
// writers a tool cannot reach are covered too: portable.go's ImportMemory is the
// route an artifact takes, and it is the one that can carry megabytes.
func TestImportMemoryRefusesAnOverLongAgent(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	_, _, _, err := s.ImportMemory(ctx, PortableMemory{
		ID:        "imported-agent",
		ProjectID: testProject,
		Category:  "fact",
		Content:   "an artifact whose agent field is a document",
		Source:    "mcp",
		Agent:     strings.Repeat("h", MaxAgentLen+1),
	}, ImportOptions{Apply: true})
	if err == nil {
		t.Fatal("ImportMemory accepted an agent past MaxAgentLen")
	}
	if !strings.Contains(err.Error(), "agent") {
		t.Errorf("error does not name the offending field: %v", err)
	}

	// The refusal is a whole-write refusal, not a dropped column: an artifact
	// row that kept its content with a trimmed agent would read as a memory whose
	// writer Ghost chose not to name.
	mems, err := s.GetByIDs(ctx, []string{"imported-agent"})
	if err != nil {
		t.Fatalf("GetByIDs: %v", err)
	}
	if len(mems) != 0 {
		t.Errorf("the refused import still stored a row: agent=%q", mems[0].Agent)
	}
}

// Create is the other route a tool can reach, and the one a store written before
// the cap gets exercised through.
func TestCreateRefusesAnOverLongAgent(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	if _, err := s.Create(ctx, testProject, Memory{
		Category: "fact",
		Content:  "a row whose agent is longer than a harness name",
		Source:   "mcp",
		Agent:    strings.Repeat("h", MaxAgentLen+1),
	}); err == nil {
		t.Fatal("Create accepted an agent past MaxAgentLen")
	} else if !strings.Contains(err.Error(), "agent") {
		t.Errorf("error does not name the offending field: %v", err)
	}
}

// Exactly at the cap is not past it. A cap that refuses the boundary value is
// off by one against its own documentation, and the only way to know is to say so.
func TestAgentBoundAcceptsTheCapItself(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	agent := strings.Repeat("h", MaxAgentLen)
	if _, err := boundedAgent(agent); err != nil {
		t.Errorf("boundedAgent refused a %d-byte agent, the value MaxAgentLen allows: %v", MaxAgentLen, err)
	}
	if _, err := s.Create(ctx, testProject, Memory{
		Category: "fact",
		Content:  "a row whose agent is exactly the cap",
		Source:   "mcp",
		Agent:    agent,
	}); err != nil {
		t.Fatalf("Create refused an agent of exactly MaxAgentLen bytes: %v", err)
	}
}
