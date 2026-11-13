package memory

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// Issue #727. A consolidation that re-emits a memory verbatim — the explicit
// `keep`, the pass-through of a memory the response never named, and
// RetainGuardedDrops' re-add of a memory the drop guard rescued — reaches
// ReplaceNonManual's exact-content reuse. That path used to write the row's own
// values back and set updated_at = datetime('now'), and the pass then appended a
// `reflect` history row for every one of them.
//
// On a real store that was measured at 2,669 of 3,340 history rows (80%) being
// re-emissions, one memory holding 17 versions of itself, and both caps evicting
// real events (a save, an update, a supersede) within days. It also made
// updated_at mean "the last reflect that saw this row", which is what supersede
// orients a candidate pair by and what --skip-unchanged fingerprints a project
// with.
//
// A verbatim re-emission is a NO-OP: nothing about the row changes, so nothing is
// written. The run that saw it is already in lifecycle.log, and "which run
// touched it" is not state.

// keptReemit is the emission reflection sends for a memory it carried through:
// internal/reflection's verbatimMemory copies these four fields, turns nil tags
// into an empty list, and leaves scope unset (a keep must not restate a
// machine-readable scope the harness never saw). Read back out of the store, so
// the fixture is what a real `keep` produces rather than what the test assumed
// a keep produces.
func keptReemit(t *testing.T, s *Store, id string) Memory {
	t.Helper()
	rows, err := s.GetByIDs(context.Background(), []string{id})
	if err != nil || len(rows) != 1 {
		t.Fatalf("GetByIDs(%s): %v (%d rows)", id, err, len(rows))
	}
	m := rows[0]
	tags := m.Tags
	if tags == nil {
		tags = []string{}
	}
	return Memory{Category: m.Category, Content: m.Content, Importance: m.Importance, Tags: tags}
}

