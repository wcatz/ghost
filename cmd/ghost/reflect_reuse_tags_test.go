package main

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"sort"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
	"github.com/wcatz/ghost/internal/reflection"
)

// #730's review finding asked a question this file answers with a running store
// rather than with a reading of the code: can the reflect path's REUSE branch
// change a memory's tags or scope while every column `memory_history` records
// stays byte for byte the same?
//
// The answer decides the shape of the repair. If it cannot, the compaction can
// delete any redundant `reflect` version and say so. If it can, no state-column
// predicate can tell that row from the pre-#727 damage, and the repair has to be
// bounded by something other than the columns — which is what `--before` is for.
//
// The scope half is answered too, and the answer is different: a reflect emission
// cannot change a scope AT ALL, because the field is never copied across. Both
// halves are pinned by tests, because the two conclusions are the whole argument
// and prose is not checkable.

// TestAReflectReuseChangesTagsWithoutChangingAnyRecordedColumn is the half that
// matters, and it runs the real production path end to end: the mechanical
// consolidator, then the conversion the CLI does, then the store's own apply.
//
// The fixture is a pair the SQLite tier must merge — one fact contained in the
// other, so containment scores 1.0 — arranged so the survivor is the row with the
// LONGER text, the SAME category and the SAME importance, while the merge adds a
// tag the survivor does not carry. That is the only arrangement that matters, and
// it is a completely ordinary corpus: equal importance is what most memories save
// with, and a memory that restates a shorter version of a longer one is the
// everyday case consolidation exists for.
//
// What it produces is a reused row whose TAGS moved, with a `reflect` version
// appended for it — and the version records content, category, importance,
// resolved_at and source, none of which moved, because `memory_history` has no
// tags column. #727 was right that the row is a real change and recorded on
// purpose; the point of this test is that `memory_history` cannot see it.
func TestAReflectReuseChangesTagsWithoutChangingAnyRecordedColumn(t *testing.T) {
	s, dbPath := reflectReuseStore(t)
	ctx := context.Background()
	const project = "p1"

	short, longer := seedContainmentPair(t, s, dbPath, project)
	before := mustMemory(t, s, longer)

	// The real pass, in the order the lifecycle runs it: the store's own clock
	// first (that is what marks the older rows as replaceable rather than saved
	// concurrently with the round), then the consolidator, then the conversion,
	// then the apply.
	consolidatedSince, err := s.CurrentTimestamp(ctx)
	if err != nil {
		t.Fatalf("CurrentTimestamp: %v", err)
	}
	result, err := reflection.NewSQLiteConsolidator().Consolidate(ctx, reflection.ReflectionInput{
		ExistingMemories: []memory.Memory{mustMemory(t, s, short), mustMemory(t, s, longer)},
	})
	if err != nil {
		t.Fatalf("SQLiteConsolidator: %v", err)
	}
	if len(result.Memories) != 1 {
		t.Fatalf("the tier emitted %d memories, want 1 (the pair is one fact)", len(result.Memories))
	}
	if _, err := applyReflection(ctx, s, project, result.Memories, nil, consolidatedSince, false, replacedIDsFor(&result), nil); err != nil {
		t.Fatalf("applyReflection: %v", err)
	}

	// The change really happened, on the row that was REUSED rather than replaced:
	// a reuse is the branch that updates a row in place, keeping its id, and it is
	// the branch that files a version for a change this table cannot see.
	after := mustMemory(t, s, longer)
	if after.ID != longer {
		t.Fatalf("the merge replaced the row (%s -> %s) instead of reusing it: "+
			"this fixture no longer exercises the branch the finding is about", longer, after.ID)
	}
	if fmt.Sprint(sortedCopy(after.Tags)) == fmt.Sprint(sortedCopy(before.Tags)) {
		t.Fatalf("the merge did not change the reused row's tags (%v before, %v after): "+
			"this fixture no longer exercises the case the finding is about", before.Tags, after.Tags)
	}
	if after.UpdatedAt == before.UpdatedAt {
		t.Error("the reuse did not move updated_at, so there is no stamp for a repair to take back")
	}

	// And the version it recorded is indistinguishable, from memory_history alone,
	// from the damage #730 removes. This is the whole point, so it is read off the
	// history the writer produced rather than reasoned about.
	if phases := redundantVersionPhases(t, s, longer); len(phases) != 1 || phases[0] != "reflect" {
		t.Fatalf("the reuse filed %v as versions recording nothing, want exactly one byte-identical `reflect` version", phases)
	}
}

