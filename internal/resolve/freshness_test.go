package resolve

// #1052: a note rewritten WHILE it is being classified must not be stamped
// resolved.
//
// The pass reads candidates, spends a CLI-harness call adjudicating their text,
// and only then writes. A save or edit that lands in that window leaves the
// verdict about text the row no longer holds — and `resolved_at` is persistent
// and removes the row from every session start, so stamping it hides a fresh
// instruction on a judgement about its previous text. The write asserts the
// exact content the verdict was judged against, and the rows it refuses are
// reported as stale rather than as resolved.
//
// The positive half is as load-bearing as the negative one: the guard must
// refuse a rewrite and NOTHING else, or the pass would stop resolving the notes
// nobody touched.

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// rewritingClassifier answers RESOLVED for every note, and on every call
// rewrites the one note under test before returning — the race the issue
// reproduces, with the classifier standing in for the concurrent save that lands
// mid-classify.
type rewritingClassifier struct {
	store   *memory.Store
	project string
	id      string
	newText string
	calls   int
}

func (c *rewritingClassifier) IsResolvedBatch(ctx context.Context, contents []string) ([]Verdict, error) {
	c.calls++
	if err := c.store.UpdateMemory(ctx, c.project, c.id, &c.newText, nil, nil, nil); err != nil {
		return nil, err
	}
	out := make([]Verdict, len(contents))
	for i := range out {
		out[i] = VerdictResolved
	}
	return out, nil
}

