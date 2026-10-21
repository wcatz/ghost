package memory

import (
	"context"
	"strings"
	"testing"
)

// A decision's companion memory is found by the link in its source_ref, and
// the link is Ghost's to manage. An edit that writes a source_ref replaced
// that link with the caller's own value, so the companion stopped being the
// decision's companion: the lookup found no linked row, the composed-text
// fallback needed an EMPTY source_ref, and the supersede answered a plain
// success while the reversed decision kept ranking.
//
// The edit therefore keeps the links the row already carries and writes the
// caller's reference beside them.
func TestASourceRefEditKeepsTheDecisionLinkOnTheCompanion(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	decisionID, companionID, _, err := s.RecordDecision(ctx, testProject,
		"Use Redis for the job queue", "Redis lists as the queue backend",
		"Already deployed for caching", nil, nil)
	if err != nil {
		t.Fatalf("RecordDecision: %v", err)
	}

	if err := s.UpdateMemoryWithOptions(ctx, testProject, companionID, UpdateOptions{
		Provenance: Provenance{SourceRef: "docs/queue.md"},
	}); err != nil {
		t.Fatalf("UpdateMemoryWithOptions: %v", err)
	}

	var ref string
	if err := s.db.QueryRow(`SELECT COALESCE(source_ref, '') FROM memories WHERE id = ?`, companionID).Scan(&ref); err != nil {
		t.Fatalf("read the companion's source_ref: %v", err)
	}
	if !sourceRefNamesDecision(ref, decisionID) {
		t.Fatalf("the companion's source_ref is %q, which does not name decision %s: the edit dropped the link", ref, decisionID)
	}
	if ref != "docs/queue.md,"+decisionCompanionRef(decisionID) {
		t.Errorf("the companion's source_ref is %q, want the caller's reference beside the link", ref)
	}
}

// The link surviving is the whole point: a supersede after the edit must still
// find the companion and retire it, so the reversed decision stops ranking.
func TestASupersedeAfterASourceRefEditStillRetiresTheCompanion(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	decisionID, companionID, _, err := s.RecordDecision(ctx, testProject,
		"Use Redis for the job queue", "Redis lists as the queue backend",
		"Already deployed for caching", nil, nil)
	if err != nil {
		t.Fatalf("RecordDecision: %v", err)
	}
	if err := s.UpdateMemoryWithOptions(ctx, testProject, companionID, UpdateOptions{
		Provenance: Provenance{SourceRef: "docs/queue.md"},
	}); err != nil {
		t.Fatalf("UpdateMemoryWithOptions: %v", err)
	}

	newDecisionID, _, _, err := s.RecordDecision(ctx, testProject,
		"Reverse: use Postgres for the job queue", "SKIP LOCKED on Postgres",
		"Redis lost jobs on failover", nil, nil)
	if err != nil {
		t.Fatalf("RecordDecision (new): %v", err)
	}
	got, err := s.SupersedeDecisionReport(ctx, testProject, decisionID, newDecisionID)
	if err != nil {
		t.Fatalf("SupersedeDecisionReport: %v", err)
	}
	if len(got.Retired) != 1 || got.Retired[0] != companionID {
		t.Errorf("report = %+v, want the companion %s retired", got, companionID)
	}
	if len(got.Declined) != 0 || len(got.Ambiguous) != 0 || len(got.Shared) != 0 {
		t.Errorf("report = %+v, want no companion left standing", got)
	}
	if !decisionMemoryResolved(t, s, companionID) {
		t.Error("the companion is still live: search and session start keep answering with the reversed decision")
	}
}

// An edit that says nothing about the reference leaves the stored one whole,
// links and all, and so does an edit on a row that carries no link at all.
func TestASourceRefEditLeavesAnUnlinkedRowAlone(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	id, err := s.Create(ctx, testProject, Memory{Category: "fact", Content: "the queue backend", Source: "mcp", Importance: 0.5})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := s.UpdateMemoryWithOptions(ctx, testProject, id, UpdateOptions{
		Provenance: Provenance{SourceRef: "docs/queue.md"},
	}); err != nil {
		t.Fatalf("UpdateMemoryWithOptions: %v", err)
	}
	var ref string
	if err := s.db.QueryRow(`SELECT COALESCE(source_ref, '') FROM memories WHERE id = ?`, id).Scan(&ref); err != nil {
		t.Fatalf("read the row's source_ref: %v", err)
	}
	if ref != "docs/queue.md" {
		t.Errorf("the row's source_ref is %q, want %q", ref, "docs/queue.md")
	}
}

