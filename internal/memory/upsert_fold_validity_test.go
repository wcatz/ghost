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
func TestUpsertFoldAppliesTheCallersValidityToTheTarget(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	target, _, _, err := s.UpsertWithOptions(ctx, testProject, "gotcha",
		"the vector identity is compared as an opaque string, never parsed",
		"mcp", 0.6, nil, UpsertOptions{
			Validity: Validity{ValidUntil: stampPtr("2026-10-01 23:59:59")},
		})
	if err != nil {
		t.Fatalf("UpsertWithOptions (target): %v", err)
	}

	// A near-duplicate that folds into it rather than a distinct memory.
	_, dup, _, err := s.UpsertWithOptions(ctx, testProject, "gotcha",
		"the vector identity is an opaque compared string, not parsed",
		"mcp", 0.6, nil, UpsertOptions{
			Validity:   Validity{ValidUntil: stampPtr("2027-03-31 23:59:59")},
			Provenance: Provenance{Agent: "codex", SourceRef: "internal/memory/vector.go"},
		})
	if err != nil {
		t.Fatalf("UpsertWithOptions (fold): %v", err)
	}
	if dup != target {
		t.Skipf("wording did not fold onto %s (dup=%q); the fold branch was not reached", target, dup)
	}

	mems, err := s.GetByIDs(ctx, []string{target})
	if err != nil || len(mems) != 1 {
		t.Fatalf("GetByIDs(%s): %v (n=%d)", target, err, len(mems))
	}
	if mems[0].ValidUntil == nil {
		t.Fatalf("fold target %s has no valid_until: the caller's claim reached only the copy it also inserted", target)
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

	target, _, _, err := s.UpsertWithOptions(ctx, testProject, "gotcha",
		"the vector identity is compared as an opaque string, never parsed",
		"mcp", 0.6, nil, UpsertOptions{
			Validity: Validity{ValidUntil: stampPtr("2026-10-01 23:59:59")},
		})
	if err != nil {
		t.Fatalf("UpsertWithOptions (target): %v", err)
	}

	_, dup, _, err := s.UpsertWithOptions(ctx, testProject, "gotcha",
		"the vector identity is an opaque compared string, not parsed",
		"mcp", 0.6, nil, UpsertOptions{})
	if err != nil {
		t.Fatalf("UpsertWithOptions (fold): %v", err)
	}
	if dup != target {
		t.Skipf("wording did not fold onto %s (dup=%q); the fold branch was not reached", target, dup)
	}

	mems, err := s.GetByIDs(ctx, []string{target})
	if err != nil || len(mems) != 1 {
		t.Fatalf("GetByIDs(%s): %v (n=%d)", target, err, len(mems))
	}
	if mems[0].ValidUntil == nil || *mems[0].ValidUntil != "2026-10-01 23:59:59" {
		t.Errorf("fold target ValidUntil = %v, want the 2026-10-01 23:59:59 it already held", mems[0].ValidUntil)
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
