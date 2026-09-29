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
	if _, err := s.ReplaceNonManual(ctx, testProject, emitted, ""); err != nil {
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
// computes `MIN(1.0, importance + importance * 0.2)` in SQLite's own float64, so
// a folded row usually holds a value float32 cannot name — 0.55 + 0.11 lands at
// 0.6600000187754631, and widening float32(0.55) is 0.550000011920929. Reading
// the row back narrows it to a float32 (scanMemories), so the keep path re-emits
// 0.6600000262260437, the no-op predicate compared that against the column's
// 0.6600000187754631, called the row changed, and every applied reflect wrote a
// byte-identical version of it and stamped it touched — the exact behaviour
// #727 removed, restored for 15 of 1,170 memories on the store it was measured
// on. A corpus that never folds looks fine, which is why #727's tests did too.
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
	// 0.55 is chosen because 0.55 + 0.55*0.2 is one of the strengthen results
	// that float32 cannot represent, so the fixture reaches the state under
	// test without a hand-written column value. Nothing here may assert that
	// by hand: the value has to come from the writer that produces it.
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

	if _, err := s.ReplaceNonManual(ctx, testProject, []Memory{keptReemit(t, s, id)}, ""); err != nil {
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

// TestReplaceNonManualAStrengthenedRowReweightedIsStillAChange is the other half
// of #750, and the half the fix could have cost. Narrowing the stored value to
// the emission's precision is a lossy direction, so the question it has to answer
// is whether it can hide a reweight — and the answer has to be asserted on a
// STRENGTHENED row, at a distance of ONE ULP. Every other importance case in this
// file reweights a value Create wrote, which is a float32 already and therefore
// float32-exact, so all of them keep passing under a comparator loose enough to be
// wrong: an epsilon, or any threshold, swallows the reweight below and no other
// test here would notice. A strengthened row is the only fixture where a loosened
// comparison shows, which is why this one is built through the same two writers as
// the test above.
//
// So the reweight is the next representable float32 above the stored value, not a
// comfortable step: a real change at the smallest distance the emission can
// express, which the exact comparison catches and nothing else would.
func TestReplaceNonManualAStrengthenedRowReweightedIsStillAChange(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	const content = "the epoch rotation is driven by the KES operational cert"
	if _, _, _, err := s.Upsert(ctx, testProject, "architecture", content, "mcp", 0.55, nil); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	id, dupOf, _, err := s.UpsertWithOptions(ctx, testProject, "architecture",
		"The Epoch Rotation Is Driven By The KES Operational Cert",
		"reflection", 0.55, nil, UpsertOptions{FoldOnly: true})
	if err != nil {
		t.Fatalf("fold-only Upsert: %v", err)
	}
	if dupOf != id {
		t.Fatalf("the fold returned id %q as duplicate-of %q; it did not fold onto one row", id, dupOf)
	}
	if stored := storedImportance(t, s, id); stored == float64(float32(stored)) {
		t.Fatalf("fixture: the fold left importance at %v, which float32 represents exactly — "+
			"this test needs a value that does not survive the round trip", stored)
	}
	setAgesDaysAgo(t, s, id, 30)
	beforeRows := historyRowCount(t, s, id)
	updatedAtBefore := memoryUpdatedAt(t, s, id)

	// One float32 ULP above what the fold left: 0.6600001 widens to
	// 0.66000008583068848, the representable value immediately above the
	// stored 0.6600000262260437, so it is a rating a caller CAN state and a
	// different one by the only measure every reader of this column uses. It is
	// deliberately the smallest change the fix could conceivably hide rather
	// than a comfortable 0.2, because a comparator loosened to an epsilon — the
	// obvious way to "fix" this — swallows this value and would swallow every
	// importance case in this file, since the others all reweight a Create'd
	// value that was float32-exact to begin with. This is the assertion that
	// says the comparison is exact.
	const reweight float32 = 0.6600001
	emitted := keptReemit(t, s, id)
	emitted.Importance = reweight
	if _, err := s.ReplaceNonManual(ctx, testProject, []Memory{emitted}, ""); err != nil {
		t.Fatalf("ReplaceNonManual: %v", err)
	}

	row := mustGetOneByID(t, s, id)
	if row.Importance != reweight {
		t.Errorf("importance = %v, want the reweight %v applied in place — the narrowed comparison "+
			"swallowed a real change to a strengthened row", row.Importance, reweight)
	}
	if got := historyRowCount(t, s, id); got != beforeRows+1 {
		t.Errorf("history rows %d -> %d (%v), want exactly one more: a reweight of a strengthened "+
			"row is a real update", beforeRows, got, phaseCounts(t, s, id))
	}
	entries := mustHistory(t, s, id)
	if got := phasesOf(t, entries); !equalStrings(got, []string{phaseSave + ":", phaseMerge + ":", phaseReflect + ":"}) {
		t.Errorf("phases = %v, want [save merge reflect]", got)
	}
	if got := entries[len(entries)-1].Importance; got != float64(reweight) {
		t.Errorf("the appended row records importance %v, want %v — the version must be the row as "+
			"the write left it", got, float64(reweight))
	}
	if got := memoryUpdatedAt(t, s, id); got == updatedAtBefore {
		t.Errorf("updated_at is still %q after a real reweight; supersede orients a pair by it", got)
	}
	if got := memoryCreatedAt(t, s, id); got != updatedAtBefore {
		t.Errorf("created_at = %q, want the stored %q — a reweight is not refreshed knowledge (#279)",
			got, updatedAtBefore)
	}
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

// TestReplaceNonManualRepeatedAllKeepApplyIsStable is the same rule measured
// over rounds, which is where the measurement in #727 was taken: the lifecycle
// runs every lifecycle.min_interval, so the question is not whether ONE apply is
// quiet but whether a corpus left alone stays quiet.
func TestReplaceNonManualRepeatedAllKeepApplyIsStable(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	ids := seedVerbatimCorpus(t, s)

	for round := range 5 {
		emitted := make([]Memory, 0, len(ids))
		for _, id := range ids {
			emitted = append(emitted, keptReemit(t, s, id))
		}
		if _, err := s.ReplaceNonManual(ctx, testProject, emitted, ""); err != nil {
			t.Fatalf("ReplaceNonManual round %d: %v", round, err)
		}
	}
	for _, id := range ids {
		if got := historyRowCount(t, s, id); got != 1 {
			t.Errorf("%s: %d history rows after 5 all-keep applies, want the 1 its save wrote", id, got)
		}
	}
}

// TestReplaceNonManualGenuineRewriteStillAppendsOneRow is the other half of the
// rule, and the reason it is not "append nothing": a rewrite is a real change, it
// mints a new id, and the row the new id holds has never been in the log. One
// row, on the new id — and the old id keeps its save and its tombstone.
func TestReplaceNonManualGenuineRewriteStillAppendsOneRow(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	// A one-memory corpus, so the rewrite is the only thing this pass can do and
	// the successor is identifiable without a content search. The rest of the
	// pass-through corpus is covered by the all-keep test above.
	const content = "the epoch rotation is driven by the KES operational cert"
	const rewritten = "epoch rotation is driven by the KES operational certificate, not the genesis one"
	id, err := s.Create(ctx, testProject, Memory{
		Category: "architecture", Content: content, Source: "mcp", Importance: 0.6,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	setAgesDaysAgo(t, s, id, 30)

	emitted := keptReemit(t, s, id)
	emitted.Content = rewritten
	if _, err := s.ReplaceNonManual(ctx, testProject, []Memory{emitted}, ""); err != nil {
		t.Fatalf("ReplaceNonManual: %v", err)
	}

	if live, err := s.GetByIDs(ctx, []string{id}); err != nil || len(live) != 0 {
		t.Fatalf("the rewritten row kept its id (%d rows); a text change cannot be a reuse", len(live))
	}
	successor := theOnlyMemoryID(t, s, testProject)
	if successor == id {
		t.Fatal("the rewrite reused the old id")
	}
	if got := historyRowCount(t, s, successor); got != 1 {
		t.Errorf("the rewritten memory has %d history rows, want exactly 1 (%v)", got, phaseCounts(t, s, successor))
	}
	if got := phasesOf(t, mustHistory(t, s, successor)); !equalStrings(got, []string{"reflect:"}) {
		t.Errorf("successor phases = %v, want [reflect]", got)
	}
	// And the version the new id holds is the row as the write left it, which is
	// the whole reason the successor gets one.
	if got := mustHistory(t, s, successor)[0].Content; got != rewritten {
		t.Errorf("the appended row records %q, want the rewritten text", got)
	}
	if got := phasesOf(t, mustHistory(t, s, id)); !equalStrings(got, []string{"save:", "delete:"}) {
		t.Errorf("rewritten id phases = %v, want [save delete]", got)
	}
}

// TestReplaceNonManualImportanceOnlyChangeAppendsOneRow is the field the issue
// names: the consolidator may reweight a memory whose text and category it left
// exactly as they were, and a reweight is a real update — it changes what the
// row says about itself, it moves the decay ranking, and it is a version the log
// must be able to show. So: one row, updated_at bumped, created_at still the
// stored one (#279 does not apply to a row whose wording did not move).
func TestReplaceNonManualImportanceOnlyChangeAppendsOneRow(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	ids := seedVerbatimCorpus(t, s)
	target := ids[0]
	createdAt := memoryCreatedAt(t, s, target)
	beforeRows := historyRowCount(t, s, target)
	updatedAtBefore := memoryUpdatedAt(t, s, target)

	// A float32 literal, because Memory.Importance is one: the column holds the
	// exact widening of this value, and comparing the column against the
	// untyped 0.95 would be comparing two different numbers.
	const reweight float32 = 0.95
	emitted := []Memory{keptReemit(t, s, target)}
	emitted[0].Importance = reweight
	for _, id := range ids[1:] {
		emitted = append(emitted, keptReemit(t, s, id))
	}

	if _, err := s.ReplaceNonManual(ctx, testProject, emitted, ""); err != nil {
		t.Fatalf("ReplaceNonManual: %v", err)
	}

	rows, err := s.GetByIDs(ctx, []string{target})
	if err != nil || len(rows) != 1 {
		t.Fatalf("GetByIDs: %v", err)
	}
	if rows[0].Importance != reweight {
		t.Fatalf("importance = %v, want the reweight applied", rows[0].Importance)
	}
	if got := historyRowCount(t, s, target); got != beforeRows+1 {
		t.Errorf("history rows %d -> %d (%v), want exactly one more", beforeRows, got, phaseCounts(t, s, target))
	}
	entries := mustHistory(t, s, target)
	if got := phasesOf(t, entries); !equalStrings(got, []string{"save:", "reflect:"}) {
		t.Errorf("phases = %v, want [save reflect]", got)
	}
	if entries[len(entries)-1].Importance != float64(reweight) {
		t.Errorf("the appended row records importance %v, want %v — the version must be the row as the write left it",
			entries[len(entries)-1].Importance, float64(reweight))
	}
	if got := memoryUpdatedAt(t, s, target); got == updatedAtBefore {
		t.Errorf("updated_at is still %q after a real update; supersede orients a pair by it", got)
	}
	if got := memoryCreatedAt(t, s, target); got != createdAt {
		t.Errorf("created_at = %q, want the stored %q — a reweight is not refreshed knowledge", got, createdAt)
	}
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
			if _, err := s.ReplaceNonManual(ctx, testProject, emitted, ""); err != nil {
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

// TestReplaceNonManualAnUnreadableStoredValueIsTreatedAsChanged: the predicate
// compares what it can read, and a value it cannot read is a change. A tags
// column holding something that is not a JSON list must not read as "the same
// tags as the emission stated" and silently suppress a write that would have
// replaced it — the safe direction is the loud one.
func TestReplaceNonManualAnUnreadableStoredValueIsTreatedAsChanged(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	id, err := s.Create(ctx, testProject, Memory{
		Category: "fact", Content: "a memory whose tags column is hand-broken", Source: "mcp", Importance: 0.5, Tags: []string{"a"},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.db.Exec(`UPDATE memories SET tags = 'not json' WHERE id = ?`, id); err != nil {
		t.Fatalf("break the tags column: %v", err)
	}
	beforeRows := historyRowCount(t, s, id)

	emitted := []Memory{keptReemit(t, s, id)}
	emitted[0].Tags = []string{"a"}
	if _, err := s.ReplaceNonManual(ctx, testProject, emitted, ""); err != nil {
		t.Fatalf("ReplaceNonManual: %v", err)
	}
	if n := historyRowCount(t, s, id); n != beforeRows+1 {
		t.Errorf("history rows %d -> %d: an unreadable tags column was read as unchanged, so the repair went unrecorded",
			beforeRows, n)
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

func mustHistory(t *testing.T, s *Store, id string) []HistoryEntry {
	t.Helper()
	entries, err := s.MemoryHistory(context.Background(), id, 0)
	if err != nil {
		t.Fatalf("MemoryHistory(%s): %v", id, err)
	}
	return entries
}
