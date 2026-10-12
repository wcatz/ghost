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
