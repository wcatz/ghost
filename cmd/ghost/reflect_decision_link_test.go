package main

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
	"github.com/wcatz/ghost/internal/reflection"
)

// TestReflectKeepsADecisionLinkWhenTheSuccessorIsKept covers the drop whose
// named successor the response KEEPS.
//
// The companion memory of a decision carries that decision's link in its
// source_ref, and the link is what a later supersede finds the companion by.
// A drop of the companion against a kept successor left the companion
// unaccounted for: nothing in the emitted set claimed it, so the replace
// deleted it as a row nothing carried, its delete history named no successor,
// and the link went with it. The row the response kept — the near-copy that
// states the same decision — carried no link, so the decision was left with no
// companion at all and a later supersede retired nothing while the reversed
// text kept ranking.
//
// A MERGE of the successor with other facts inherits the link, because the
// merge's own ids name the dropped row. A keep names no ids of its own, so the
// link was lost exactly where the merge kept it.
func TestReflectKeepsADecisionLinkWhenTheSuccessorIsKept(t *testing.T) {
	store, ctx := newReflectStore(t, "proj")

	const title = "Keep the corpus in one SQLite file"
	const decision = "The corpus is stored in SQLite"
	const rationale = "One file to copy and one file to back up"
	decisionID, companionID, _, err := store.RecordDecision(ctx, "proj", title, decision, rationale, nil, nil)
	if err != nil {
		t.Fatalf("RecordDecision: %v", err)
	}

	// The near-copy: a second row stating the same decision, which the response
	// will name as the successor of the companion. It is a near-copy and not the
	// same text, so it is a row of its own — and it carries the companion's
	// substance, which is what lets the drop stand at all.
	copyID, err := store.Create(ctx, "proj", memory.Memory{
		Category:   "fact",
		Content:    title + ": " + decision + ". Rationale: " + rationale + ", and one file to copy",
		Importance: 0.7,
		Source:     "mcp",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	input := reflectInputFor(t, store, "proj")
	result := consolidateForTest(t, input, func() string {
		return fmt.Sprintf(`{"learned_context":"ctx","ops":["drop %s reason: superseded by %s","keep %s"]}`,
			companionID, copyID, copyID)
	})
	if guarded := reflection.AuditGuardedDrops(input, result); len(guarded) != 0 {
		t.Fatalf("the drop guard handed %d row(s) back: %+v — the fixture does not test the case", len(guarded), guarded)
	}

	projectMems, globalMems := splitByScope(result.Memories)
	if len(globalMems) != 0 {
		t.Fatalf("%d candidates are global: %+v", len(globalMems), result.Memories)
	}
	if _, _, _, _, _, _, _, _, _, _, err := applyReflection(ctx, store, "proj", projectMems, globalMems, "", false, replacedIDsFor(&result), nil); err != nil {
		t.Fatalf("applyReflection: %v", err)
	}

	// The link the companion carried is on a live row.
	linked := liveRowsCarryingDecision(t, store, "proj", decisionID)
	if len(linked) != 1 {
		t.Fatalf("live rows carrying the decision's link: %d, want 1 — the link died with the companion: %+v", len(linked), linked)
	}
	if linked[0].ID != copyID {
		t.Errorf("the link is on row %s, want the kept successor %s", linked[0].ID, copyID)
	}

	// And a later supersede of that decision retires the row that carries it.
	newDecisionID, _, _, err := store.RecordDecision(ctx, "proj", "Serve the corpus over the network", "The corpus is served by a service", "Several writers", nil, nil)
	if err != nil {
		t.Fatalf("RecordDecision: %v", err)
	}
	retirement, err := store.SupersedeDecisionReport(ctx, "proj", decisionID, newDecisionID)
	if err != nil {
		t.Fatalf("SupersedeDecisionReport: %v", err)
	}
	if len(retirement.Retired) != 1 || retirement.Retired[0] != copyID {
		t.Errorf("retirement = %+v, want the kept successor %s retired", retirement, copyID)
	}
}

// liveRowsCarryingDecision lists the project's live rows whose source_ref names
// the given decision.
func liveRowsCarryingDecision(t *testing.T, store *memory.Store, project, decisionID string) []memory.Memory {
	t.Helper()
	all, err := store.GetAll(context.Background(), project, -1)
	if err != nil {
		t.Fatalf("GetAll: %v", err)
	}
	var out []memory.Memory
	for _, m := range all {
		if m.ResolvedAt != nil {
			continue
		}
		if strings.Contains(m.SourceRef, "decision:"+decisionID) {
			out = append(out, m)
		}
	}
	return out
}

// TestReflectKeepsADroppedRowTheGuardHandedBack covers the other side of the
// same claim.
//
// A successor's claim on the row a drop disposed of is a claim, not a
// deletion: when the drop guard hands the dropped row back — no output
// accounts for it, so the corpus cannot show it is gone — that row is live, and
// the successor must not take it away. The guard is keep-biased on purpose (a
// stale row that is kept is repairable; a deleted row is not), so the claim
// may only ever narrow what the successor inherits, never remove the row the
// corpus still holds.
func TestReflectKeepsADroppedRowTheGuardHandedBack(t *testing.T) {
	store, ctx := newReflectStore(t, "proj")

	_, companionID, _, err := store.RecordDecision(ctx, "proj", "Keep the corpus in one SQLite file", "The corpus is stored in SQLite", "One file to copy", nil, nil)
	if err != nil {
		t.Fatalf("RecordDecision: %v", err)
	}
	// A row that says nothing about the decision, so no output accounts for the
	// companion and the guard hands it back.
	otherID, err := store.Create(ctx, "proj", memory.Memory{
		Category:   "fact",
		Content:    "the ledger ingests through the bastion on port 2222",
		Importance: 0.6,
		Source:     "mcp",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	input := reflectInputFor(t, store, "proj")
	llm := reflection.NewLlmConsolidator(stubHarness{reply: func() string {
		return fmt.Sprintf(`{"learned_context":"ctx","ops":["drop %s reason: superseded by %s","keep %s"]}`,
			companionID, otherID, otherID)
	}})
	result, err := llm.Consolidate(context.Background(), input)
	if err != nil {
		t.Fatalf("Consolidate: %v", err)
	}
	guarded := reflection.AuditGuardedDrops(input, result)
	if len(guarded) != 1 || guarded[0].ID != companionID {
		t.Fatalf("the drop guard handed back %+v, want the companion %s", guarded, companionID)
	}
	result.Memories = append(result.Memories, reflection.RetainGuardedDrops(guarded)...)

	projectMems, globalMems := splitByScope(result.Memories)
	if len(globalMems) != 0 {
		t.Fatalf("%d candidates are global: %+v", len(globalMems), result.Memories)
	}
	if _, _, _, _, _, _, _, _, _, _, err := applyReflection(ctx, store, "proj", projectMems, globalMems, "", false, replacedIDsFor(&result), nil); err != nil {
		t.Fatalf("applyReflection: %v", err)
	}

	if _, err := store.GetMemoryContent(ctx, companionID); err != nil {
		t.Errorf("the dropped row was deleted: %v — the guard handed it back, so it is live", err)
	}
}
