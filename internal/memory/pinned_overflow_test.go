package memory

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

func pinnedOverflowStore(t *testing.T) *Store {
	t.Helper()
	db, err := OpenDB(":memory:")
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	s := NewStore(db, nil)
	ctx := context.Background()
	for _, p := range []string{"proj", "_global"} {
		if err := s.EnsureProject(ctx, p, "/tmp/"+p, p); err != nil {
			t.Fatalf("EnsureProject %s: %v", p, err)
		}
	}
	return s
}

// Actually need to return Store, not DB; helper above creates Store directly
func createPinned(t *testing.T, s *Store, project, id string, importance float32, contentLen int) {
	t.Helper()
	ctx := context.Background()
	content := strings.Repeat("x", contentLen)
	// Insert directly to control importance and content length
	db := s.db
	stamp := "2026-01-01 00:00:00"
	if _, err := db.Exec(`INSERT INTO memories (id, project_id, category, content, source, importance, pinned, created_at, updated_at) VALUES (?, ?, 'fact', ?, 'manual', ?, 1, ?, ?)`, id, project, content, importance, stamp, stamp); err != nil {
		t.Fatalf("insert %s: %v", id, err)
	}
	_ = ctx
}

func TestPinnedOverflowAtCapReportsNone(t *testing.T) {
	s := pinnedOverflowStore(t)
	ctx := context.Background()
	now := time.Now()
	// project at cap
	for i := 0; i < PinnedProjectCap; i++ {
		createPinned(t, s, "proj", fmt.Sprintf("p%03d", i), 0.7, 10)
	}
	if ov, err := s.PinnedOverflow(ctx, "proj", now, nil); err != nil {
		t.Fatalf("PinnedOverflow: %v", err)
	} else if ov != nil {
		t.Fatalf("expected no overflow at cap, got %+v", ov)
	}
	// _global at cap
	s2 := pinnedOverflowStore(t)
	for i := 0; i < PinnedGlobalCap; i++ {
		createPinned(t, s2, "_global", fmt.Sprintf("g%03d", i), 0.7, 10)
	}
	if ov, err := s2.PinnedOverflow(ctx, "_global", now, nil); err != nil {
		t.Fatalf("PinnedOverflow: %v", err)
	} else if ov != nil {
		t.Fatalf("expected no overflow at cap for _global, got %+v", ov)
	}
}

func TestPinnedOverflowCapPlusOneReportsOverflow(t *testing.T) {
	s := pinnedOverflowStore(t)
	ctx := context.Background()
	now := time.Now()
	// Create PinnedProjectCap+1 with varying importance so lowest is known
	for i := 0; i < PinnedProjectCap+1; i++ {
		imp := float32(0.9 - float32(i)*0.01)
		createPinned(t, s, "proj", fmt.Sprintf("p%03d", i), imp, 10)
	}
	ov, err := s.PinnedOverflow(ctx, "proj", now, nil)
	if err != nil {
		t.Fatalf("PinnedOverflow: %v", err)
	}
	if ov == nil {
		t.Fatalf("expected overflow at cap+1, got none")
	}
	if ov.Count != PinnedProjectCap+1 {
		t.Errorf("Count=%d want %d", ov.Count, PinnedProjectCap+1)
	}
	if ov.Cap != PinnedProjectCap {
		t.Errorf("Cap=%d want %d", ov.Cap, PinnedProjectCap)
	}

	if len(ov.CutIDs) != 1 {
		t.Fatalf("CutIDs=%v want 1", ov.CutIDs)
	}
	// Lowest-ranked should be last created with lowest importance: p015
	if ov.CutIDs[0] != fmt.Sprintf("p%03d", PinnedProjectCap) {
		t.Errorf("CutIDs[0]=%s want p%03d", ov.CutIDs[0], PinnedProjectCap)
	}
	// Also test _global
	s2 := pinnedOverflowStore(t)
	for i := 0; i < PinnedGlobalCap+1; i++ {
		imp := float32(0.9 - float32(i)*0.01)
		createPinned(t, s2, "_global", fmt.Sprintf("g%03d", i), imp, 10)
	}
	ov2, err := s2.PinnedOverflow(ctx, "_global", now, nil)
	if err != nil {
		t.Fatalf("PinnedOverflow: %v", err)
	}
	if ov2 == nil || ov2.Count != PinnedGlobalCap+1 || len(ov2.CutIDs) != 1 {
		t.Fatalf("global overflow unexpected: %+v err %v", ov2, err)
	}
}