// historyRowCount reads the TABLE, not the reader: MemoryHistory defaults to the
// per-memory cap, so a read cannot tell a prune that ran from one that did not,
// and neither can it tell a row this pass appended from one an earlier one did.
func historyRowCount(t *testing.T, s *Store, memoryID string) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT count(*) FROM memory_history WHERE memory_id = ?`, memoryID).Scan(&n); err != nil {
		t.Fatalf("count history rows for %s: %v", memoryID, err)
	}
	return n
}

func phaseCounts(t *testing.T, s *Store, memoryID string) map[string]int {
	t.Helper()
	rows, err := s.db.Query(`SELECT phase, count(*) FROM memory_history WHERE memory_id = ? GROUP BY phase`, memoryID)
	if err != nil {
		t.Fatalf("phase counts for %s: %v", memoryID, err)
	}
	defer rows.Close() //nolint:errcheck
	out := map[string]int{}
	for rows.Next() {
		var phase string
		var n int
		if err := rows.Scan(&phase, &n); err != nil {
			t.Fatalf("scan phase counts: %v", err)
		}
		out[phase] = n
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate phase counts: %v", err)
	}
	return out
}

// memoryUpdatedAt reads the raw column, because the whole point is that it did
// not move.
func memoryUpdatedAt(t *testing.T, s *Store, id string) string {
	t.Helper()
	var updatedAt string
	if err := s.db.QueryRow(`SELECT updated_at FROM memories WHERE id = ?`, id).Scan(&updatedAt); err != nil {
		t.Fatalf("read updated_at for %s: %v", id, err)
	}
	return updatedAt
}

// seedVerbatimCorpus stores three memories a consolidator can carry through
// unchanged: distinct text (so consolidation's near-duplicate fold is not what
// this exercises), and between them every field the reuse UPDATE writes, so a
// predicate that forgot one is visible in the result rather than in a fixture
// that never exercised it.
//
// Two of the three are saved with NO tags, which is the case that makes the
// comparison a value comparison: the column holds "null", while the keep path
// (internal/reflection's verbatimMemory) normalises nil to an empty list and
// the emission marshals to "[]". Both read back as the same empty list, so both
// must read as unchanged.
func seedVerbatimCorpus(t *testing.T, s *Store) []string {
	t.Helper()
	ctx := context.Background()
	var ids []string
	for _, m := range []Memory{
		{Category: "architecture", Content: "the block producer writes its KES epochs under /var/lib/kes", Source: "mcp", Importance: 0.6, Tags: []string{"kes", "bp"}},
		{Category: "gotcha", Content: "the cardano-node metrics port is 12798 and stays off the public interface", Source: "mcp", Importance: 0.5},
		{Category: "fact", Content: "unit tests run with the race detector on linux only", Source: "mcp", Importance: 0.7, Scope: map[string]string{"environment": "ci"}},
	} {
		id, err := s.Create(ctx, testProject, m)
		if err != nil {
			t.Fatalf("create %q: %v", m.Content, err)
		}
		setAgesDaysAgo(t, s, id, 30)
		ids = append(ids, id)
	}
	return ids
}

// setAgesDaysAgo backdates BOTH timestamps. Only created_at is enough to make a
// row replaceable, but updated_at has to move too or an assertion that it was
// bumped is comparing two reads inside one second — and datetime('now') has
// one-second resolution, so "did this write move the timestamp" cannot be asked
// of a row stamped a moment ago.
func setAgesDaysAgo(t *testing.T, s *Store, id string, days int) {
	t.Helper()
	if _, err := s.db.Exec(
		`UPDATE memories SET created_at = datetime('now', ?), updated_at = datetime('now', ?) WHERE id = ?`,
		fmt.Sprintf("-%d days", days), fmt.Sprintf("-%d days", days), id,
	); err != nil {
		t.Fatalf("backdate %s: %v", id, err)
	}
}

// TestReplaceNonManualVerbatimReemissionWritesNothing is #727 at the store: an
// all-keep apply must leave the corpus and the change log exactly as it found
// them. Not "no extra row for a row that already had one" — no row at all, and
// no movement in updated_at, because those are the two things that flooded
// memory_history and made the timestamp mean "the last reflect".
func TestReplaceNonManualVerbatimReemissionWritesNothing(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	ids := seedVerbatimCorpus(t, s)

	before := map[string]string{}
	beforeRows := map[string]int{}
	for _, id := range ids {
		before[id] = memoryUpdatedAt(t, s, id)
		beforeRows[id] = historyRowCount(t, s, id)
		if beforeRows[id] != 1 {
			t.Fatalf("fixture: %s has %d history rows, want the one its save wrote", id, beforeRows[id])
		}
	}

	emitted := make([]Memory, 0, len(ids))
	for _, id := range ids {
		emitted = append(emitted, keptReemit(t, s, id))
	}
	if _, _, _, _, err := s.ReplaceNonManual(ctx, testProject, emitted, "", nil); err != nil {
		t.Fatalf("ReplaceNonManual: %v", err)
	}

	for _, id := range ids {
		if got := historyRowCount(t, s, id); got != beforeRows[id] {
			t.Errorf("%s: history rows %d -> %d (%v) — a verbatim re-emission wrote a version of a row it did not change",
				id, beforeRows[id], got, phaseCounts(t, s, id))
		}
		if got := memoryUpdatedAt(t, s, id); got != before[id] {
			t.Errorf("%s: updated_at %q -> %q — a verbatim re-emission stamped the row as touched, "+
				"which is what supersede orients a pair by", id, before[id], got)
		}
	}
	var total int
	if err := s.db.QueryRow(`SELECT count(*) FROM memory_history WHERE phase = ?`, phaseReflect).Scan(&total); err != nil {
		t.Fatalf("count reflect rows: %v", err)
	}
	if total != 0 {
		t.Errorf("the apply wrote %d reflect history row(s), want none", total)
	}
}

// TestReplaceNonManualAStrengthenedRowKeptVerbatimIsStillANoOp is #750, the
// precision half of #727's rule, and it needed a value the earlier corpus never
// produced.
//
// Memory.Importance is a float32 and the column is a float64. Upsert's strengthen
// is `SET importance = MIN(1.0, importance + ?)` with `importance*0.2` bound as a
// float32, so the add happens in SQLite's own float64 and a folded row usually
// holds a value float32 cannot name — the column float32(0.55) is
// 0.550000011920929, the bound increment widens to 0.11000000685453415, and their
// sum lands at 0.6600000187754631. Reading the row back narrows it to a float32
// (scanMemories), so the keep path re-emits 0.6600000262260437, the no-op
// predicate compared that against the column's 0.6600000187754631, called the row
// changed, and every applied reflect wrote a byte-identical version of it and
// stamped it touched — the exact behaviour #727 removed, restored for 15 of
// 1,170 memories on the store it was measured on. A corpus that never folds
// looks fine, which is why #727's tests did too.
//
// The fix is to compare at the precision the emission carries
// (float32(stored) == emitted.Importance): narrowing is the one lossy direction,
// so it is exact for a row that really is unchanged, and the companion test
// below is the other half — a strengthened row that IS reweighted still takes
// the write branch.
func TestReplaceNonManualAStrengthenedRowKeptVerbatimIsStillANoOp(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	const content = "the release tag is signed, not the release commit"
	// 0.55 is chosen because this fold's strengthen result is one float32
	// cannot represent, so the fixture reaches the state under test without a
	// hand-written column value. Nothing here may assert that by hand: the
	// value has to come from the writer that produces it.
	if _, _, _, err := s.Upsert(ctx, testProject, "convention", content, "mcp", 0.55, nil); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	// The same sentence re-cased, folded into the stored row and strengthened
	// there. FoldOnly is the shape reflection's _global promotion uses, and the
	// strengthen UPDATE it runs is the one the default fold runs too (that
	// statement sits outside FoldOnly's own block), so a project row lands on a
	// non-representable importance through an ordinary save just as this one
	// does.
	id, dupOf, _, err := s.UpsertWithOptions(ctx, testProject, "convention",
		"The Release Tag Is Signed, Not The Release Commit",
		"reflection", 0.55, nil, UpsertOptions{FoldOnly: true})
	if err != nil {
		t.Fatalf("fold-only Upsert: %v", err)
	}
	if dupOf != id {
		t.Fatalf("the fold returned id %q as duplicate-of %q; it did not fold onto one row", id, dupOf)
	}

	// The fixture is only this test if the column really does hold a value
	// float32 cannot name. Asserted rather than assumed, because a strength
	// formula that stopped producing such values would otherwise leave the
	// test passing for the wrong reason: the comparison it is about would be
	// the one #727 already got right.
	stored := storedImportance(t, s, id)
	if stored == float64(float32(stored)) {
		t.Fatalf("fixture: the fold left importance at %v, which float32 represents exactly — "+
			"this test needs a value that does not survive the round trip", stored)
	}
	setAgesDaysAgo(t, s, id, 30)
	beforeRows := historyRowCount(t, s, id)
	beforePhases := phaseCounts(t, s, id)
	if beforeRows != 2 || beforePhases[phaseSave] != 1 || beforePhases[phaseMerge] != 1 {
		t.Fatalf("fixture: %s has %d history rows %v, want the save and the fold", id, beforeRows, beforePhases)
	}
	updatedAtBefore := memoryUpdatedAt(t, s, id)

	if _, _, _, _, err := s.ReplaceNonManual(ctx, testProject, []Memory{keptReemit(t, s, id)}, "", nil); err != nil {
		t.Fatalf("ReplaceNonManual: %v", err)
	}

	// The phases and not only the count: a no-op that dropped the fold's merge
	// row and appended a reflect one would hold the count at 2, and that merge
	// row is the record of the very strengthen this test exists to be quiet
	// about.
	if got := phaseCounts(t, s, id); !equalPhases(got, beforePhases) {
		t.Errorf("history phases %v -> %v: a verbatim keep rewrote the log of a row it did not change",
			beforePhases, got)
	}
	if got := historyRowCount(t, s, id); got != beforeRows {
		t.Errorf("history rows %d -> %d (%v): a verbatim keep of a strengthened row appended a "+
			"version of a row it did not change", beforeRows, got, phaseCounts(t, s, id))
	}
	if got := memoryUpdatedAt(t, s, id); got != updatedAtBefore {
		t.Errorf("updated_at %q -> %q: a verbatim keep stamped the row as touched, which is what "+
			"supersede orients a pair by", updatedAtBefore, got)
	}
	// The column itself is the sharpest statement of "nothing was written": a
	// write would bind the emitted float32 back into the column and replace
	// the fold's float64 with a different number, so this row would read as
	// though the fold had never happened.
	if got := storedImportance(t, s, id); got != stored {
		t.Errorf("importance %v -> %v: the keep rewrote the column, so a fold's exact value does not "+
			"survive a run that changed nothing", stored, got)
	}
}

// storedImportance reads the raw column, not the Memory, because the whole
// subject is the value the widening to float64 carries and float32 does not.
func storedImportance(t *testing.T, s *Store, id string) float64 {
	t.Helper()
	var importance float64
	if err := s.db.QueryRow(`SELECT importance FROM memories WHERE id = ?`, id).Scan(&importance); err != nil {
		t.Fatalf("read importance for %s: %v", id, err)
	}
	return importance
}

// equalPhases compares two phase histograms for equality, for the assertion that
// a no-op left the log alone rather than swapping one row for another.
func equalPhases(got, want map[string]int) bool {
	if len(got) != len(want) {
		return false
	}
	for phase, n := range want {
		if got[phase] != n {
			return false
		}
	}
	return true
}

// TestReplaceNonManualEveryRecordedFieldBreaksTheNoOp is the no-op predicate's
// own boundary, one case per field it reads. A predicate that forgot a field
// would report "unchanged" for a pass that changed the row, and the symptom is
// silent: the value is written, the timestamp is not, and the log says the row
// never moved. Each case changes exactly ONE field, so a failure names it.
//
// Two assertions per case, and both are load-bearing. The field must LAND in the
// row, or the case is not testing the write. And the pass must RECORD it: a
// future edit that applies a field without appending a version would satisfy the
// first assertion alone, and the recorded row is the reason the value can be
// trusted after the next overwrite.
//
// content is the one case that does not reuse its row — a changed text is a
// different memory and cannot be a reuse of the stored one — so it is asserted
// on the successor the rewrite inserts, which is where its single reflect row
// belongs.
func TestReplaceNonManualEveryRecordedFieldBreaksTheNoOp(t *testing.T) {
	for _, tc := range []struct {
		name string
		// pick is which of the seeded rows the case changes: the first carries
		// tags, the second is untagged, the third carries a scope.
		pick   int
		mutate func(m *Memory)
		want   func(got Memory) bool
	}{
		{
			name:   "content",
			pick:   0,
			mutate: func(m *Memory) { m.Content += " (restated)" },
			want:   func(got Memory) bool { return strings.HasSuffix(got.Content, " (restated)") },
		},
		{
			name:   "category",
			pick:   0,
			mutate: func(m *Memory) { m.Category = "convention" },
			want:   func(got Memory) bool { return got.Category == "convention" },
		},
		{
			name:   "importance",
			pick:   0,
			mutate: func(m *Memory) { m.Importance = 0.11 },
			want:   func(got Memory) bool { return got.Importance == 0.11 },
		},
		{
			name:   "tags added",
			pick:   0,
			mutate: func(m *Memory) { m.Tags = []string{"kes", "bp", "rotation"} },
			want:   func(got Memory) bool { return len(got.Tags) == 3 },
		},
		{
			name:   "tags dropped",
			pick:   0,
			mutate: func(m *Memory) { m.Tags = nil },
			want:   func(got Memory) bool { return len(got.Tags) == 0 },
		},
		{
			name:   "scope stated",
			pick:   2,
			mutate: func(m *Memory) { m.Scope = map[string]string{"environment": "staging"} },
			want:   func(got Memory) bool { return got.Scope["environment"] == "staging" },
		},
		{
			// The scope of a row that HAS one, restated as something else. This
			// is the case the COALESCE in the reuse UPDATE makes load-bearing: an
			// emission that states no scope leaves the column alone, and one that
			// states a scope rewrites it. Neither is a no-op when the stated value
			// differs.
			name:   "scope rewritten",
			pick:   2,
			mutate: func(m *Memory) { m.Scope = map[string]string{"environment": "production"} },
			want:   func(got Memory) bool { return got.Scope["environment"] == "production" },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := testStore(t)
			ctx := context.Background()
			ids := seedVerbatimCorpus(t, s)
			target := ids[tc.pick]
			beforeRows := historyRowCount(t, s, target)

			emitted := []Memory{keptReemit(t, s, target)}
			tc.mutate(&emitted[0])
			for _, id := range ids {
				if id == target {
					continue
				}
				emitted = append(emitted, keptReemit(t, s, id))
			}
			if _, _, _, _, err := s.ReplaceNonManual(ctx, testProject, emitted, "", nil); err != nil {
				t.Fatalf("ReplaceNonManual: %v", err)
			}

			// Where the change landed, which is the target's own row for every
			// field except content.
			var got Memory
			recordedID := target
			if tc.name == "content" {
				got = mustGetOneByContent(t, s, emitted[0].Content)
				recordedID = got.ID
			} else {
				got = mustGetOneByID(t, s, target)
			}
			if !tc.want(got) {
				t.Errorf("the changed field was not applied: %+v", got)
			}
			// "Exactly once" is the property, and it means different counts for the
			// two shapes. A content change leaves the old id a tombstone and inserts
			// a row that has never been in the log, so the successor's single row IS
			// the event. Every other field updates the reused row in place, which
			// already carried its save row, so the event is the one row ADDED. A
			// rule that recorded the same change twice would be as wrong here as one
			// that recorded it never, which is the defect this fixes.
			wantRows := beforeRows + 1
			if recordedID != target {
				wantRows = 1
			}
			if n := historyRowCount(t, s, recordedID); n != wantRows {
				t.Errorf("the changed row has %d history rows, want %d (%v): a change to %s is a real "+
					"update and must be recorded exactly once", n, wantRows, phaseCounts(t, s, recordedID), tc.name)
			}
		})
	}
}

// mustGetOneByID reads one stored row or fails, for assertions that are about a
// specific memory rather than about a set.
func mustGetOneByID(t *testing.T, s *Store, id string) Memory {
	t.Helper()
	rows, err := s.GetByIDs(context.Background(), []string{id})
	if err != nil {
		t.Fatalf("GetByIDs(%s): %v", id, err)
	}
	if len(rows) != 1 {
		t.Fatalf("GetByIDs(%s) returned %d rows, want 1", id, len(rows))
	}
	return rows[0]
}

// mustGetOneByContent finds the live row holding an exact text, for the case
// where the write under test gave the memory a new id.
func mustGetOneByContent(t *testing.T, s *Store, content string) Memory {
	t.Helper()
	var id string
	if err := s.db.QueryRow(
		`SELECT id FROM memories WHERE content = ? AND project_id = ?`, content, testProject,
	).Scan(&id); err != nil {
		t.Fatalf("no live row holds %q: %v", content, err)
	}
	return mustGetOneByID(t, s, id)
}
