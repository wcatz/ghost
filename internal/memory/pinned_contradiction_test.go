package memory

import (
	"context"
	"testing"
)

func TestPinnedRowsWithContradictions(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	mk := func(content, updated string) string {
		id := makeMemory(t, s, content)
		if _, err := s.db.ExecContext(ctx, `UPDATE memories SET updated_at = ? WHERE id = ?`, updated, id); err != nil {
			t.Fatalf("stamp: %v", err)
		}
		return id
	}
	pin := mk("the database is postgres", "2026-01-01 00:00:00")
	if _, err := s.db.ExecContext(ctx, `UPDATE memories SET pinned = 1 WHERE id = ?`, pin); err != nil {
		t.Fatalf("pin: %v", err)
	}
	newer := mk("the database is mysql", "2026-06-01 00:00:00")
	older := mk("the database was sqlite", "2025-01-01 00:00:00")
	closed := mk("the database is oracle", "2026-06-02 00:00:00")
	withdrawn := mk("the database is db2", "2026-06-03 00:00:00")
	scoped := mk("the database is mariadb", "2026-06-04 00:00:00")
	if _, err := s.db.ExecContext(ctx, `UPDATE memories SET scope = '{"environment":"production"}' WHERE id = ?`, pin); err != nil {
		t.Fatalf("scope: %v", err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE memories SET scope = '{"environment":"development"}' WHERE id = ?`, scoped); err != nil {
		t.Fatalf("scope: %v", err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE memories SET resolved_at = datetime('now') WHERE id = ?`, closed); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	for _, e := range [][2]string{{newer, pin}, {pin, older}, {closed, pin}, {withdrawn, pin}, {scoped, pin}} {
		if err := s.CreateLink(ctx, e[0], e[1], "contradicts", 1, "manual"); err != nil {
			t.Fatalf("link: %v", err)
		}
	}
	if n, err := s.InvalidateLink(ctx, withdrawn, pin, "contradicts"); err != nil || n != 1 {
		t.Fatalf("withdraw: %d %v", n, err)
	}

	got, total, err := s.PinnedRowsWithContradictions(ctx, 10)
	if err != nil {
		t.Fatalf("PinnedRowsWithContradictions: %v", err)
	}
	if total != 1 || len(got) != 1 || got[0].ID != pin || got[0].ContradictedBy != newer {
		t.Fatalf("got %+v (total %d), want only %s contradicted by %s", got, total, pin, newer)
	}

	// The bound limits the rows returned and still counts them all.
	if err := s.CreateLink(ctx, mk("the database is derby", "2026-07-01 00:00:00"), pin, "contradicts", 1, "manual"); err != nil {
		t.Fatalf("link: %v", err)
	}
	got, total, err = s.PinnedRowsWithContradictions(ctx, 1)
	if err != nil || len(got) != 1 || total != 2 {
		t.Fatalf("limit 1: %d rows, total %d, err %v; want 1 row, total 2", len(got), total, err)
	}
}

// A pinned row with an open contradiction stays out of every maintenance pool:
// resolve and supersede read pools that exclude pinned rows, and the edge does
// not change that.
func TestPinnedContradictedRowStaysOutOfTheResolvePools(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	pin := makeMemory(t, s, "pinned claim")
	newer := makeMemory(t, s, "newer claim")
	if _, err := s.db.ExecContext(ctx, `UPDATE memories SET pinned = 1 WHERE id = ?`, pin); err != nil {
		t.Fatalf("pin: %v", err)
	}
	if err := s.CreateLink(ctx, newer, pin, "contradicts", 1, "llm"); err != nil {
		t.Fatalf("a contradicts edge against a pinned row must be recordable: %v", err)
	}
	cands, err := s.ResolveCandidates(ctx, testProject)
	if err != nil {
		t.Fatalf("ResolveCandidates: %v", err)
	}
	for _, m := range cands {
		if m.ID == pin {
			t.Fatalf("the pinned row is a resolve candidate")
		}
	}
	if len(cands) != 1 || cands[0].ID != newer {
		t.Fatalf("resolve candidates = %d rows, want only the unpinned one", len(cands))
	}
}
