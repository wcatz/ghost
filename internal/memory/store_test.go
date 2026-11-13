package memory

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const testProject = "test-project"

func testStore(t *testing.T) *Store {
	t.Helper()
	db, err := OpenDB(":memory:")
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	s := NewStore(db, logger)

	ctx := context.Background()
	if err := s.EnsureProject(ctx, testProject, "/tmp/test", "test"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	return s
}

func TestStoreCreate(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	m := Memory{
		Category:   "fact",
		Content:    "Go uses goroutines for concurrency",
		Source:     "manual",
		Importance: 0.7,
		Tags:       []string{"go", "concurrency"},
	}

	id, err := s.Create(ctx, testProject, m)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if id == "" {
		t.Fatal("Create returned empty ID")
	}

	// Verify the memory is stored by retrieving all memories.
	all, err := s.GetAll(ctx, testProject, 100)
	if err != nil {
		t.Fatalf("GetAll: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("expected 1 memory, got %d", len(all))
	}
	if all[0].ID != id {
		t.Errorf("expected ID %s, got %s", id, all[0].ID)
	}
	if all[0].Content != m.Content {
		t.Errorf("expected content %q, got %q", m.Content, all[0].Content)
	}
	if all[0].Category != m.Category {
		t.Errorf("expected category %q, got %q", m.Category, all[0].Category)
	}
	if all[0].Importance != m.Importance {
		t.Errorf("expected importance %f, got %f", m.Importance, all[0].Importance)
	}
	if all[0].Source != m.Source {
		t.Errorf("expected source %q, got %q", m.Source, all[0].Source)
	}
}

func TestStoreUpsert(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	// First insert via Upsert — should create new.
	id1, dupOf, _, err := s.Upsert(ctx, testProject, "fact", "SQLite supports full-text search via FTS5", "reflection", 0.6, []string{"sqlite"})
	if err != nil {
		t.Fatalf("Upsert (new): %v", err)
	}
	if dupOf != "" {
		t.Error("first Upsert should not report a duplicate")
	}
	if id1 == "" {
		t.Fatal("first Upsert returned empty ID")
	}

	// Second Upsert with overlapping content — should link as a duplicate of id1.
	id2, dupOf, _, err := s.Upsert(ctx, testProject, "fact", "SQLite FTS5 provides full-text search capabilities", "reflection", 0.5, []string{"sqlite", "fts"})
	if err != nil {
		t.Fatalf("Upsert (merge): %v", err)
	}
	if dupOf != id1 {
		t.Errorf("second Upsert should report duplicateOf %s, got %q", id1, dupOf)
	}
	if id2 == id1 {
		t.Error("second Upsert should have created its own row, not reused id1")
	}

	// Both rows exist, with their original content untouched.
	all, err := s.GetAll(ctx, testProject, 100)
	if err != nil {
		t.Fatalf("GetAll: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("expected 2 memories after duplicate-link, got %d", len(all))
	}
	byID := map[string]Memory{}
	for _, m := range all {
		byID[m.ID] = m
	}
	if byID[id1].Content != "SQLite supports full-text search via FTS5" {
		t.Errorf("existing row content was overwritten: %q", byID[id1].Content)
	}
	if byID[id2].Content != "SQLite FTS5 provides full-text search capabilities" {
		t.Errorf("new row content wrong: %q", byID[id2].Content)
	}

	// Existing row's importance was strengthened: 0.6 + (0.5 * 0.2) = 0.7
	expected := float32(0.6 + 0.5*0.2)
	if diff := byID[id1].Importance - expected; diff > 0.01 || diff < -0.01 {
		t.Errorf("expected existing-row importance ~%f, got %f", expected, byID[id1].Importance)
	}

	// A 'duplicate' link connects the two rows in the right direction.
	links, err := s.GetLinks(ctx, id1)
	if err != nil {
		t.Fatalf("GetLinks: %v", err)
	}
	found := false
	for _, l := range links {
		if l.Relation == "duplicate" && l.SourceID == id2 && l.TargetID == id1 {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a 'duplicate' link source_id=%s target_id=%s, links: %+v", id2, id1, links)
	}
}

func TestMergeScore(t *testing.T) {
	tests := []struct {
		name      string
		a, b      string
		wantMerge bool
	}{
		{"identical", "cache TTL is 300 seconds", "cache TTL is 300 seconds", true},
		{"length-asymmetric paraphrase", "cache TTL is 300 seconds",
			"the cache TTL is set to 300 seconds in config", true},
		{"empty vs empty", "a b c", "x y z", false},
		{"empty vs populated", "a b c", "the embedding worker sweeps every minute", false},
		{"short containment below min tokens", "pnpm workspaces",
			"we use pnpm workspaces because npm workspaces hoists transitive deps wrongly", false},
		{"unrelated same topic", "SQLite busy timeout must be set on the read-only hook connection",
			"SQLite FTS5 rank ordering is unstable across identical RRF scores in hybrid search", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := mergeScore(tokenizeContent(tt.a), tokenizeContent(tt.b)) > 0
			if got != tt.wantMerge {
				t.Errorf("mergeScore(%q, %q) merge = %v, want %v (jaccard=%.3f overlap=%.3f)",
					tt.a, tt.b, got, tt.wantMerge,
					jaccard(tokenizeContent(tt.a), tokenizeContent(tt.b)),
					overlapCoefficient(tokenizeContent(tt.a), tokenizeContent(tt.b)))
			}
		})
	}
}

func TestStoreUpsertImportanceCap(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	// Create with high importance.
	firstID, _, _, err := s.Upsert(ctx, testProject, "fact", "Critical architecture pattern for the system", "reflection", 0.95, nil)
	if err != nil {
		t.Fatalf("Upsert (new): %v", err)
	}

	// Upsert a near-duplicate — existing row's importance should be capped at 1.0.
	_, dupOf, _, err := s.Upsert(ctx, testProject, "fact", "Critical architecture pattern for the entire system design", "reflection", 0.9, nil)
	if err != nil {
		t.Fatalf("Upsert (cap): %v", err)
	}
	if dupOf != firstID {
		t.Fatalf("expected duplicate link to %s, got %q", firstID, dupOf)
	}

	var importance float32
	if err := s.db.QueryRowContext(ctx, `SELECT importance FROM memories WHERE id = ?`, firstID).Scan(&importance); err != nil {
		t.Fatalf("query importance: %v", err)
	}
	if importance > 1.0 {
		t.Errorf("importance should be capped at 1.0, got %f", importance)
	}
}

// canonicalMemoryCount counts memories that are not themselves the SOURCE of
// an active 'duplicate' link — i.e. fold targets, not linked copies. A fold
// preserves both texts as rows (the non-destructive design TestStoreUpsert
// pins: 2 rows after a same-category fold), so the raw row count cannot tell
// "one rule plus its linked copy" from "two competing duplicates"; this count
// can. Cross-category folding must yield exactly one canonical memory.
func canonicalMemoryCount(t *testing.T, s *Store, projectID string) int {
	t.Helper()
	var count int
	err := s.db.QueryRowContext(context.Background(), `
		SELECT COUNT(*) FROM memories m
		WHERE m.project_id = ?
		  AND NOT EXISTS (
		    SELECT 1 FROM memory_links l
		    WHERE l.source_id = m.id
		      AND l.relation = 'duplicate'
		      AND l.invalidated_at IS NULL
		  )
	`, projectID).Scan(&count)
	if err != nil {
		t.Fatalf("canonical memory count: %v", err)
	}
	return count
}

// TestUpsert_FoldsCrossCategoryDuplicate pins the regression this branch
// fixes: the same rule saved as preference and then as gotcha must FOLD into
// the first memory instead of landing as a second, competing canonical copy.
// Before the cross-category probe existed, the gotcha save missed entirely
// (the probe only looked within its own category) and inserted an unlinked
// duplicate — how one production-safety rule accumulated as 12 copies split
// across preference/gotcha.
func TestUpsert_FoldsCrossCategoryDuplicate(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	const rule = "SQLite busy timeout must be set on the read-only hook connection"

	firstID, dupOf, _, err := s.Upsert(ctx, testProject, "preference", rule, "reflection", 0.6, nil)
	if err != nil {
		t.Fatalf("Upsert (first, preference): %v", err)
	}
	if dupOf != "" {
		t.Fatalf("first save should not report a duplicate, got %q", dupOf)
	}

	secondID, dupOf, score, err := s.Upsert(ctx, testProject, "gotcha", rule, "reflection", 0.6, nil)
	if err != nil {
		t.Fatalf("Upsert (second, gotcha): %v", err)
	}
	if dupOf != firstID {
		t.Errorf("cross-category re-save of the same rule must fold into the existing memory: duplicateOf = %q, want %q", dupOf, firstID)
	}
	if score < 0.7 {
		t.Errorf("fold score = %v, want >= 0.7 (identical text)", score)
	}
	if secondID == firstID {
		t.Error("fold must still insert the new text as its own linked row (non-destructive), not reuse the target's ID")
	}

	// ONE canonical memory — the fold target plus at most a linked copy of
	// the incoming save. Before the fix this counted 2 (two unlinked,
	// competing copies of one rule).
	if got := canonicalMemoryCount(t, s, testProject); got != 1 {
		t.Errorf("canonical memory count = %d, want 1 (fold target + linked copy)", got)
	}

	// Raw rows mirror the SAME-CATEGORY fold exactly: the non-destructive
	// fold keeps both texts (TestStoreUpsert asserts 2 rows after a
	// same-category fold and forbids content overwrites), so cross-category
	// must not invent a suppress-insert or content-overwrite path.
	if got, err := s.CountMemories(ctx, testProject); err != nil {
		t.Fatalf("CountMemories: %v", err)
	} else if got != 2 {
		t.Errorf("raw row count = %d, want 2 (fold keeps both texts, mirroring same-category semantics)", got)
	}

	all, err := s.GetAll(ctx, testProject, 100)
	if err != nil {
		t.Fatalf("GetAll: %v", err)
	}
	byID := map[string]Memory{}
	for _, m := range all {
		byID[m.ID] = m
	}

	// Surviving-category rule: the EXISTING memory keeps its category — a
	// save never silently recategorizes the (possibly pinned/canonical) row
	// it folds into — and its content is untouched. The linked copy carries
	// the incoming category so nothing the caller saved is lost.
	if byID[firstID].Category != "preference" {
		t.Errorf("fold target category = %q, want preference (existing memory keeps its category)", byID[firstID].Category)
	}
	if byID[firstID].Content != rule {
		t.Errorf("fold target content was overwritten: %q", byID[firstID].Content)
	}
	if byID[secondID].Category != "gotcha" {
		t.Errorf("linked copy category = %q, want gotcha (incoming category preserved)", byID[secondID].Category)
	}

	// The 'duplicate' link connects copy -> target, same direction the
	// same-category fold writes.
	links, err := s.GetLinks(ctx, firstID)
	if err != nil {
		t.Fatalf("GetLinks: %v", err)
	}
	found := false
	for _, l := range links {
		if l.Relation == "duplicate" && l.SourceID == secondID && l.TargetID == firstID {
			found = true
		}
	}
	if !found {
		t.Errorf("expected duplicate link source=%s target=%s, links: %+v", secondID, firstID, links)
	}
}

func TestStoreDelete(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	id, err := s.Create(ctx, testProject, Memory{
		Category:   "gotcha",
		Content:    "Temporary memory to delete",
		Source:     "chat",
		Importance: 0.3,
		Tags:       []string{},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := s.Delete(ctx, id); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	// Verify it's gone.
	all, err := s.GetAll(ctx, testProject, 100)
	if err != nil {
		t.Fatalf("GetAll: %v", err)
	}
	if len(all) != 0 {
		t.Errorf("expected 0 memories after delete, got %d", len(all))
	}

	// Deleting a non-existent memory should return an error.
	err = s.Delete(ctx, "nonexistent-id")
	if err == nil {
		t.Error("expected error deleting non-existent memory")
	}
}

func TestStoreReplaceNonManual(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	t.Run("empty set guard", func(t *testing.T) {
		_, _, _, _, err := s.ReplaceNonManual(ctx, testProject, []Memory{}, "", nil)
		if err == nil {
			t.Fatal("expected error for empty set")
		}
		if !strings.Contains(err.Error(), "empty set") {
			t.Errorf("error should mention empty set, got: %v", err)
		}
	})

	t.Run("manual memories survive replace", func(t *testing.T) {
		// Create one manual and one reflection memory.
		manualID, err := s.Create(ctx, testProject, Memory{
			Category:   "preference",
			Content:    "I prefer tabs over spaces",
			Source:     "manual",
			Importance: 0.8,
			Tags:       []string{},
		})
		if err != nil {
			t.Fatalf("Create manual: %v", err)
		}
		_, err = s.Create(ctx, testProject, Memory{
			Category:   "fact",
			Content:    "Old reflection fact that should be replaced",
			Source:     "reflection",
			Importance: 0.5,
			Tags:       []string{},
		})
		if err != nil {
			t.Fatalf("Create reflection: %v", err)
		}

		// Replace non-manual with a new set.
		replacement := []Memory{
			{Category: "fact", Content: "New consolidated fact", Importance: 0.6, Tags: []string{}},
		}
		if _, _, _, _, err := s.ReplaceNonManual(ctx, testProject, replacement, "", nil); err != nil {
			t.Fatalf("ReplaceNonManual: %v", err)
		}

		all, err := s.GetAll(ctx, testProject, 100)
		if err != nil {
			t.Fatalf("GetAll: %v", err)
		}

		// Should have manual + 1 new reflection = 2.
		if len(all) != 2 {
			t.Fatalf("expected 2 memories, got %d", len(all))
		}

		foundManual := false
		foundReplacement := false
		for _, m := range all {
			if m.ID == manualID && m.Source == "manual" {
				foundManual = true
			}
			if m.Content == "New consolidated fact" && m.Source == "reflection" {
				foundReplacement = true
			}
		}
		if !foundManual {
			t.Error("manual memory should survive replace")
		}
		if !foundReplacement {
			t.Error("replacement memory should be present")
		}
	})

	t.Run("preserves memories saved concurrently with consolidation", func(t *testing.T) {
		s := testStore(t)

		// Simulate ghost reflect's flow: capture "now" before the consolidation
		// input is fetched, then a ghost_memory_save lands on the live MCP
		// server while the (slow) consolidation round trip is in flight.
		since, err := s.CurrentTimestamp(ctx)
		if err != nil {
			t.Fatalf("CurrentTimestamp: %v", err)
		}

		concurrentID, err := s.Create(ctx, testProject, Memory{
			Category:   "gotcha",
			Content:    "saved while reflection was running",
			Source:     "mcp",
			Importance: 0.7,
			Tags:       []string{},
		})
		if err != nil {
			t.Fatalf("Create concurrent memory: %v", err)
		}

		replacement := []Memory{
			{Category: "fact", Content: "consolidator output, stale snapshot", Importance: 0.6, Tags: []string{}},
		}
		if _, _, _, _, err := s.ReplaceNonManual(ctx, testProject, replacement, since, nil); err != nil {
			t.Fatalf("ReplaceNonManual: %v", err)
		}

		all, err := s.GetAll(ctx, testProject, 100)
		if err != nil {
			t.Fatalf("GetAll: %v", err)
		}

		var foundConcurrent, foundReplacement bool
		for _, m := range all {
			if m.Content == "saved while reflection was running" && m.Source == "mcp" {
				foundConcurrent = true
			}
			if m.Content == "consolidator output, stale snapshot" {
				foundReplacement = true
			}
		}
		if !foundConcurrent {
			t.Errorf("memory %s saved during consolidation was dropped by ReplaceNonManual", concurrentID)
		}
		if !foundReplacement {
			t.Error("consolidator output should still be present")
		}
	})

	t.Run("pinned non-manual memory survives replace", func(t *testing.T) {
		s := testStore(t)

		pinnedID, err := s.Create(ctx, testProject, Memory{
			Category:   "gotcha",
			Content:    "user-pinned mcp memory",
			Source:     "mcp",
			Importance: 0.7,
			Tags:       []string{},
		})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if err := s.TogglePin(ctx, pinnedID, true); err != nil {
			t.Fatalf("TogglePin: %v", err)
		}

		replacement := []Memory{
			{Category: "fact", Content: "new consolidated fact", Importance: 0.6, Tags: []string{}},
		}
		if _, _, _, _, err := s.ReplaceNonManual(ctx, testProject, replacement, "", nil); err != nil {
			t.Fatalf("ReplaceNonManual: %v", err)
		}

		all, err := s.GetAll(ctx, testProject, 100)
		if err != nil {
			t.Fatalf("GetAll: %v", err)
		}

		var found *Memory
		for i := range all {
			if all[i].ID == pinnedID {
				found = &all[i]
			}
		}
		if found == nil {
			t.Fatalf("pinned memory %s was dropped by ReplaceNonManual", pinnedID)
		}
		if !found.Pinned {
			t.Error("pinned memory lost its pinned flag after ReplaceNonManual")
		}
	})
}

func TestStoreRestoreSnapshot(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	pinnedID, err := s.Create(ctx, testProject, Memory{
		Category:   "gotcha",
		Content:    "user-pinned mcp memory",
		Source:     "mcp",
		Importance: 0.7,
		Tags:       []string{},
	})
	if err != nil {
		t.Fatalf("Create pinned: %v", err)
	}
	if err := s.TogglePin(ctx, pinnedID, true); err != nil {
		t.Fatalf("TogglePin: %v", err)
	}

	if _, err := s.Create(ctx, testProject, Memory{
		Category:   "fact",
		Content:    "old reflection fact to be snapshotted",
		Source:     "reflection",
		Importance: 0.5,
		Tags:       []string{},
	}); err != nil {
		t.Fatalf("Create old: %v", err)
	}

	replacement := []Memory{
		{Category: "fact", Content: "new consolidated fact", Importance: 0.6, Tags: []string{}},
	}
	if _, _, _, _, err := s.ReplaceNonManual(ctx, testProject, replacement, "", nil); err != nil {
		t.Fatalf("ReplaceNonManual: %v", err)
	}

	if _, err := s.RestoreSnapshot(ctx, testProject); err != nil {
		t.Fatalf("RestoreSnapshot: %v", err)
	}

	all, err := s.GetAll(ctx, testProject, 100)
	if err != nil {
		t.Fatalf("GetAll: %v", err)
	}

	var found *Memory
	var foundOld bool
	for i := range all {
		if all[i].ID == pinnedID {
			found = &all[i]
		}
		if all[i].Content == "old reflection fact to be snapshotted" {
			foundOld = true
		}
	}
	if found == nil {
		t.Fatalf("pinned memory %s was dropped by RestoreSnapshot", pinnedID)
	}
	if !found.Pinned {
		t.Error("pinned memory lost its pinned flag after RestoreSnapshot")
	}
	if !foundOld {
		t.Error("snapshotted memory should have been restored")
	}
}

// TestStoreReplaceNonManual_PreservesResolved guards issue #318: a memory marked
// resolved by ghost resolve must survive a reflect --apply round trip untouched,
// not be deleted and not be resurrected as a fresh unresolved duplicate.
func TestStoreReplaceNonManual_PreservesResolved(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	resolvedID, err := s.Create(ctx, testProject, Memory{
		Category: "gotcha", Content: "changelog evidence, already resolved", Source: "reflection", Importance: 0.5,
	})
	if err != nil {
		t.Fatalf("create resolved candidate: %v", err)
	}
	if _, err := s.SetResolved(ctx, []string{resolvedID}); err != nil {
		t.Fatalf("SetResolved: %v", err)
	}

	if _, err := s.Create(ctx, testProject, Memory{
		Category: "fact", Content: "old reflection fact", Source: "reflection", Importance: 0.5,
	}); err != nil {
		t.Fatalf("create unresolved: %v", err)
	}

	replacement := []Memory{
		{Category: "fact", Content: "new consolidated fact", Importance: 0.6, Tags: []string{}},
	}
	if _, _, _, _, err := s.ReplaceNonManual(ctx, testProject, replacement, "", nil); err != nil {
		t.Fatalf("ReplaceNonManual: %v", err)
	}

	all, err := s.GetAll(ctx, testProject, 100)
	if err != nil {
		t.Fatalf("GetAll: %v", err)
	}

	var foundResolved, foundReplacement, foundOld bool
	for _, m := range all {
		if m.ID == resolvedID && m.ResolvedAt != nil {
			foundResolved = true
		}
		if m.Content == "new consolidated fact" {
			foundReplacement = true
		}
		if m.Content == "old reflection fact" {
			foundOld = true
		}
	}
	if !foundResolved {
		t.Fatal("resolved memory was dropped by ReplaceNonManual")
	}
	if !foundReplacement {
		t.Error("replacement memory should be present")
	}
	if foundOld {
		t.Error("old unresolved reflection fact should have been replaced")
	}
}

// liveRowCount counts the project's live rows, the number a caller comparing a
// round's retention against its own input has to agree with.
func liveRowCount(t *testing.T, s *Store) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(
		`SELECT count(*) FROM memories WHERE project_id = ? AND resolved_at IS NULL`, testProject).Scan(&n); err != nil {
		t.Fatalf("count live rows: %v", err)
	}
	return n
}

// projectRowCount counts every row the project holds, resolved ones included: a
// resolved row is still a row, and the retained count is about rows.
func projectRowCount(t *testing.T, s *Store) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT count(*) FROM memories WHERE project_id = ?`, testProject).Scan(&n); err != nil {
		t.Fatalf("count project rows: %v", err)
	}
	return n
}

// TestReplaceNonManualReportsTheRowsItLeavesLive: the count a caller needs
// after an apply is the rows the project still holds, not the proposals that
// survived a filter. Every refusal the store makes — a scope that disagrees, a
// decision-link overflow, a claim on a row that moved — writes no row of its
// own and takes none of its sources, so a refused N-source merge leaves N rows
// live and is not one retained memory. Retained is that count, and it is the
// store's own: the replaceable rows it kept live from the corpus it was handed,
// plus the rows it inserted.
//
// Every case asserts the count against the rows the project really holds, so a
// count that drifts from the corpus cannot pass.
func TestReplaceNonManualReportsTheRowsItLeavesLive(t *testing.T) {
	ctx := context.Background()

	t.Run("a merge refused for decision links keeps every source live", func(t *testing.T) {
		s := testStore(t)
		// Thirteen decisions, whose links joined exceed one source_ref, so the
		// merge that names them is refused as an overflow and writes nothing.
		var mems []string
		for i := 0; i < 13; i++ {
			_, mem, _, err := s.RecordDecision(ctx, testProject,
				fmt.Sprintf("Decision %d", i), fmt.Sprintf("choice %d", i), "because", nil, nil)
			if err != nil {
				t.Fatalf("RecordDecision %d: %v", i, err)
			}
			mems = append(mems, mem)
		}
		aID, err := s.Create(ctx, testProject, Memory{
			Category: "fact", Content: "the deploy pipeline requires a manual approval gate",
			Source: "mcp", Importance: 0.5, Tags: []string{},
		})
		if err != nil {
			t.Fatalf("Create A: %v", err)
		}
		bID, err := s.Create(ctx, testProject, Memory{
			Category: "fact", Content: "the deploy pipeline refuses an unreviewed release",
			Source: "mcp", Importance: 0.5, Tags: []string{},
		})
		if err != nil {
			t.Fatalf("Create B: %v", err)
		}
		fed := corpusRows(t, s, append(append([]string{}, mems...), aID, bID)...)

		_, dropped, _, _, err := s.ReplaceNonManual(ctx, testProject, []Memory{
			{Category: "decision", Content: "thirteen decisions in one", Importance: 0.9, ReplacesIDs: mems},
			{Category: "fact", Content: "the deploy pipeline needs an approval gate and refuses an unreviewed release",
				Importance: 0.6, ReplacesIDs: []string{aID, bID}},
		}, "", fed)
		if err != nil {
			t.Fatalf("ReplaceNonManual: %v", err)
		}
		if dropped.Overflow != 1 {
			t.Fatalf("dropped.Overflow = %d, want 1: the merge whose links do not fit is refused", dropped.Overflow)
		}
		// The thirteen sources stay live, and the other merge wrote one row.
		if dropped.Retained != 14 {
			t.Errorf("dropped.Retained = %d, want 14: the thirteen sources the refusal kept and the row the other merge wrote",
				dropped.Retained)
		}
		if got := liveRowCount(t, s); got != 14 {
			t.Errorf("the project holds %d live rows, want the 14 the count names", got)
		}
	})

	t.Run("a merge refused for a scope conflict keeps every source live", func(t *testing.T) {
		s := testStore(t)
		aID, err := s.Create(ctx, testProject, Memory{
			Category: "fact", Content: "the deploy host is alpha", Source: "mcp", Importance: 0.5,
			Tags: []string{}, Scope: map[string]string{"environment": "production"},
		})
		if err != nil {
			t.Fatalf("Create A: %v", err)
		}
		bID, err := s.Create(ctx, testProject, Memory{
			Category: "fact", Content: "the deploy host is bravo", Source: "mcp", Importance: 0.5,
			Tags: []string{}, Scope: map[string]string{"environment": "staging"},
		})
		if err != nil {
			t.Fatalf("Create B: %v", err)
		}
		cID, err := s.Create(ctx, testProject, Memory{
			Category: "fact", Content: "the deploy host is charlie", Source: "mcp", Importance: 0.5, Tags: []string{},
		})
		if err != nil {
			t.Fatalf("Create C: %v", err)
		}
		fed := corpusRows(t, s, aID, bID, cID)

		// The sources disagree on scope, so the merge is refused and each of
		// them stays live under its own scope.
		_, dropped, _, _, err := s.ReplaceNonManual(ctx, testProject, []Memory{{
			Category: "fact", Content: "the deploy hosts are one fact", Importance: 0.6,
			Tags: []string{}, ReplacesIDs: []string{aID, bID, cID},
		}}, "", fed)
		if err != nil {
			t.Fatalf("ReplaceNonManual: %v", err)
		}
		if dropped.Scope != 1 {
			t.Fatalf("dropped.Scope = %d, want 1: the merge's sources disagree on scope", dropped.Scope)
		}
		if dropped.Retained != 3 {
			t.Errorf("dropped.Retained = %d, want 3: every source the refusal kept is still live", dropped.Retained)
		}
		if got := liveRowCount(t, s); got != 3 {
			t.Errorf("the project holds %d live rows, want the 3 the count names", got)
		}
	})

	t.Run("a merge claiming an edited row keeps that source live", func(t *testing.T) {
		s := testStore(t)
		aID, err := s.Create(ctx, testProject, Memory{
			Category: "fact", Content: "the deploy host is alpha", Source: "mcp", Importance: 0.6, Tags: []string{},
		})
		if err != nil {
			t.Fatalf("Create A: %v", err)
		}
		bID, err := s.Create(ctx, testProject, Memory{
			Category: "fact", Content: "the deploy host is bravo", Source: "mcp", Importance: 0.6, Tags: []string{},
		})
		if err != nil {
			t.Fatalf("Create B: %v", err)
		}
		fed := corpusRows(t, s, aID, bID)
		// The edit lands while the harness is answering, so the merge names a
		// row that is no longer the one the pass read.
		edited := "the deploy host is alpha (edited)"
		if err := s.UpdateMemory(ctx, testProject, aID, &edited, nil, nil, nil); err != nil {
			t.Fatalf("UpdateMemory A: %v", err)
		}

		_, dropped, _, _, err := s.ReplaceNonManual(ctx, testProject, []Memory{{
			Category: "fact", Content: "the deploy host is alpha and bravo, one fact", Importance: 0.7,
			Tags: []string{}, ReplacesIDs: []string{aID, bID},
		}}, "", fed)
		if err != nil {
			t.Fatalf("ReplaceNonManual: %v", err)
		}
		if dropped.Changed != 1 {
			t.Fatalf("dropped.Changed = %d, want 1: the merge claimed a row that moved", dropped.Changed)
		}
		// Both sources are still live — the edited one and the one beside it —
		// and nothing was written, so the count is the two rows.
		if dropped.Retained != 2 {
			t.Errorf("dropped.Retained = %d, want 2: the edited source and the one beside it are both still live",
				dropped.Retained)
		}
		if got := liveRowCount(t, s); got != 2 {
			t.Errorf("the project holds %d live rows, want the 2 the count names", got)
		}
	})

	t.Run("a corpus row pinned mid-round is still a row the project holds", func(t *testing.T) {
		s := testStore(t)
		aID, err := s.Create(ctx, testProject, Memory{
			Category: "fact", Content: "the deploy host is alpha", Source: "mcp", Importance: 0.6, Tags: []string{},
		})
		if err != nil {
			t.Fatalf("Create A: %v", err)
		}
		bID, err := s.Create(ctx, testProject, Memory{
			Category: "fact", Content: "the deploy host is bravo", Source: "mcp", Importance: 0.6, Tags: []string{},
		})
		if err != nil {
			t.Fatalf("Create B: %v", err)
		}
		cID, err := s.Create(ctx, testProject, Memory{
			Category: "fact", Content: "the deploy host is charlie", Source: "mcp", Importance: 0.6, Tags: []string{},
		})
		if err != nil {
			t.Fatalf("Create C: %v", err)
		}
		fed := corpusRows(t, s, aID, bID, cID)
		// The pin lands while the harness is answering. A is no longer a
		// replaceable candidate, so the replace never reaches it: it is in
		// neither the delete set nor the set of candidates the pass keeps live.
		// It is still a row the project holds.
		if err := s.TogglePin(ctx, aID, true); err != nil {
			t.Fatalf("TogglePin A: %v", err)
		}

		_, dropped, _, _, err := s.ReplaceNonManual(ctx, testProject, []Memory{{
			Category: "fact", Content: "the deploy hosts are one fact", Importance: 0.7,
			Tags: []string{}, ReplacesIDs: []string{bID, cID},
		}}, "", fed)
		if err != nil {
			t.Fatalf("ReplaceNonManual: %v", err)
		}
		if dropped.Total() != 0 {
			t.Fatalf("dropped.Total() = %d, want 0: the merge named two rows that were there", dropped.Total())
		}
		if dropped.Retained != 2 {
			t.Errorf("dropped.Retained = %d, want 2: the pinned row is still there beside the row the merge wrote",
				dropped.Retained)
		}
		if got := liveRowCount(t, s); got != 2 {
			t.Errorf("the project holds %d live rows, want the 2 the count names", got)
		}
	})

	t.Run("a corpus row resolved mid-round is still a row the project holds", func(t *testing.T) {
		s := testStore(t)
		aID, err := s.Create(ctx, testProject, Memory{
			Category: "fact", Content: "the deploy host is alpha", Source: "mcp", Importance: 0.6, Tags: []string{},
		})
		if err != nil {
			t.Fatalf("Create A: %v", err)
		}
		bID, err := s.Create(ctx, testProject, Memory{
			Category: "fact", Content: "the deploy host is bravo", Source: "mcp", Importance: 0.6, Tags: []string{},
		})
		if err != nil {
			t.Fatalf("Create B: %v", err)
		}
		cID, err := s.Create(ctx, testProject, Memory{
			Category: "fact", Content: "the deploy host is charlie", Source: "mcp", Importance: 0.6, Tags: []string{},
		})
		if err != nil {
			t.Fatalf("Create C: %v", err)
		}
		fed := corpusRows(t, s, aID, bID, cID)
		// The resolve lands while the harness is answering. A is resolved, so
		// it is not a replaceable candidate either — and it is still a row of
		// the project.
		if _, err := s.SetResolved(ctx, []string{aID}); err != nil {
			t.Fatalf("SetResolved A: %v", err)
		}

		_, dropped, _, _, err := s.ReplaceNonManual(ctx, testProject, []Memory{{
			Category: "fact", Content: "the deploy hosts are one fact", Importance: 0.7,
			Tags: []string{}, ReplacesIDs: []string{bID, cID},
		}}, "", fed)
		if err != nil {
			t.Fatalf("ReplaceNonManual: %v", err)
		}
		if dropped.Total() != 0 {
			t.Fatalf("dropped.Total() = %d, want 0: the merge named two rows that were there", dropped.Total())
		}
		if dropped.Retained != 2 {
			t.Errorf("dropped.Retained = %d, want 2: the resolved row is still there beside the row the merge wrote",
				dropped.Retained)
		}
		if got := projectRowCount(t, s); got != 2 {
			t.Errorf("the project holds %d rows, want the 2 the count names", got)
		}
	})

	t.Run("a plain successful merge counts the rows the project holds", func(t *testing.T) {
		s := testStore(t)
		aID, err := s.Create(ctx, testProject, Memory{
			Category: "fact", Content: "the queue consumer drains the backlog in one pass",
			Source: "mcp", Importance: 0.5, Tags: []string{},
		})
		if err != nil {
			t.Fatalf("Create A: %v", err)
		}
		bID, err := s.Create(ctx, testProject, Memory{
			Category: "fact", Content: "the queue consumer retries a failed delivery",
			Source: "mcp", Importance: 0.5, Tags: []string{},
		})
		if err != nil {
			t.Fatalf("Create B: %v", err)
		}
		cID, err := s.Create(ctx, testProject, Memory{
			Category: "fact", Content: "the deploy pipeline requires a manual approval gate",
			Source: "mcp", Importance: 0.5, Tags: []string{},
		})
		if err != nil {
			t.Fatalf("Create C: %v", err)
		}
		fed := corpusRows(t, s, aID, bID, cID)

		_, dropped, _, _, err := s.ReplaceNonManual(ctx, testProject, []Memory{
			{Category: "fact", Content: "the queue consumer drains the backlog in one pass and retries a failed delivery",
				Importance: 0.6, Tags: []string{}, ReplacesIDs: []string{aID, bID}},
			{Category: "fact", Content: "the deploy pipeline requires a manual approval gate",
				Importance: 0.5, Tags: []string{}},
		}, "", fed)
		if err != nil {
			t.Fatalf("ReplaceNonManual: %v", err)
		}
		if dropped.Total() != 0 {
			t.Fatalf("dropped.Total() = %d, want 0: nothing was refused", dropped.Total())
		}
		// The untouched row of the corpus is still live beside the row the
		// merge wrote, and those two are all the project holds.
		if dropped.Retained != 2 {
			t.Errorf("dropped.Retained = %d, want 2: the row the merge left untouched and the row it wrote",
				dropped.Retained)
		}
		if got := liveRowCount(t, s); got != 2 {
			t.Errorf("the project holds %d live rows, want the 2 the count names", got)
		}
		if getOne(t, s, cID).Content != "the deploy pipeline requires a manual approval gate" {
			t.Errorf("the untouched row was not kept as it is")
		}
	})

	t.Run("an apply with no project half reports no project retention", func(t *testing.T) {
		s := testStore(t)
		aID, err := s.Create(ctx, testProject, Memory{
			Category: "fact", Content: "the deploy host is alpha", Source: "mcp", Importance: 0.6, Tags: []string{},
		})
		if err != nil {
			t.Fatalf("Create A: %v", err)
		}
		fed := corpusRows(t, s, aID)

		// A cross-project candidate alone: no project half, so no replace runs
		// and the store reports no project retention at all.
		_, promoted, kept, projectDropped, globalDropped, _, _, err := s.ApplyReflection(ctx, testProject, nil,
			[]Memory{{
				Category: "fact", Content: "the deploy host is alpha, everywhere", Importance: 0.6,
				Tags: []string{}, ReplacesIDs: []string{aID},
			}}, "", true, fed)
		if err != nil {
			t.Fatalf("ApplyReflection: %v", err)
		}
		if promoted != 1 || len(kept) != 0 {
			t.Errorf("promoted = %d, kept = %d, want the candidate promoted and none kept back", promoted, len(kept))
		}
		if projectDropped.Total() != 0 || globalDropped.Total() != 0 {
			t.Errorf("dropped = %d/%d, want no refusal on either half", projectDropped.Total(), globalDropped.Total())
		}
		// Zero here means "no project replace ran", not "nothing was retained":
		// the project still holds the row the global-only apply consumed
		// nothing from.
		if projectDropped.Retained != 0 {
			t.Errorf("projectDropped.Retained = %d, want 0: no project replace ran", projectDropped.Retained)
		}
		if got := liveRowCount(t, s); got != 1 {
			t.Errorf("the project holds %d live rows, want the 1 it held: a global-only apply consumes nothing", got)
		}
	})
}

// TestStoreRestoreSnapshot_PreservesResolved guards the restore leg of #318:
// --restore must not delete a resolved memory that reflection never touched.
func TestStoreRestoreSnapshot_PreservesResolved(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	resolvedID, err := s.Create(ctx, testProject, Memory{
		Category: "gotcha", Content: "resolved evidence stays resolved", Source: "reflection", Importance: 0.5,
	})
	if err != nil {
		t.Fatalf("create resolved: %v", err)
	}
	if _, err := s.SetResolved(ctx, []string{resolvedID}); err != nil {
		t.Fatalf("SetResolved: %v", err)
	}

	if _, err := s.Create(ctx, testProject, Memory{
		Category: "fact", Content: "old reflection fact to be snapshotted", Source: "reflection", Importance: 0.5,
	}); err != nil {
		t.Fatalf("create old: %v", err)
	}

	replacement := []Memory{
		{Category: "fact", Content: "new consolidated fact", Importance: 0.6, Tags: []string{}},
	}
	if _, _, _, _, err := s.ReplaceNonManual(ctx, testProject, replacement, "", nil); err != nil {
		t.Fatalf("ReplaceNonManual: %v", err)
	}
	if _, err := s.RestoreSnapshot(ctx, testProject); err != nil {
		t.Fatalf("RestoreSnapshot: %v", err)
	}

	all, err := s.GetAll(ctx, testProject, 100)
	if err != nil {
		t.Fatalf("GetAll: %v", err)
	}

	var foundResolved bool
	for _, m := range all {
		if m.ID == resolvedID && m.ResolvedAt != nil {
			foundResolved = true
		}
	}
	if !foundResolved {
		t.Fatal("resolved memory was dropped by RestoreSnapshot")
	}
}

func TestStoreSearchFTS(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	// Insert several memories with distinct content.
	contents := []string{
		"Kubernetes pods run containers",
		"Go channels enable concurrency",
		"SQLite is an embedded database engine",
	}
	for _, c := range contents {
		_, err := s.Create(ctx, testProject, Memory{
			Category:   "fact",
			Content:    c,
			Source:     "reflection",
			Importance: 0.5,
			Tags:       []string{},
		})
		if err != nil {
			t.Fatalf("Create(%q): %v", c, err)
		}
	}

	// Search for "Kubernetes".
	results, err := s.SearchFTS(ctx, testProject, "Kubernetes", 10)
	if err != nil {
		t.Fatalf("SearchFTS: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("expected at least 1 result for 'Kubernetes'")
	}
	if !strings.Contains(results[0].Content, "Kubernetes") {
		t.Errorf("expected Kubernetes result, got %q", results[0].Content)
	}

	// Search for "concurrency" — should find the Go channels memory.
	results, err = s.SearchFTS(ctx, testProject, "concurrency", 10)
	if err != nil {
		t.Fatalf("SearchFTS concurrency: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("expected at least 1 result for 'concurrency'")
	}
	if !strings.Contains(results[0].Content, "concurrency") {
		t.Errorf("expected concurrency result, got %q", results[0].Content)
	}

	// Search for something that doesn't exist.
	results, err = s.SearchFTS(ctx, testProject, "blockchain", 10)
	if err != nil {
		t.Fatalf("SearchFTS no results: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("expected 0 results for 'blockchain', got %d", len(results))
	}
}

func TestStoreSearchFTSSpecialCharacters(t *testing.T) {
	// Verify that FTS5 operators in content don't break queries.
	s := testStore(t)
	ctx := context.Background()

	_, err := s.Create(ctx, testProject, Memory{
		Category:   "gotcha",
		Content:    "NEAR AND OR NOT are FTS5 operators that need quoting",
		Source:     "manual",
		Importance: 0.5,
		Tags:       []string{},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// sanitizeFTS is called internally by Upsert. Test via SearchFTS with operator-laden input.
	// This should not error — special chars should be handled.
	results, err := s.SearchFTS(ctx, testProject, "operators", 10)
	if err != nil {
		t.Fatalf("SearchFTS with special chars: %v", err)
	}
	if len(results) == 0 {
		t.Error("expected results for 'operators' query")
	}
}

// TestSearchFTSRespectsLimit: SearchFTS must never return more than the
// requested limit, even when the per-bucket statement fetches limit from each
// bucket (#1771). This test passes on origin/main (it guards the new trim).
func TestSearchFTSRespectsLimit(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if err := s.EnsureProject(ctx, "_global", "_global", "global"); err != nil {
		t.Fatalf("EnsureProject(_global): %v", err)
	}
	for i := 0; i < 15; i++ {
		if _, err := s.Create(ctx, "_global", Memory{
			Category:   "preference",
			Content:    fmt.Sprintf("shared note about helm service %d", i),
			Source:     "manual",
			Importance: 0.7,
		}); err != nil {
			t.Fatalf("Create global %d: %v", i, err)
		}
	}
	for i := 0; i < 5; i++ {
		if _, err := s.Create(ctx, testProject, Memory{
			Category:   "gotcha",
			Content:    fmt.Sprintf("project helm release %d", i),
			Source:     "manual",
			Importance: 0.7,
		}); err != nil {
			t.Fatalf("Create project %d: %v", i, err)
		}
	}

	results, err := s.SearchFTS(ctx, testProject, "helm", 10)
	if err != nil {
		t.Fatalf("SearchFTS: %v", err)
	}
	if len(results) > 10 {
		t.Fatalf("SearchFTS returned %d rows, want <= 10", len(results))
	}
}

func TestSanitizeFTS(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string // expected output
	}{
		{
			name:  "plain words",
			input: "hello world",
			want:  `"hello" OR "world"`,
		},
		{
			name:  "keeps single-char words",
			input: "a Go is great",
			want:  `"a" OR "Go" OR "is" OR "great"`,
		},
		{
			name:  "strips punctuation from edges",
			input: "(hello) [world]!",
			want:  `"hello" OR "world"`,
		},
		{
			name:  "strips FTS operators",
			input: "NEAR AND OR NOT test",
			want:  `"NEAR" OR "AND" OR "OR" OR "NOT" OR "test"`,
		},
		{
			name:  "empty input",
			input: "",
			want:  `""`,
		},
		{
			name:  "only punctuation",
			input: "* + - !",
			want:  `""`,
		},
		{
			name:  "escapes mid-token quote instead of leaving an unbalanced literal",
			input: `fo"o`,
			want:  `"fo""o"`,
		},
		{
			name:  "escapes mid-token quotes while preserving exact identifiers",
			input: `evil"NEAR"foo 192.168.9.150 sealed-secrets`,
			want:  `"evil""NEAR""foo" OR "192.168.9.150" OR "sealed-secrets"`,
		},
		{
			name:  "limits to 10 words",
			input: "one two three four five six seven eight nine ten eleven twelve",
			want:  `"one" OR "two" OR "three" OR "four" OR "five" OR "six" OR "seven" OR "eight" OR "nine" OR "ten"`,
		},
		{
			name:  "preserves a trailing prefix star outside the quotes",
			input: "sqlite* helm",
			want:  `"sqlite"* OR "helm"`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := sanitizeFTS(tc.input, nil)
			if got != tc.want {
				t.Errorf("sanitizeFTS(%q)\n  got:  %s\n  want: %s", tc.input, got, tc.want)
			}
		})
	}
}