// An edit that mentions no reference keeps the stored one whole, links and all.
func TestAnEditWithNoSourceRefKeepsTheStoredReference(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	decisionID, companionID, _, err := s.RecordDecision(ctx, testProject,
		"Use Redis for the job queue", "Redis lists as the queue backend",
		"Already deployed for caching", nil, nil)
	if err != nil {
		t.Fatalf("RecordDecision: %v", err)
	}
	if err := s.UpdateMemoryWithOptions(ctx, testProject, companionID, UpdateOptions{
		Tags: []string{"queue"},
	}); err != nil {
		t.Fatalf("UpdateMemoryWithOptions: %v", err)
	}
	var ref string
	if err := s.db.QueryRow(`SELECT COALESCE(source_ref, '') FROM memories WHERE id = ?`, companionID).Scan(&ref); err != nil {
		t.Fatalf("read the companion's source_ref: %v", err)
	}
	if !sourceRefNamesDecision(ref, decisionID) {
		t.Errorf("the companion's source_ref is %q, which does not name decision %s", ref, decisionID)
	}
}

// A reference that does not fit beside the link is refused rather than
// shortened: a truncated path is a different path, and the link is never
// traded for the reference.
func TestASourceRefEditThatDoesNotFitBesideTheLinkIsRefused(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	decisionID, companionID, _, err := s.RecordDecision(ctx, testProject,
		"Use Redis for the job queue", "Redis lists as the queue backend",
		"Already deployed for caching", nil, nil)
	if err != nil {
		t.Fatalf("RecordDecision: %v", err)
	}
	long := strings.Repeat("p", MaxSourceRefLen)
	err = s.UpdateMemoryWithOptions(ctx, testProject, companionID, UpdateOptions{
		Provenance: Provenance{SourceRef: long},
	})
	if err == nil {
		t.Fatal("the edit was accepted: the reference and the link do not fit in one source_ref")
	}
	if !strings.Contains(err.Error(), "do not fit in one source_ref") {
		t.Errorf("the refusal does not say why: %v", err)
	}
	var ref string
	if err := s.db.QueryRow(`SELECT COALESCE(source_ref, '') FROM memories WHERE id = ?`, companionID).Scan(&ref); err != nil {
		t.Fatalf("read the companion's source_ref: %v", err)
	}
	if !sourceRefNamesDecision(ref, decisionID) {
		t.Errorf("the companion's source_ref is %q, which does not name decision %s", ref, decisionID)
	}
}

// A SAVE that folds onto a decision's companion writes its own reference onto
// the row it folded into, so the same hole was open one door over: the link
// was replaced and the decision was left with no companion.
func TestASaveThatFoldsOntoTheCompanionKeepsTheDecisionLink(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	decisionID, companionID, _, err := s.RecordDecision(ctx, testProject,
		"Use Redis for the job queue", "Redis lists as the queue backend",
		"Already deployed for caching", nil, nil)
	if err != nil {
		t.Fatalf("RecordDecision: %v", err)
	}
	content, _ := decisionCompanionContent("Use Redis for the job queue", "Redis lists as the queue backend", "Already deployed for caching")
	if _, _, _, err := s.UpsertWithOptions(ctx, testProject, "decision", content, "mcp", 0.5, nil, UpsertOptions{
		Provenance: Provenance{SourceRef: "docs/queue.md"},
	}); err != nil {
		t.Fatalf("UpsertWithOptions: %v", err)
	}
	var ref string
	if err := s.db.QueryRow(`SELECT COALESCE(source_ref, '') FROM memories WHERE id = ?`, companionID).Scan(&ref); err != nil {
		t.Fatalf("read the companion's source_ref: %v", err)
	}
	if !sourceRefNamesDecision(ref, decisionID) {
		t.Errorf("the companion's source_ref is %q, which does not name decision %s: the save dropped the link", ref, decisionID)
	}
}
