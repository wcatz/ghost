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