// splitFTSTERMs splits a sanitizer result into its emitted terms, unescaped
// and with any prefix star stripped. Test inputs never embed a literal " OR "
// inside a term, so splitting on it is safe.
func splitFTSTERMs(t *testing.T, got string) []string {
	t.Helper()
	if got == `""` {
		return nil
	}
	parts := strings.Split(got, " OR ")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if len(p) < 2 || !strings.HasPrefix(p, `"`) {
			t.Fatalf("malformed term %q in %q", p, got)
		}
		p = strings.TrimSuffix(p, "*")
		p = strings.Trim(p, `"`)
		out = append(out, strings.ReplaceAll(p, `""`, `"`))
	}
	return out
}

// assertOriginalOrder checks that every emitted term occurs in the query and
// that the emitted terms appear in original query order.
func assertOriginalOrder(t *testing.T, query string, terms []string) {
	t.Helper()
	fields := strings.Fields(query)
	idx := make(map[string]int, len(fields))
	for i, w := range fields {
		if _, ok := idx[w]; !ok {
			idx[w] = i
		}
	}
	last := -1
	for _, term := range terms {
		i, ok := idx[term]
		if !ok {
			t.Fatalf("emitted term %q is not part of query %q", term, query)
		}
		if i <= last {
			t.Errorf("term %q emitted out of original order (position %d after %d) in %v", term, i, last, terms)
		}
		last = i
	}
}