func TestPinnedOverflowByteOverflowWithFewLongPins(t *testing.T) {
	s := pinnedOverflowStore(t)
	ctx := context.Background()
	now := time.Now()
	// Fewer than cap, but bytes exceed share: 3 rows of 1500 each =4500 >3000.
	// With preview truncation and KeepPinned, the block never cuts pinned rows for
	// bytes; only count overflow is reported.
	for i := 0; i < 3; i++ {
		imp := float32(0.9 - float32(i)*0.01)
		createPinned(t, s, "proj", fmt.Sprintf("b%03d", i), imp, 1500)
	}
	ov, err := s.PinnedOverflow(ctx, "proj", now, nil)
	if err != nil {
		t.Fatalf("PinnedOverflow: %v", err)
	}
	if ov != nil {
		t.Fatalf("expected no overflow (byte-only, count under cap), got %+v", ov)
	}
}

func TestPinnedOverflowSessionHeaderUnchanged(t *testing.T) {
	// The session-start header's pinned-cut count is the cap's cut, not the
	// overflow warning. Ensure a bucket whose pinned rows exceed the cap still
	// reports the same cut via the candidate set after the new overflow code.
	s := pinnedOverflowStore(t)
	ctx := context.Background()
	// OverFetch is 45, so 46 pinned leaves 1 beyond the window.
	for i := 0; i < 46; i++ {
		imp := float32(0.9 - float32(i)*0.001)
		createPinned(t, s, "proj", fmt.Sprintf("p%03d", i), imp, 10)
	}
	pol := SlicePolicy{Bucket: "proj", Order: OrderDecay, OverFetch: 45, ItemCap: 15}
	set, err := s.Candidates(ctx, passiveRequest("proj", pol))
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	if got := set.PinnedBeyond["proj"]; got != 1 {
		t.Errorf("PinnedBeyond=%d want 1 (the count the block reports)", got)
	}
	ov, err := s.PinnedOverflow(ctx, "proj", time.Now(), nil)
	if err != nil {
		t.Fatalf("PinnedOverflow: %v", err)
	}
	if ov == nil || ov.Count != 46 {
		t.Fatalf("overflow count %v", ov)
	}
	if len(ov.CutIDs) != 31 {
		t.Errorf("CutIDs=%d want 31 (46-15)", len(ov.CutIDs))
	}
}

func TestPinnedOverflowScopeFiltersConflictingPins(t *testing.T) {
	s := pinnedOverflowStore(t)
	ctx := context.Background()
	now := time.Now()
	// 15 in-scope pins + 1 scope-conflicting pin under configured scope -> no overflow
	for i := 0; i < PinnedProjectCap; i++ {
		imp := float32(0.9 - float32(i)*0.01)
		createPinned(t, s, "proj", fmt.Sprintf("p%03d", i), imp, 10)
	}
	// Add conflicting scoped pin
	if _, err := s.db.Exec(`INSERT INTO memories (id, project_id, category, content, source, importance, pinned, created_at, updated_at, scope) VALUES (?, ?, 'fact', ?, 'manual', ?, 1, ?, ?, ?)`, "conflict", "proj", strings.Repeat("x", 10), float32(0.5), "2026-01-01 00:00:00", "2026-01-01 00:00:00", `{"environment":"staging"}`); err != nil {
		t.Fatalf("insert conflict: %v", err)
	}
	// With scope requiring production, the conflicting pin is excluded -> total 15, no overflow
	if ov, err := s.PinnedOverflow(ctx, "proj", now, map[string]string{"environment": "production"}); err != nil {
		t.Fatalf("PinnedOverflow: %v", err)
	} else if ov != nil {
		t.Fatalf("expected no overflow when conflicting pin excluded by scope, got %+v", ov)
	}
	// Same 16 with no scope report count 16
	if ov, err := s.PinnedOverflow(ctx, "proj", now, nil); err != nil {
		t.Fatalf("PinnedOverflow: %v", err)
	} else if ov == nil || ov.Count != PinnedProjectCap+1 {
		t.Fatalf("expected overflow count 16 with no scope, got %+v", ov)
	}
}
