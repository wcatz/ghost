package memory

import (
	"context"
	"strings"
	"testing"
)

// stampPtr is a stored-layout validity stamp as a caller would hand it over.
func stampPtr(v string) *string { return &v }

const foldText = "the vector identity is compared as an opaque string, never parsed"

// foldRestated is foldText with a leading capital, which FoldOnly folds on
// (case, whitespace and terminal-punctuation normalization), so the fold is a
// property of the text rather than of the Jaccard bar.
const foldRestated = "The vector identity is compared as an opaque string, never parsed"

// TestUpsertFoldAppliesTheCallersValidityToTheTarget: a fold writes the target
// and a copy, and the target is the row search keeps answering, so a window the
// save states must reach it. A caller who states no window leaves the target's
// alone (COALESCE).
func TestUpsertFoldAppliesTheCallersValidityToTheTarget(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	target, dup, _, err := s.UpsertWithOptions(ctx, testProject, "gotcha",
		foldText, "mcp", 0.6, nil, UpsertOptions{Validity: Validity{ValidUntil: stampPtr("2026-10-01 23:59:59")}})
	if err != nil || dup != "" {
		t.Fatalf("seed: %v (duplicateOf %q)", err, dup)
	}

	// A fold that states no window leaves the held one alone.
	if _, dup, _, err := s.UpsertWithOptions(ctx, testProject, "gotcha",
		foldRestated, "mcp", 0.6, nil, UpsertOptions{FoldOnly: true}); err != nil || dup != target {
		t.Fatalf("unstated fold: %v (duplicateOf %q, want %q)", err, dup, target)
	}
	mems, err := s.GetByIDs(ctx, []string{target})
	if err != nil || len(mems) != 1 {
		t.Fatalf("GetByIDs(%s): %v (n=%d)", target, err, len(mems))
	}
	if mems[0].ValidUntil == nil || *mems[0].ValidUntil != "2026-10-01 23:59:59" {
		t.Errorf("an unstated fold changed ValidUntil to %v", mems[0].ValidUntil)
	}

	got, dup, _, err := s.UpsertWithOptions(ctx, testProject, "gotcha",
		foldRestated, "mcp", 0.6, nil, UpsertOptions{
			Validity:   Validity{ValidUntil: stampPtr("2027-03-31 23:59:59")},
			Provenance: Provenance{Agent: "codex", SourceRef: "internal/memory/vector.go"},
			FoldOnly:   true,
		})
	if err != nil {
		t.Fatalf("UpsertWithOptions (fold): %v", err)
	}
	if got != target || dup != target {
		t.Fatalf("fold returned (%q, duplicateOf %q), want the target %q twice", got, dup, target)
	}
	mems, err = s.GetByIDs(ctx, []string{target})
	if err != nil || len(mems) != 1 {
		t.Fatalf("GetByIDs(%s): %v (n=%d)", target, err, len(mems))
	}
	if mems[0].ValidUntil == nil || *mems[0].ValidUntil != "2027-03-31 23:59:59" {
		t.Errorf("fold target ValidUntil = %v, want the caller's 2027-03-31 23:59:59", mems[0].ValidUntil)
	}
	if mems[0].Agent != "codex" || mems[0].SourceRef != "internal/memory/vector.go" {
		t.Errorf("fold target provenance = %q / %q, want the caller's", mems[0].Agent, mems[0].SourceRef)
	}
}

// TestUpsertFoldRefusesAWindowThatEndsBeforeTheTargetsWindowStarts: the merge
// joins two boundaries stated independently, so it can build a window no caller
// wrote, and it must refuse the whole save when it would.
func TestUpsertFoldRefusesAWindowThatEndsBeforeTheTargetsWindowStarts(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	target, _, _, err := s.UpsertWithOptions(ctx, testProject, "gotcha",
		foldText, "mcp", 0.6, nil, UpsertOptions{Validity: Validity{ValidFrom: stampPtr("2026-12-01 00:00:00")}})
	if err != nil {
		t.Fatalf("UpsertWithOptions (target): %v", err)
	}
	_, _, _, err = s.UpsertWithOptions(ctx, testProject, "gotcha",
		foldRestated, "mcp", 0.6, nil, UpsertOptions{
			Validity: Validity{ValidUntil: stampPtr("2026-10-01 00:00:00")},
			FoldOnly: true,
		})
	if err == nil || !strings.Contains(err.Error(), "not after valid_from") {
		t.Fatalf("fold err = %v, want a refusal naming the contradiction", err)
	}
	mems, err := s.GetByIDs(ctx, []string{target})
	if err != nil || len(mems) != 1 {
		t.Fatalf("GetByIDs(%s): %v (n=%d)", target, err, len(mems))
	}
	if mems[0].ValidUntil != nil {
		t.Errorf("a refused fold still wrote valid_until = %q to the target", *mems[0].ValidUntil)
	}
}

// TestImportMemoryRefusesAnOverLongAgent: the agent bound is on the column, so an
// artifact row with a document for an agent is refused whole, not trimmed.
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
	if err == nil || !strings.Contains(err.Error(), "agent") {
		t.Fatalf("ImportMemory err = %v, want a refusal naming the agent field", err)
	}
	if mems, err := s.GetByIDs(ctx, []string{"imported-agent"}); err != nil || len(mems) != 0 {
		t.Errorf("the refused import still stored a row: %v, %v", mems, err)
	}
}