// TestSanitizeFTSN_SelectsTailTerms is the core term-selection regression:
// a natural-language query's tail carries its most specific terms, and
// positional truncation used to discard them before they ever reached FTS.
// The query below is 13 words; the old first-10 truncation dropped
// "ghost_windows_arm64 sha256 digest" entirely. Selection must keep the tail,
// emit exactly ftsSearchWordLimit terms, and preserve original query order.
func TestSanitizeFTSN_SelectsTailTerms(t *testing.T) {
	const query = "please can you tell me exactly how to pin the ghost_windows_arm64 sha256 digest"
	got := sanitizeFTS(query, nil)
	terms := splitFTSTERMs(t, got)
	t.Logf("sanitizeFTS(%q) = %s", query, got)

	if len(terms) != ftsSearchWordLimit {
		t.Errorf("emitted %d terms, want exactly %d", len(terms), ftsSearchWordLimit)
	}
	for _, tail := range []string{"ghost_windows_arm64", "sha256", "digest"} {
		found := false
		for _, term := range terms {
			if term == tail {
				found = true
			}
		}
		if !found {
			t.Errorf("tail term %q missing from emitted terms %v — positional truncation discarded the query tail", tail, terms)
		}
	}
	assertOriginalOrder(t, query, terms)
}

func TestStoreResolveProject_ByID(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	id, name, err := s.ResolveProject(ctx, testProject)
	if err != nil {
		t.Fatalf("ResolveProject: %v", err)
	}
	if id != testProject || name != "test" {
		t.Errorf("got id=%q name=%q, want id=%q name=%q", id, name, testProject, "test")
	}
}

func TestStoreResolveProject_NoMatch(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	id, name, err := s.ResolveProject(ctx, "nonexistent")
	if err != nil {
		t.Fatalf("ResolveProject: %v", err)
	}
	if id != "" || name != "" {
		t.Errorf("expected empty on no match, got id=%q name=%q", id, name)
	}
}