// seedContainmentPair writes the two memories whose merge lands on the second one.
// The tag sets are what make the case: the longer row does NOT carry the shorter
// row's tag, so the union the merge builds is a tag the reused row has never had.
func seedContainmentPair(t *testing.T, s *memory.Store, dbPath, project string) (short, longer string) {
	t.Helper()
	ctx := context.Background()
	short, err := s.Create(ctx, project, memory.Memory{
		Category: "pattern", Content: "the relay is reached over the vpn bastion",
		Source: "mcp", Importance: 0.5, Tags: []string{"vpn"},
	})
	if err != nil {
		t.Fatalf("Create the shorter fact: %v", err)
	}
	longer, err = s.Create(ctx, project, memory.Memory{
		Category: "pattern", Content: "the relay is reached over the vpn bastion in staging",
		Source: "mcp", Importance: 0.5, Tags: []string{"bastion"},
	})
	if err != nil {
		t.Fatalf("Create the longer fact: %v", err)
	}
	backdateMemory(t, dbPath, short)
	backdateMemory(t, dbPath, longer)
	return short, longer
}

// redundantVersionPhases lists the phases of the versions that record exactly what
// the row before them records, over every column a version stores — which is the
// compaction's own predicate, read through the store's public history reader.
//
// Spelled out here rather than called, because the predicate is unexported and
// this file is outside the package. That is the point worth recording: every
// reader of this store's history has nothing but these five fields to work with,
// which is why a change to tags or scope cannot be ruled out from the table.
func redundantVersionPhases(t *testing.T, s *memory.Store, memoryID string) []string {
	t.Helper()
	entries, err := s.MemoryHistory(context.Background(), memoryID, 0)
	if err != nil {
		t.Fatalf("MemoryHistory(%s): %v", memoryID, err)
	}
	same := func(a, b memory.HistoryEntry) bool {
		return a.Content == b.Content && a.Category == b.Category && a.Importance == b.Importance &&
			a.Source == b.Source && (a.ResolvedAt == nil) == (b.ResolvedAt == nil) &&
			(a.ResolvedAt == nil || *a.ResolvedAt == *b.ResolvedAt)
	}
	var out []string
	for i := 1; i < len(entries); i++ {
		if same(entries[i], entries[i-1]) {
			out = append(out, entries[i].Phase)
		}
	}
	return out
}

func mustMemory(t *testing.T, s *memory.Store, id string) memory.Memory {
	t.Helper()
	rows, err := s.GetByIDs(context.Background(), []string{id})
	if err != nil || len(rows) != 1 {
		t.Fatalf("GetByIDs(%s): %v (%d rows)", id, err, len(rows))
	}
	return rows[0]
}

// reflectReuseStore opens a FILE-backed store, because the fixtures have to
// backdate a row (below) and the store exposes no write handle for that.
func reflectReuseStore(t *testing.T) (*memory.Store, string) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "reflect.sqlite")
	db, err := memory.OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	s := memory.NewStore(db, nil)
	if err := s.EnsureProject(context.Background(), "p1", "/tmp/p1", "P1"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	return s, dbPath
}

// backdateMemory makes a row older than the round's own `consolidatedSince` stamp.
// Without it a row created in the same second as the run is CONCURRENT with it,
// and ReplaceNonManual keeps concurrent rows as they are instead of reusing them —
// the same reason the in-package reflect tests backdate, and the reason
// datetime('now')'s one-second resolution makes an un-backdated fixture silently
// exercise nothing.
func backdateMemory(t *testing.T, dbPath, id string) {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open %s to backdate: %v", dbPath, err)
	}
	defer db.Close() //nolint:errcheck
	if _, err := db.Exec(
		`UPDATE memories SET created_at = datetime('now', '-1 hour'), updated_at = datetime('now', '-1 hour') WHERE id = ?`,
		id); err != nil {
		t.Fatalf("backdate %s: %v", id, err)
	}
}

func sortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}