// newFreshnessStore is a real store holding one project, because the property
// under test is the write's own WHERE clause and a fake store could only prove
// it copies whatever it is handed.
func newFreshnessStore(t *testing.T) (*memory.Store, context.Context, string) {
	t.Helper()
	ctx := context.Background()
	db, err := memory.OpenDB(filepath.Join(t.TempDir(), "freshness.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	s := memory.NewStore(db, nil)
	const project = "freshness"
	if err := s.EnsureProject(ctx, project, "/work/freshness", project); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	return s, ctx, project
}

// TestANoteRewrittenWhileItIsClassifiedIsNotResolved drives the issue's exact
// sequence: create a note that reads like concluded work, rewrite it to a live
// instruction during classification, and answer RESOLVED. The note must keep its
// new text and stay live.
func TestANoteRewrittenWhileItIsClassifiedIsNotResolved(t *testing.T) {
	s, ctx, project := newFreshnessStore(t)
	oldText := "the migration was shipped in v2 and is closed"
	id, err := s.Create(ctx, project, memory.Memory{Category: "fact", Content: oldText, Importance: 0.5, Source: "mcp"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	newText := "the migration rollback runbook is still the live procedure; keep following it"
	cls := &rewritingClassifier{store: s, project: project, id: id, newText: newText}

	res, _, err := Run(ctx, s, cls, project, true, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Confirmed != 1 {
		t.Fatalf("Confirmed = %d, want 1 (the classifier answered RESOLVED)", res.Confirmed)
	}
	if res.Resolved != 0 {
		t.Errorf("Resolved = %d, want 0 — the note was rewritten while it was classified", res.Resolved)
	}
	if res.Stale != 1 {
		t.Errorf("Stale = %d, want 1 — the refused row must be reported, not silently dropped", res.Stale)
	}

	rows, err := s.GetByIDs(ctx, []string{id})
	if err != nil || len(rows) != 1 {
		t.Fatalf("GetByIDs: %v (%d rows)", err, len(rows))
	}
	if rows[0].Content != newText {
		t.Errorf("stored content = %q, want the rewritten text %q", rows[0].Content, newText)
	}
	if rows[0].ResolvedAt != nil {
		t.Errorf("resolved_at = %q, want NULL — a verdict about the old text must not hide the new one", *rows[0].ResolvedAt)
	}
}

// TestAMixedBatchStampsOnlyTheRowsThatKeptTheirContent is the mutation guard for
// the UPDATE's OWN WHERE clause, which the issue requires and which the
// single-row test above cannot reach.
//
// When every confirmed row was rewritten, the writer's early-out
// (`if len(fresh) == 0 { continue }`) skips the UPDATE entirely, so that test
// passes even if the UPDATE carries no content check at all. Putting one
// rewritten row and one untouched row in the SAME pass makes the UPDATE run — and
// then the content match in its WHERE clause is the only thing keeping the
// rewritten row live. Dropping that match stamps the rewritten row, which this
// test fails on.
func TestAMixedBatchStampsOnlyTheRowsThatKeptTheirContent(t *testing.T) {
	s, ctx, project := newFreshnessStore(t)
	staleText := "the migration was shipped in v2 and is closed"
	freshText := "the graph experiment was reverted after the postmortem"
	staleID, err := s.Create(ctx, project, memory.Memory{Category: "fact", Content: staleText, Importance: 0.5, Source: "mcp"})
	if err != nil {
		t.Fatalf("Create stale: %v", err)
	}
	freshID, err := s.Create(ctx, project, memory.Memory{Category: "fact", Content: freshText, Importance: 0.5, Source: "mcp"})
	if err != nil {
		t.Fatalf("Create fresh: %v", err)
	}
	newText := "the migration rollback runbook is still the live procedure; keep following it"
	cls := &rewritingClassifier{store: s, project: project, id: staleID, newText: newText}

	res, _, err := Run(ctx, s, cls, project, true, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Confirmed != 2 {
		t.Fatalf("Confirmed = %d, want 2 (both notes reached the classifier and answered RESOLVED)", res.Confirmed)
	}
	if res.Resolved != 1 || res.Stale != 1 {
		t.Fatalf("Resolved = %d, Stale = %d; want 1 and 1", res.Resolved, res.Stale)
	}

	rows, err := s.GetByIDs(ctx, []string{staleID, freshID})
	if err != nil || len(rows) != 2 {
		t.Fatalf("GetByIDs: %v (%d rows)", err, len(rows))
	}
	byID := make(map[string]memory.Memory, len(rows))
	for _, r := range rows {
		byID[r.ID] = r
	}
	if got := byID[staleID]; got.Content != newText || got.ResolvedAt != nil {
		t.Errorf("rewritten row: content = %q, resolved_at = %v; want the new text and NULL", got.Content, got.ResolvedAt)
	}
	if got := byID[freshID]; got.Content != freshText || got.ResolvedAt == nil {
		t.Errorf("untouched row: content = %q, resolved_at = %v; want the judged text and a stamp", got.Content, got.ResolvedAt)
	}
}

// TestRunReturnsOnlyStampedRowsWhenApplying is the listing half of #1052: Run's
// second return is what the CLI and ghost_resolve print as the resolved
// listing, so a confirmed row the writer refused must not be in it — with its
// judged text as if it had been resolved, or at all. The store keeps the new
// text and stays live; the listing names only the rows the write really
// stamped, which are the rows whose read-time content still matched.
func TestRunReturnsOnlyStampedRowsWhenApplying(t *testing.T) {
	s, ctx, project := newFreshnessStore(t)
	staleText := "the migration was shipped in v2 and is closed"
	freshText := "the graph experiment was reverted after the postmortem"
	staleID, err := s.Create(ctx, project, memory.Memory{Category: "fact", Content: staleText, Importance: 0.5, Source: "mcp"})
	if err != nil {
		t.Fatalf("Create stale: %v", err)
	}
	freshID, err := s.Create(ctx, project, memory.Memory{Category: "fact", Content: freshText, Importance: 0.5, Source: "mcp"})
	if err != nil {
		t.Fatalf("Create fresh: %v", err)
	}
	newText := "the migration rollback runbook is still the live procedure; keep following it"
	cls := &rewritingClassifier{store: s, project: project, id: staleID, newText: newText}

	res, applied, err := Run(ctx, s, cls, project, true, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Resolved != 1 || res.Stale != 1 {
		t.Fatalf("Resolved = %d, Stale = %d; want 1 and 1", res.Resolved, res.Stale)
	}
	if len(applied) != 1 || applied[0].ID != freshID {
		t.Fatalf("applied listing = %d rows (%v), want exactly the untouched row %s",
			len(applied), memoryIDs(applied), freshID)
	}
	if applied[0].Content != freshText {
		t.Errorf("applied row content = %q, want %q — the listing must show what the store holds, not a judged text that was refused", applied[0].Content, freshText)
	}

	// The store keeps the same split the listing reported: the untouched row is
	// stamped under its judged text, the rewritten row keeps the NEW text and
	// stays live (resolved_at NULL) so a later pass can re-judge it.
	rows, err := s.GetByIDs(ctx, []string{staleID, freshID})
	if err != nil || len(rows) != 2 {
		t.Fatalf("GetByIDs: %v (%d rows)", err, len(rows))
	}
	byID := make(map[string]memory.Memory, len(rows))
	for _, r := range rows {
		byID[r.ID] = r
	}
	if got := byID[staleID]; got.Content != newText || got.ResolvedAt != nil {
		t.Errorf("rewritten row: content = %q, resolved_at = %v; want the new text and NULL", got.Content, got.ResolvedAt)
	}
	if got := byID[freshID]; got.Content != freshText || got.ResolvedAt == nil {
		t.Errorf("untouched row: content = %q, resolved_at = %v; want the judged text and a stamp", got.Content, got.ResolvedAt)
	}
}

// memoryIDs renders the ids of a listing for a failure message.
func memoryIDs(mems []memory.Memory) []string {
	ids := make([]string, len(mems))
	for i, m := range mems {
		ids[i] = m.ID
	}
	return ids
}

// promotingClassifier answers RESOLVED for every note, and on every call
// promotes the one note under test to _global before returning — a
// ghost_memory_promote landing inside the read-to-write window. PromoteToGlobal
// touches project_id ONLY, so the note still holds the judged text and a content
// CAS alone cannot refuse it; the project the pass read is the CAS's second
// field, and this classifier is what proves it is compared.
type promotingClassifier struct {
	store   *memory.Store
	project string
	id      string
	calls   int
}

func (c *promotingClassifier) IsResolvedBatch(ctx context.Context, contents []string) ([]Verdict, error) {
	c.calls++
	if err := c.store.PromoteToGlobal(ctx, c.project, c.id); err != nil {
		return nil, err
	}
	out := make([]Verdict, len(contents))
	for i := range out {
		out[i] = VerdictResolved
	}
	return out, nil
}

// TestAPromotedCandidateIsNotResolvedByAProjectPass is the mutation guard for
// the CAS's PROJECT field, which the content match alone cannot see and which
// the single-axis tests above never exercise.
//
// A promotion changes project_id and nothing else — no content, no updated_at —
// so a note promoted to _global mid-pass still matches the judged text byte for
// byte, and an unbound or content-only writer stamps it: a project-scoped pass
// deciding for the shared scope, the same read-to-write window the issue
// exists to close and the same row MarkResolved refuses by name. Comparing the
// read-time project too makes the row eligible-but-no-longer-the-row-the-
// verdict-was-about: NOT stamped, and REPORTED stale rather than dropped in
// silence, because "confirmed, and then the row changed under the verdict" is
// the one state the stale count exists to name.
func TestAPromotedCandidateIsNotResolvedByAProjectPass(t *testing.T) {
	s, ctx, project := newFreshnessStore(t)
	text := "the migration was shipped in v2 and is closed"
	id, err := s.Create(ctx, project, memory.Memory{Category: "fact", Content: text, Importance: 0.5, Source: "mcp"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	cls := &promotingClassifier{store: s, project: project, id: id}

	res, _, err := Run(ctx, s, cls, project, true, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Confirmed != 1 {
		t.Fatalf("Confirmed = %d, want 1 (the classifier answered RESOLVED)", res.Confirmed)
	}
	if res.Resolved != 0 {
		t.Errorf("Resolved = %d, want 0 — the note left the project while it was classified", res.Resolved)
	}
	if res.Stale != 1 {
		t.Errorf("Stale = %d, want 1 — the refuse must be reported, not silent: the row is confirmed and then no longer the one the verdict was about", res.Stale)
	}

	rows, err := s.GetByIDs(ctx, []string{id})
	if err != nil || len(rows) != 1 {
		t.Fatalf("GetByIDs: %v (%d rows)", err, len(rows))
	}
	got := rows[0]
	if got.ProjectID != memory.GlobalProjectID {
		t.Errorf("project_id = %q, want %q — the promotion itself must land", got.ProjectID, memory.GlobalProjectID)
	}
	if got.Content != text || got.ResolvedAt != nil {
		t.Errorf("row: content = %q, resolved_at = %v; want the judged text unchanged and NULL", got.Content, got.ResolvedAt)
	}
}

// TestANoteThatKeptItsJudgedContentIsStillResolved is the other half: the guard
// refuses a rewrite and nothing else. A confirmed note no writer touched is
// stamped exactly as it was before the freshness check existed.
func TestANoteThatKeptItsJudgedContentIsStillResolved(t *testing.T) {
	s, ctx, project := newFreshnessStore(t)
	content := "kill experiment: the graph bonus is gone, PR #210 landed it"
	id, err := s.Create(ctx, project, memory.Memory{Category: "fact", Content: content, Importance: 0.5, Source: "mcp"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	cls := &fakeClassifier{drop: map[string]bool{content: true}}

	res, _, err := Run(ctx, s, cls, project, true, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Resolved != 1 || res.Stale != 0 {
		t.Fatalf("Resolved = %d, Stale = %d; want 1 and 0", res.Resolved, res.Stale)
	}
	rows, err := s.GetByIDs(ctx, []string{id})
	if err != nil || len(rows) != 1 {
		t.Fatalf("GetByIDs: %v (%d rows)", err, len(rows))
	}
	if rows[0].ResolvedAt == nil {
		t.Errorf("resolved_at = NULL, want a stamp: the content did not change")
	}
}