// TestStoreResolveProject_WindowsBackslashPath pins the native-Windows
// resolution path: a stored and input path that use backslashes must resolve
// (exact path, prefix, and an unmatched path must fall through to ("", "", nil),
// not an error), because the old branch gate checked only for '/' and silently
// skipped every backslash path — the bug PR #510's Windows CI legs caught.
func TestStoreResolveProject_WindowsBackslashPath(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	root := t.TempDir()
	exact := filepath.Join(root, "exact")
	repo := filepath.Join(root, "repo")
	child := filepath.Join(repo, "sub", "deeper")
	for _, dir := range []string{exact, child} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	toBackslashes := func(path string) string {
		return strings.ReplaceAll(path, string(filepath.Separator), `\`)
	}
	exactWin := toBackslashes(exact)
	repoWin := toBackslashes(repo)
	childWin := toBackslashes(child)

	if err := s.EnsureProject(ctx, "win1", exactWin, "win-exact"); err != nil {
		t.Fatalf("EnsureProject win1: %v", err)
	}
	id, name, err := s.ResolveProject(ctx, exactWin)
	if err != nil {
		t.Fatalf("ResolveProject exact: %v", err)
	}
	if id != "win1" || name != "win-exact" {
		t.Errorf("exact backslash path: got id=%q name=%q, want id=%q name=%q", id, name, "win1", "win-exact")
	}

	if err := s.EnsureProject(ctx, "win2", repoWin, "win-prefix"); err != nil {
		t.Fatalf("EnsureProject win2: %v", err)
	}
	id, name, err = s.ResolveProject(ctx, childWin)
	if err != nil {
		t.Fatalf("ResolveProject prefix: %v", err)
	}
	if id != "win2" || name != "win-prefix" {
		t.Errorf("backslash prefix: got id=%q name=%q, want id=%q name=%q", id, name, "win2", "win-prefix")
	}

	id, name, err = s.ResolveProject(ctx, toBackslashes(filepath.Join(root, "nowhere", "else")))
	if err != nil {
		t.Fatalf("ResolveProject no match: %v", err)
	}
	if id != "" || name != "" {
		t.Errorf("unmatched backslash path: got id=%q name=%q, want empty", id, name)
	}
}

func TestStoreLearnedContext(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	// Initially empty.
	lc, err := s.GetLearnedContext(ctx, testProject)
	if err != nil {
		t.Fatalf("GetLearnedContext: %v", err)
	}
	if lc != "" {
		t.Errorf("expected empty learned context, got %q", lc)
	}

	// Update it.
	if err := s.UpdateLearnedContext(ctx, testProject, "Go project with SQLite", "consolidated summary"); err != nil {
		t.Fatalf("UpdateLearnedContext: %v", err)
	}

	lc, err = s.GetLearnedContext(ctx, testProject)
	if err != nil {
		t.Fatalf("GetLearnedContext after update: %v", err)
	}
	if lc != "Go project with SQLite" {
		t.Errorf("expected 'Go project with SQLite', got %q", lc)
	}

	// Non-existent project returns empty, no error.
	lc, err = s.GetLearnedContext(ctx, "nonexistent")
	if err != nil {
		t.Fatalf("GetLearnedContext nonexistent: %v", err)
	}
	if lc != "" {
		t.Errorf("expected empty for nonexistent, got %q", lc)
	}
}

func TestStoreDecisions(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	// Record a decision.
	id, memID, _, err := s.RecordDecision(ctx, testProject,
		"Use SQLite for storage",
		"SQLite provides embedded persistence with FTS5",
		"Simple, no external dependencies",
		[]string{"PostgreSQL", "BoltDB"},
		[]string{"storage", "database"},
	)
	if err != nil {
		t.Fatalf("RecordDecision: %v", err)
	}
	if id == "" {
		t.Fatal("RecordDecision returned empty ID")
	}
	if memID == "" || memID == id {
		t.Fatalf("RecordDecision returned invalid memory ID: %q (decision ID: %q)", memID, id)
	}

	// List decisions.
	decisions, err := s.ListDecisions(ctx, testProject, "", 10)
	if err != nil {
		t.Fatalf("ListDecisions: %v", err)
	}
	if len(decisions) != 1 {
		t.Fatalf("expected 1 decision, got %d", len(decisions))
	}

	d := decisions[0]
	if d.Title != "Use SQLite for storage" {
		t.Errorf("expected title 'Use SQLite for storage', got %q", d.Title)
	}
	if d.Decision != "SQLite provides embedded persistence with FTS5" {
		t.Errorf("unexpected decision text: %q", d.Decision)
	}
	if d.Rationale != "Simple, no external dependencies" {
		t.Errorf("unexpected rationale: %q", d.Rationale)
	}
	if len(d.Alternatives) != 2 || d.Alternatives[0] != "PostgreSQL" {
		t.Errorf("unexpected alternatives: %v", d.Alternatives)
	}
	if len(d.Tags) != 2 || d.Tags[0] != "storage" {
		t.Errorf("unexpected tags: %v", d.Tags)
	}
	if d.Status != "active" {
		t.Errorf("expected status 'active', got %q", d.Status)
	}

	// Filter by status.
	active, err := s.ListDecisions(ctx, testProject, "active", 10)
	if err != nil {
		t.Fatalf("ListDecisions active: %v", err)
	}
	if len(active) != 1 {
		t.Errorf("expected 1 active decision, got %d", len(active))
	}

	superseded, err := s.ListDecisions(ctx, testProject, "superseded", 10)
	if err != nil {
		t.Fatalf("ListDecisions superseded: %v", err)
	}
	if len(superseded) != 0 {
		t.Errorf("expected 0 superseded decisions, got %d", len(superseded))
	}

	// RecordDecision also creates a memory — verify.
	results, err := s.SearchFTS(ctx, testProject, "SQLite storage", 10)
	if err != nil {
		t.Fatalf("SearchFTS decision memory: %v", err)
	}
	foundDecisionMemory := false
	for _, r := range results {
		if r.Category == "decision" && strings.Contains(r.Content, "Use SQLite for storage") {
			foundDecisionMemory = true
		}
	}
	if !foundDecisionMemory {
		t.Error("RecordDecision should also create a decision-category memory")
	}
}

// TestSupersedeDecision covers the eval finding that a reversed decision kept
// reporting status "active" and could head ghost_decisions_list ahead of the
// decision that replaced it. The status/superseded_by columns existed but had
// no writer, and ListDecisions ordered purely by created_at DESC.
func TestSupersedeDecision(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	oldID, _, _, err := s.RecordDecision(ctx, testProject,
		"Use Redis for the job queue", "Redis lists as the queue backend",
		"Already deployed for caching", nil, nil)
	if err != nil {
		t.Fatalf("RecordDecision (old): %v", err)
	}
	newID, _, _, err := s.RecordDecision(ctx, testProject,
		"Reverse: use Postgres for the job queue", "SKIP LOCKED on Postgres",
		"Redis lost jobs on failover", nil, nil)
	if err != nil {
		t.Fatalf("RecordDecision (new): %v", err)
	}

	if err := s.SupersedeDecision(ctx, testProject, oldID, newID); err != nil {
		t.Fatalf("SupersedeDecision: %v", err)
	}

	all, err := s.ListDecisions(ctx, testProject, "", 10)
	if err != nil {
		t.Fatalf("ListDecisions: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("expected 2 decisions, got %d", len(all))
	}
	// The live decision must come first, and the reversed one must report its
	// real status and point at its replacement.
	if all[0].ID != newID {
		t.Errorf("live decision should rank first, got %s", all[0].ID)
	}
	if all[1].ID != oldID {
		t.Fatalf("superseded decision should rank last, got %s", all[1].ID)
	}
	if all[1].Status != "superseded" {
		t.Errorf("reversed decision status = %q, want superseded", all[1].Status)
	}
	if all[1].SupersededBy != newID {
		t.Errorf("superseded_by = %q, want %s", all[1].SupersededBy, newID)
	}
	if all[0].Status != "active" {
		t.Errorf("replacement status = %q, want active", all[0].Status)
	}

	// Status filters now actually partition.
	active, err := s.ListDecisions(ctx, testProject, "active", 10)
	if err != nil {
		t.Fatalf("ListDecisions active: %v", err)
	}
	if len(active) != 1 || active[0].ID != newID {
		t.Errorf("expected only the replacement to be active, got %v", active)
	}

	// Re-running repoints rather than failing.
	if err := s.SupersedeDecision(ctx, testProject, oldID, newID); err != nil {
		t.Errorf("re-running SupersedeDecision should be safe: %v", err)
	}
}

func TestStoreTasks(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	// Create tasks.
	id1, err := s.CreateTask(ctx, testProject, "Fix bug", "Null pointer in handler", 1)
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	if id1 == "" {
		t.Fatal("CreateTask returned empty ID")
	}

	id2, err := s.CreateTask(ctx, testProject, "Add feature", "New memory search", 2)
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	// List all tasks.
	tasks, err := s.ListTasks(ctx, testProject, "", 10)
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if len(tasks) != 2 {
		t.Fatalf("expected 2 tasks, got %d", len(tasks))
	}
	// Ordered by priority ASC — priority 1 first.
	if tasks[0].Title != "Fix bug" {
		t.Errorf("expected 'Fix bug' first (priority 1), got %q", tasks[0].Title)
	}

	// Filter by status — both should be "pending" (default).
	pending, err := s.ListTasks(ctx, testProject, "pending", 10)
	if err != nil {
		t.Fatalf("ListTasks pending: %v", err)
	}
	if len(pending) != 2 {
		t.Errorf("expected 2 pending tasks, got %d", len(pending))
	}

	// Complete a task.
	if _, err := s.CompleteTask(ctx, id1, "Fixed in PR #42"); err != nil {
		t.Fatalf("CompleteTask: %v", err)
	}

	done, err := s.ListTasks(ctx, testProject, "done", 10)
	if err != nil {
		t.Fatalf("ListTasks done: %v", err)
	}
	if len(done) != 1 {
		t.Fatalf("expected 1 done task, got %d", len(done))
	}
	if done[0].Notes != "Fixed in PR #42" {
		t.Errorf("expected notes 'Fixed in PR #42', got %q", done[0].Notes)
	}
	if done[0].CompletedAt == "" {
		t.Error("expected completed_at to be set")
	}

	// Update a task.
	updStatus, updPriority, updDesc := "active", 1, "Updated description"
	if _, err := s.UpdateTask(ctx, id2, &updStatus, &updPriority, &updDesc); err != nil {
		t.Fatalf("UpdateTask: %v", err)
	}

	active, err := s.ListTasks(ctx, testProject, "active", 10)
	if err != nil {
		t.Fatalf("ListTasks active: %v", err)
	}
	if len(active) != 1 {
		t.Fatalf("expected 1 active task, got %d", len(active))
	}
	if active[0].Description != "Updated description" {
		t.Errorf("expected updated description, got %q", active[0].Description)
	}
}

func TestUpdateTaskTracksCompletedAt(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	id, err := s.CreateTask(ctx, testProject, "Track completion", "", 2)
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	// completedAt reads the stored column verbatim, NULL included, so a cleared
	// stamp is distinguishable from one that was never written.
	completedAt := func() sql.NullString {
		t.Helper()
		var v sql.NullString
		if err := s.db.QueryRowContext(ctx,
			`SELECT completed_at FROM tasks WHERE id = ?`, id).Scan(&v); err != nil {
			t.Fatalf("read completed_at: %v", err)
		}
		return v
	}

	// A pending task has no completion stamp.
	if v := completedAt(); v.Valid {
		t.Fatalf("a new task should have a NULL completed_at, got %q", v.String)
	}

	// pending -> done stamps it, on the stored row and on the returned value.
	done := "done"
	updated, err := s.UpdateTask(ctx, id, &done, nil, nil)
	if err != nil {
		t.Fatalf("UpdateTask pending->done: %v", err)
	}
	if updated.CompletedAt == "" {
		t.Error("UpdateTask pending->done returned an empty CompletedAt")
	}
	if v := completedAt(); !v.Valid || v.String == "" {
		t.Errorf("UpdateTask pending->done should store a completion stamp, got %+v", v)
	}
	got, err := s.GetTask(ctx, id)
	if err != nil {
		t.Fatalf("GetTask after done: %v", err)
	}
	if got.CompletedAt != updated.CompletedAt {
		t.Errorf("returned CompletedAt %q disagrees with the stored %q", updated.CompletedAt, got.CompletedAt)
	}

	// done -> active clears it, on the stored row and on the returned value.
	active := "active"
	updated, err = s.UpdateTask(ctx, id, &active, nil, nil)
	if err != nil {
		t.Fatalf("UpdateTask done->active: %v", err)
	}
	if updated.CompletedAt != "" {
		t.Errorf("UpdateTask done->active returned CompletedAt %q, want empty", updated.CompletedAt)
	}
	if v := completedAt(); v.Valid {
		t.Errorf("UpdateTask done->active should NULL completed_at, got %q", v.String)
	}

	// A non-status edit leaves a pending task's stamp empty.
	priority := 1
	updated, err = s.UpdateTask(ctx, id, nil, &priority, nil)
	if err != nil {
		t.Fatalf("UpdateTask priority only: %v", err)
	}
	if updated.CompletedAt != "" {
		t.Errorf("a non-status edit returned CompletedAt %q, want empty", updated.CompletedAt)
	}

	// done -> done, and a status-nil edit while done, must PRESERVE an existing
	// stamp rather than re-stamp it: the stamp records when the task BECAME
	// done, not when it was last written.
	seed := "2000-01-01 00:00:00"
	if _, err := s.db.ExecContext(ctx,
		`UPDATE tasks SET status = 'done', completed_at = ? WHERE id = ?`, seed, id); err != nil {
		t.Fatalf("seed completed_at: %v", err)
	}
	for _, status := range []*string{&done, nil} {
		updated, err = s.UpdateTask(ctx, id, status, nil, nil)
		if err != nil {
			t.Fatalf("UpdateTask preserving done: %v", err)
		}
		if updated.CompletedAt != seed {
			t.Errorf("UpdateTask re-stamped an already-done task: got %q, want preserved %q", updated.CompletedAt, seed)
		}
		if v := completedAt(); !v.Valid || v.String != seed {
			t.Errorf("stored completed_at moved on an already-done task: got %+v, want %q", v, seed)
		}
	}
}

// TestMergeProject_ReassignsSnapshots pins the regression: memory_snapshots
// .project_id is ON DELETE CASCADE, so a merge that reassigns every other child
// table but not snapshots silently destroys the merged project's undo history
// when the old project row is deleted.
func TestMergeProject_ReassignsSnapshots(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if err := s.EnsureProject(ctx, "test", "test", "dup-project"); err != nil {
		t.Fatalf("EnsureProject dup: %v", err)
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO memory_snapshots (snapshot_id, project_id, category, content, importance, source, tags)
		 VALUES ('snap-1', 'test', 'fact', 'c', 0.7, 'reflection', '[]')`); err != nil {
		t.Fatalf("seed snapshot: %v", err)
	}

	if err := s.MergeProject(ctx, "test", testProject); err != nil {
		t.Fatalf("MergeProject: %v", err)
	}

	var pid string
	if err := s.db.QueryRowContext(ctx,
		`SELECT project_id FROM memory_snapshots WHERE snapshot_id = 'snap-1'`).Scan(&pid); err != nil {
		t.Fatalf("snapshot lost by merge: %v", err)
	}
	if pid != testProject {
		t.Errorf("snapshot project_id = %q, want %q", pid, testProject)
	}
}

// seedFullProject creates four memories, four memory-link pairs, two tasks,
// one decision (which also creates its own linked memory row,
// source='decision_log', bringing the memory total to 5), three token_usage
// rows, and six audit_log rows for projectID — a row in every table
// DeleteProject must account for, with every field's final count (memories=5,
// memory_links=4, tasks=2, decisions=1, token_usage=3, audit_log=6) distinct
// from every other field's, so a test that transposes any pair of scan
// targets in DeleteProject's count query fails instead of passing silently.
// Returns the first two linked memory IDs.
func seedFullProject(t *testing.T, s *Store, ctx context.Context, projectID string) (mem1, mem2 string) {
	t.Helper()

	var err error
	mem1, err = s.Create(ctx, projectID, Memory{
		Category: "fact", Content: "seed memory one", Source: "manual", Importance: 0.5, Tags: []string{},
	})
	if err != nil {
		t.Fatalf("seed mem1: %v", err)
	}
	mem2, err = s.Create(ctx, projectID, Memory{
		Category: "fact", Content: "seed memory two", Source: "manual", Importance: 0.5, Tags: []string{},
	})
	if err != nil {
		t.Fatalf("seed mem2: %v", err)
	}
	mem3, err := s.Create(ctx, projectID, Memory{
		Category: "fact", Content: "seed memory three", Source: "manual", Importance: 0.5, Tags: []string{},
	})
	if err != nil {
		t.Fatalf("seed mem3: %v", err)
	}
	mem4, err := s.Create(ctx, projectID, Memory{
		Category: "fact", Content: "seed memory four", Source: "manual", Importance: 0.5, Tags: []string{},
	})
	if err != nil {
		t.Fatalf("seed mem4: %v", err)
	}
	for _, pair := range [][2]string{{mem1, mem2}, {mem1, mem3}, {mem1, mem4}, {mem2, mem3}} {
		if err := s.CreateLink(ctx, pair[0], pair[1], "related", 0.8, "auto"); err != nil {
			t.Fatalf("seed link %v: %v", pair, err)
		}
	}
	if _, err := s.CreateTask(ctx, projectID, "seed task one", "desc", 1); err != nil {
		t.Fatalf("seed task one: %v", err)
	}
	if _, err := s.CreateTask(ctx, projectID, "seed task two", "desc", 1); err != nil {
		t.Fatalf("seed task two: %v", err)
	}
	if _, _, _, err := s.RecordDecision(ctx, projectID, "seed decision", "did the thing", "because", nil, nil); err != nil {
		t.Fatalf("seed decision: %v", err)
	}
	for i := 0; i < 6; i++ {
		if _, err := s.db.ExecContext(ctx,
			`INSERT INTO audit_log (action, project_id) VALUES ('test-action', ?)`, projectID,
		); err != nil {
			t.Fatalf("seed audit_log %d: %v", i, err)
		}
	}
	return mem1, mem2
}

// countRows returns the number of rows in table matching a project_id column,
// queried directly so the assertion doesn't depend on DeleteProject's own
// counting logic being correct.
func countRows(t *testing.T, s *Store, table, projectID string) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT count(*) FROM `+table+` WHERE project_id = ?`, projectID).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

func TestDeleteProject_RefusesGlobal(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if err := s.SeedGlobalMemories(ctx); err != nil {
		t.Fatalf("SeedGlobalMemories: %v", err)
	}

	for _, apply := range []bool{false, true} {
		_, err := s.DeleteProject(ctx, "_global", apply)
		if err == nil {
			t.Fatalf("expected error deleting _global (apply=%v)", apply)
		}
	}

	// The seeded global preference must have survived both attempts.
	mems, err := s.GetAll(ctx, "_global", 100)
	if err != nil {
		t.Fatalf("GetAll _global: %v", err)
	}
	if len(mems) == 0 {
		t.Error("expected _global memories to survive refused delete attempts")
	}
}

func TestDeleteProject_ApplyRemovesEverythingIncludingOrphanTables(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	mem1, _ := seedFullProject(t, s, ctx, testProject)

	// Seed one row in every remaining cascading table seedFullProject
	// doesn't already cover, so this test proves the full cascade chain
	// documented on DeleteProject, not just the tables exercised elsewhere.
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO memory_embeddings (memory_id, embedding) VALUES (?, ?)`, mem1, []byte{0x01, 0x02},
	); err != nil {
		t.Fatalf("seed memory_embeddings: %v", err)
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO link_scans (memory_id) VALUES (?)`, mem1,
	); err != nil {
		t.Fatalf("seed link_scans: %v", err)
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO ghost_state (project_id, learned_context) VALUES (?, ?)
		 ON CONFLICT(project_id) DO UPDATE SET learned_context = excluded.learned_context`,
		testProject, "seeded learned context",
	); err != nil {
		t.Fatalf("seed ghost_state: %v", err)
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO memory_snapshots (snapshot_id, project_id, category, content, importance, source) VALUES (?, ?, ?, ?, ?, ?)`,
		"snap-1", testProject, "fact", "seed snapshot content", 0.5, "manual",
	); err != nil {
		t.Fatalf("seed memory_snapshots: %v", err)
	}

	summary, err := s.DeleteProject(ctx, testProject, true)
	if err != nil {
		t.Fatalf("DeleteProject apply: %v", err)
	}
	if summary.Memories != 5 || summary.MemoryLinks != 4 || summary.Tasks != 2 ||
		summary.Decisions != 1 || summary.TokenUsage != 0 || summary.AuditLog != 6 {
		t.Fatalf("unexpected summary: %+v", summary)
	}

	for _, table := range []string{"memories", "tasks", "decisions", "token_usage", "audit_log"} {
		if n := countRows(t, s, table, testProject); n != 0 {
			t.Errorf("%s after apply = %d, want 0", table, n)
		}
	}
	var linkCount int
	if err := s.db.QueryRow(`
		SELECT count(*) FROM memory_links
		WHERE source_id NOT IN (SELECT id FROM memories) OR target_id NOT IN (SELECT id FROM memories)
	`).Scan(&linkCount); err != nil {
		t.Fatalf("check dangling links: %v", err)
	}
	if linkCount != 0 {
		t.Errorf("expected no dangling memory_links pointing at deleted memories, got %d", linkCount)
	}

	// The memories_fts index (maintained by the memories_ad AFTER DELETE
	// trigger in schema.go) must also be emptied by the cascade delete —
	// it has no project_id column, so countRows can't check it directly.
	var ftsCount int
	if err := s.db.QueryRow(`SELECT count(*) FROM memories_fts`).Scan(&ftsCount); err != nil {
		t.Fatalf("count memories_fts: %v", err)
	}
	if ftsCount != 0 {
		t.Errorf("memories_fts retains %d rows after apply, want 0", ftsCount)
	}

	// memory_embeddings and link_scans key off memory_id, not project_id, so
	// countRows can't check them directly.
	var embedCount int
	if err := s.db.QueryRow(`SELECT count(*) FROM memory_embeddings WHERE memory_id = ?`, mem1).Scan(&embedCount); err != nil {
		t.Fatalf("count memory_embeddings: %v", err)
	}
	if embedCount != 0 {
		t.Errorf("memory_embeddings retains %d rows after apply, want 0", embedCount)
	}
	var scanCount int
	if err := s.db.QueryRow(`SELECT count(*) FROM link_scans WHERE memory_id = ?`, mem1).Scan(&scanCount); err != nil {
		t.Fatalf("count link_scans: %v", err)
	}
	if scanCount != 0 {
		t.Errorf("link_scans retains %d rows after apply, want 0", scanCount)
	}

	for _, table := range []string{"ghost_state", "memory_snapshots"} {
		if n := countRows(t, s, table, testProject); n != 0 {
			t.Errorf("%s after apply = %d, want 0", table, n)
		}
	}

	// The project row itself is gone.
	id, _, err := s.ResolveProject(ctx, testProject)
	if err != nil {
		t.Fatalf("ResolveProject after apply: %v", err)
	}
	if id != "" {
		t.Errorf("expected project to be gone, ResolveProject still returns id %q", id)
	}
}

// TestDeleteProject_ConcurrentWriteFromSeparateProcessDoesNotUndercountSummary
// exercises the race DeleteProject's write-lock-first ordering exists to
// close: two separate *Store instances (simulating two separate ghost
// processes, e.g. a CLI `project delete --apply` and a long-running MCP
// server) opened against the same on-disk SQLite file — :memory: databases
// aren't shared across handles, so this needs a real temp file. One store
// races a write against the project the other is deleting. Whichever side
// wins, the returned summary must never lie about what was actually deleted.
func TestDeleteProject_ConcurrentWriteFromSeparateProcessDoesNotUndercountSummary(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "race.sqlite")
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	dbA, err := OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB A: %v", err)
	}
	defer func() { _ = dbA.Close() }()
	storeA := NewStore(dbA, logger)

	dbB, err := OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB B: %v", err)
	}
	defer func() { _ = dbB.Close() }()
	storeB := NewStore(dbB, logger)

	ctx := context.Background()
	if err := storeA.EnsureProject(ctx, testProject, "/tmp/race", testProject); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}

	var wg sync.WaitGroup
	var bErr error
	var bSucceeded bool
	wg.Add(1)
	go func() {
		defer wg.Done()
		// Retry briefly: the first attempt or two may land before storeA
		// even starts its transaction, or may block on storeA's held write
		// lock until it commits or rolls back.
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			_, _, _, upsertErr := storeB.Upsert(ctx, testProject, "fact", "racing write from a second process", "manual", 0.5, []string{})
			if upsertErr == nil {
				bSucceeded = true
				return
			}
			bErr = upsertErr
			if strings.Contains(upsertErr.Error(), "FOREIGN KEY") {
				// The project is already gone — a definitive loss, not a
				// transient lock contention. Stop retrying.
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()

	summary, err := storeA.DeleteProject(ctx, testProject, true)
	wg.Wait()

	if err != nil {
		t.Fatalf("DeleteProject: %v", err)
	}

	// The invariant this test protects: the summary must never undercount
	// what actually got deleted. If B's write landed before A's count ran
	// (bSucceeded), A's count — taken only after the write lock is forced —
	// must have seen it; if B's write never landed, the count must be 0.
	if bSucceeded && summary.Memories != 1 {
		t.Errorf("B's concurrent write succeeded but summary.Memories = %d, want 1 (undercounted)", summary.Memories)
	}
	if !bSucceeded && summary.Memories != 0 {
		t.Errorf("B's concurrent write did not succeed (last err=%v) but summary.Memories = %d, want 0", bErr, summary.Memories)
	}

	// Whichever side won, the DB must end up fully consistent: no memories
	// left over under a project that no longer exists.
	var remaining int
	if err := dbA.QueryRowContext(ctx, `SELECT count(*) FROM memories WHERE project_id = ?`, testProject).Scan(&remaining); err != nil {
		t.Fatalf("count remaining: %v", err)
	}
	if remaining != 0 {
		t.Errorf("expected 0 memories remaining for deleted project, got %d", remaining)
	}
}

func TestSeedGlobalMemories(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	// Seed should create _global project and insert seed memories.
	if err := s.SeedGlobalMemories(ctx); err != nil {
		t.Fatalf("SeedGlobalMemories: %v", err)
	}

	// Verify _global project exists.
	projects, _ := s.ListProjects(ctx)
	found := false
	for _, p := range projects {
		if p.ID == "_global" {
			found = true
		}
	}
	if !found {
		t.Fatal("_global project not created")
	}

	// Verify seed memories are present, pinned, and carry the distinct builtin
	// source rather than being presented as user-authored manual rows.
	mems, err := s.GetAll(ctx, "_global", 50)
	if err != nil {
		t.Fatalf("GetAll _global: %v", err)
	}
	if len(mems) == 0 {
		t.Fatal("no seed memories found")
	}

	var coAuthorFound bool
	for _, m := range mems {
		if strings.Contains(m.Content, "Co-Authored-By") {
			coAuthorFound = true
			if m.Source != "builtin" {
				t.Errorf("seed memory source = %q, want 'builtin'", m.Source)
			}
			if !m.Pinned {
				t.Error("seed memory should be pinned")
			}
			if m.Importance != 1.0 {
				t.Errorf("seed memory importance = %v, want 1.0", m.Importance)
			}
		}
	}
	if !coAuthorFound {
		t.Error("Co-Authored-By seed memory not found")
	}

	// Run again — should be idempotent (no duplicates).
	if err := s.SeedGlobalMemories(ctx); err != nil {
		t.Fatalf("SeedGlobalMemories (2nd call): %v", err)
	}
	mems2, _ := s.GetAll(ctx, "_global", 50)
	if len(mems2) != len(mems) {
		t.Errorf("idempotency broken: %d memories after 2nd seed (was %d)", len(mems2), len(mems))
	}

	// Verify consolidation cannot remove it: ReplaceNonManual skips manual source.
	replaceMems := []Memory{
		{ProjectID: "_global", Category: "fact", Content: "some new fact", Source: "reflection", Importance: 0.5},
	}
	if _, _, _, _, err := s.ReplaceNonManual(ctx, "_global", replaceMems, "", nil); err != nil {
		t.Fatalf("ReplaceNonManual: %v", err)
	}

	memsAfter, _ := s.GetAll(ctx, "_global", 50)
	var seedSurvived bool
	for _, m := range memsAfter {
		if strings.Contains(m.Content, "Co-Authored-By") {
			seedSurvived = true
		}
	}
	if !seedSurvived {
		t.Error("seed memory was deleted by ReplaceNonManual — consolidation protection broken")
	}
}

// TestSeedGlobalMemories_GhostStateInsertFailureLogged covers issue #291:
// the ghost_state seed insert's error was discarded via `_, _ =`. The
// ghost_state table is renamed away before calling SeedGlobalMemories so
// that specific INSERT OR IGNORE fails deterministically ("no such table"),
// while the preceding "ensure _global project" statement (projects table,
// already error-checked) and the seed-memory insert loop (memories table)
// are unaffected and still succeed.
func TestSeedGlobalMemories_GhostStateInsertFailureLogged(t *testing.T) {
	db, err := OpenDB(":memory:")
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	s := NewStore(db, logger)
	ctx := context.Background()

	if _, err := db.ExecContext(ctx, `ALTER TABLE ghost_state RENAME TO ghost_state_broken`); err != nil {
		t.Fatalf("rename ghost_state: %v", err)
	}

	// Non-fatal: the issue treats a missing ghost_state row as acceptable
	// (GetLearnedContext just sees sql.ErrNoRows), so SeedGlobalMemories
	// must still return nil even though the insert failed underneath.
	if err := s.SeedGlobalMemories(ctx); err != nil {
		t.Fatalf("SeedGlobalMemories should not return a hard error, got: %v", err)
	}

	if !strings.Contains(logBuf.String(), "seed global ghost_state insert failed") {
		t.Errorf("expected ghost_state insert failure to be logged, got log output: %q", logBuf.String())
	}
}

func TestStoreUpdateMemory(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	newID := func(t *testing.T) string {
		t.Helper()
		id, err := s.Create(ctx, testProject, Memory{
			Category:   "fact",
			Content:    "Original content about the deploy pipeline",
			Source:     "mcp",
			Importance: 0.7,
			Tags:       []string{"deploy"},
		})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		return id
	}

	getOne := func(t *testing.T, id string) Memory {
		t.Helper()
		mems, err := s.GetByIDs(ctx, []string{id})
		if err != nil || len(mems) != 1 {
			t.Fatalf("GetByIDs(%s): %v (n=%d)", id, err, len(mems))
		}
		return mems[0]
	}

	t.Run("content only, others preserved", func(t *testing.T) {
		id := newID(t)
		content := "Corrected content about the deploy pipeline"
		if err := s.UpdateMemory(ctx, testProject, id, &content, nil, nil, nil); err != nil {
			t.Fatalf("UpdateMemory: %v", err)
		}
		m := getOne(t, id)
		if m.Content != content {
			t.Errorf("content = %q, want %q", m.Content, content)
		}
		if m.Category != "fact" || m.Importance != 0.7 || m.Source != "mcp" {
			t.Errorf("unchanged fields clobbered: category=%q importance=%f source=%q",
				m.Category, m.Importance, m.Source)
		}
		if len(m.Tags) != 1 || m.Tags[0] != "deploy" {
			t.Errorf("tags clobbered: %v", m.Tags)
		}
	})

	t.Run("category and importance", func(t *testing.T) {
		id := newID(t)
		cat := "gotcha"
		imp := float32(0.9)
		if err := s.UpdateMemory(ctx, testProject, id, nil, &cat, &imp, nil); err != nil {
			t.Fatalf("UpdateMemory: %v", err)
		}
		m := getOne(t, id)
		if m.Category != "gotcha" || m.Importance != 0.9 {
			t.Errorf("category=%q importance=%f, want gotcha/0.9", m.Category, m.Importance)
		}
		if m.Content != "Original content about the deploy pipeline" {
			t.Errorf("content clobbered: %q", m.Content)
		}
	})

	t.Run("nil tags preserve, empty tags clear", func(t *testing.T) {
		id := newID(t)
		imp := float32(0.5)
		if err := s.UpdateMemory(ctx, testProject, id, nil, nil, &imp, nil); err != nil {
			t.Fatalf("UpdateMemory (nil tags): %v", err)
		}
		if m := getOne(t, id); len(m.Tags) != 1 {
			t.Errorf("nil tags should preserve, got %v", m.Tags)
		}
		if err := s.UpdateMemory(ctx, testProject, id, nil, nil, nil, []string{}); err != nil {
			t.Fatalf("UpdateMemory (empty tags): %v", err)
		}
		if m := getOne(t, id); len(m.Tags) != 0 {
			t.Errorf("empty tags should clear, got %v", m.Tags)
		}
	})

	t.Run("content change invalidates embedding and link scan", func(t *testing.T) {
		id := newID(t)
		vec := make([]float32, 8)
		vec[0] = 1
		if err := s.StoreEmbedding(ctx, id, vec, "test-model"); err != nil {
			t.Fatalf("StoreEmbedding: %v", err)
		}
		if err := s.MarkLinkScanned(ctx, id, vec); err != nil {
			t.Fatalf("MarkLinkScanned: %v", err)
		}
		content := "Entirely rewritten content"
		if err := s.UpdateMemory(ctx, testProject, id, &content, nil, nil, nil); err != nil {
			t.Fatalf("UpdateMemory: %v", err)
		}
		if _, err := s.GetEmbedding(ctx, id); err == nil {
			t.Error("embedding should be deleted after content change")
		}
		var n int
		if err := s.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM link_scans WHERE memory_id = ?`, id).Scan(&n); err != nil {
			t.Fatalf("count link_scans: %v", err)
		}
		if n != 0 {
			t.Error("link_scans row should be deleted after content change")
		}
	})

	t.Run("non-content change keeps embedding", func(t *testing.T) {
		id := newID(t)
		vec := make([]float32, 8)
		vec[0] = 1
		if err := s.StoreEmbedding(ctx, id, vec, "test-model"); err != nil {
			t.Fatalf("StoreEmbedding: %v", err)
		}
		imp := float32(0.4)
		if err := s.UpdateMemory(ctx, testProject, id, nil, nil, &imp, nil); err != nil {
			t.Fatalf("UpdateMemory: %v", err)
		}
		if _, err := s.GetEmbedding(ctx, id); err != nil {
			t.Error("embedding should survive a non-content update")
		}
	})

	t.Run("unknown id errors", func(t *testing.T) {
		content := "x"
		err := s.UpdateMemory(ctx, testProject, "does-not-exist", &content, nil, nil, nil)
		if err == nil {
			t.Error("expected error for unknown memory id")
		}
	})

	t.Run("pinned survives update", func(t *testing.T) {
		id := newID(t)
		if err := s.TogglePin(ctx, id, true); err != nil {
			t.Fatalf("TogglePin: %v", err)
		}
		content := "Pinned memory, corrected"
		if err := s.UpdateMemory(ctx, testProject, id, &content, nil, nil, nil); err != nil {
			t.Fatalf("UpdateMemory: %v", err)
		}
		if m := getOne(t, id); !m.Pinned {
			t.Error("pinned flag should survive an update")
		}
	})

	t.Run("tags replaced with new values", func(t *testing.T) {
		id := newID(t)
		if err := s.UpdateMemory(ctx, testProject, id, nil, nil, nil, []string{"alpha", "beta"}); err != nil {
			t.Fatalf("UpdateMemory: %v", err)
		}
		m := getOne(t, id)
		if len(m.Tags) != 2 || m.Tags[0] != "alpha" || m.Tags[1] != "beta" {
			t.Errorf("tags = %v, want [alpha beta]", m.Tags)
		}
	})

	t.Run("wrong project cannot update", func(t *testing.T) {
		id := newID(t)
		if err := s.EnsureProject(ctx, "other-proj", "/tmp/other-proj", "other-proj"); err != nil {
			t.Fatalf("EnsureProject: %v", err)
		}
		content := "hijacked"
		if err := s.UpdateMemory(ctx, "other-proj", id, &content, nil, nil, nil); err == nil {
			t.Error("expected error updating another project's memory")
		}
		if m := getOne(t, id); m.Content == "hijacked" {
			t.Error("content must be unchanged after cross-project update attempt")
		}
	})
}

func TestStorePromoteToGlobal(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	t.Run("moves memory preserving id, pin, importance, embedding", func(t *testing.T) {
		id, err := s.Create(ctx, testProject, Memory{
			Category:   "preference",
			Content:    "Always use two-space YAML indentation",
			Source:     "mcp",
			Importance: 0.8,
			Tags:       []string{"yaml"},
		})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if err := s.TogglePin(ctx, id, true); err != nil {
			t.Fatalf("TogglePin: %v", err)
		}
		vec := make([]float32, 8)
		vec[0] = 1
		if err := s.StoreEmbedding(ctx, id, vec, "test-model"); err != nil {
			t.Fatalf("StoreEmbedding: %v", err)
		}

		if err := s.PromoteToGlobal(ctx, testProject, id); err != nil {
			t.Fatalf("PromoteToGlobal: %v", err)
		}

		mems, err := s.GetByIDs(ctx, []string{id})
		if err != nil || len(mems) != 1 {
			t.Fatalf("GetByIDs: %v (n=%d)", err, len(mems))
		}
		m := mems[0]
		if m.ProjectID != "_global" {
			t.Errorf("project = %q, want _global", m.ProjectID)
		}
		if !m.Pinned || m.Importance != 0.8 {
			t.Errorf("pin/importance not preserved: pinned=%v importance=%f", m.Pinned, m.Importance)
		}
		if _, err := s.GetEmbedding(ctx, id); err != nil {
			t.Error("embedding should survive promotion")
		}
	})

	t.Run("works without prior _global row", func(t *testing.T) {
		// testStore never seeds _global — the first subtest exercised the
		// inline ensure; this one proves a fresh store promotes fine too.
		s2 := testStore(t)
		id, err := s2.Create(ctx, testProject, Memory{
			Category: "fact", Content: "promotable", Source: "mcp", Importance: 0.5, Tags: []string{},
		})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if err := s2.PromoteToGlobal(ctx, testProject, id); err != nil {
			t.Fatalf("PromoteToGlobal on fresh store: %v", err)
		}

		mems, err := s2.GetByIDs(ctx, []string{id})
		if err != nil || len(mems) != 1 {
			t.Fatalf("GetByIDs: %v (n=%d)", err, len(mems))
		}
		if mems[0].ProjectID != "_global" {
			t.Errorf("project = %q, want _global", mems[0].ProjectID)
		}
	})

	t.Run("unknown id errors", func(t *testing.T) {
		if err := s.PromoteToGlobal(ctx, testProject, "does-not-exist"); err == nil {
			t.Error("expected error for unknown memory id")
		}
	})

	t.Run("wrong project cannot promote", func(t *testing.T) {
		id, err := s.Create(ctx, testProject, Memory{
			Category: "fact", Content: "stays put", Source: "mcp", Importance: 0.5, Tags: []string{},
		})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if err := s.EnsureProject(ctx, "other-proj2", "/tmp/other-proj2", "other-proj2"); err != nil {
			t.Fatalf("EnsureProject: %v", err)
		}
		if err := s.PromoteToGlobal(ctx, "other-proj2", id); err == nil {
			t.Error("expected error promoting another project's memory")
		}
		mems, err := s.GetByIDs(ctx, []string{id})
		if err != nil || len(mems) != 1 {
			t.Fatalf("GetByIDs: %v", err)
		}
		if mems[0].ProjectID != testProject {
			t.Errorf("memory moved to %q, must stay in %q", mems[0].ProjectID, testProject)
		}
	})
}

// TestPromoteToGlobalReassignsSupersedeChecked: supersede_checked.project_id
// cascades from projects, so a check left naming the project a promoted endpoint
// just left is destroyed when that project is deleted — discarding a NEITHER
// verdict the pass already paid for, and re-asking a pair the cache had settled.
// The row names its memory as one of TWO endpoints, so the reassignment has to
// match either column.
func TestPromoteToGlobalReassignsSupersedeChecked(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	newer, err := s.Create(ctx, testProject, Memory{
		Category: "fact", Content: "the newer claim", Source: "mcp", Importance: 0.5,
	})
	if err != nil {
		t.Fatalf("Create newer: %v", err)
	}
	older, err := s.Create(ctx, testProject, Memory{
		Category: "fact", Content: "the older claim", Source: "mcp", Importance: 0.5,
	})
	if err != nil {
		t.Fatalf("Create older: %v", err)
	}
	if err := s.MarkSupersedeNeither(ctx, testProject, map[[2]string]SupersedeCheck{
		{newer, older}: {NewerHash: "newer-hash", OlderHash: "older-hash"},
	}); err != nil {
		t.Fatalf("MarkSupersedeNeither: %v", err)
	}

	if err := s.PromoteToGlobal(ctx, testProject, newer); err != nil {
		t.Fatalf("PromoteToGlobal: %v", err)
	}

	var projectID string
	if err := s.db.QueryRow(
		`SELECT project_id FROM supersede_checked WHERE newer_id = ? AND older_id = ?`, newer, older,
	).Scan(&projectID); err != nil {
		t.Fatalf("read supersede_checked: %v", err)
	}
	if projectID != GlobalProjectID {
		t.Errorf("supersede_checked.project_id = %q, want %q", projectID, GlobalProjectID)
	}
}

func TestEnsureProject_AutoMerge(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	// Simulate MCP creating a project with name-as-ID.
	if err := s.EnsureProject(ctx, "myproject", "myproject", "myproject"); err != nil {
		t.Fatalf("EnsureProject MCP: %v", err)
	}

	// Save a memory under the MCP project.
	_, _, _, err := s.Upsert(ctx, "myproject", "fact", "deployed on k8s cluster alpha-7", "mcp", 0.8, []string{})
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	// Simulate orchestrator creating the real project (abs path, hash ID).
	hashID := "abc123def456" // deterministic fake hash
	if err := s.EnsureProject(ctx, hashID, "/home/wayne/git/myproject", "myproject"); err != nil {
		t.Fatalf("EnsureProject orchestrator: %v", err)
	}

	// The MCP project should have been auto-merged.
	projects, _ := s.ListProjects(ctx)
	for _, p := range projects {
		if p.ID == "myproject" {
			t.Error("MCP project should have been merged away")
		}
	}

	// Memory should now be under the hash ID.
	mems, _ := s.GetTopMemories(ctx, hashID, 10)
	found := false
	for _, m := range mems {
		if strings.Contains(m.Content, "alpha-7") {
			found = true
		}
	}
	if !found {
		t.Error("expected MCP memory to be reassigned to hash-ID project")
	}
}

// TestEnsureProject_AutoMerge_PathCollision covers EnsureProject's *other*
// auto-merge path: a different incoming id colliding with an absolute path
// that's already owned by another project (as opposed to
// TestEnsureProject_AutoMerge, which covers the name/non-absolute-path
// duplicate case). Guards that the DRY refactor onto mergeProjectLocked
// preserves this success-path outcome (canonical id survives, incoming id's
// records are reassigned and its row is gone).
func TestEnsureProject_AutoMerge_PathCollision(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	// Canonical project already owns this absolute path.
	if err := s.EnsureProject(ctx, "hash-original", "/home/wayne/git/pathcollide", "pathcollide"); err != nil {
		t.Fatalf("EnsureProject original: %v", err)
	}

	// A second, distinct project id accumulates its own record first.
	if err := s.EnsureProject(ctx, "hash-new", "hash-new", "hash-new"); err != nil {
		t.Fatalf("EnsureProject placeholder: %v", err)
	}
	if _, _, _, err := s.Upsert(ctx, "hash-new", "fact", "belongs to the incoming duplicate id", "manual", 0.5, []string{}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	// Re-registering "hash-new" against the path already owned by
	// hash-original must self-heal: fold hash-new into the canonical
	// project instead of violating the projects.path UNIQUE constraint.
	if err := s.EnsureProject(ctx, "hash-new", "/home/wayne/git/pathcollide", "pathcollide"); err != nil {
		t.Fatalf("EnsureProject collision: %v", err)
	}

	projects, _ := s.ListProjects(ctx)
	for _, p := range projects {
		if p.ID == "hash-new" {
			t.Error("incoming colliding id should have been merged away, not kept")
		}
	}

	mems, _ := s.GetTopMemories(ctx, "hash-original", 10)
	found := false
	for _, m := range mems {
		if strings.Contains(m.Content, "belongs to the incoming duplicate id") {
			found = true
		}
	}
	if !found {
		t.Error("expected memory reassigned to canonical (pre-existing) project id")
	}
}

// TestEnsureProject_AutoMergeErrorNotSwallowed guards issue #290: a failed
// UPDATE/DELETE inside EnsureProject's auto-merge paths must surface as a
// real error instead of being silently discarded while the function reports
// success — and, worse, having already partially reassigned records with no
// atomicity. Forces the merge to fail partway through by dropping one of the
// child tables the merge touches.
func TestEnsureProject_AutoMergeErrorNotSwallowed(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	// Simulate MCP creating a project with name-as-ID.
	if err := s.EnsureProject(ctx, "myproject", "myproject", "myproject"); err != nil {
		t.Fatalf("EnsureProject MCP: %v", err)
	}
	if _, _, _, err := s.Upsert(ctx, "myproject", "fact", "deployed on k8s cluster alpha-7", "mcp", 0.8, []string{}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	// Break one of the tables the merge must touch, so the merge fails
	// partway through instead of completing cleanly.
	if _, err := s.db.ExecContext(ctx, `DROP TABLE token_usage`); err != nil {
		t.Fatalf("drop token_usage: %v", err)
	}

	// Orchestrator creates the real project (abs path, hash ID). This
	// triggers the name/path-duplicate auto-merge, which must now return a
	// real error instead of silently reporting success.
	hashID := "abc123def456"
	err := s.EnsureProject(ctx, hashID, "/home/wayne/git/myproject", "myproject")
	if err == nil {
		t.Fatal("EnsureProject returned nil despite the merge failing — error was silently swallowed")
	}

	// The merge must have rolled back entirely, not just reported an error
	// while the old code's uncommitted-transaction-free loop had already
	// deleted "myproject" and reassigned some (but not all) of its records.
	// This is what actually distinguishes the fixed, transactional merge
	// from a version that merely wraps the same errors and returns them.
	projects, _ := s.ListProjects(ctx)
	found := false
	for _, p := range projects {
		if p.ID == "myproject" {
			found = true
		}
	}
	if !found {
		t.Error("myproject should still exist — the failed merge must not have deleted it")
	}

	mems, _ := s.GetTopMemories(ctx, "myproject", 10)
	memFound := false
	for _, m := range mems {
		if strings.Contains(m.Content, "alpha-7") {
			memFound = true
		}
	}
	if !memFound {
		t.Error("memory should still be under myproject — the failed merge must not have partially reassigned it")
	}

	hashMems, _ := s.GetTopMemories(ctx, hashID, 10)
	for _, m := range hashMems {
		if strings.Contains(m.Content, "alpha-7") {
			t.Error("memory should NOT have been reassigned to hashID — the failed merge must not partially apply")
		}
	}
}

func TestResolveCandidatesAndSetResolved(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	// Eligible: a resolvable gotcha.
	evID, err := s.Create(ctx, testProject, Memory{
		Category: "gotcha", Content: "kill experiment returned NO-GO", Source: "manual", Importance: 0.6,
	})
	if err != nil {
		t.Fatalf("create evidence: %v", err)
	}
	// Exempt by category.
	if _, err := s.Create(ctx, testProject, Memory{
		Category: "convention", Content: "never push to main", Source: "manual", Importance: 0.9,
	}); err != nil {
		t.Fatalf("create convention: %v", err)
	}
	if _, err := s.Create(ctx, testProject, Memory{
		Category: "preference", Content: "user prefers tabs", Source: "manual", Importance: 0.5,
	}); err != nil {
		t.Fatalf("create preference: %v", err)
	}
	// Exempt by pin.
	pinID, err := s.Create(ctx, testProject, Memory{
		Category: "gotcha", Content: "pinned gotcha stays", Source: "manual", Importance: 0.5,
	})
	if err != nil {
		t.Fatalf("create pinned: %v", err)
	}
	if err := s.TogglePin(ctx, pinID, true); err != nil {
		t.Fatalf("pin: %v", err)
	}

	// Candidates exclude convention, preference, and pinned rows.
	cands, err := s.ResolveCandidates(ctx, testProject)
	if err != nil {
		t.Fatalf("ResolveCandidates: %v", err)
	}
	if len(cands) != 1 || cands[0].ID != evID {
		ids := make([]string, len(cands))
		for i, c := range cands {
			ids[i] = c.ID + "/" + c.Category
		}
		t.Fatalf("candidates = %v, want exactly [%s/gotcha]", ids, evID)
	}

	// Marking resolved removes it from the candidate set (idempotent re-run).
	if _, err := s.SetResolved(ctx, []string{evID}); err != nil {
		t.Fatalf("SetResolved: %v", err)
	}
	cands, err = s.ResolveCandidates(ctx, testProject)
	if err != nil {
		t.Fatalf("ResolveCandidates after SetResolved: %v", err)
	}
	if len(cands) != 0 {
		t.Errorf("candidates after resolve = %d, want 0", len(cands))
	}
}

// TestSetResolvedRechecksEligibilityAtWriteTime guards the TOCTOU window
// between ResolveCandidates (read) and SetResolved (write): a candidate
// pinned or recategorized into an exempt bucket after classification started
// must not be stamped, even though its ID was in the confirmed batch.
func TestSetResolvedRechecksEligibilityAtWriteTime(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	pinnedLate, err := s.Create(ctx, testProject, Memory{
		Category: "gotcha", Content: "kill experiment returned NO-GO", Source: "manual", Importance: 0.6,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	recategorizedLate, err := s.Create(ctx, testProject, Memory{
		Category: "gotcha", Content: "fixed in PR #210, removed", Source: "manual", Importance: 0.6,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	stillEligible, err := s.Create(ctx, testProject, Memory{
		Category: "gotcha", Content: "shipped and archived", Source: "manual", Importance: 0.6,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Simulate state changing between ResolveCandidates and SetResolved: pin
	// one, recategorize another into an exempt bucket.
	if err := s.TogglePin(ctx, pinnedLate, true); err != nil {
		t.Fatalf("pin: %v", err)
	}
	category := "convention"
	if err := s.UpdateMemory(ctx, testProject, recategorizedLate, nil, &category, nil, nil); err != nil {
		t.Fatalf("recategorize: %v", err)
	}

	n, err := s.SetResolved(ctx, []string{pinnedLate, recategorizedLate, stillEligible})
	if err != nil {
		t.Fatalf("SetResolved: %v", err)
	}
	if n != 1 {
		t.Errorf("SetResolved returned %d, want 1 (only stillEligible)", n)
	}

	assertActive(t, s, testProject, pinnedLate)
	assertActive(t, s, testProject, recategorizedLate)

	cands, err := s.ResolveCandidates(ctx, testProject)
	if err != nil {
		t.Fatalf("ResolveCandidates: %v", err)
	}
	for _, c := range cands {
		if c.ID == stillEligible {
			t.Errorf("stillEligible %s should be resolved and excluded from candidates", stillEligible)
		}
	}
}

// TestMarkResolvedIsProjectScoped: the store guard, independent of the caller's
// read. `MemoryIDsByIDPrefix` reaches a project plus `_global` — a promotion
// moves a row while keeping the links pointing at it, so both ends of an edge
// have to stay nameable — and a ref that lands on a `_global` row must not be
// stampable from a project that does not own it. The write binds project_id in
// the same statement as the stamp, so this holds whatever the caller checked.
func TestMarkResolvedIsProjectScoped(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if err := s.EnsureProject(ctx, "other-id", "/tmp/other", "other"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	theirs, err := s.Create(ctx, "other-id", Memory{
		Category: "gotcha", Content: "kill experiment returned NO-GO", Source: "manual", Importance: 0.6,
	})
	if err != nil {
		t.Fatalf("create in other project: %v", err)
	}
	ours, err := s.Create(ctx, testProject, Memory{
		Category: "gotcha", Content: "postmortem concluded, no follow-up", Source: "manual", Importance: 0.6,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	stamped, err := s.MarkResolved(ctx, testProject, []string{theirs, ours}, Provenance{Agent: "operator"})
	if err != nil {
		t.Fatalf("MarkResolved: %v", err)
	}
	if len(stamped) != 1 || stamped[0] != ours {
		t.Errorf("MarkResolved stamped %v, want only [%s] — another project's row must not be stamped", stamped, ours)
	}
	// ResolveCandidates returns the UNRESOLVED rows, so theirs must still be in
	// it: a stamp would have taken it out, and the loop below is a guard on the
	// count above rather than on the write's absence of an error.
	got, err := s.ResolveCandidates(ctx, "other-id")
	if err != nil {
		t.Fatalf("ResolveCandidates: %v", err)
	}
	var found bool
	for _, m := range got {
		if m.ID == theirs {
			found = true
		}
	}
	if !found {
		t.Error("the other project's row left the unresolved set: it was stamped")
	}
	// The refusal the shared statement's empty-project case would otherwise make
	// silent: no project, no reach.
	if _, err := s.MarkResolved(ctx, "", []string{ours}, Provenance{}); err == nil {
		t.Error("MarkResolved accepted an empty project, which would unbind the guard it exists to hold")
	}
	// And the sentinel, which the project binding does NOT catch: a promoted
	// row's own project_id IS _global, so naming _global satisfies every
	// predicate this method has and buries a memory every project injects. The
	// refusal is a separate one, at this layer, so a caller reaching the store
	// directly inherits it rather than re-deriving it.
	promoted, err := s.Create(ctx, testProject, Memory{
		Category: "gotcha", Content: "kill experiment returned NO-GO", Source: "manual", Importance: 0.6,
	})
	if err != nil {
		t.Fatalf("create promoted: %v", err)
	}
	if err := s.PromoteToGlobal(ctx, testProject, promoted); err != nil {
		t.Fatalf("PromoteToGlobal: %v", err)
	}
	if _, err := s.MarkResolved(ctx, GlobalProjectID, []string{promoted}, Provenance{}); err == nil {
		t.Error("MarkResolved accepted the _global project, so a promoted memory can be buried for every project at once")
	}
	// ResolveCandidates returns the UNRESOLVED rows, so the promoted row must
	// still be in it. Its presence is the proof the stamp did not happen: a
	// committed one would have taken it out of that set.
	globalCands, err := s.ResolveCandidates(ctx, GlobalProjectID)
	if err != nil {
		t.Fatalf("ResolveCandidates(_global): %v", err)
	}
	var stillThere bool
	for _, m := range globalCands {
		if m.ID == promoted {
			stillThere = true
		}
	}
	if !stillThere {
		t.Error("the promoted row was stamped: one command buried a memory every project injects")
	}
}

// TestMarkResolvedRecordsThePerformerOnItsHistoryRow: the reason an operator's
// mark is a separate method rather than a SetResolved call. Every `resolve` row a
// lifecycle pass writes leaves the performer empty, because a pass knows no
// session — so a reader of the history cannot tell that row's stamp from a
// person's. MarkResolved is the one resolve writer that can, and the column is
// what it writes into.
func TestMarkResolvedRecordsThePerformerOnItsHistoryRow(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	id, err := s.Create(ctx, testProject, Memory{
		Category: "gotcha", Content: "shipped and archived", Source: "manual", Importance: 0.6,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if _, err := s.MarkResolved(ctx, testProject, []string{id}, Provenance{Agent: "operator"}); err != nil {
		t.Fatalf("MarkResolved: %v", err)
	}
	entries, err := s.MemoryHistory(ctx, id, 10)
	if err != nil {
		t.Fatalf("MemoryHistory: %v", err)
	}
	var resolves int
	for _, e := range entries {
		if e.Phase != "resolve" {
			continue
		}
		resolves++
		if e.Agent != "operator" {
			t.Errorf("the resolve history row's agent = %q, want %q", e.Agent, "operator")
		}
		if e.ResolvedAt == nil || *e.ResolvedAt == "" {
			t.Errorf("the resolve history row records no resolved_at, so it is not a version of the stamped state: %+v", e)
		}
	}
	if resolves != 1 {
		t.Errorf("got %d resolve history row(s), want 1", resolves)
	}

	// And the pass's own path still leaves it empty, which is the contract the
	// history comment states: an empty field is an admission, not a claim.
	other, err := s.Create(ctx, testProject, Memory{
		Category: "gotcha", Content: "fixed in PR #210, removed", Source: "manual", Importance: 0.6,
	})
	if err != nil {
		t.Fatalf("create other: %v", err)
	}
	if n, err := s.SetResolved(ctx, []string{other}); err != nil || n != 1 {
		t.Fatalf("SetResolved = %d, %v; want 1 and no error", n, err)
	}
	entries, err = s.MemoryHistory(ctx, other, 10)
	if err != nil {
		t.Fatalf("MemoryHistory(other): %v", err)
	}
	for _, e := range entries {
		if e.Phase == "resolve" && e.Agent != "" {
			t.Errorf("SetResolved's resolve row carries agent %q, want empty — a lifecycle pass knows no session", e.Agent)
		}
	}
}

// TestMarkResolvedRollsBackWhenTheHistoryAppendFails: "in the same transaction" is
// not a description of two writes that happen to agree. A memory stamped without
// its `resolve` history row is exactly the state writing resolved_at by hand
// leaves behind and the reason this path exists — the record of how a memory
// reached its current state would show it resolved with nothing saying who said
// so. So the stamp and the record are one unit, and this makes the record fail
// AFTER the UPDATE has run, inside the open transaction.
func TestMarkResolvedRollsBackWhenTheHistoryAppendFails(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	id, err := s.Create(ctx, testProject, Memory{
		Category: "gotcha", Content: "shipped and archived", Source: "manual", Importance: 0.6,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// In-package, so the raw handle is reachable: this test is about what a
	// failed statement does to the open transaction, and that needs a statement
	// that fails after the UPDATE rather than a fake store.
	if _, err := s.db.ExecContext(ctx, `DROP TABLE memory_history`); err != nil {
		t.Fatalf("drop memory_history: %v", err)
	}

	stamped, err := s.MarkResolved(ctx, testProject, []string{id}, Provenance{})
	if err == nil {
		t.Fatal("MarkResolved reported a stamp whose history row could not be written")
	}
	// The id list is empty on this path, and deliberately: the transaction rolled
	// back, so nothing landed, and a caller that treated the attempted ids as
	// written would be reporting a stamp that is not there.
	if len(stamped) != 0 {
		t.Errorf("MarkResolved returned %v with an error, but a rolled-back transaction wrote nothing", stamped)
	}
	// ResolveCandidates returns the UNRESOLVED rows, so the row must still be in
	// it. Its presence is the proof the UPDATE was rolled back rather than
	// committed: a committed stamp would have taken it out of that set.
	got, err := s.ResolveCandidates(ctx, testProject)
	if err != nil {
		t.Fatalf("ResolveCandidates: %v", err)
	}
	var found bool
	for _, m := range got {
		if m.ID == id {
			found = true
		}
	}
	if !found {
		t.Error("resolved_at survived a failed history append: the stamp and its record are not one transaction")
	}
}

// TestResolvedCandidatesAndClearResolved covers the reassess repair path
// (issue #640): ResolvedCandidates returns exactly the rows resolve stamped,
// in ResolveCandidates' eligibility shape with the resolved_at predicate
// inverted, and ClearResolved returns a memory to ranked injection without
// touching anything else.
func TestResolvedCandidatesAndClearResolved(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	resolved, err := s.Create(ctx, testProject, Memory{
		Category: "gotcha", Content: "NEVER run restore with one spindle", Source: "manual", Importance: 0.6,
	})
	if err != nil {
		t.Fatalf("create resolved: %v", err)
	}
	active, err := s.Create(ctx, testProject, Memory{
		Category: "gotcha", Content: "kill experiment returned NO-GO", Source: "manual", Importance: 0.6,
	})
	if err != nil {
		t.Fatalf("create active: %v", err)
	}
	// Stamped, then pinned: a pin is a user override, so reassess leaves it.
	pinned, err := s.Create(ctx, testProject, Memory{
		Category: "gotcha", Content: "the plugin env vars override --data-dir", Source: "manual", Importance: 0.6,
	})
	if err != nil {
		t.Fatalf("create pinned: %v", err)
	}
	if _, err := s.Create(ctx, testProject, Memory{
		Category: "convention", Content: "never push to main", Source: "manual", Importance: 0.9,
	}); err != nil {
		t.Fatalf("create convention: %v", err)
	}
	if _, err := s.SetResolved(ctx, []string{resolved, pinned}); err != nil {
		t.Fatalf("SetResolved: %v", err)
	}
	if err := s.TogglePin(ctx, pinned, true); err != nil {
		t.Fatalf("pin: %v", err)
	}

	got, err := s.ResolvedCandidates(ctx, testProject)
	if err != nil {
		t.Fatalf("ResolvedCandidates: %v", err)
	}
	if len(got) != 1 || got[0].ID != resolved {
		t.Fatalf("resolved candidates = %v, want exactly [%s]", got, resolved)
	}
	if got[0].ResolvedAt == nil {
		t.Error("ResolvedCandidates must return the stamped resolved_at, or the caller cannot tell what it is undoing")
	}

	// Clearing is project-scoped and only touches already-resolved rows.
	n, err := s.ClearResolved(ctx, testProject, []string{resolved, active, pinned})
	if err != nil {
		t.Fatalf("ClearResolved: %v", err)
	}
	if n != 1 {
		t.Errorf("ClearResolved returned %d, want 1 (only the stamped, unpinned row)", n)
	}
	cands, err := s.ResolveCandidates(ctx, testProject)
	if err != nil {
		t.Fatalf("ResolveCandidates: %v", err)
	}
	found := false
	for _, c := range cands {
		if c.ID == resolved {
			found = true
		}
	}
	if !found {
		t.Error("a cleared memory must be a resolve candidate again")
	}
	if empty, err := s.ClearResolved(ctx, testProject, nil); err != nil || empty != 0 {
		t.Errorf("ClearResolved(nil) = (%d, %v), want a no-op", empty, err)
	}
}

// TestClearResolvedIsAtomic: every batch runs in one transaction, so a caller
// that treats an error as "nothing was repaired" can trust it. A mid-loop
// failure that left earlier batches cleared would report a half-repair as a
// failure (review finding on PR #643).
func TestClearResolvedIsAtomic(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	ids := make([]string, 0, setResolvedBatchSize+5)
	// The first batch carries ordinary content; only the five rows past
	// setResolvedBatchSize get a marker, so a trigger can fail exactly the
	// second batch after the first one's statements have already run.
	for i := 0; i < setResolvedBatchSize+5; i++ {
		content := fmt.Sprintf("kill experiment note %d concluded", i)
		if i >= setResolvedBatchSize {
			content = fmt.Sprintf("kill experiment reassess tail %d concluded", i)
		}
		id, err := s.Create(ctx, testProject, Memory{
			Category: "gotcha", Content: content, Source: "manual", Importance: 0.5,
		})
		if err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
		ids = append(ids, id)
	}
	if _, err := s.SetResolved(ctx, ids); err != nil {
		t.Fatalf("SetResolved: %v", err)
	}

	// Fail on a row of the SECOND batch only, so the first batch's statements
	// have already run when the abort fires and only a rollback can undo them.
	if _, err := s.db.ExecContext(ctx, `CREATE TRIGGER fail_second_batch
		BEFORE UPDATE ON memories WHEN NEW.resolved_at IS NULL AND OLD.content LIKE '%reassess tail%'
		BEGIN SELECT RAISE(ABORT, 'clear refused'); END;`); err != nil {
		t.Fatalf("create trigger: %v", err)
	}

	if _, err := s.ClearResolved(ctx, testProject, ids); err == nil {
		t.Fatal("ClearResolved: want the trigger abort propagated, got nil")
	}

	// Nothing may be cleared: the first batch rolled back with the rest.
	got, err := s.ResolvedCandidates(ctx, testProject)
	if err != nil {
		t.Fatalf("ResolvedCandidates: %v", err)
	}
	if len(got) != len(ids) {
		t.Errorf("resolved rows after a failed clear = %d, want %d — the repair must be all or nothing",
			len(got), len(ids))
	}
}

// TestStoreSupersedeCheckedRoundTrip: MarkSupersedeNeither records
// {newer,older} -> SupersedeCheck and SupersedeChecked reads them back keyed
// by the ordered pair; pairs never marked are absent, and an empty mark call
// is a no-op.
func TestStoreSupersedeCheckedRoundTrip(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	mk := func(content string) string {
		t.Helper()
		id, err := s.Create(ctx, testProject, Memory{
			Category: "gotcha", Content: content, Source: "manual", Importance: 0.6,
		})
		if err != nil {
			t.Fatalf("create %q: %v", content, err)
		}
		return id
	}
	a := mk("prod database is postgres 16")
	b := mk("staging database is postgres 16")
	c := mk("grafana listens on port 80")

	if err := s.MarkSupersedeNeither(ctx, testProject, map[[2]string]SupersedeCheck{}); err != nil {
		t.Fatalf("MarkSupersedeNeither(empty): %v", err)
	}
	got, err := s.SupersedeChecked(ctx, testProject)
	if err != nil {
		t.Fatalf("SupersedeChecked(empty): %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("empty mark wrote %v, want no entries", got)
	}

	marked := map[[2]string]SupersedeCheck{
		{a, b}: {NewerHash: "hash-a", OlderHash: "hash-b"},
		{b, c}: {NewerHash: "hash-b", OlderHash: "hash-c"},
	}
	if err := s.MarkSupersedeNeither(ctx, testProject, marked); err != nil {
		t.Fatalf("MarkSupersedeNeither: %v", err)
	}
	got, err = s.SupersedeChecked(ctx, testProject)
	if err != nil {
		t.Fatalf("SupersedeChecked: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("checked = %v, want 2 entries", got)
	}
	for key, want := range marked {
		if got[key] != want {
			t.Errorf("checked[%v] = %+v, want %+v", key, got[key], want)
		}
	}
}

func TestGetTopMemoriesExcludesResolved(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	activeID, err := s.Create(ctx, testProject, Memory{
		Category: "gotcha", Content: "active gotcha keep", Source: "manual", Importance: 0.6,
	})
	if err != nil {
		t.Fatalf("create active: %v", err)
	}
	resolvedID, err := s.Create(ctx, testProject, Memory{
		Category: "gotcha", Content: "resolved evidence drop", Source: "manual", Importance: 0.9,
	})
	if err != nil {
		t.Fatalf("create resolved: %v", err)
	}
	if _, err := s.SetResolved(ctx, []string{resolvedID}); err != nil {
		t.Fatalf("SetResolved: %v", err)
	}

	// Ranked browse drops the resolved memory even though it has higher importance.
	top, err := s.GetTopMemories(ctx, testProject, 25)
	if err != nil {
		t.Fatalf("GetTopMemories: %v", err)
	}
	for _, m := range top {
		if m.ID == resolvedID {
			t.Errorf("resolved memory %s must not appear in GetTopMemories", resolvedID)
		}
	}
	var sawActive bool
	for _, m := range top {
		if m.ID == activeID {
			sawActive = true
		}
	}
	if !sawActive {
		t.Errorf("active memory %s missing from GetTopMemories", activeID)
	}

	// ...but it is still searchable.
	hits, err := s.SearchFTS(ctx, testProject, "resolved evidence", 10)
	if err != nil {
		t.Fatalf("SearchFTS: %v", err)
	}
	var sawInSearch bool
	for _, m := range hits {
		if m.ID == resolvedID {
			sawInSearch = true
		}
	}
	if !sawInSearch {
		t.Errorf("resolved memory %s must remain searchable", resolvedID)
	}
}

// TestUnresolveOnWrite pins when a resolve verdict is undone: only a content
// change counts as the memory being reasserted. A metadata-only edit (tags,
// importance, category) and an Upsert strengthen (which never overwrites the
// target's content) must both leave resolved_at intact — otherwise a stray
// retag resurrects resolved evidence into ranked injection.
func TestUnresolveOnWrite(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	// A content change reasserts the memory: resolved_at clears.
	id, err := s.Create(ctx, testProject, Memory{
		Category: "gotcha", Content: "resumed via update", Source: "manual", Importance: 0.5,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.SetResolved(ctx, []string{id}); err != nil {
		t.Fatalf("SetResolved: %v", err)
	}
	newContent := "resumed via update — now with more detail"
	if err := s.UpdateMemory(ctx, testProject, id, &newContent, nil, nil, nil); err != nil {
		t.Fatalf("UpdateMemory: %v", err)
	}
	assertActive(t, s, testProject, id)

	// A metadata-only edit does NOT clear it.
	mid, err := s.Create(ctx, testProject, Memory{
		Category: "gotcha", Content: "metadata-only edit target", Source: "manual", Importance: 0.5,
	})
	if err != nil {
		t.Fatalf("create metadata-only target: %v", err)
	}
	if _, err := s.SetResolved(ctx, []string{mid}); err != nil {
		t.Fatalf("SetResolved metadata-only target: %v", err)
	}
	if err := s.UpdateMemory(ctx, testProject, mid, nil, nil, nil, []string{"retagged"}); err != nil {
		t.Fatalf("UpdateMemory metadata-only: %v", err)
	}
	assertResolved(t, s, testProject, mid)

	// Upsert of a near-duplicate (strengthen branch) does NOT clear it.
	uid, _, _, err := s.Upsert(ctx, testProject, "gotcha", "duplicate detection strengthen path here", "manual", 0.5, nil)
	if err != nil {
		t.Fatalf("upsert create: %v", err)
	}
	if _, err := s.SetResolved(ctx, []string{uid}); err != nil {
		t.Fatalf("SetResolved upsert row: %v", err)
	}
	_, dupOf, _, err := s.Upsert(ctx, testProject, "gotcha", "duplicate detection strengthen path here", "manual", 0.5, nil)
	if err != nil {
		t.Fatalf("upsert dup: %v", err)
	}
	if dupOf != uid {
		t.Fatalf("upsert dup did not link to existing row: dupOf=%s want %s", dupOf, uid)
	}
	assertResolved(t, s, testProject, uid)
}

// assertActive fails if the memory's resolved_at is not NULL.
func assertActive(t *testing.T, s *Store, projectID, id string) {
	t.Helper()
	var resolvedAt sql.NullString
	if err := s.db.QueryRow(
		`SELECT resolved_at FROM memories WHERE id = ? AND project_id = ?`, id, projectID,
	).Scan(&resolvedAt); err != nil {
		t.Fatalf("read resolved_at for %s: %v", id, err)
	}
	if resolvedAt.Valid {
		t.Errorf("memory %s should be active (resolved_at NULL), got %q", id, resolvedAt.String)
	}
}

// assertResolved fails if the memory's resolved_at is NULL.
func assertResolved(t *testing.T, s *Store, projectID, id string) {
	t.Helper()
	var resolvedAt sql.NullString
	if err := s.db.QueryRow(
		`SELECT resolved_at FROM memories WHERE id = ? AND project_id = ?`, id, projectID,
	).Scan(&resolvedAt); err != nil {
		t.Fatalf("read resolved_at for %s: %v", id, err)
	}
	if !resolvedAt.Valid {
		t.Errorf("memory %s should stay resolved, got NULL", id)
	}
}

// TestGetTopMemoriesDemotesSuperseded: injection (GetTopMemories) must agree
// with the search path — a superseded memory never outranks its co-present
// replacement. The stale memory is deliberately higher-importance so only the
// supersede demote can flip the order. Runs under limit (no truncation) to
// pin that demote reorders even when the whole window survives.
func TestGetTopMemoriesDemotesSuperseded(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	stale, err := s.Create(ctx, testProject, Memory{
		Category: "fact", Content: "payments service deploys to eu-west-1",
		Source: "manual", Importance: 0.9,
	})
	if err != nil {
		t.Fatalf("create stale: %v", err)
	}
	fresh, err := s.Create(ctx, testProject, Memory{
		Category: "fact", Content: "payments service deploys to us-east-1",
		Source: "manual", Importance: 0.5,
	})
	if err != nil {
		t.Fatalf("create fresh: %v", err)
	}

	top, err := s.GetTopMemories(ctx, testProject, 10)
	if err != nil {
		t.Fatalf("GetTopMemories (before): %v", err)
	}
	if len(top) < 2 || top[0].ID != stale {
		t.Fatalf("baseline must rank stale first; got %+v", top)
	}

	if err := s.CreateLink(ctx, fresh, stale, "supersedes", 0.95, "llm"); err != nil {
		t.Fatalf("CreateLink: %v", err)
	}

	top, err = s.GetTopMemories(ctx, testProject, 10)
	if err != nil {
		t.Fatalf("GetTopMemories (after): %v", err)
	}
	if len(top) < 2 {
		t.Fatalf("demote must not drop results: got %d", len(top))
	}
	if top[0].ID != fresh || top[1].ID != stale {
		t.Errorf("superseded memory must rank below its replacement in injection; got %s then %s (fresh=%s stale=%s)",
			top[0].ID, top[1].ID, fresh, stale)
	}
}

func TestSetDemotionThresholdOverridesDefault(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	a, err := s.Create(ctx, testProject, Memory{Category: "fact", Content: "alpha fact", Source: "manual", Importance: 0.9})
	if err != nil {
		t.Fatalf("create a: %v", err)
	}
	b, err := s.Create(ctx, testProject, Memory{Category: "fact", Content: "alpha fact restated", Source: "manual", Importance: 0.85})
	if err != nil {
		t.Fatalf("create b: %v", err)
	}
	c, err := s.Create(ctx, testProject, Memory{Category: "fact", Content: "distinct fact", Source: "manual", Importance: 0.8})
	if err != nil {
		t.Fatalf("create c: %v", err)
	}

	// Link strength (0.80) clears linking.threshold but sits below the
	// default demotion threshold (0.90) — lowering the override to 0.75
	// must make it demote.
	if err := s.CreateLink(ctx, a, b, "related", 0.80, "auto"); err != nil {
		t.Fatalf("CreateLink: %v", err)
	}
	s.SetDemotionThreshold(0.75)

	top, err := s.GetTopMemories(ctx, testProject, 2)
	if err != nil {
		t.Fatalf("GetTopMemories: %v", err)
	}
	got := map[string]bool{top[0].ID: true, top[1].ID: true}
	if got[b] {
		t.Errorf("lowered threshold should have demoted b; got %+v", top)
	}
	if !got[c] {
		t.Errorf("expected backfill to include c; got %+v", top)
	}
}

// --- Decay floor boundary coverage (issue #282) ---

// setCreatedAtDaysAgo backdates a memory's created_at by the given number of
// days, the same datetime('now', modifier) pattern already used to backdate
// decisions (see TestListDecisionsLimitPicksNewestNotLiveOnly) — Create and
// Upsert always stamp "now", so decay-boundary tests must reach into the DB
// directly to simulate an old memory.
func setCreatedAtDaysAgo(t *testing.T, s *Store, id string, days int) {
	t.Helper()
	if _, err := s.db.Exec(
		`UPDATE memories SET created_at = datetime('now', ?) WHERE id = ?`,
		fmt.Sprintf("-%d days", days), id,
	); err != nil {
		t.Fatalf("backdate created_at for %s: %v", id, err)
	}
}

// memoryScore evaluates the exact shared DecayRankingSQL expression for a
// single memory, so boundary tests exercise the real production formula
// instead of a Go reimplementation of it.
func memoryScore(t *testing.T, s *Store, id string) float64 {
	t.Helper()
	var score float64
	if err := s.db.QueryRow(
		`SELECT (`+DecayRankingSQL+`) FROM memories WHERE id = ?`, id,
	).Scan(&score); err != nil {
		t.Fatalf("score memory %s: %v", id, err)
	}
	return score
}

// memoryCreatedAt reads a memory's raw created_at column.
func memoryCreatedAt(t *testing.T, s *Store, id string) string {
	t.Helper()
	var createdAt string
	if err := s.db.QueryRow(
		`SELECT created_at FROM memories WHERE id = ?`, id,
	).Scan(&createdAt); err != nil {
		t.Fatalf("read created_at for %s: %v", id, err)
	}
	return createdAt
}

// TestDecaySQLParity pins DecayFactor (Go) to the exact DecayRankingSQL
// expression by evaluating both over a grid of categories × ages × pinned.
// This is the drift guard between the SQL constant (GetTopMemories +
// session-start hook) and the Go function (search ranking) — if one is ever
// edited without the other, this test fails.
func TestDecaySQLParity(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	categories := []string{
		"preference", "convention", "fact",
		"pattern", "architecture",
		"decision", "gotcha", "dependency",
	}
	ages := []int{0, 1, 7, 30, 45, 90, 180, 365, 1000}

	const eps = 1e-4 // absorbs ms-level julianday('now') drift vs time.Now()
	for _, cat := range categories {
		for _, age := range ages {
			for _, pinned := range []bool{false, true} {
				content := cat
				if cat == "fact" {
					content = "timeless claim"
				}
				id, err := s.Create(ctx, testProject, Memory{
					Category: cat, Content: content, Source: "manual",
					Importance: 1.0, // importance must be 1 so the SQL result IS the factor
				})
				if err != nil {
					t.Fatalf("create %s/%d/pinned=%v: %v", cat, age, pinned, err)
				}
				setCreatedAtDaysAgo(t, s, id, age)
				if pinned {
					if err := s.TogglePin(ctx, id, true); err != nil {
						t.Fatalf("pin %s/%d: %v", cat, age, err)
					}
				}

				sqlFactor := memoryScore(t, s, id)
				// Recompute age in Go from the same now used by SQLite's
				// julianday('now') at query time (sub-second drift, absorbed
				// by eps).
				goAge := time.Since(memoryCreatedAtAsTime(t, s, id)).Hours() / 24.0
				if goAge < 0 {
					goAge = 0
				}
				goFactor := DecayFactor(cat, content, RetentionProject, pinned, goAge)

				if math.Abs(sqlFactor-goFactor) > eps {
					t.Errorf("category=%s content=%q age=%d pinned=%v: SQL=%v Go=%v (diff %v)",
						cat, content, age, pinned, sqlFactor, goFactor, math.Abs(sqlFactor-goFactor))
				}
			}
		}
	}
	// A dated fact decays on the pattern/architecture curve; the SQL and Go
	// paths must agree on the same heuristic, including where the date sits in
	// the content and on near-misses (a malformed date, non-ASCII digits).
	datedContents := []string{
		"component upgraded on 2026-01-15",
		"2026-01-15 component upgraded",
		"upgraded 2026-01-15",
		"12026-01-15x",
		"2026-01-15",
		"component upgraded on 2026-1-15",
		"component upgraded on 2026/01/15",
		"component upgraded on 20260115",
		"component upgraded on 2026-01-1",
		"component upgraded on \u0662\u0660\u0662\u0666-\u0660\u0661-\u0661\u0665",
		"component upgraded on \uff12\uff10\uff12\uff16-\uff10\uff11-\uff11\uff15",
	}
	for _, content := range datedContents {
		for _, age := range []int{0, 90, 1000} {
			for _, pinned := range []bool{false, true} {
				id, err := s.Create(ctx, testProject, Memory{
					Category: "fact", Content: content, Source: "manual", Importance: 1.0,
				})
				if err != nil {
					t.Fatalf("create fact %q/%d/pinned=%v: %v", content, age, pinned, err)
				}
				setCreatedAtDaysAgo(t, s, id, age)
				if pinned {
					if err := s.TogglePin(ctx, id, true); err != nil {
						t.Fatalf("pin fact %q/%d: %v", content, age, err)
					}
				}
				sqlFactor := memoryScore(t, s, id)
				goAge := time.Since(memoryCreatedAtAsTime(t, s, id)).Hours() / 24.0
				if goAge < 0 {
					goAge = 0
				}
				goFactor := DecayFactor("fact", content, RetentionProject, pinned, goAge)
				if math.Abs(sqlFactor-goFactor) > eps {
					t.Errorf("fact %q age=%d pinned=%v: SQL=%v Go=%v (diff %v)",
						content, age, pinned, sqlFactor, goFactor, math.Abs(sqlFactor-goFactor))
				}
			}
		}
	}
}

// memoryCreatedAtAsTime parses a memory's created_at into a time.Time.
func memoryCreatedAtAsTime(t *testing.T, s *Store, id string) time.Time {
	t.Helper()
	raw := memoryCreatedAt(t, s, id)
	parsed, err := time.Parse("2006-01-02 15:04:05", raw)
	if err != nil {
		t.Fatalf("parse created_at %q: %v", raw, err)
	}
	return parsed
}

// TestGetTopMemoriesCrossCategoryRankInversion confirms a never-decaying
// category can outrank a heavily-decayed one despite a much lower importance
// — fact/preference/convention always score importance*1.0, while gotcha/
// decision/dependency floor at importance*0.15 once past 170 days old.
func TestGetTopMemoriesCrossCategoryRankInversion(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	factID, err := s.Create(ctx, testProject, Memory{
		Category: "fact", Content: "never decays", Source: "manual",
		Importance: 0.4, Tags: []string{},
	})
	if err != nil {
		t.Fatalf("Create fact: %v", err)
	}

	gotchaID, err := s.Create(ctx, testProject, Memory{
		Category: "gotcha", Content: "floored by age", Source: "manual",
		Importance: 1.0, Tags: []string{},
	})
	if err != nil {
		t.Fatalf("Create gotcha: %v", err)
	}
	setCreatedAtDaysAgo(t, s, gotchaID, 200)

	factScore := memoryScore(t, s, factID)
	gotchaScore := memoryScore(t, s, gotchaID)
	if want := 0.4; math.Abs(factScore-want) > 1e-6 {
		t.Errorf("fact score = %v, want %v", factScore, want)
	}
	if want := 0.15; math.Abs(gotchaScore-want) > 1e-6 {
		t.Errorf("gotcha score = %v, want %v", gotchaScore, want)
	}
	if factScore <= gotchaScore {
		t.Fatalf("fact score %v should exceed gotcha score %v despite its much lower importance", factScore, gotchaScore)
	}

	top, err := s.GetTopMemories(ctx, testProject, 2)
	if err != nil {
		t.Fatalf("GetTopMemories: %v", err)
	}
	if len(top) != 2 {
		t.Fatalf("expected 2 memories, got %d", len(top))
	}
	if top[0].ID != factID {
		t.Errorf("expected fact (importance 0.4, no decay) to outrank the 200-day gotcha (importance 1.0, floored); got top[0]=%q", top[0].Content)
	}
}

// TestUpdateMemoryPreservesCreatedAt confirms UpdateMemory never touches
// created_at (it only bumps updated_at — see UpdateMemory in store.go), so it
// cannot be used to reset a memory's decay clock the way ReplaceNonManual
// intentionally does for reflection (see TestReplaceNonManualResetsCreatedAt
// and issue #279, closed as by-design — the two code paths are deliberately
// different, not a case to reconcile).
func TestUpdateMemoryPreservesCreatedAt(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	staleID, err := s.Create(ctx, testProject, Memory{
		Category: "gotcha", Content: "stale gotcha", Source: "manual",
		Importance: 0.5, Tags: []string{},
	})
	if err != nil {
		t.Fatalf("Create stale: %v", err)
	}
	setCreatedAtDaysAgo(t, s, staleID, 15)

	freshID, err := s.Create(ctx, testProject, Memory{
		Category: "gotcha", Content: "fresh gotcha", Source: "manual",
		Importance: 0.5, Tags: []string{},
	})
	if err != nil {
		t.Fatalf("Create fresh: %v", err)
	}

	beforeCreatedAt := memoryCreatedAt(t, s, staleID)
	beforeScore := memoryScore(t, s, staleID)

	top, err := s.GetTopMemories(ctx, testProject, 2)
	if err != nil {
		t.Fatalf("GetTopMemories (before): %v", err)
	}
	if len(top) != 2 || top[0].ID != freshID {
		t.Fatalf("setup invariant broken: fresh memory should outrank the 15-day-old one before update; got %+v", top)
	}

	newContent := "stale gotcha, edited"
	if err := s.UpdateMemory(ctx, testProject, staleID, &newContent, nil, nil, nil); err != nil {
		t.Fatalf("UpdateMemory: %v", err)
	}

	afterCreatedAt := memoryCreatedAt(t, s, staleID)
	if beforeCreatedAt != afterCreatedAt {
		t.Errorf("UpdateMemory changed created_at: before=%q after=%q", beforeCreatedAt, afterCreatedAt)
	}

	// Importance and category are unchanged, so if created_at is also truly
	// unchanged the decayed score should be identical (up to the negligible
	// drift from the real time elapsed between these two queries — bounded
	// well under 1e-4 for a 15-day-old memory; a genuine clock reset would
	// move this score by roughly 0.17, three orders of magnitude more).
	afterScore := memoryScore(t, s, staleID)
	if math.Abs(afterScore-beforeScore) > 1e-4 {
		t.Errorf("UpdateMemory changed the decay score with importance/category unchanged: before=%v after=%v", beforeScore, afterScore)
	}

	top, err = s.GetTopMemories(ctx, testProject, 2)
	if err != nil {
		t.Fatalf("GetTopMemories (after): %v", err)
	}
	if len(top) != 2 || top[0].ID != freshID {
		t.Errorf("UpdateMemory reset the stale memory's decay clock — it no longer ranks below the fresh one: %+v", top)
	}
}

// TestReplaceNonManualResetsCreatedAt confirms reflection-sourced memories
// inserted by ReplaceNonManual get created_at = now (the INSERT never sets
// created_at, so the schema default datetime('now') applies — see
// ReplaceNonManual in store.go). This is documented/intentional behavior per
// issue #279 ("Reflection resets created_at for non-manual memories", closed
// as by-design: "consolidated = refreshed knowledge"). This test confirms
// existing behavior rather than changing it.
func TestReplaceNonManualResetsCreatedAt(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	oldID, err := s.Create(ctx, testProject, Memory{
		Category: "fact", Content: "old reflection fact", Source: "reflection",
		Importance: 0.5, Tags: []string{},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	setCreatedAtDaysAgo(t, s, oldID, 100)
	oldCreatedAt := memoryCreatedAt(t, s, oldID)

	before, err := s.CurrentTimestamp(ctx)
	if err != nil {
		t.Fatalf("CurrentTimestamp: %v", err)
	}

	replacement := []Memory{
		{Category: "fact", Content: "new consolidated fact", Importance: 0.6, Tags: []string{}},
	}
	if _, _, _, _, err := s.ReplaceNonManual(ctx, testProject, replacement, "", nil); err != nil {
		t.Fatalf("ReplaceNonManual: %v", err)
	}

	all, err := s.GetAll(ctx, testProject, 100)
	if err != nil {
		t.Fatalf("GetAll: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("expected exactly 1 memory (old reflection fact replaced), got %d: %+v", len(all), all)
	}
	newMem := all[0]
	if newMem.Content != "new consolidated fact" {
		t.Fatalf("expected replacement memory, got %q", newMem.Content)
	}

	if newMem.CreatedAt < before {
		t.Errorf("new reflection memory created_at = %q, want >= pre-replace timestamp %q (should be fresh, not preserved)", newMem.CreatedAt, before)
	}
	if newMem.CreatedAt == oldCreatedAt {
		t.Errorf("new reflection memory kept the old memory's backdated created_at %q instead of resetting to now", oldCreatedAt)
	}
}

// TestReplaceNonManualSnapshotIDsUnique pins the snapshot-ID collision fix:
// two replaces in the same earlier-second (Unix()) shared one snapshot_id,
// merging distinct pre-replace states and defeating the 3-most-recent prune.
func TestReplaceNonManualSnapshotIDsUnique(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	mems := []Memory{{Category: "fact", Content: "snapshot uniqueness fact", Importance: 0.5, Tags: []string{}}}
	if _, err := s.Create(ctx, testProject, Memory{
		Category: "fact", Content: "seed before first replace", Source: "mcp", Importance: 0.5,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if _, _, _, _, err := s.ReplaceNonManual(ctx, testProject, mems, "", nil); err != nil {
		t.Fatalf("first ReplaceNonManual: %v", err)
	}
	mems2 := []Memory{{Category: "fact", Content: "second generation fact", Importance: 0.5, Tags: []string{}}}
	if _, _, _, _, err := s.ReplaceNonManual(ctx, testProject, mems2, "", nil); err != nil {
		t.Fatalf("second ReplaceNonManual: %v", err)
	}

	var n int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(DISTINCT snapshot_id) FROM memory_snapshots WHERE project_id = ?`, testProject).Scan(&n); err != nil {
		t.Fatalf("count snapshots: %v", err)
	}
	if n != 2 {
		t.Errorf("expected 2 distinct snapshot IDs, got %d (same-second collision)", n)
	}

	// RestoreSnapshot must choose the most recent snapshot deterministically:
	// the state before the second replace (mems), not the first (seed).
	if _, err := s.RestoreSnapshot(ctx, testProject); err != nil {
		t.Fatalf("RestoreSnapshot: %v", err)
	}
	restored, err := s.GetAll(ctx, testProject, 50)
	if err != nil {
		t.Fatalf("GetAll after restore: %v", err)
	}
	var sawFirst, sawSecond bool
	for _, m := range restored {
		switch m.Content {
		case "snapshot uniqueness fact":
			sawFirst = true
		case "second generation fact":
			sawSecond = true
		}
	}
	if !sawFirst || sawSecond {
		t.Errorf("restore picked the wrong snapshot: sawFirst=%v sawSecond=%v", sawFirst, sawSecond)
	}
}

// TestReplaceNonManualPreservesIdentity pins the identity fix: a memory the
// consolidator re-emits with unchanged content used to be deleted and
// re-inserted under a fresh ID, and because memory_embeddings and memory_links
// are ON DELETE CASCADE, every reflection cost that memory its embedding and
// its links (and its access stats).
func TestReplaceNonManualPreservesIdentity(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	targetID, err := s.Create(ctx, testProject, Memory{
		Category: "preference", Content: "manual anchor memory", Source: "manual",
		Importance: 0.8, Tags: []string{},
	})
	if err != nil {
		t.Fatalf("Create manual anchor: %v", err)
	}
	id, err := s.Create(ctx, testProject, Memory{
		Category: "gotcha", Content: "identical content survives", Source: "reflection",
		Importance: 0.5, Tags: []string{},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := s.StoreEmbedding(ctx, id, []float32{1, 0, 0}, "test-model"); err != nil {
		t.Fatalf("StoreEmbedding: %v", err)
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO memory_links (source_id, target_id, relation) VALUES (?, ?, 'related')`,
		id, targetID); err != nil {
		t.Fatalf("insert link: %v", err)
	}

	// Re-emit the same content (category change only, which must be applied).
	replacement := []Memory{
		{Category: "convention", Content: "identical content survives", Importance: 0.9, Tags: []string{}},
	}
	if _, _, _, _, err := s.ReplaceNonManual(ctx, testProject, replacement, "", nil); err != nil {
		t.Fatalf("ReplaceNonManual: %v", err)
	}

	all, err := s.GetAll(ctx, testProject, 100)
	if err != nil {
		t.Fatalf("GetAll: %v", err)
	}
	var got *Memory
	for i := range all {
		if all[i].ID == id {
			got = &all[i]
		}
	}
	if got == nil {
		t.Fatalf("memory %s was not preserved; IDs are now %v", id, memoryIDs(all))
	}
	if got.Content != "identical content survives" {
		t.Errorf("content = %q, want %q", got.Content, "identical content survives")
	}
	if got.Category != "convention" || got.Importance != 0.9 {
		t.Errorf("rewrite not applied: category=%q importance=%v", got.Category, got.Importance)
	}
	if got.Source != "reflection" {
		t.Errorf("source = %q, want reflection", got.Source)
	}

	var embeddings int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM memory_embeddings WHERE memory_id = ?`, id).Scan(&embeddings); err != nil {
		t.Fatalf("count embeddings: %v", err)
	}
	if embeddings != 1 {
		t.Errorf("embedding for %s was cascaded away (count=%d)", id, embeddings)
	}

	var links int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM memory_links WHERE source_id = ? OR target_id = ?`, id, id).Scan(&links); err != nil {
		t.Fatalf("count links: %v", err)
	}
	if links != 1 {
		t.Errorf("link for %s was cascaded away (count=%d)", id, links)
	}
}

func memoryIDs(mems []Memory) []string {
	ids := make([]string, 0, len(mems))
	for _, m := range mems {
		ids = append(ids, m.ID)
	}
	return ids
}

// TestStoreResolveKeptHashesRoundTrip: MarkResolveKept records id -> hash and
// ResolveKeptHashes reads them back; unmarked rows are absent, and an empty mark
// call is a no-op.
func TestStoreResolveKeptHashesRoundTrip(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	var ids []string
	for _, c := range []string{"kill experiment closed, removed", "postmortem concluded, no follow-up", "active gotcha stays"} {
		id, err := s.Create(ctx, testProject, Memory{Category: "gotcha", Content: c, Source: "manual", Importance: 0.6})
		if err != nil {
			t.Fatalf("create %q: %v", c, err)
		}
		ids = append(ids, id)
	}
	a, b, unmarked := ids[0], ids[1], ids[2]

	if err := s.MarkResolveKept(ctx, testProject, map[string]string{}); err != nil {
		t.Fatalf("MarkResolveKept(empty): %v", err)
	}
	if err := s.MarkResolveKept(ctx, testProject, map[string]string{a: "hash-a", b: "hash-b"}); err != nil {
		t.Fatalf("MarkResolveKept: %v", err)
	}
	got, err := s.ResolveKeptHashes(ctx, testProject)
	if err != nil {
		t.Fatalf("ResolveKeptHashes: %v", err)
	}
	if _, ok := got[unmarked]; len(got) != 2 || got[a] != "hash-a" || got[b] != "hash-b" || ok {
		t.Fatalf("hashes = %v, want only {%s: hash-a, %s: hash-b}", got, a, b)
	}
}

// TestStoreSmallAccessors covers the thin readers and setters: the on-save hook,
// the project name list, the category read and the reflect input signature.
func TestStoreSmallAccessors(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	called := make(chan string, 1)
	s.SetOnSave(func(projectID string) { called <- projectID })
	if _, err := s.Create(ctx, testProject, Memory{Category: "fact", Content: "Callback test", Source: "manual", Importance: 0.5}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	select {
	case pid := <-called:
		if pid != testProject {
			t.Errorf("onSave project = %q, want %q", pid, testProject)
		}
	case <-time.After(time.Second):
		t.Error("onSave callback not called after Create")
	}

	if names, err := s.ListProjectNames(ctx); err != nil || len(names) == 0 {
		t.Errorf("ListProjectNames = %v, %v; want the test project", names, err)
	}
	if mems, err := s.GetByCategory(ctx, testProject, "fact", 10); err != nil || len(mems) != 1 {
		t.Errorf("GetByCategory = %d rows, %v; want 1", len(mems), err)
	}

	if got, err := s.GetReflectInputSignature(ctx, testProject); err != nil || got != "" {
		t.Fatalf("initial signature = (%q, %v), want empty", got, err)
	}
	if err := s.SetReflectInputSignature(ctx, testProject, "sig123"); err != nil {
		t.Fatal(err)
	}
	if got, err := s.GetReflectInputSignature(ctx, testProject); err != nil || got != "sig123" {
		t.Fatalf("signature = (%q, %v), want sig123", got, err)
	}
}
