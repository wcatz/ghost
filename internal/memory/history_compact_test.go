package memory

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"testing"
)

// Issue #730, at the layer the issue is filed against. #727 stopped a verbatim
// consolidation re-emission from appending a byte-identical `reflect` version;
// these are the tests for the command that removes the ones already stored.
//
// The fixtures build the damaged history DIRECTLY rather than through writers, and
// the one writer that would produce it no longer exists. The verbatim re-emission
// is a no-op since #727, so there is no call left to make, and a test that drove
// one would assert the fix rather than the repair. The damage is dated, too: every
// fixture below states the instant it wrote its rows at, because a row's recorded_at
// is one of the three things that decides whether it is removable (the cut is a
// time), and a fixture leaning on datetime('now') would stop being damaged the day
// the wall clock passed the cutoff. The two instants below are the two sides of
// that cut.

// firstVersionText and secondVersionText are the two states the shared fixture
// moves between, so an assertion can name which version survived rather than
// counting rows and hoping.
const (
	compactFirstText  = "the relay listens on port 2222 in staging"
	compactSecondText = "the relay listens on port 2222 in production"
)

// preFixReflectAt is when the restatements in this file were "written": a month
// before #727 reached main, so every one of them is older than reflectNoOpCutoff
// and the compaction is looking at exactly the rows it is meant to. A test that
// needs a row on the other side of the cut passes its own instant and says why.
//
// postFixReflectAt is the other side: an hour after the fix landed, which is what
// "a row a current build wrote" looks like to the repair. It is a fixed instant
// rather than the clock, because a fixture reading datetime('now') would depend on
// where the cutoff sits relative to the day the suite runs — and the cutoff is a
// historical fact, so a test about it should not be.
const (
	preFixReflectAt  = "2026-08-01 09:00:00"
	postFixReflectAt = "2026-09-28 18:00:00"
)

// appendVersionRow records ONE history row the way a write path does: the state
// columns are copied out of the live memories row, so a caller cannot record a
// state its memory never held. Only the parts a fixture has to choose differ
// between rows — the phase, when it was recorded, the event's other end, and
// optionally overrides of the recorded state itself.
//
// recordedAt nil takes the schema's datetime('now') default, which is what a
// real writer leaves behind. stateEdits then rewrite individual recorded
// columns ON THE ROW, which is how a fixture builds the pair of adjacent rows
// that differ in exactly one column.
func appendVersionRow(t *testing.T, s *Store, memoryID, phase string, recordedAt any,
	relatedID, mergedContent string, stateEdits map[string]any) {
	t.Helper()
	res, err := s.db.Exec(`
		INSERT INTO memory_history
			(memory_id, project_id, phase, recorded_at, related_id, merged_content,
			 content, category, importance, resolved_at, source)
		SELECT id, project_id, ?, COALESCE(?, datetime('now')), ?, ?,
		       content, category, importance, resolved_at, source
		FROM memories WHERE id = ?`,
		phase, recordedAt, nullIfEmpty(relatedID), nullIfEmpty(mergedContent), memoryID)
	if err != nil {
		t.Fatalf("append %s version row: %v", phase, err)
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		t.Fatalf("append %s version row: %d rows (err %v), want 1", phase, n, err)
	}
	if len(stateEdits) == 0 {
		return
	}
	sets := make([]string, 0, len(stateEdits))
	args := make([]any, 0, len(stateEdits)+1)
	for col, v := range stateEdits {
		sets = append(sets, col+" = ?")
		args = append(args, v)
	}
	args = append(args, lastRowID(t, s))
	if _, err := s.db.Exec(`UPDATE memory_history SET `+strings.Join(sets, ", ")+` WHERE rowid = ?`, args...); err != nil {
		t.Fatalf("override recorded state of the last version row: %v", err)
	}
}

// appendVerbatimVersion is the damage in one call: a version row that restates
// the memory's current state exactly, which is what every applied reflection
// used to leave behind for every memory it kept, recorded at preFixReflectAt
// because that is when such a row was written and a row's age is part of what
// makes it removable.
func appendVerbatimVersion(t *testing.T, s *Store, memoryID string) {
	t.Helper()
	appendVersionRow(t, s, memoryID, phaseReflect, preFixReflectAt, "", "", nil)
}

func lastRowID(t *testing.T, s *Store) int64 {
	t.Helper()
	var rowid int64
	if err := s.db.QueryRow(`SELECT max(rowid) FROM memory_history`).Scan(&rowid); err != nil {
		t.Fatalf("read newest history rowid: %v", err)
	}
	return rowid
}

// createCompactMemory is one saved memory, the first version every history in
// this file starts with.
func createCompactMemory(t *testing.T, s *Store, text string) string {
	t.Helper()
	id, err := s.Create(context.Background(), testProject, Memory{
		Category: "architecture", Content: text, Source: "mcp", Importance: 0.4,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return id
}

// seedDamagedHistory builds the exact sequence the issue names: a save, twenty
// identical reflects, a real edit, five more identical reflects and a resolve.
// It returns the memory id and the timestamps it placed on the two real changes,
// because --fix-updated-at is asserted against those and not against "roughly
// now".
func seedDamagedHistory(t *testing.T, s *Store) (id string, savedAt, updatedAt, resolvedAt string) {
	t.Helper()
	ctx := context.Background()
	id = createCompactMemory(t, s, compactFirstText)
	for i := 0; i < 20; i++ {
		appendVerbatimVersion(t, s, id)
	}
	if err := s.UpdateMemory(ctx, testProject, id, strPtr(compactSecondText), nil, nil, nil); err != nil {
		t.Fatalf("UpdateMemory: %v", err)
	}
	for i := 0; i < 5; i++ {
		appendVerbatimVersion(t, s, id)
	}
	if _, err := s.SetResolved(ctx, []string{id}); err != nil {
		t.Fatalf("SetResolved: %v", err)
	}

	// recorded_at has one-second resolution and a fixture runs in one second,
	// so the three real events would otherwise be indistinguishable and
	// --fix-updated-at could be asserted against any of them. Stamping them
	// apart is what makes the assertion about WHICH event the answer comes
	// from rather than about the clock.
	savedAt, updatedAt, resolvedAt = "2026-01-01 10:00:00", "2026-01-01 11:00:00", "2026-01-01 12:00:00"
	stampHistoryRow(t, s, id, phaseSave, savedAt)
	stampHistoryRow(t, s, id, phaseUpdate, updatedAt)
	stampHistoryRow(t, s, id, phaseResolve, resolvedAt)
	return id, savedAt, updatedAt, resolvedAt
}

// seedStampedHistory is the shape the updated_at DAMAGE actually takes, which is
// not seedDamagedHistory's: for a stamp to be past the last real change, the
// reflect run that moved it has to come AFTER that change. So a save, an edit, a
// resolve, and then `runs` identical reflect versions recorded in the run's own
// window, with the memory's updated_at at the end of it.
//
// seedDamagedHistory is the issue's own ordering and the right shape for the
// DELETION tests; this is the right shape for the updated_at ones, and the
// difference is the whole of the difference between the two repairs.
func seedStampedHistory(t *testing.T, s *Store, runs int) (id, savedAt, updatedAt, resolvedAt, runTo string) {
	t.Helper()
	ctx := context.Background()
	savedAt, updatedAt, resolvedAt = "2026-01-01 10:00:00", "2026-01-01 11:00:00", "2026-01-01 12:00:00"
	id = createCompactMemory(t, s, compactFirstText)
	if err := s.UpdateMemory(ctx, testProject, id, strPtr(compactSecondText), nil, nil, nil); err != nil {
		t.Fatalf("UpdateMemory: %v", err)
	}
	if _, err := s.SetResolved(ctx, []string{id}); err != nil {
		t.Fatalf("SetResolved: %v", err)
	}
	stampHistoryRow(t, s, id, phaseSave, savedAt)
	stampHistoryRow(t, s, id, phaseUpdate, updatedAt)
	stampHistoryRow(t, s, id, phaseResolve, resolvedAt)

	// The run's versions, a minute apart, and the LAST of them is the one the
	// damage left as the memory's updated_at.
	for i := range runs {
		appendVersionRow(t, s, id, phaseReflect, fmt.Sprintf("2026-01-01 13:%02d:00", i), "", "", nil)
	}
	runTo = fmt.Sprintf("2026-01-01 13:%02d:00", runs-1)
	setUpdatedAt(t, s, id, runTo)
	return id, savedAt, updatedAt, resolvedAt, runTo
}

// stampPhaseRows moves EVERY row of one phase to a fixed instant, which a fixture
// needs when a phase's rows all have to sit on the same side of a bound. It is not
// stampHistoryRow: that one moves the newest row of a phase, so calling it twice
// for two rows of the same phase restamps the same row twice and leaves the other
// where the clock put it.
func stampPhaseRows(t *testing.T, s *Store, memoryID, phase, recordedAt string) {
	t.Helper()
	if _, err := s.db.Exec(
		`UPDATE memory_history SET recorded_at = ? WHERE memory_id = ? AND phase = ?`,
		recordedAt, memoryID, phase); err != nil {
		t.Fatalf("stamp the %s rows of %s at %q: %v", phase, memoryID, recordedAt, err)
	}
}

// versionsAtOrBefore counts a memory's versions of ONE PHASE recorded at or before
// a bound. It is the side of the bound a fixture has to be able to state, and
// asserting it is what turns "the test failed" into "the fixture put the row on the
// wrong side" — without it the same three assertions fail for a reason the reader
// cannot see.
//
// The phase is a parameter because an unscoped count is a false alarm waiting to
// happen: it would also count the save row, whose stamp is whatever the clock said,
// and the compaction never looks at that row. A guard that trips on a row the repair
// does not read is a guard about the clock, which is the fragility this exists to
// remove. Scoped, it says exactly the thing it is there to say.
func versionsAtOrBefore(t *testing.T, s *Store, memoryID, phase, cutoff string) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(
		`SELECT count(*) FROM memory_history WHERE memory_id = ? AND phase = ? AND recorded_at <= ?`,
		memoryID, phase, cutoff).Scan(&n); err != nil {
		t.Fatalf("read how many of %s's %s versions sit at or before %q: %v", memoryID, phase, cutoff, err)
	}
	return n
}

// stampHistoryRow moves the newest row of the given phase to a fixed instant,
// which is how a fixture separates events a single-second clock cannot.
func stampHistoryRow(t *testing.T, s *Store, memoryID, phase, recordedAt string) {
	t.Helper()
	if _, err := s.db.Exec(`
		UPDATE memory_history SET recorded_at = ?
		WHERE rowid = (SELECT max(rowid) FROM memory_history WHERE memory_id = ? AND phase = ?)`,
		recordedAt, memoryID, phase); err != nil {
		t.Fatalf("stamp the %s row of %s: %v", phase, memoryID, err)
	}
}

// setUpdatedAt writes a memory's freshness stamp directly, so a fixture can
// stage the damage --fix-updated-at exists to repair: a reflect that changed
// nothing still moved updated_at to its own run's time.
func setUpdatedAt(t *testing.T, s *Store, memoryID, stamp string) {
	t.Helper()
	if _, err := s.db.Exec(`UPDATE memories SET updated_at = ? WHERE id = ?`, stamp, memoryID); err != nil {
		t.Fatalf("set updated_at of %s: %v", memoryID, err)
	}
}

func readUpdatedAt(t *testing.T, s *Store, memoryID string) string {
	t.Helper()
	var v string
	if err := s.db.QueryRow(`SELECT updated_at FROM memories WHERE id = ?`, memoryID).Scan(&v); err != nil {
		t.Fatalf("read updated_at of %s: %v", memoryID, err)
	}
	return v
}

// TestCompactHistoryDoesNotRewindADeliberateBumpUnderARemovableRow is the
// blocker, and it is the one the other findings left open: they both made a
// DELIBERATE writer's row non-removable, and neither asked whether a non-removable
// row is an ANCHOR.
//
// The gate in restorableStamp is "a removable version sits ABOVE the anchor", and
// the anchor used to be the newest version that CHANGED its state, found by negating
// the state comparison. That cannot see a row recording the state of its
// predecessor, whatever else is true of it — and a deliberate writer's row is always
// one of those. A tags-only UpdateMemory bumps updated_at on purpose and records a
// state-identical `update` version; ReplaceNonManual's reuse branch does the same
// with a `reflect` one. Neither moves a column this table has, so the anchor skipped
// over both, and the pre-#727 no-op reflect row beneath them read as evidence of a
// rewind over a change the store made on purpose.
//
// The interleaving is the finding, which is why this is not the tags-only test
// above: there, the two `update` rows are the memory's NEWEST versions, so the phase
// allowlist alone kept them and the gate never opened. Here the retag is the newest
// row and a pre-#727 no-op reflect sits between the save and it.
//
// ONE shape, and it is the only one a shipped writer produces. A second was drafted
// here — a state-identical `reflect` version carrying related_id, described as the
// importance fold's version — and no writer emits it: the fold in Upsert files
// phaseMerge with the folded text and moves no updated_at, and related_id is set on
// exactly three phases, none of them reflect (delete, supersede, unsupersede). A
// fixture staging a shape no writer produces pins the predicate against a fiction,
// and the fiction is what the next reader takes as the reason. The real
// related_id and merged_content rows are covered by
// TestCompactHistoryStillRepairsAMemoryThatWasSupersededAfterwards, which is where
// they belong: they are evidence that those rows must NOT be an anchor.
func TestCompactHistoryDoesNotRewindADeliberateBumpUnderARemovableRow(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	id := createCompactMemory(t, s, compactFirstText)

	// The save at an instant of its own, then the damage: a byte-identical reflect
	// version from before #727. Nothing after the save is a state change, so under
	// the old rule the save WAS the last state change and the no-op reflect sat
	// above it as textbook evidence.
	stampHistoryRow(t, s, id, phaseSave, "2026-01-01 10:00:00")
	appendVersionRow(t, s, id, phaseReflect, preFixReflectAt, "", "", nil)

	// The deliberate bump, after the damage and above it, at an instant of its own
	// so a rewind is unambiguous.
	if err := s.UpdateMemory(ctx, testProject, id, nil, nil, nil,
		[]string{"retag", "deliberate"}); err != nil {
		t.Fatalf("UpdateMemory: %v", err)
	}
	deliberate := "2026-05-01 12:00:00"
	setUpdatedAt(t, s, id, deliberate)

	// The preconditions, asserted rather than assumed. The retag's version being
	// state-identical is the whole finding, and its being non-removable on nothing
	// but its phase is what makes this the case rather than a near neighbour: a
	// fixture whose deliberate row were spared by two clauses would still pass a fix
	// that only addressed one of them.
	if n := historyRowCount(t, s, id); n != 3 {
		t.Fatalf("history rows = %d, want 3 (the save, the no-op reflect, the deliberate bump)", n)
	}
	if n := redundantVersionCount(t, s, id); n != 2 {
		t.Fatalf("%d versions restate their predecessor byte for byte, want 2: the finding needs a "+
			"row no state column can tell from a no-op", n)
	}
	if n := versionsAtOrBefore(t, s, id, phaseReflect, reflectNoOpCutoff); n != 1 {
		t.Fatalf("%d reflect version(s) at or before the bound, want 1: the retag's own version is "+
			"phase update, so the pre-#727 row is the only one this repair owns", n)
	}
	if n := versionExemptOnlyBy(t, s, id, "phase", reflectNoOpCutoff); n != 1 {
		t.Fatalf("%d version(s) are exempt on nothing but their phase, want 1: the fixture has to "+
			"stage a row the phase alone has to hold back", n)
	}

	res, err := s.CompactHistory(ctx, testProject, HistoryCompactOptions{Apply: true, FixUpdatedAt: true})
	if err != nil {
		t.Fatalf("CompactHistory: %v", err)
	}
	if res.Removed != 1 {
		t.Errorf("Removed = %d, want 1: the pre-#727 no-op reflect is real damage whether or not a "+
			"deliberate bump sits above it", res.Removed)
	}
	if res.UpdatedAt != 0 {
		t.Errorf("UpdatedAt = %d, want 0: the only version above the last real change is one a writer "+
			"filed on purpose, so nothing here is evidence that a reflection moved the stamp", res.UpdatedAt)
	}
	if got := readUpdatedAt(t, s, id); got != deliberate {
		t.Errorf("updated_at = %q, want the deliberate writer's own %q: a row this repair would not "+
			"remove is not a row it may read as a no-op", got, deliberate)
	}
}

// The two instants the anchor tests below share: the save whose recorded_at is the
// answer, and the state change whose recorded_at is NOT. A fixture whose two events
// sat at one instant could not tell the two apart, and one-second resolution plus a
// suite that runs inside one second is exactly how that happens by accident.
const (
	anchorSavedAt  = "2026-01-01 10:00:00"
	anchorChangeAt = "2026-01-01 11:00:00"
	anchorDamageTo = "2026-08-01 09:30:00"
)

// stateChangeIsReal fails a fixture whose writer did not actually change a recorded
// column, because a state-identical row is a DIFFERENT case and a test that stages
// it while claiming to stage the other pins the wrong predicate.
//
// It is the check the previous rounds' fixtures were missing. Each of them staged
// its inert phase's version with the state columns copied out of the live row, so
// the row read as byte-identical — which is what a `supersede` is and what a
// `resolve` is not. `SetResolved` writes resolved_at and `Upsert`'s fold writes
// importance, and a version row is a SNAPSHOT of the live row taken after the
// write, so a fixture driving the real writer gets both for free.
func stateChangeIsReal(t *testing.T, s *Store, memoryID, phase string) {
	t.Helper()
	var identical int
	if err := s.db.QueryRow(`
		SELECT count(*) FROM memory_history cur
		WHERE cur.memory_id = ? AND cur.phase = ? AND `+historyEqualPredecessorSQL("cur"),
		memoryID, phase).Scan(&identical); err != nil {
		t.Fatalf("read whether the %s version changed anything: %v", phase, err)
	}
	if identical != 0 {
		t.Fatalf("the %s version records exactly what the row before it records, so the fixture is "+
			"staging a byte-identical row and not the state change it claims to be", phase)
	}
}

// dropAllHistory takes a memory's whole recorded past away, which is what a
// pre-v17 memory looks like: migrateV17 records no starting row, so a store
// upgraded from before the history table holds memories whose first recorded
// version is whatever the next writer happened to file.
func dropAllHistory(t *testing.T, s *Store, memoryID string) {
	t.Helper()
	if _, err := s.db.Exec(`DELETE FROM memory_history WHERE memory_id = ?`, memoryID); err != nil {
		t.Fatalf("drop the recorded history of %s: %v", memoryID, err)
	}
}

// setResolvedAt stamps a memory resolved without filing a version, which is the
// state an upgraded store is in: a build old enough not to record resolved_at's
// write is also old enough to predate the table.
func setResolvedAt(t *testing.T, s *Store, memoryID, stamp string) {
	t.Helper()
	if _, err := s.db.Exec(`UPDATE memories SET resolved_at = ? WHERE id = ?`, stamp, memoryID); err != nil {
		t.Fatalf("stamp %s resolved at %q: %v", memoryID, stamp, err)
	}
}

// TestCompactHistoryFixUpdatedAtAnchorsOnAStampMoveNotOnAStateChange: the
// anchor is a KEPT version whose WRITER moved updated_at, and this is the finding
// that the writer is the whole of it.
//
// The anchor used to accept any version that CHANGED state. A change of state is
// not a change of stamp, and the two writers here prove it: `SetResolved` writes
// resolved_at and says in as many words that it leaves updated_at alone, and
// `Upsert`'s fold writes importance and moves nothing. Under the state-change
// reading either one became the answer, and the repair set a memory's stamp to an
// instant the store never held on either column — a fresh value, invented, in a
// column `ghost supersede` orients candidates by.
//
// The measured damage on a real store was 49 of 288 restored stamps, so this is
// not a corner: a corpus that runs `ghost resolve` and saves duplicates folds
// constantly, and both of those land above the last save.
func TestCompactHistoryFixUpdatedAtAnchorsOnAStampMoveNotOnAStateChange(t *testing.T) {
	// Same words, different case — the only pair `FoldOnly` folds, and
	// fold_only_test.go pins that equivalence. A restatement further from the
	// stored text would be inserted as its own row and file no merge version.
	const restated = "The Relay Listens On Port 2222 In Staging"

	for _, tc := range []struct {
		name  string
		phase string
		apply func(t *testing.T, s *Store, id string)
	}{
		{
			name:  "a resolve",
			phase: phaseResolve,
			apply: func(t *testing.T, s *Store, id string) {
				t.Helper()
				n, err := s.SetResolved(context.Background(), []string{id})
				if err != nil || n != 1 {
					t.Fatalf("SetResolved: %d rows (err %v), want 1 — the fixture needs a real resolve", n, err)
				}
			},
		},
		{
			name:  "an unsaturated fold",
			phase: phaseMerge,
			apply: func(t *testing.T, s *Store, id string) {
				t.Helper()
				got, dupOf, _, err := s.UpsertWithOptions(context.Background(), testProject, "architecture",
					restated, "mcp", 0.6, nil, UpsertOptions{FoldOnly: true})
				if err != nil {
					t.Fatalf("FoldOnly Upsert: %v", err)
				}
				if got != id || dupOf != id {
					t.Fatalf("FoldOnly returned (%q, %q), want the stored memory %q twice — the fixture is "+
						"not exercising the fold", got, dupOf, id)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := testStore(t)
			ctx := context.Background()
			id := createCompactMemory(t, s, compactFirstText)

			stampHistoryRow(t, s, id, phaseSave, anchorSavedAt)
			tc.apply(t, s, id)
			stampHistoryRow(t, s, id, tc.phase, anchorChangeAt)
			// The precondition, read rather than assumed. A resolve that resolved
			// nothing, or a fold whose MIN(1.0, …) landed back on the stored
			// importance, would file a state-identical version and quietly turn
			// this into a different test that still passed.
			stateChangeIsReal(t, s, id, tc.phase)

			// The damage the run is here for: pre-#727 no-op reflect versions above
			// the state change, and a stamp at the end of that run's window. Three
			// rows for two removals, because the newest version is the memory's own
			// statement of what it says and no guard in this file will take it — so
			// the two under it are removable only because it is there. Two rows
			// would leave one removable row UNDER the state change, and that is the
			// shape where the old rule's answer came from closing the gate rather
			// than from answering wrongly; the finding is about the answer, so the
			// fixture has to leave the gate open under both readings.
			appendVerbatimVersion(t, s, id)
			appendVerbatimVersion(t, s, id)
			appendVerbatimVersion(t, s, id)
			setUpdatedAt(t, s, id, anchorDamageTo)

			if n := redundantVersionCount(t, s, id); n != 3 {
				t.Fatalf("%d versions restate their predecessor byte for byte, want 3: the reflect "+
					"versions copy the live row, which the %s left holding the state they record", n, tc.phase)
			}
			if !stampGateIsOpen(t, s, id, reflectNoOpCutoff) {
				t.Fatal("the gate is closed before the repair runs, so the fixture is not staging the damage")
			}

			res, err := s.CompactHistory(ctx, testProject,
				HistoryCompactOptions{Apply: true, FixUpdatedAt: true})
			if err != nil {
				t.Fatalf("CompactHistory: %v", err)
			}
			if res.Removed != 2 {
				t.Errorf("Removed = %d, want 2: the two byte-identical reflect versions are the damage "+
					"whether or not an inert writer filed something above them", res.Removed)
			}
			if res.UpdatedAt != 1 {
				t.Errorf("UpdatedAt = %d, want 1: a %s version moves no live memory's updated_at, so it "+
					"cannot be the thing that moved the stamp", res.UpdatedAt, tc.phase)
			}
			// The load-bearing assertion, and the whole finding: the answer is the
			// SAVE's own instant. Naming the one the rule must not produce keeps the
			// failure legible — a stamp restored to anchorChangeAt is a stamp set to
			// a time memories.updated_at never held.
			if got := readUpdatedAt(t, s, id); got != anchorSavedAt {
				t.Errorf("updated_at = %q, want the save's own %q and NOT the %s's %q: a writer that "+
					"changed state without moving the stamp is not the last change to the stamp",
					got, anchorSavedAt, tc.phase, anchorChangeAt)
			}
		})
	}
}

// TestCompactHistoryLeavesAStampWithNoRecordedStampWrite: a memory with no kept
// version whose writer moved the stamp has no anchor at all, and the honest answer
// is to leave the stamp alone and say so.
//
// It is reachable, and not narrowly. `memory_history` records no updated_at, so the
// only evidence a stamp write happened is a version whose PHASE is one whose writer
// moves it — and a pre-v17 memory (migrateV17 files no starting row) whose next
// writer was `ClearResolved` has exactly that: an unresolve version and nothing
// else. Under the state-change reading the unresolve passed trivially, as a first
// version has no predecessor to compare against, and the repair restored a stamp to
// an instant the store had never recorded on any column. One real memory in the
// measured store had seven no-op reflects above that row and a stamp "restored" to
// the moment the row was written.
func TestCompactHistoryLeavesAStampWithNoRecordedStampWrite(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	id := createCompactMemory(t, s, compactFirstText)

	// A pre-v17 memory, resolved by a build that recorded nothing, and then
	// unresolved by the current one.
	dropAllHistory(t, s, id)
	setResolvedAt(t, s, id, "2025-12-01 09:00:00")
	n, err := s.ClearResolved(ctx, testProject, []string{id})
	if err != nil || n != 1 {
		t.Fatalf("ClearResolved: %d rows (err %v), want 1 — the fixture needs a real unresolve", n, err)
	}
	stampHistoryRow(t, s, id, phaseUnresolve, anchorChangeAt)
	appendVerbatimVersion(t, s, id)
	appendVerbatimVersion(t, s, id)
	appendVerbatimVersion(t, s, id)
	setUpdatedAt(t, s, id, anchorDamageTo)

	// The unresolve really is the first version, which is what makes the case
	// reachable at all: a first version has no predecessor, so under the old rule
	// the state comparison declared every first version a change.
	if first, err := firstVersionPhase(t, s, id); err != nil {
		t.Fatal(err)
	} else if first != phaseUnresolve {
		t.Fatalf("the memory's first version is a %s, want the unresolve — the fixture is not the "+
			"pre-v17 case this is about", first)
	}
	if n := redundantVersionCount(t, s, id); n != 3 {
		t.Fatalf("%d versions restate their predecessor byte for byte, want 3", n)
	}

	res, err := s.CompactHistory(ctx, testProject, HistoryCompactOptions{Apply: true, FixUpdatedAt: true})
	if err != nil {
		t.Fatalf("CompactHistory: %v", err)
	}
	// The damage is still real damage and still goes: the version removal does not
	// depend on there being a stamp to move.
	if res.Removed != 2 {
		t.Errorf("Removed = %d, want 2: the two no-op reflect versions are removable whatever the stamp "+
			"can be restored to", res.Removed)
	}
	if res.UpdatedAt != 0 {
		t.Errorf("UpdatedAt = %d, want 0: nothing in this memory's history was written by a writer that "+
			"moves the stamp, so there is no instant to restore it to", res.UpdatedAt)
	}
	// Counted, not skipped in silence. A stamp left reading as a reflect run's time
	// is still wrong, and a report whose numbers read "nothing left to do" about
	// one is a report that says the repair finished.
	if res.StampsUnrecorded != 1 {
		t.Errorf("StampsUnrecorded = %d, want 1: the memory is left with a stamp nothing in its history "+
			"explains, and an operator is told so", res.StampsUnrecorded)
	}
	if res.StampsUnreadable != 0 {
		t.Errorf("StampsUnreadable = %d, want 0: nothing here is unreadable, there is simply no recorded "+
			"stamp write to read a time from", res.StampsUnreadable)
	}
	if got := readUpdatedAt(t, s, id); got != anchorDamageTo {
		t.Errorf("updated_at = %q, want it untouched at %q: an anchor the history cannot supply is not a "+
			"reason to invent one", got, anchorDamageTo)
	}
}

// firstVersionPhase reads a memory's oldest recorded phase, for the fixtures whose
// whole point is what its FIRST row is.
func firstVersionPhase(t *testing.T, s *Store, memoryID string) (string, error) {
	t.Helper()
	var phase string
	err := s.db.QueryRow(
		`SELECT phase FROM memory_history WHERE memory_id = ? ORDER BY rowid LIMIT 1`, memoryID).Scan(&phase)
	if err != nil {
		return "", fmt.Errorf("read %s's first version phase: %w", memoryID, err)
	}
	return phase, nil
}

// TestCompactHistoryFixUpdatedAtOnlyMovesBackward is the direction rule's own
// regression test, and it is here because a mutation to `!current.After(target)` —
// dropping the case where the stamp is EXACTLY the target — left the whole suite
// green. Every fixture in this file moves a stamp strictly forward before the
// repair runs, so the boundary was never the thing under test.
//
// The claim is that the damage moved the stamp forward and the repair undoes that,
// never inventing a freshness Ghost never recorded. A stamp already at the target
// is not damage, and a stamp BEFORE it is not damage either: it is a writer that
// moved it without filing a history row, a clock that ran ahead, or a row some
// other tool edited. Both are left exactly as they are.
func TestCompactHistoryFixUpdatedAtOnlyMovesBackward(t *testing.T) {
	for _, tc := range []struct {
		name       string
		current    string
		wantFixed  int64
		wantRemain string
	}{
		{
			name:       "a stamp forward of the last real change is taken back to it",
			current:    "2026-03-01 09:00:00",
			wantFixed:  1,
			wantRemain: directionLastRealChange,
		},
		{
			// This is the case the mutation that passed the whole suite would have
			// broken and no fixture exercised: `current.Equal(target)` is false here,
			// so the mutated rule takes the stamp FORWARD from a value the store
			// already had behind the change. Every other fixture in this file moves
			// a stamp strictly forward, so the mutated rule agreed with the real one
			// on all of them.
			name:       "a stamp already behind the last real change is left alone",
			current:    "2026-01-15 10:00:00",
			wantFixed:  0,
			wantRemain: "2026-01-15 10:00:00",
		},
		{
			name:       "a stamp exactly at the last real change is left alone",
			current:    directionLastRealChange,
			wantFixed:  0,
			wantRemain: directionLastRealChange,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := testStore(t)
			ctx := context.Background()
			id := createCompactMemory(t, s, compactFirstText)

			// A save, a real edit, then TWO no-op reflect versions above it. Both
			// counts are forced, and each one was got wrong on the way here:
			//
			//   - The edit has to MOVE a state column, or it is not a real change
			//     and the anchor never moves off the save.
			//   - There have to be TWO of them. A single reflect version is the
			//     memory's newest row, the newest-version guard spares it, and there
			//     is no removable row at all — the gate is closed before direction is
			//     ever read. This is also the shape the damage really takes: a
			//     reflect run's LAST version is always its memory's newest row, which
			//     is why the anchor is the newest row this repair will not remove
			//     rather than the newest row that changed state.
			// The edit goes through the real writer, so the live row moves with it
			// and the reflect versions below copy a state their memory actually
			// held. Staging the edit's version with an override instead leaves the
			// LIVE row saying something else, and the next reflect version then
			// differs from the edit rather than restating it — a fixture that looks
			// like the damage and is not.
			stampHistoryRow(t, s, id, phaseSave, "2026-01-01 10:00:00")
			if err := s.UpdateMemory(ctx, testProject, id, strPtr(compactSecondText), nil, nil, nil); err != nil {
				t.Fatalf("UpdateMemory: %v", err)
			}
			stampHistoryRow(t, s, id, phaseUpdate, directionLastRealChange)
			appendVerbatimVersion(t, s, id)
			appendVerbatimVersion(t, s, id)
			setUpdatedAt(t, s, id, tc.current)

			// The gate has to be open or none of this is about direction. It is
			// checked as the gate and not as restorableStamp's whole verdict, because
			// two of the three cases are about the repair declining a stamp it COULD
			// have moved — the direction rule is the only thing under test here, and
			// asserting the decision as well would fail the two cases that are
			// supposed to decline.
			if open := stampGateIsOpen(t, s, id, reflectNoOpCutoff); !open {
				t.Fatalf("the fixture's gate is closed, so it tests nothing about direction")
			}

			res, err := s.CompactHistory(ctx, testProject,
				HistoryCompactOptions{Apply: true, FixUpdatedAt: true})
			if err != nil {
				t.Fatalf("CompactHistory: %v", err)
			}
			if res.UpdatedAt != tc.wantFixed {
				t.Errorf("UpdatedAt = %d, want %d", res.UpdatedAt, tc.wantFixed)
			}
			if got := readUpdatedAt(t, s, id); got != tc.wantRemain {
				t.Errorf("updated_at = %q, want %q — the repair only ever moves a stamp BACKWARD",
					got, tc.wantRemain)
			}
		})
	}
}

// directionLastRealChange is the instant of the edit every case in
// TestCompactHistoryFixUpdatedAtOnlyMovesBackward shares, so the three cases differ
// only in where the stamp sits relative to ONE target.
const directionLastRealChange = "2026-02-01 10:00:00"

// stampGateIsOpen reports whether restorableStamp's FIRST gate would pass for this
// memory — a removable version above the anchor — read through the same statement
// the run reads. It is the precondition a direction test needs, and it is
// deliberately not restorableStamp's whole verdict: a test about the direction
// rule has to be able to stage a stamp the repair declines, so asserting the
// decision here would fail the cases that are supposed to decline.
func stampGateIsOpen(t *testing.T, s *Store, memoryID, cutoff string) bool {
	t.Helper()
	query, args := compactCandidatesStmt(testProject, "", cutoff)
	var found bool
	rows, err := s.db.Query(query, args...)
	if err != nil {
		t.Fatalf("read the stamp candidates: %v", err)
	}
	defer rows.Close() //nolint:errcheck
	for rows.Next() {
		var c compactStamp
		var recorded, updatedAt string
		if err := rows.Scan(&c.memoryID, &c.target, &recorded, &updatedAt, &c.removableLast); err != nil {
			t.Fatalf("scan a stamp candidate: %v", err)
		}
		if c.memoryID != memoryID {
			continue
		}
		c.recorded, c.updatedAt = recorded, updatedAt
		found = c.removableLast != 0 && c.removableLast > c.target
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the stamp candidates: %v", err)
	}
	return found
}

// historyPhases reads the table rather than the reader, for the reason
// historyRowCount documents: MemoryHistory defaults to the per-memory cap.
func historyPhases(t *testing.T, s *Store, memoryID string) []string {
	t.Helper()
	rows, err := s.db.Query(`SELECT phase FROM memory_history WHERE memory_id = ? ORDER BY rowid`, memoryID)
	if err != nil {
		t.Fatalf("read history phases: %v", err)
	}
	defer rows.Close() //nolint:errcheck
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			t.Fatalf("scan history phase: %v", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate history phases: %v", err)
	}
	return out
}

func wantPhases(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// TestCompactHistoryRemovesNoOpVersionsAndKeepsEveryChange: the issue's own
// fixture. A save, twenty identical reflects, a real edit, five more identical
// reflects and a resolve is exactly what a store that ran the unattended
// lifecycle looks like, and compaction must leave the three events that say
// something and nothing else.
func TestCompactHistoryRemovesNoOpVersionsAndKeepsEveryChange(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	id, _, _, _ := seedDamagedHistory(t, s)

	if got := historyRowCount(t, s, id); got != 28 {
		t.Fatalf("fixture wrote %d history rows, want 28 (1 save + 20 reflect + 1 update + 5 reflect + 1 resolve)", got)
	}

	res, err := s.CompactHistory(ctx, testProject, HistoryCompactOptions{Apply: true})
	if err != nil {
		t.Fatalf("CompactHistory: %v", err)
	}
	if res.ProjectID != testProject {
		t.Errorf("result project = %q, want %q", res.ProjectID, testProject)
	}
	if res.Removed != 25 {
		t.Errorf("Removed = %d, want 25 (the 20 + 5 verbatim reflect versions)", res.Removed)
	}
	if got := historyPhases(t, s, id); !wantPhases(got, []string{phaseSave, phaseUpdate, phaseResolve}) {
		t.Fatalf("surviving phases = %v, want [save update resolve]", got)
	}
	// The survivors are the right EVENTS, not merely three rows: the save still
	// holds the text the memory was first saved with, which is the only copy of
	// it anywhere once the row was edited.
	entries, err := s.MemoryHistory(ctx, id, 0)
	if err != nil {
		t.Fatalf("MemoryHistory: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("MemoryHistory returned %d entries, want 3", len(entries))
	}
	if entries[0].Content != compactFirstText {
		t.Errorf("surviving save holds %q, want the first wording (%q)", entries[0].Content, compactFirstText)
	}
	if entries[1].Content != compactSecondText {
		t.Errorf("surviving update holds %q, want the rewritten wording (%q)", entries[1].Content, compactSecondText)
	}
	if entries[2].ResolvedAt == nil {
		t.Error("surviving resolve holds no resolved_at, so the resolution is no longer in the record")
	}
}

// TestCompactHistoryKeepsTombstonesAndEdgeEvents: the rows compaction must never
// take, one per case, each with a predecessor recording the SAME state so the
// state comparison cannot be what spared it. A delete row, a supersede, an
// unsupersede, a resolve, an unresolve, a merge, an import and a restore each
// say something the recorded state does not — which memory replaced this one,
// which memory claims it, who decided it was resolved — and that is the whole
// reason they are written.
func TestCompactHistoryKeepsTombstonesAndEdgeEvents(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	id := createCompactMemory(t, s, compactFirstText)

	protected := []struct {
		name    string
		phase   string
		related string
		merged  string
	}{
		{name: "a delete tombstone", phase: phaseDelete},
		{name: "a supersede claim", phase: phaseSupersede, related: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"},
		{name: "a supersede withdrawal", phase: phaseUnsupersede, related: "BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB"},
		{name: "a resolve", phase: phaseResolve},
		{name: "an unresolve", phase: phaseUnresolve},
		{name: "a merge", phase: phaseMerge},
		{name: "an import", phase: phaseImport},
		{name: "a restore", phase: phaseRestore},
		{name: "a row naming the memory that replaced this one", phase: phaseReflect, related: "CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC"},
		{name: "a merge whose folded-in wording no row keeps", phase: phaseMerge, merged: "the wording a FoldOnly fold dropped"},
	}
	for _, c := range protected {
		// Two rows per case: a verbatim one, then the protected event recording
		// the SAME state. The predecessor matters — with none, the "identical to
		// the previous row" test would never be reached and the row would
		// survive for the wrong reason.
		appendVersionRow(t, s, id, phaseReflect, preFixReflectAt, "", "", nil)
		appendVersionRow(t, s, id, c.phase, preFixReflectAt, c.related, c.merged, nil)
	}
	// And one genuinely no-op version after the last of them, so the fixture
	// also proves the compaction still runs over a memory whose history is full
	// of rows it must not touch.
	appendVerbatimVersion(t, s, id)

	res, err := s.CompactHistory(ctx, testProject, HistoryCompactOptions{Apply: true})
	if err != nil {
		t.Fatalf("CompactHistory: %v", err)
	}
	// The verbatim predecessor of each protected row IS removable — it records the
	// same state as the row after it. The trailing version is not: it is the
	// memory's NEWEST, and a memory's newest version is the statement of what it
	// says now, which nothing removes. The protected rows are what must survive,
	// and the count is what says they did.
	if want := int64(len(protected)); res.Removed != want {
		t.Errorf("Removed = %d, want %d (each protected row's verbatim predecessor)", res.Removed, want)
	}
	for _, c := range protected {
		if n := countPhase(t, s, id, c.phase); n < 1 {
			t.Errorf("compaction removed %s: %d %s row(s) survive, want at least 1", c.name, n, c.phase)
		}
	}
	// The save, one row per protected event, and the newest version. Counting them
	// would not tell the three apart, so the per-phase check above is what says the
	// protected events survived.
	if got := historyRowCount(t, s, id); got != len(protected)+2 {
		t.Errorf("history kept %d rows, want %d (the save, one row per protected event, the newest version)",
			got, len(protected)+2)
	}
}

func countPhase(t *testing.T, s *Store, memoryID, phase string) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(
		`SELECT count(*) FROM memory_history WHERE memory_id = ? AND phase = ?`, memoryID, phase).Scan(&n); err != nil {
		t.Fatalf("count %s rows: %v", phase, err)
	}
	return n
}

// TestCompactHistoryEqualityCoversEveryRecordedColumn: the predicate is the
// whole of what may be deleted, and it is a conjunction over the columns a
// version records. Dropping one column from it would make every row that
// differs from its predecessor in THAT column alone look like a no-op, so each
// case here differs in exactly one column and must survive.
//
// The three rows per case are the shape that exposes it: A differs from the
// live row in one column, B restates the live row (so B differs from A in that
// column and nothing else), and C restates the live row again. Omit the column
// and B and C are both "identical to the previous row" and both go, taking the
// only record of a change with them.
func TestCompactHistoryEqualityCoversEveryRecordedColumn(t *testing.T) {
	for _, tc := range []struct {
		column string
		edited any
	}{
		{column: "content", edited: "a wording this memory once held"},
		{column: "category", edited: "gotcha"},
		{column: "importance", edited: 0.9},
		{column: "resolved_at", edited: "2026-01-01 09:00:00"},
		{column: "source", edited: "reflection"},
	} {
		t.Run(tc.column, func(t *testing.T) {
			s := testStore(t)
			ctx := context.Background()
			id := createCompactMemory(t, s, compactFirstText)

			appendVersionRow(t, s, id, phaseReflect, preFixReflectAt, "", "", nil)
			appendVersionRow(t, s, id, phaseReflect, preFixReflectAt, "", "", map[string]any{tc.column: tc.edited})
			appendVerbatimVersion(t, s, id)
			if got := historyRowCount(t, s, id); got != 4 {
				t.Fatalf("fixture wrote %d history rows, want 4 (1 save + 3 reflect)", got)
			}

			if _, err := s.CompactHistory(ctx, testProject, HistoryCompactOptions{Apply: true}); err != nil {
				t.Fatalf("CompactHistory: %v", err)
			}

			// The save plus both reflect rows that differ from their predecessor
			// in this one column. The second of them is the row the omitted
			// column would have cost: nothing else about it differs.
			if got, want := historyRowCount(t, s, id), 3; got != want {
				t.Errorf("history kept %d rows, want %d: a row differing from its predecessor only in %s is not a no-op",
					got, want, tc.column)
			}
		})
	}
}

// TestCompactHistoryColumnPartitionNamesEveryColumn: the predicate claims to
// compare "every recorded column" and the exemptions claim to name the rest. A
// column added to the table and to neither list would be silently ignored by
// both, which is how a state change stops being a state change.
//
// Read off the live table rather than a hand-written list, so the assertion is
// about the schema this build ships.
func TestCompactHistoryColumnPartitionNamesEveryColumn(t *testing.T) {
	s := testStore(t)
	rows, err := s.db.Query(`PRAGMA table_info(memory_history)`)
	if err != nil {
		t.Fatalf("read memory_history columns: %v", err)
	}
	defer rows.Close() //nolint:errcheck
	var got []string
	for rows.Next() {
		var cid int
		var name, colType string
		var notNull int
		var dflt sql.NullString
		var pk int
		if err := rows.Scan(&cid, &name, &colType, &notNull, &dflt, &pk); err != nil {
			t.Fatalf("scan column: %v", err)
		}
		got = append(got, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate columns: %v", err)
	}

	// Every column is in exactly one of three lists: the state a version
	// records, the event's own columns, or the row's identity and time. Nothing
	// is left to fall through, because a column in no list is a column the
	// equality does not compare and the exemptions do not mention.
	classified := map[string]string{}
	for _, c := range historyVersionColumns {
		classified[c] = "a version's recorded state (compared by the equality predicate)"
	}
	for _, c := range historyEventColumns {
		classified[c] = "the event (protected: related_id/merged_content must be NULL, and every other event column is a row's claim, not the memory's state)"
	}
	for _, c := range historyRowColumns {
		classified[c] = "the row's identity or its time (never part of the state two versions can agree about)"
	}
	var unclassified []string
	for _, c := range got {
		if _, ok := classified[c]; !ok {
			unclassified = append(unclassified, c)
		}
	}
	if len(unclassified) > 0 {
		sort.Strings(unclassified)
		t.Errorf("memory_history columns in no list: %v", unclassified)
	}
	if len(classified) != len(got) {
		t.Errorf("the lists name %d columns and the table has %d", len(classified), len(got))
	}
}

// TestCompactHistoryFixUpdatedAtRestoresTheLastStampWrite: the second half of the
// repair. updated_at has to say when the memory last really changed — the last
// recorded version whose WRITER moved the stamp — and not when a reflection that
// changed nothing looked at it.
//
// The fixture is seedStampedHistory, not the issue's own ordering, and the
// difference is load-bearing: for a stamp to be PAST the last stamp write the
// reflect run has to come after that write, so a run that happened before it never
// produced damage to repair and the repair correctly declines to invent any.
//
// The answer is the UPDATE's 11:00 and not the resolve's 12:00, and that is the
// rule rather than a quirk of this fixture: SetResolved files its version from a
// statement that writes resolved_at alone, so 12:00 is a time updated_at never
// held. TestCompactHistoryFixUpdatedAtAnchorsOnAStampMoveNotOnAStateChange drives
// the two shapes that made the difference measurable; this one pins that the common
// case still lands on a real change rather than skipping past it.
func TestCompactHistoryFixUpdatedAtRestoresTheLastStampWrite(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	id, _, updatedAt, _, runTo := seedStampedHistory(t, s, 5)
	if got := readUpdatedAt(t, s, id); got != runTo {
		t.Fatalf("fixture did not stage the damage: updated_at = %q, want the run's %q", got, runTo)
	}

	res, err := s.CompactHistory(ctx, testProject, HistoryCompactOptions{Apply: true, FixUpdatedAt: true})
	if err != nil {
		t.Fatalf("CompactHistory: %v", err)
	}
	if res.UpdatedAt != 1 {
		t.Errorf("UpdatedAt = %d, want 1 (one live memory whose stamp was restored)", res.UpdatedAt)
	}
	if got := readUpdatedAt(t, s, id); got != updatedAt {
		t.Errorf("updated_at = %q, want the last version whose writer moved the stamp (%q), and not the "+
			"resolve's 12:00: a statement that writes resolved_at alone never held that value on "+
			"updated_at", got, updatedAt)
	}
	if res.StampsUnreadable != 0 {
		t.Errorf("StampsUnreadable = %d, want 0: every fixture stamp is readable", res.StampsUnreadable)
	}
	if res.StampsUnrecorded != 0 {
		t.Errorf("StampsUnrecorded = %d, want 0: the memory does have a version whose writer moved the "+
			"stamp, so this is not the no-anchor case", res.StampsUnrecorded)
	}
	// The versions that moved the stamp are gone, except the memory's newest, which
	// is the statement of what it says now and which nothing removes.
	if got, want := historyRowCount(t, s, id), 4; got != want {
		t.Errorf("history kept %d rows, want %d (save, update, resolve, and the newest version)", got, want)
	}
}

// TestCompactHistoryFixUpdatedAtIsBounded: two rules that keep the repair from
// becoming a way to make a row look older than it is. A stamp is never moved
// FORWARD, and a memory with no recorded change is left exactly as it was.
func TestCompactHistoryFixUpdatedAtIsBounded(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	// Already correct: the stamp equals the last change, so even with a redundant
	// version newer than it there is nothing to restore and the row is not counted
	// as fixed.
	correct := createCompactMemory(t, s, "a memory whose stamp is already right")
	appendVersionRow(t, s, correct, phaseSave, "2026-01-01 08:00:00", "", "", nil)
	appendVersionRow(t, s, correct, phaseReflect, "2026-01-01 08:30:00", "", "", nil)
	setUpdatedAt(t, s, correct, "2026-01-01 08:00:00")

	// Ahead of its own history: the stamp claims a moment before any recorded
	// change, and the only direction this repair moves is BACKWARD, so a stamp
	// already behind the last change is not touched however much noise sits above
	// it.
	behind := createCompactMemory(t, s, "a memory stamped before anything its history records")
	appendVersionRow(t, s, behind, phaseSave, "2026-01-01 10:00:00", "", "", nil)
	appendVersionRow(t, s, behind, phaseReflect, "2026-01-01 10:30:00", "", "", nil)
	setUpdatedAt(t, s, behind, "2026-01-01 09:00:00")

	// No history at all: a pre-v17 memory nothing has written since. There is
	// no recorded change to restore from, and inventing one would be a
	// timestamp Ghost never recorded.
	orphan := createCompactMemory(t, s, "a memory no write has ever recorded")
	if _, err := s.db.Exec(`DELETE FROM memory_history WHERE memory_id = ?`, orphan); err != nil {
		t.Fatalf("strip the orphan's history: %v", err)
	}
	const orphanStamp = "2026-01-01 07:00:00"
	setUpdatedAt(t, s, orphan, orphanStamp)

	// Unreadable: a hand-edited or artifact-carried recorded_at no layout in
	// StampLayouts accepts, on a row that DID change state, so it is the
	// compaction's answer and not merely the newest row. Left alone and
	// DISCLOSED, never guessed at.
	unreadable := createCompactMemory(t, s, "a memory whose last recorded_at cannot be read")
	const restated = "a wording this memory reached, at a moment nothing can read"
	appendVersionRow(t, s, unreadable, phaseReflect, "not a timestamp", "", "", map[string]any{"content": restated})
	// The two versions above it restate THAT state, so they are removable and the
	// target is the unreadable row between them — which is the shape the decision
	// has to be reached on.
	appendVersionRow(t, s, unreadable, phaseReflect, "2026-01-01 06:30:00", "", "", map[string]any{"content": restated})
	appendVersionRow(t, s, unreadable, phaseReflect, "2026-01-01 06:45:00", "", "", map[string]any{"content": restated})
	const unreadableStamp = "2026-01-01 06:00:00"
	setUpdatedAt(t, s, unreadable, unreadableStamp)

	res, err := s.CompactHistory(ctx, testProject, HistoryCompactOptions{Apply: true, FixUpdatedAt: true})
	if err != nil {
		t.Fatalf("CompactHistory: %v", err)
	}
	if res.UpdatedAt != 0 {
		t.Errorf("UpdatedAt = %d, want 0: none of these four rows may be restamped", res.UpdatedAt)
	}
	if res.StampsUnreadable != 1 {
		t.Errorf("StampsUnreadable = %d, want 1 (the row whose recorded_at no layout reads)", res.StampsUnreadable)
	}
	for _, c := range []struct{ id, want string }{
		{correct, "2026-01-01 08:00:00"},
		{behind, "2026-01-01 09:00:00"},
		{orphan, orphanStamp},
		{unreadable, unreadableStamp},
	} {
		if got := readUpdatedAt(t, s, c.id); got != c.want {
			t.Errorf("updated_at of %s = %q, want it untouched at %q", c.id, got, c.want)
		}
	}
}

// TestCompactHistoryFixUpdatedAtOnlyTouchesLiveMemories: a tombstone is the
// record that a memory existed, and there is no memories row left to stamp. The
// repair reaches live rows only, so the delete row's own history is evidence
// about a memory that is gone and is left exactly as recorded.
func TestCompactHistoryFixUpdatedAtOnlyTouchesLiveMemories(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	id := createCompactMemory(t, s, compactFirstText)
	// Delete files its own tombstone, recorded from the last live state — so
	// the row that would be compacted if it were eligible is the one the store
	// writes here, not one a fixture invents.
	if err := s.Delete(context.Background(), id); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	res, err := s.CompactHistory(ctx, testProject, HistoryCompactOptions{Apply: true, FixUpdatedAt: true})
	if err != nil {
		t.Fatalf("CompactHistory: %v", err)
	}
	if res.UpdatedAt != 0 {
		t.Errorf("UpdatedAt = %d, want 0: a deleted memory has no row to stamp", res.UpdatedAt)
	}
	// Nothing is removed either, and the count says so rather than leaving the row
	// count to imply it: the two rows a deleted memory has are its save — the only
	// record of what it said — and its tombstone, and neither is a phase the repair
	// touches even when the state comparison would allow it.
	if res.Removed != 0 {
		t.Errorf("Removed = %d, want 0: a deleted memory's own save and tombstone are the only records it left", res.Removed)
	}
	if got := historyRowCount(t, s, id); got != 2 {
		t.Errorf("a deleted memory kept %d history rows, want 2 (the save and its tombstone)", got)
	}
}

// TestCompactHistoryLeavesADeletedMemorysHistoryAlone: the version removal stops
// at a memory's tombstone, and the reason is as_of rather than this repair.
//
// A deleted memory has no `memories` row, so asOfCreatedAt falls back to the
// ANSWERING version's recorded_at for the age it measures a ranking from. Remove a
// version from such a memory and the version that answers a given instant changes,
// so the age a historical listing computes for it changes with it — a reading that
// was correct yesterday reads differently today because a repair ran, about a
// memory nobody is editing and nobody can restore. The live memories next to it are
// different: their `memories` row answers created_at, so removing a redundant
// version moves which write the answer NAMES and not what it says.
//
// So the rule is a scope rule and not a retention one: a memory whose NEWEST
// version is a `delete` tombstone is not compacted at all. The #727 flood it would
// have cleaned up is frozen the moment the memory is deleted, which is exactly why
// it costs nothing to leave it — nothing will ever write there again.
func TestCompactHistoryLeavesADeletedMemorysHistoryAlone(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	id := createCompactMemory(t, s, compactFirstText)

	// The damage a live memory accumulates, and then the retirement that freezes
	// it. The reflects go in BEFORE the delete so the tombstone is the newest
	// version, which is the state a deleted memory is in from then on.
	stampHistoryRow(t, s, id, phaseSave, "2026-01-01 10:00:00")
	appendVerbatimVersion(t, s, id)
	appendVerbatimVersion(t, s, id)
	// Stamped at once, because appendVerbatimVersion leaves recorded_at on the wall
	// clock and every claim this test makes is a claim about rows on one side of a
	// HISTORICAL bound. A fixture that left the clock to decide would be asserting
	// something about the day it ran on.
	stampPhaseRows(t, s, id, phaseReflect, preFixReflectAt)
	if err := s.Delete(ctx, id); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if newest, err := lastVersionPhase(t, s, id); err != nil {
		t.Fatal(err)
	} else if newest != phaseDelete {
		t.Fatalf("the memory's newest version is a %s, want its tombstone — the fixture is not the "+
			"retired case this is about", newest)
	}
	// Asserted rather than assumed, because it is the whole premise: those two
	// reflect versions are byte-identical, older than the bound, name no other
	// memory, and are not the newest — so nothing but the tombstone rule keeps them.
	//
	// Three rather than two, and the extra one is the tombstone itself: Delete files
	// its version from the last live state, so it restates its predecessor as
	// faithfully as a no-op reflect does. Counting the restatements rather than the
	// removable rows is what surfaces that, and it is why the rule below is a
	// separate conjunct and not a phase: the tombstone is spared by both, and a
	// count of three here is the fixture telling us so.
	if n := redundantVersionCount(t, s, id); n != 3 {
		t.Fatalf("%d versions restate their predecessor byte for byte, want 3: the fixture has to be "+
			"damaged for the tombstone rule to be what spares it", n)
	}
	// The damage shape on its own, with neither retention guard: two of the memory's
	// four versions are rows any build would remove but for the tombstone. Counted
	// through the production predicate rather than restated in the test, because a
	// hand-written copy of the rules would pass on the day the rules changed.
	//
	// The project and the phase list are spelled in beside it because
	// historyRemovableLikeSQL carries neither: the delete and the count add them, so
	// a caller reaching for the predicate alone has to add them too. The list is read
	// from compactablePhases rather than written out, so this count cannot drift from
	// the phases the delete actually removes.
	args := []any{id, testProject}
	for _, p := range compactablePhases() {
		args = append(args, p)
	}
	args = append(args, reflectNoOpCutoff)
	var inReach int
	if err := s.db.QueryRow(
		`SELECT count(*) FROM memory_history h WHERE h.memory_id = ? AND h.project_id = ? AND `+
			historyRemovableLikeSQL("h"), args...).Scan(&inReach); err != nil {
		t.Fatalf("count the rows the tombstone guard is holding back: %v", err)
	}
	if inReach != 2 {
		t.Fatalf("%d of the memory's versions are in the damage shape ignoring both retention "+
			"guards, want 2: the tombstone guard is only load-bearing if the rows are otherwise "+
			"in reach, and a third would mean the fixture staged one the newest-version guard "+
			"already spared", inReach)
	}

	res, err := s.CompactHistory(ctx, testProject, HistoryCompactOptions{Apply: true, FixUpdatedAt: true})
	if err != nil {
		t.Fatalf("CompactHistory: %v", err)
	}
	if res.Removed != 0 {
		t.Errorf("Removed = %d, want 0: a retired memory's history is frozen, and compacting it would "+
			"change what as_of answers about a memory nobody can edit", res.Removed)
	}
	if got := historyRowCount(t, s, id); got != 4 {
		t.Errorf("a retired memory kept %d history rows, want 4 (the save, two no-op reflects, the "+
			"tombstone)", got)
	}
	// A live memory beside it is still repaired, so the rule is scoped to the
	// tombstoned one and not a run that stopped early. Three reflects for two
	// removals, as above: the newest version of any memory is its own statement of
	// what it says, and nothing here takes it.
	live := createCompactMemory(t, s, compactSecondText)
	stampHistoryRow(t, s, live, phaseSave, "2026-01-01 10:00:00")
	appendVerbatimVersion(t, s, live)
	appendVerbatimVersion(t, s, live)
	appendVerbatimVersion(t, s, live)
	stampPhaseRows(t, s, live, phaseReflect, preFixReflectAt)
	setUpdatedAt(t, s, live, anchorDamageTo)
	res, err = s.CompactHistory(ctx, testProject, HistoryCompactOptions{Apply: true, FixUpdatedAt: true})
	if err != nil {
		t.Fatalf("CompactHistory: %v", err)
	}
	if res.Removed != 2 {
		t.Errorf("Removed = %d, want 2: a live memory's no-op reflects are still the damage this "+
			"command exists to remove", res.Removed)
	}
	if got := readUpdatedAt(t, s, live); got != "2026-01-01 10:00:00" {
		t.Errorf("updated_at of the live memory = %q, want its save's own 2026-01-01 10:00:00", got)
	}
}

// lastVersionPhase reads a memory's NEWEST recorded phase, which is the one
// reading that decides whether a memory is retired.
func lastVersionPhase(t *testing.T, s *Store, memoryID string) (string, error) {
	t.Helper()
	var phase string
	err := s.db.QueryRow(
		`SELECT phase FROM memory_history WHERE memory_id = ? ORDER BY rowid DESC LIMIT 1`, memoryID).Scan(&phase)
	if err != nil {
		return "", fmt.Errorf("read %s's newest version phase: %w", memoryID, err)
	}
	return phase, nil
}

// TestCompactHistoryFixUpdatedAtWritesTheStoredLayout: the two timestamp
// columns are written by different statements, so they are not the same shape.
// memory_history.recorded_at is datetime('now'); memories.updated_at is too on
// every writer Ghost ships, but the columns are unconstrained TEXT and a
// portable artifact, a hand edit or SQLite's own date() can leave a whole-day
// value behind. So the repair has to READ both over the store's own layouts
// and WRITE the one the store writes — reading only its own shape would treat a
// date-only stamp as no stamp at all and leave the row broken, and writing back
// whatever it read would leave a row in a shape no writer produces.
func TestCompactHistoryFixUpdatedAtWritesTheStoredLayout(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	id := createCompactMemory(t, s, compactFirstText)
	appendVersionRow(t, s, id, phaseSave, "2026-01-01 08:00:00", "", "", nil)
	// The last change at a whole-day stamp, a redundant version above it, and a
	// current stamp at a LATER whole-day stamp: all three readable, none in a shape
	// a writer produces, and the target behind the current value, so the stamp
	// moves back and is written in the layout the store writes.
	appendVersionRow(t, s, id, phaseUpdate, "2026-01-02", "", "", map[string]any{
		"content": compactSecondText,
	})
	appendVersionRow(t, s, id, phaseReflect, "2026-01-02 12:00:00", "", "", map[string]any{
		"content": compactSecondText,
	})
	appendVersionRow(t, s, id, phaseReflect, "2026-01-02 12:30:00", "", "", map[string]any{
		"content": compactSecondText,
	})
	setUpdatedAt(t, s, id, "2026-01-03")

	res, err := s.CompactHistory(ctx, testProject, HistoryCompactOptions{Apply: true, FixUpdatedAt: true})
	if err != nil {
		t.Fatalf("CompactHistory: %v", err)
	}
	if res.UpdatedAt != 1 {
		t.Fatalf("UpdatedAt = %d, want 1: a whole-day stamp is a stamp", res.UpdatedAt)
	}
	if res.StampsUnreadable != 0 {
		t.Errorf("StampsUnreadable = %d, want 0: a whole-day recorded_at is readable", res.StampsUnreadable)
	}
	if got, want := readUpdatedAt(t, s, id), "2026-01-02 00:00:00"; got != want {
		t.Errorf("updated_at = %q, want %q — the layout the store writes, not the one it read", got, want)
	}
	if _, ok := ParseStamp(readUpdatedAt(t, s, id)); !ok {
		t.Error("the restored stamp is not readable by the store's own parser")
	}
}

// TestCompactHistoryDryRunWritesNothing: the default, and the reason a dry run
// is the default. It reports exactly what the apply would remove and restore
// and changes no row — not the history it names, not the memories it would
// restamp, and not even the count of what it looked at.
func TestCompactHistoryDryRunWritesNothing(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	id, _, updatedAt, _, runTo := seedStampedHistory(t, s, 5)
	rowsBefore := historyRowCount(t, s, id)

	res, err := s.CompactHistory(ctx, testProject, HistoryCompactOptions{FixUpdatedAt: true})
	if err != nil {
		t.Fatalf("CompactHistory: %v", err)
	}

	// Four of the five run versions: the last is the memory's newest version, which
	// nothing removes.
	if res.Removed != 4 {
		t.Errorf("Removed = %d, want 4: the dry run has to report exactly what the apply would do", res.Removed)
	}
	if res.UpdatedAt != 1 {
		t.Errorf("UpdatedAt = %d, want 1: the dry run has to report the restamp the apply would make", res.UpdatedAt)
	}
	if got := historyRowCount(t, s, id); got != rowsBefore {
		t.Errorf("the dry run changed the history: %d rows, want the %d it started with", got, rowsBefore)
	}
	if got := readUpdatedAt(t, s, id); got != runTo {
		t.Errorf("the dry run changed updated_at: %q, want the %q it started with", got, runTo)
	}
	// And the apply that follows does exactly what the dry run said.
	applied, err := s.CompactHistory(ctx, testProject, HistoryCompactOptions{Apply: true, FixUpdatedAt: true})
	if err != nil {
		t.Fatalf("CompactHistory --apply: %v", err)
	}
	if applied.Removed != res.Removed || applied.UpdatedAt != res.UpdatedAt {
		t.Errorf("the apply removed %d / restamped %d, but the dry run reported %d / %d",
			applied.Removed, applied.UpdatedAt, res.Removed, res.UpdatedAt)
	}
	if got := readUpdatedAt(t, s, id); got != updatedAt {
		t.Errorf("after the apply updated_at = %q, want %q", got, updatedAt)
	}
}

// TestCompactHistorySecondRunRemovesNothing: compaction has to be idempotent,
// because the first run is a dry run an operator reads, the second is the apply
// they then run, and a third is the "did it work" check. A run that removed
// something the previous run left behind would be reporting a repair that did
// not finish.
func TestCompactHistorySecondRunRemovesNothing(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	id, _, _, _ := seedDamagedHistory(t, s)

	first, err := s.CompactHistory(ctx, testProject, HistoryCompactOptions{Apply: true, FixUpdatedAt: true})
	if err != nil {
		t.Fatalf("first CompactHistory: %v", err)
	}
	if first.Removed == 0 {
		t.Fatal("the first run removed nothing, so the fixture is not damaged and the rest proves nothing")
	}
	kept := historyRowCount(t, s, id)

	second, err := s.CompactHistory(ctx, testProject, HistoryCompactOptions{Apply: true, FixUpdatedAt: true})
	if err != nil {
		t.Fatalf("second CompactHistory: %v", err)
	}
	if second.Removed != 0 {
		t.Errorf("the second run removed %d rows, want 0", second.Removed)
	}
	if second.UpdatedAt != 0 {
		t.Errorf("the second run restamped %d memories, want 0", second.UpdatedAt)
	}
	if got := historyRowCount(t, s, id); got != kept {
		t.Errorf("the second run left %d rows, want the %d the first left", got, kept)
	}
}

// TestCompactHistoryIsScopedToOneProject: the counts are reported PER PROJECT
// and the command takes one, so a run scoped to one must not touch another's
// rows — including when both are damaged, which is the case where a missing
// project predicate would show up.
func TestCompactHistoryIsScopedToOneProject(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	const otherProject = "other-project"
	if err := s.EnsureProject(ctx, otherProject, "/tmp/other", "other"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	mine := createCompactMemory(t, s, compactFirstText)
	for i := 0; i < 3; i++ {
		appendVerbatimVersion(t, s, mine)
	}
	theirs, err := s.Create(ctx, otherProject, Memory{
		Category: "gotcha", Content: "the other project's note", Source: "mcp", Importance: 0.5,
	})
	if err != nil {
		t.Fatalf("Create in %s: %v", otherProject, err)
	}
	for i := 0; i < 7; i++ {
		appendVersionRow(t, s, theirs, phaseReflect, nil, "", "", nil)
	}

	res, err := s.CompactHistory(ctx, testProject, HistoryCompactOptions{Apply: true})
	if err != nil {
		t.Fatalf("CompactHistory: %v", err)
	}
	// Two of the three, not three: the last one is this memory's newest version and
	// nothing removes that.
	if res.Removed != 2 {
		t.Errorf("Removed = %d, want 2 of this project's three verbatim versions (the newest is never removable)", res.Removed)
	}
	if got := historyRowCount(t, s, mine); got != 2 {
		t.Errorf("this project's memory kept %d rows, want 2 (the save and its newest version)", got)
	}
	if got := historyRowCount(t, s, theirs); got != 8 {
		t.Errorf("the other project's memory kept %d rows, want 8 — compaction crossed a project boundary", got)
	}
}

// TestCompactHistoryLeavesTheRestatementACurrentWriterStillMakes is #730's review
// finding, reproduced against the real writer rather than a fixture, and the test
// that decides what the compaction is bounded BY.
//
// ReplaceNonManual's reusePreservesAge branch (#623) re-tags a reused row and
// records a version of it, and #727 was RIGHT that this is a real change: a re-tag
// is a change to what the memory says about itself, and
// TestReplaceNonManualEveryRecordedFieldBreaksTheNoOp pins that the version is
// appended. But the version restates content, category, importance, resolved_at
// and source byte for byte, because this table has no column for tags or scope —
// so from here it is indistinguishable, by any state column, from the pre-#727
// damage this command removes.
//
// A compaction that deleted it would re-grow the rows the repair just pruned on
// every lifecycle pass, and --fix-updated-at would move the stamp back over the
// bump the same branch made. No column can prevent that, so what prevents it is a
// time: both versions here are recorded by a current build, and a current build's
// rows are outside the repair.
//
// The convergence is the load-bearing half. TWO passes, not one, so the first
// version is NOT the memory's newest and the newest-version guard cannot be what
// spared it — the case the guard was previously credited with. If the bound is the
// cut rather than the guard, the second run still removes nothing.
func TestCompactHistoryLeavesTheRestatementACurrentWriterStillMakes(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	ids := seedVerbatimCorpus(t, s)
	target := ids[0] // the one carrying tags
	before := memoryUpdatedAt(t, s, target)

	// A pass that changes the tags and nothing else: the reusePreservesAge branch,
	// since content and category match byte for byte. A DIFFERENT tag set each
	// time, because a pass that re-states the tags the row already has is the
	// #727 no-op and writes nothing at all.
	retag := func(tags ...string) {
		t.Helper()
		emitted := []Memory{keptReemit(t, s, target)}
		emitted[0].Tags = tags
		for _, id := range ids[1:] {
			emitted = append(emitted, keptReemit(t, s, id))
		}
		if _, err := s.ReplaceNonManual(ctx, testProject, emitted, ""); err != nil {
			t.Fatalf("ReplaceNonManual: %v", err)
		}
	}
	retag("kes", "bp", "rotation")
	retag("kes", "bp", "rotation", "verified")

	// State the instant rather than reading it. appendHistoryGroupTx names no
	// recorded_at, so both versions carry the schema's datetime('now') default, and
	// the assertions below are true ONLY because those rows are newer than the bound:
	// leave them on the clock and the test passes on a machine that reads 17:15 on
	// the day #727 shipped and fails on every machine that reads anything earlier.
	//
	// Only the two re-tag versions are stamped and only they are counted. The save
	// row keeps the clock's stamp deliberately: the compaction never reads it, so
	// asserting anything about it would be the same wall-clock dependency with an
	// extra step.
	stampPhaseRows(t, s, target, phaseReflect, postFixReflectAt)
	if n := versionsAtOrBefore(t, s, target, phaseReflect, reflectNoOpCutoff); n != 0 {
		t.Fatalf("%d of this memory's re-tag versions are recorded at or before the bound %q, so "+
			"the assertions below would be about a pre-#727 row rather than about the row a current "+
			"build writes on purpose", n, reflectNoOpCutoff)
	}

	// The preconditions the whole finding rests on, asserted rather than assumed:
	// both passes DID write a version, both versions restate the state byte for
	// byte, and the first is no longer the memory's newest version. If any of that
	// stopped being true the finding would be moot and this test would be passing
	// for the wrong reason.
	if n := historyRowCount(t, s, target); n != 3 {
		t.Fatalf("the two re-tag passes left %d history rows, want 3 (the save and one version per pass)", n)
	}
	if n := redundantVersionCount(t, s, target); n != 2 {
		t.Fatalf("%d of the re-tag's versions restate their predecessor byte for byte, want 2: "+
			"this is no longer the case the finding is about", n)
	}
	if newestRemovableVersion(t, s, target) == 0 {
		t.Fatal("no restatement is removable at any bound, so the first version being non-newest " +
			"no longer means the newest-version guard spared it and the test proves nothing")
	}
	if memoryUpdatedAt(t, s, target) == before {
		t.Fatal("the re-tag passes did not move updated_at, so there is no stamp for --fix-updated-at to take back")
	}

	res, err := s.CompactHistory(ctx, testProject, HistoryCompactOptions{Apply: true, FixUpdatedAt: true})
	if err != nil {
		t.Fatalf("CompactHistory: %v", err)
	}
	if res.Removed != 0 {
		t.Errorf("Removed = %d, want 0: both versions were written by a current build, and a row this "+
			"build wrote is not this repair's to remove", res.Removed)
	}
	if res.UpdatedAt != 0 {
		t.Errorf("UpdatedAt = %d, want 0: neither version is evidence of a pre-#727 run, so there is no "+
			"damage to take the stamp back over", res.UpdatedAt)
	}
	if n := historyRowCount(t, s, target); n != 3 {
		t.Errorf("history rows = %d, want 3: the compaction removed a version the store deliberately recorded", n)
	}
	if got := memoryUpdatedAt(t, s, target); got == before {
		t.Error("updated_at was rewound over a deliberate retag, which is the failure the bound exists to prevent")
	}
}

// newestRemovableVersion reports whether ANY of a memory's versions satisfies the
// compaction's own predicate with every time bound taken out. It is what lets the
// test above say the guard is not what spared the row, rather than assuming it.
func newestRemovableVersion(t *testing.T, s *Store, memoryID string) int64 {
	t.Helper()
	predicate := historyRemovableRowSQL("h")
	predicate = strings.Replace(predicate, " AND h.recorded_at < ?", "", 1)
	var n int64
	if err := s.db.QueryRow(
		`SELECT max(h.rowid) FROM memory_history h WHERE `+predicate, "reflect", memoryID).Scan(&n); err != nil {
		t.Fatalf("read the removable versions of %s with no time bound: %v", memoryID, err)
	}
	return n
}

// TestCompactHistoryFixUpdatedAtNeedsARunAboveTheLastChange: the other half of
// the evidence gate, and the direction the repair is conservative in.
//
// A memory whose only removable version sits BELOW its last real change has no
// unexplained run above that change, so a stamp past the change is not this
// repair's to move. Something wrote it — another tool, a clock, a writer that
// moved updated_at without filing a history row — and the repair's claim is
// specifically about a reflection that restated a memory's state and said nothing
// about it. Moving a stamp it cannot account for would be the same invention
// #727 removed, one layer down.
func TestCompactHistoryFixUpdatedAtNeedsARunAboveTheLastChange(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	id := createCompactMemory(t, s, compactFirstText)
	stampHistoryRow(t, s, id, phaseSave, "2026-01-01 10:00:00")
	// A redundant version, and then a REAL change above it.
	appendVerbatimVersion(t, s, id)
	stampHistoryRow(t, s, id, phaseReflect, "2026-01-01 10:30:00")
	appendVersionRow(t, s, id, phaseUpdate, "2026-01-01 11:00:00", "", "", map[string]any{
		"content": compactSecondText,
	})
	// And a stamp past the change, with nothing in the history to explain it.
	const unexplained = "2026-01-01 12:00:00"
	setUpdatedAt(t, s, id, unexplained)

	res, err := s.CompactHistory(ctx, testProject, HistoryCompactOptions{Apply: true, FixUpdatedAt: true})
	if err != nil {
		t.Fatalf("CompactHistory: %v", err)
	}
	if res.Removed != 1 {
		t.Errorf("Removed = %d, want 1: the redundant version below the change is still removable", res.Removed)
	}
	if res.UpdatedAt != 0 {
		t.Errorf("UpdatedAt = %d, want 0: the removable version is older than the last change, so no run explains the stamp", res.UpdatedAt)
	}
	if got := readUpdatedAt(t, s, id); got != unexplained {
		t.Errorf("updated_at = %q, want it untouched at %q", got, unexplained)
	}
}

// TestCompactHistoryKeepsMemoryWithOnlyOneVersion: a memory nothing has edited
// has a single version row, and that row is the only statement of what it says.
// The "previous row" test is what spares it — there is none — so this pins the
// rule that a first version is never a no-op.
func TestCompactHistoryKeepsMemoryWithOnlyOneVersion(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	id := createCompactMemory(t, s, compactFirstText)

	res, err := s.CompactHistory(ctx, testProject, HistoryCompactOptions{Apply: true, FixUpdatedAt: true})
	if err != nil {
		t.Fatalf("CompactHistory: %v", err)
	}
	if res.Removed != 0 {
		t.Errorf("Removed = %d, want 0: a memory's first version is the only record of what it said", res.Removed)
	}
	if got := historyRowCount(t, s, id); got != 1 {
		t.Errorf("history kept %d rows, want 1", got)
	}
}

// TestCompactHistoryBatchesTheSameAnswer: the apply runs in bounded batches so
// one transaction cannot hold the write lock over a whole store's history. The
// batch size is lowered to 2 against a 25-row run of no-ops, and the answer has
// to be the answer the unbounded count gives — a loop that stopped a batch early
// or a cursor that skipped rows would leave some behind.
func TestCompactHistoryBatchesTheSameAnswer(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	id, _, _, _ := seedDamagedHistory(t, s)

	prev := historyCompactBatchSize
	historyCompactBatchSize = 2
	t.Cleanup(func() { historyCompactBatchSize = prev })

	res, err := s.CompactHistory(ctx, testProject, HistoryCompactOptions{Apply: true})
	if err != nil {
		t.Fatalf("CompactHistory: %v", err)
	}
	if res.Removed != 25 {
		t.Errorf("Removed = %d, want 25 in batches of 2", res.Removed)
	}
	if got := historyPhases(t, s, id); !wantPhases(got, []string{phaseSave, phaseUpdate, phaseResolve}) {
		t.Fatalf("surviving phases = %v, want [save update resolve]", got)
	}
}

// TestCompactHistoryLeavesATagsOnlyEditAndItsStamp is #730's second review
// finding, reproduced against the real writer: a tags-only edit is a REAL change
// that this table cannot see.
//
// UpdateMemory appends its `update` version unconditionally (store.go, the branch
// that sets `tags = ?, updated_at = datetime('now')`) — it has no no-op guard the
// way reuseChangesNothing gives the reflect path one — and the version it appends
// records content, category, importance, resolved_at and source, none of which
// moved. `memory_history` has no tags column, so two deliberate retags produce two
// rows byte-identical to their predecessors over every column the equality
// predicate compares.
//
// So on a store with no #730 damage at all, the repair read those two rows as the
// evidence that a reflection moved the stamp, took the stamp back to the save's
// instant — undoing two deliberate edits' bumps — and deleted one of the rows
// recording them. The whole trace runs here through the real UpdateMemory, and
// the outcome the repair has to give is that a row it cannot classify as damage
// is a row it leaves alone.
func TestCompactHistoryLeavesATagsOnlyEditAndItsStamp(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	id := createCompactMemory(t, s, compactFirstText)
	// The save at an instant of its own, because the edits below move updated_at to
	// the store's clock and datetime('now') has one-second resolution: with the
	// save in the same second as the edits, a stamp that is "past the last real
	// change" is indistinguishable from one that is not, and the rewind this test
	// exists to catch would not fire at all.
	stampHistoryRow(t, s, id, phaseSave, "2026-01-01 10:00:00")

	// Two tags-only edits, the shape `ghost_memory_update` takes with no content,
	// category or importance. Neither may be treated as noise.
	for _, tags := range [][]string{{"retag", "one"}, {"retag", "one", "two"}} {
		if err := s.UpdateMemory(ctx, testProject, id, nil, nil, nil, tags); err != nil {
			t.Fatalf("UpdateMemory(%v): %v", tags, err)
		}
	}

	// The preconditions, asserted rather than assumed: both edits filed a version,
	// both versions restate the state byte for byte, and both moved the stamp. If
	// any stopped being true the finding would be moot and this test would be
	// passing for the wrong reason.
	if n := countPhase(t, s, id, phaseUpdate); n != 2 {
		t.Fatalf("the two retags filed %d update version(s), want 2", n)
	}
	if n := redundantVersionCount(t, s, id); n != 2 {
		t.Fatalf("%d of the retag's versions restate their predecessor byte for byte, want 2: "+
			"this is no longer the case the finding is about", n)
	}
	deliberate := readUpdatedAt(t, s, id)

	// The edits are then dated a month before #727 shipped, which is where a store
	// that made them sat, and it is what makes this a test of the PHASE rather than
	// of the cut. Dated on either side of the cut the two rules are redundant: an
	// `update` row newer than the cut is spared by the cut whether or not its phase
	// is removable, so a test that left the clock to decide would pass with the
	// phase allowlist back at four phases — and the phase is the rule that has to
	// hold for the row a store wrote when no cut existed.
	stampPhaseRows(t, s, id, phaseUpdate, preFixReflectAt)
	// Every other clause of the predicate is asserted, so the phase is provably the
	// only thing standing between the older of the two and the delete. The clause
	// list is spelled out rather than called on purpose: the point is to name which
	// rule is being isolated, and a query that asked the real predicate would answer
	// zero whichever way the rule went. One row, not two — the newer edit's version
	// is the memory's newest version, and nothing removes that whatever its phase.
	if n := updateVersionsNoOtherClauseExempts(t, s, id, reflectNoOpCutoff); n != 1 {
		t.Fatalf("%d of the retag's versions are exempt on nothing but their phase, want 1: "+
			"the fixture has to stage a row the PHASE alone has to decline", n)
	}

	res, err := s.CompactHistory(ctx, testProject, HistoryCompactOptions{Apply: true, FixUpdatedAt: true})
	if err != nil {
		t.Fatalf("CompactHistory: %v", err)
	}
	if n := countPhase(t, s, id, phaseUpdate); n != 2 {
		t.Errorf("compaction left %d of the two deliberate retags recorded, want both: "+
			"a version no column can tell from a no-op is still a recorded change", n)
	}
	if got := readUpdatedAt(t, s, id); got != deliberate {
		t.Errorf("updated_at = %q, want it untouched at %q — the repair rewound the stamp over a deliberate retag", got, deliberate)
	}
	if res.Removed != 0 {
		t.Errorf("Removed = %d, want 0: nothing here is a reflect restatement", res.Removed)
	}
	if res.UpdatedAt != 0 {
		t.Errorf("UpdatedAt = %d, want 0: a version this repair cannot attribute to a reflection is not evidence of one", res.UpdatedAt)
	}
}

// TestCompactHistoryCarriesTheCountsItAlreadyCommitted: a run over a store with
// tens of thousands of redundant rows commits many batches, and a failure part way
// through leaves every one of them durable. The counts a caller is handed on the
// error path are the only record that this run rewrote part of the store, and a
// zero is an answer about a store that no longer exists.
//
// The failure is injected in SQL rather than through a seam, because a seam would
// be a new thing in the production code to make a test possible: a trigger that
// aborts the delete of one specific row makes the SECOND batch fail exactly the
// way a real statement error makes a later batch fail, with the first already
// committed.
func TestCompactHistoryCarriesTheCountsItAlreadyCommitted(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	id, _, _, _ := seedDamagedHistory(t, s)

	prev := historyCompactBatchSize
	historyCompactBatchSize = 1
	t.Cleanup(func() { historyCompactBatchSize = prev })

	// The second row the delete would take, so the first batch commits and the
	// second one is the one that fails.
	armed := armVersionDelete(t, s, testProject, 2)
	if armed == 0 {
		t.Fatal("the fixture holds fewer than two removable versions, so nothing can fail after a committed batch")
	}

	res, err := s.CompactHistory(ctx, testProject, HistoryCompactOptions{Apply: true})
	if err == nil {
		t.Fatalf("the run succeeded with a row the store refuses to delete: %+v", res)
	}
	if res.Removed != 1 {
		t.Errorf("Removed = %d, want 1: the first batch committed, and its count is the only record this run changed anything", res.Removed)
	}
	// And the committed batch is genuinely gone — the count is not a guess about a
	// rollback that did not happen.
	if got := historyRowCount(t, s, id); got != 27 {
		t.Errorf("history kept %d rows, want 27 (28 minus the one committed batch)", got)
	}
}

// TestCompactHistoryCarriesTheStampsItAlreadyCommitted is the same contract on
// the other pass, and it is the half with the sharper edge: a stamp this pass
// moved is a row's freshness, so a count that included a stamp whose batch rolled
// back would send an operator looking for damage that is still there.
//
// The counts are added AFTER the commit for that reason, and the test pins it by
// failing the SECOND memory's stamp in the second batch.
func TestCompactHistoryCarriesTheStampsItAlreadyCommitted(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	var ids [2]string
	for i := range ids {
		id, _, _, _, runTo := seedStampedHistory(t, s, 3)
		ids[i] = id
		if got := readUpdatedAt(t, s, id); got != runTo {
			t.Fatalf("fixture %d did not stage the damage: updated_at = %q, want %q", i, got, runTo)
		}
	}
	// The pass pages the scan `ORDER BY memory_id`, so the memory whose id sorts
	// LAST is the one in the second batch of a page size of one. Arming the other
	// would fail the FIRST batch and the count under test would be zero for a
	// reason that has nothing to do with the commit ordering.
	first, refused := ids[0], ids[1]
	if refused < first {
		first, refused = refused, first
	}

	prev := historyCompactBatchSize
	historyCompactBatchSize = 1
	t.Cleanup(func() { historyCompactBatchSize = prev })

	armMemoryStamp(t, s, refused)

	res, err := s.CompactHistory(ctx, testProject, HistoryCompactOptions{Apply: true, FixUpdatedAt: true})
	if err == nil {
		t.Fatalf("the run succeeded with a memory the store refuses to stamp: %+v", res)
	}
	if res.UpdatedAt != 1 {
		t.Errorf("UpdatedAt = %d, want 1: the first stamp committed and the failed one rolled back with its batch", res.UpdatedAt)
	}
	// Which one is the load-bearing half: the memory the trigger refused must NOT
	// be counted, because its batch was rolled back and its stamp is untouched.
	// Both memories carry seedStampedHistory's shape, so the instant to compare
	// against is the fixture's — the update's 11:00, since the resolve above it
	// moves no stamp.
	if got := readUpdatedAt(t, s, first); got != "2026-01-01 11:00:00" {
		t.Errorf("the committed memory's stamp is %q, want the last stamp write at 2026-01-01 11:00:00: "+
			"the count is not reporting a write that happened", got)
	}
	if got := readUpdatedAt(t, s, refused); got == "2026-01-01 11:00:00" {
		t.Error("the refused memory's stamp was restored, so the count included a write that rolled back")
	}
}

// versionExemptOnlyBy counts a memory's versions that the compaction's predicate
// would remove if exactly one clause were dropped, named by that clause. It is the
// other half of updateVersionsNoOtherClauseExempts and exists because a fixture
// that leaves its subject spared by TWO clauses passes a fix that only addressed
// one of them, silently.
//
// The clause is a name rather than SQL so the assertion cannot drift from the
// predicate: each name expands to the predicate with that one conjunct removed. It
// is spelled per clause and not derived, because a case that no writer can produce
// is not a case — a `reflect` version carrying related_id was staged here once and
// removed, because no shipped writer files one.
func versionExemptOnlyBy(t *testing.T, s *Store, memoryID, clause, cutoff string) int {
	t.Helper()
	var predicate string
	switch clause {
	case "phase":
		// The phase allowlist dropped, everything else the predicate says.
		predicate = historyEqualPredecessorSQL("h") +
			" AND h.rowid <> " + historyNewestVersionSQL("h.memory_id") +
			" AND h.recorded_at < ?" +
			" AND h.related_id IS NULL AND h.merged_content IS NULL"
	default:
		t.Fatalf("no such clause %q: name one the predicate is built from", clause)
	}
	var n int
	if err := s.db.QueryRow(
		`SELECT count(*) FROM memory_history h WHERE h.memory_id = ? AND `+predicate,
		memoryID, cutoff).Scan(&n); err != nil {
		t.Fatalf("count the versions exempt only by their %s: %v", clause, err)
	}
	return n
}

// updateVersionsNoOtherClauseExempts counts a memory's `update` versions that the
// compaction's predicate would remove if the phase allowed it: byte-identical to
// their predecessor, not the memory's newest version, older than the cut, and
// naming no other memory.
//
// It is the retag finding's precondition written as a check, and it is the one that
// makes the phase allowlist load-bearing rather than incidental — every other clause
// is satisfied, so a compaction that removed them would be removing a deliberate
// change that happened to look like a no-op. The two state-column helpers are the
// store's own; the phase and the cut are named here because naming them is the
// whole point.
// cutoff is passed rather than assumed, because the instant a fixture wrote its
// rows at is not the bound the run compares them against: a row stamped AT
// preFixReflectAt is older than the #727 cut and is inside the repair, which is
// the whole reason the fixture dates the rows there.
func updateVersionsNoOtherClauseExempts(t *testing.T, s *Store, memoryID, cutoff string) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`
		SELECT count(*) FROM memory_history h
		WHERE h.memory_id = ? AND h.phase = ? AND h.recorded_at < ?
		  AND `+historyEqualPredecessorSQL("h")+`
		  AND h.rowid <> `+historyNewestVersionSQL("h.memory_id")+`
		  AND h.related_id IS NULL AND h.merged_content IS NULL`,
		memoryID, phaseUpdate, cutoff).Scan(&n); err != nil {
		t.Fatalf("count the retag's versions against every clause but the phase: %v", err)
	}
	return n
}

// redundantVersionCount counts the versions of one memory that record exactly
// what the row before them records, over every column a version stores — the
// compaction's own equality predicate, read directly, so a test can assert the
// precondition a finding rests on rather than assume it.
func redundantVersionCount(t *testing.T, s *Store, memoryID string) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(
		`SELECT count(*) FROM memory_history cur
		 WHERE cur.memory_id = ? AND `+historyEqualPredecessorSQL("cur"),
		memoryID).Scan(&n); err != nil {
		t.Fatalf("read a version's predecessor comparison: %v", err)
	}
	return n
}

// armVersionDelete installs a trigger that aborts the delete of one removable
// version and reports that row's rowid, so the caller can be sure the failure
// lands on the batch it was aimed at. The row is chosen off the DELETE's OWN
// predicate rather than off a hand-written one, so the test arms the row the run
// will really reach.
func armVersionDelete(t *testing.T, s *Store, projectID string, nth int) int64 {
	t.Helper()
	cutoff, err := ResolveCompactCutoff("")
	if err != nil {
		t.Fatalf("resolve the default cutoff: %v", err)
	}
	rows, err := s.db.Query(
		`SELECT h.rowid FROM memory_history h WHERE h.project_id = ? AND `+historyRemovableRowSQL("h")+` ORDER BY h.rowid`,
		compactPredicateArgs(projectID, cutoff)...)
	if err != nil {
		t.Fatalf("read the removable versions: %v", err)
	}
	defer rows.Close() //nolint:errcheck
	var target int64
	for i := 1; rows.Next(); i++ {
		var rowid int64
		if err := rows.Scan(&rowid); err != nil {
			t.Fatalf("scan a removable version's rowid: %v", err)
		}
		if i == nth {
			target = rowid
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate the removable versions: %v", err)
	}
	if target == 0 {
		return 0
	}
	if _, err := s.db.Exec(fmt.Sprintf(`
		CREATE TRIGGER refuse_one_version_delete BEFORE DELETE ON memory_history
		WHEN OLD.rowid = %d
		BEGIN SELECT RAISE(ABORT, 'injected: this version is not deletable'); END`, target)); err != nil {
		t.Fatalf("arm the delete refusal: %v", err)
	}
	return target
}

// armMemoryStamp refuses to restore one memory's updated_at, the same way, so the
// stamp pass can be failed after a committed batch.
func armMemoryStamp(t *testing.T, s *Store, memoryID string) {
	t.Helper()
	if _, err := s.db.Exec(fmt.Sprintf(`
		CREATE TRIGGER refuse_one_stamp BEFORE UPDATE OF updated_at ON memories
		WHEN OLD.id = '%s'
		BEGIN SELECT RAISE(ABORT, 'injected: this stamp is not writable'); END`, memoryID)); err != nil {
		t.Fatalf("arm the stamp refusal: %v", err)
	}
}

// TestCompactHistoryOnlyRemovesWhatTheCutAllows: the bound is the third rule, and
// this is the test that says it is load-bearing rather than decorative.
//
// Four restatements, two on each side of the cut, and the LAST of them is the
// memory's newest version so that row is protected for a second, unrelated reason.
// Under the default cut the two pre-#727 rows go and the two post-#727 rows stay —
// and the second of those is byte-identical to its predecessor, is not the newest
// version, and names no other memory, so nothing about the row itself keeps it.
//
// The second half moves the cut past the later rows and watches them go, which is
// what shows the first half was the cut and not a coincidence. It is also the
// honest cost of the rule stated as a test: an operator whose store's clock is
// behind — a restored backup, a copied database — has rows that LOOK like damage
// and are not, and --before is how they say so.
func TestCompactHistoryOnlyRemovesWhatTheCutAllows(t *testing.T) {
	// Four restatements after the save, the two halves interleaved by instant so a
	// single mistake in one instant cannot make the fixture pass either way.
	seedBothSides := func(t *testing.T) *Store {
		t.Helper()
		s := testStore(t)
		id := createCompactMemory(t, s, compactFirstText)
		for _, at := range []string{preFixReflectAt, postFixReflectAt, preFixReflectAt, postFixReflectAt} {
			appendVersionRow(t, s, id, phaseReflect, at, "", "", nil)
		}
		if got := historyRowCount(t, s, id); got != 5 {
			t.Fatalf("fixture wrote %d history rows, want 5 (the save and four restatements)", got)
		}
		if n := redundantVersionCount(t, s, id); n != 4 {
			t.Fatalf("%d of the four restatements restate their predecessor, want 4: the fixture has to "+
				"stage rows that are byte-identical on BOTH sides of the cut", n)
		}
		return s
	}

	t.Run("the default cut spares everything a current build wrote", func(t *testing.T) {
		s := seedBothSides(t)
		res, err := s.CompactHistory(context.Background(), testProject, HistoryCompactOptions{Apply: true})
		if err != nil {
			t.Fatalf("CompactHistory: %v", err)
		}
		if res.Before != reflectNoOpCutoff {
			t.Errorf("the run used the cut %q, want the default %q", res.Before, reflectNoOpCutoff)
		}
		if res.Removed != 2 {
			t.Errorf("Removed = %d, want 2 (the two restatements older than %s)", res.Removed, reflectNoOpCutoff)
		}
	})

	t.Run("moving the cut past them takes them too", func(t *testing.T) {
		s := seedBothSides(t)
		// One second past the later rows, spelled as an RFC 3339 instant so this
		// half also carries the operator's spelling of a bound through to the store.
		const widened = "2026-09-28T18:00:01Z"
		res, err := s.CompactHistory(context.Background(), testProject, HistoryCompactOptions{
			Apply:  true,
			Before: widened,
		})
		if err != nil {
			t.Fatalf("CompactHistory: %v", err)
		}
		if res.Removed != 3 {
			t.Errorf("Removed = %d, want 3: with the cut past %s only the newest version is left", res.Removed, postFixReflectAt)
		}
		if res.Before != "2026-09-28 18:00:01" {
			t.Errorf("the run used the cut %q, want the one second past %s it was given — "+
				"the bound is reported, not merely applied", res.Before, postFixReflectAt)
		}
	})
}

// TestResolveCompactCutoff: the bound is half the meaning of every count this
// command prints, so it is resolved by one function that is exercised directly.
// The whole-day form is here because an operator who wants a wider bound knows the
// DATE the fix shipped and not the second it did, and the stored layout is here
// because the function has to be safe to call twice on the same value — the
// command resolves a flag to report it and hands the resolved stamp back.
func TestResolveCompactCutoff(t *testing.T) {
	for _, tc := range []struct {
		name, before, want string
	}{
		{name: "empty is the default", before: "", want: reflectNoOpCutoff},
		{name: "a whole day is its first second", before: "2026-09-28", want: "2026-09-28 00:00:00"},
		{name: "an RFC 3339 instant is read as written", before: "2026-09-28T18:00:01Z", want: "2026-09-28 18:00:01"},
		{name: "an offset instant is normalized to UTC", before: "2026-09-28T20:00:01+02:00", want: "2026-09-28 18:00:01"},
		{name: "the store's own layout round-trips", before: "2026-09-28 18:00:01", want: "2026-09-28 18:00:01"},
		{name: "the default is older than a row written an hour later", before: "2026-09-28T18:00:01Z", want: "2026-09-28 18:00:01"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ResolveCompactCutoff(tc.before)
			if err != nil {
				t.Fatalf("ResolveCompactCutoff(%q): %v", tc.before, err)
			}
			if got != tc.want {
				t.Errorf("ResolveCompactCutoff(%q) = %q, want %q", tc.before, got, tc.want)
			}
			// Twice, because the command does exactly that: resolve to report, hand
			// the resolved value to the store, which resolves it again.
			again, err := ResolveCompactCutoff(got)
			if err != nil || again != got {
				t.Errorf("resolving the resolved cut gave (%q, %v), want (%q, nil): the function is not idempotent", again, err, got)
			}
		})
	}
	for _, bad := range []string{"not a time", "2026-13-01", "28/09/2026", "2026-09-28 18:00"} {
		t.Run("refused: "+bad, func(t *testing.T) {
			if got, err := ResolveCompactCutoff(bad); err == nil {
				t.Errorf("ResolveCompactCutoff(%q) = %q, want a refusal: a bound this repair cannot read is a bound it must not apply", bad, got)
			}
		})
	}
}

// TestCompactHistoryRefusesACutItCannotRead: the bound decides what is deleted, so
// a cut nobody can read is refused rather than defaulted. Defaulting would be the
// worst of the three answers — it would delete the rows an operator asked to spare
// while reporting the default's numbers.
func TestCompactHistoryRefusesACutItCannotRead(t *testing.T) {
	s := testStore(t)
	id := createCompactMemory(t, s, compactFirstText)
	appendVerbatimVersion(t, s, id)
	before := historyRowCount(t, s, id)

	res, err := s.CompactHistory(context.Background(), testProject, HistoryCompactOptions{
		Apply:  true,
		Before: "yesterday-ish",
	})
	if err == nil {
		t.Fatalf("the run accepted an unreadable cut: %+v", res)
	}
	if res.Removed != 0 {
		t.Errorf("Removed = %d, want 0: a run that refused its bound removed nothing", res.Removed)
	}
	if got := historyRowCount(t, s, id); got != before {
		t.Errorf("history went from %d rows to %d, want it untouched", before, got)
	}
}

// TestCompactingRedundantVersionsLeavesTheAsOfStateAlone pins what the repair does
// to a historical read, and it is the other half of the tombstone rule.
//
// A LIVE memory's `memories` row answers created_at, so the age an as_of read
// computes is the same whichever of the memory's byte-identical versions it picks.
// Two fields DO change, and only because they NAME the answer: `VersionRecordedAt`
// and `VersionPhase` now describe an EARLIER version carrying the same state, so
// "the memory was merely saved rather than resolved or rewritten" becomes "the
// memory was resolved or rewritten" for a read of a past instant. That is a
// documented consequence rather than an accident — docs/invariants.md's memory-history
// bullet and docs/cli.md's compact section both say it — and it is the price of
// removing the flood at all, since those rows are what made a past read name a
// reflect run as the last thing that happened to a memory.
//
// The read instant is BETWEEN the reflects, and that is the whole arrangement. An
// as_of read names the newest version at or before the instant asked for, so a read
// at the far future names the memory's newest version — which no guard in this file
// will remove, and so a read there proves nothing. Reading at 2027-02-15 names the
// SECOND reflect, which the repair does remove, and the pair of reads is what shows
// the difference: the same question, the same content, a different version named.
//
// A RETIRED memory is excluded from the repair instead, and that is a different
// answer for a different reason: with no `memories` row, asOfCreatedAt falls back to
// the answering version's recorded_at, so removing a version there changes the age
// itself. TestCompactHistoryLeavesADeletedMemorysHistoryAlone covers that side.
func TestCompactingRedundantVersionsLeavesTheAsOfStateAlone(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	const content = "a memory whose history is mostly the same version"
	// Three future instants the read is placed between. They are in the future
	// because `memories.created_at` cannot be backdated, so a fixture that wants an
	// as_of read to land on a version has to put the versions after now; the run's
	// bound is widened to reach them, on purpose.
	const (
		readBetween = "2027-02-15 00:00:00"
		lastReflect = "2027-03-01 00:00:00"
	)
	id := mustSave(t, s, content)
	// The restatements, one per instant so the read has versions to choose between
	// and the run has rows spread across the side of the bound it removes.
	for _, at := range []string{asOfStampRewrite, readBetween, lastReflect} {
		appendVersionRow(t, s, id, phaseReflect, at, "", "", nil)
	}
	if n := redundantVersionCount(t, s, id); n != 3 {
		t.Fatalf("%d versions restate their predecessor byte for byte, want 3", n)
	}

	before, ok := asOfContentByID(t, mustAsOf(t, s, readBetween))[id]
	if !ok {
		t.Fatalf("at %s the memory is absent, want it live", readBetween)
	}
	if before.VersionPhase != phaseReflect || before.VersionRecordedAt != readBetween {
		t.Fatalf("before the repair the answering version is a %s at %s, want the second reflect — the "+
			"read has to be naming a version the repair will remove, or it proves nothing",
			before.VersionPhase, before.VersionRecordedAt)
	}

	res, err := s.CompactHistory(ctx, testProject, HistoryCompactOptions{Apply: true, Before: asOfStampFarFuture})
	if err != nil {
		t.Fatalf("CompactHistory: %v", err)
	}
	// Two of the three: the memory's newest version is spared, because it is the
	// statement of what the memory says now.
	if res.Removed != 2 {
		t.Fatalf("Removed = %d, want 2 (the newest version is the statement of what it says now)", res.Removed)
	}

	after, ok := asOfContentByID(t, mustAsOf(t, s, readBetween))[id]
	if !ok {
		t.Fatalf("at %s the memory is absent after the repair, want it live: compaction removes "+
			"versions, never memories", readBetween)
	}
	// Every field the state columns carry, compared one at a time so the failure
	// names the column rather than rendering two structs. These are the fields a
	// historical read is FOR, and a compaction that moved one would be answering a
	// different question than the one that was asked.
	for _, c := range []struct {
		name          string
		before, after any
	}{
		{name: "Content", before: before.Content, after: after.Content},
		{name: "Category", before: before.Category, after: after.Category},
		{name: "Importance", before: before.Importance, after: after.Importance},
		{name: "Source", before: before.Source, after: after.Source},
		{name: "ResolvedAt", before: before.ResolvedAt, after: after.ResolvedAt},
		{name: "ProjectID", before: before.ProjectID, after: after.ProjectID},
		{name: "CreatedAt", before: before.CreatedAt, after: after.CreatedAt},
		{name: "SupersededBy", before: before.SupersededBy, after: after.SupersededBy},
	} {
		if c.before != c.after {
			t.Errorf("%s = %v after the repair, want %v: a version removed because it recorded the "+
				"state of its predecessor cannot have recorded a different one", c.name, c.after, c.before)
		}
	}
	// And the two that DO move, asserted rather than left to the documentation: a
	// change here is the documented consequence, and a change anywhere else is not.
	if after.VersionPhase == before.VersionPhase || after.VersionRecordedAt == before.VersionRecordedAt {
		t.Errorf("VersionPhase/VersionRecordedAt still read %s at %s, want an EARLIER equivalent "+
			"version: the run removed the restatements the read was naming",
			after.VersionPhase, after.VersionRecordedAt)
	}
	if after.VersionPhase != phaseSave {
		t.Errorf("VersionPhase = %q, want %q: the save is the one version the repair leaves standing",
			after.VersionPhase, phaseSave)
	}
}

// TestCompactHistoryCompactsOnlyReflectVersions pins the phase allowlist itself.
// It is one phase, and that is a decision rather than an oversight, so a phase
// added to it has to be a deliberate edit of a pinned list — the state columns
// cannot tell a deliberate change from a no-op (see the test named for the retag),
// which is the whole reason the phase is what the repair goes by.
func TestCompactHistoryCompactsOnlyReflectVersions(t *testing.T) {
	if got := compactablePhases(); !wantPhases(got, []string{phaseReflect}) {
		t.Fatalf("compactablePhases() = %v, want [%s]: every other phase records a change somebody made "+
			"on purpose, and the recorded state cannot always see it", got, phaseReflect)
	}
}

// TestCompactHistoryReadsEveryColumnTheVersionStores pins the two lists the
// partition test walks, so a change to either is a deliberate act: the columns
// compared are the ones a version records, and nothing else is compared or
// exempted by name.
func TestCompactHistoryReadsEveryColumnTheVersionStores(t *testing.T) {
	want := []string{"content", "category", "importance", "resolved_at", "source"}
	if !wantPhases(historyVersionColumns, want) {
		t.Errorf("historyVersionColumns = %v, want %v", historyVersionColumns, want)
	}
	wantEvent := []string{"memory_id", "project_id", "phase", "agent", "session_id", "related_id", "merged_content"}
	if !wantPhases(historyEventColumns, wantEvent) {
		t.Errorf("historyEventColumns = %v, want %v", historyEventColumns, wantEvent)
	}
	if fmt.Sprint(historyRowColumns) != "[id recorded_at]" {
		t.Errorf("historyRowColumns = %v, want [id recorded_at]", historyRowColumns)
	}
}

// TestWidenedCompactCutoff is the predicate the command's warning rests on, and it
// is pinned at the second either side of the default because that is where a
// comparison goes wrong: `>=` instead of `>` warns on every default run, and `<`
// instead of `<=` warns one second late, on the first bound that is not a
// millisecond of risk.
func TestWidenedCompactCutoff(t *testing.T) {
	for _, tc := range []struct {
		name   string
		cutoff string
		want   bool
	}{
		{name: "the default itself is not widened", cutoff: reflectNoOpCutoff, want: false},
		{name: "a second before the fix shipped", cutoff: "2026-09-28 17:14:06", want: false},
		{name: "a day before the fix shipped", cutoff: "2026-09-27 00:00:00", want: false},
		{name: "a second after the fix shipped", cutoff: "2026-09-28 17:14:08", want: true},
		{name: "the next day", cutoff: "2026-09-29 00:00:00", want: true},
		{name: "far later", cutoff: "2030-01-01 00:00:00", want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := WidenedCompactCutoff(tc.cutoff); got != tc.want {
				t.Errorf("WidenedCompactCutoff(%q) = %v, want %v", tc.cutoff, got, tc.want)
			}
		})
	}
}

// TestWidenedCompactCutoffFollowsTheBoundTheStoreWillUse: the command warns off
// this predicate, so it has to be reading the same instant the delete reads rather
// than a copy of it. A cutoff resolved from every accepted spelling has to give the
// same answer as the same instant written out, and a spelling that resolved to a
// different one would warn about a bound the store is not using.
func TestWidenedCompactCutoffFollowsTheBoundTheStoreWillUse(t *testing.T) {
	for _, spelling := range []string{
		"", reflectNoOpCutoff, "2026-09-28", "2026-09-29", "2026-10-01",
		"2026-09-28T17:14:07Z", "2026-09-28T17:14:08Z", "2026-10-01T00:00:00Z",
	} {
		cut, err := ResolveCompactCutoff(spelling)
		if err != nil {
			t.Fatalf("ResolveCompactCutoff(%q): %v", spelling, err)
		}
		// A whole-day value means the START of that day, so 2026-09-28 is the
		// default and every other day is decided by which side of it falls.
		want := spelling != "" && spelling != "2026-09-28" && cut > reflectNoOpCutoff
		if got := WidenedCompactCutoff(cut); got != want {
			t.Errorf("spelling %q resolves to %q, widened = %v, want %v", spelling, cut, got, want)
		}
	}
}

// TestCompactHistoryStillRepairsAMemoryThatWasSupersededAfterwards is the
// regression the anchor rule introduced, read from the other side. The blocker
// fixed "a row this repair will not remove is an anchor" — and a `supersede`
// version is a row this repair will not remove, so it became an anchor, which
// closes the gate for that memory FOREVER.
//
// Forever because CreateLink writes no `memories` row at all: it files the
// supersede version and moves the stamp for nobody, so it can never be the thing
// that moved the stamp, and treating it as the last change says the stamp is
// whatever a reflection run left there.
//
// The phase is the reason, and NOT that the row records nothing new. Five of the
// nine phases here genuinely change a tracked column — `SetResolved` writes
// resolved_at, `Upsert`'s fold writes importance, a restore puts the snapshot's own
// text back — and an earlier version of this fixture staged all nine as
// byte-identical, which is a shape only the edge phases produce. It pinned the
// predicate against a fiction and hid the finding that mattered: a row that
// changed a recorded column and still moved no stamp is the harder case, and
// TestCompactHistoryFixUpdatedAtAnchorsOnAStampMoveNotOnAStateChange drives two of
// them through their real writers. Each row below is staged as ITS OWN writer files
// it, and inertPhaseRows is where that is said per phase.
//
// A superseded memory is not a rare thing, and the orientation it gets is exactly
// the one this command exists to fix: `ghost supersede` orients a candidate pair by
// updated_at, and `--skip-unchanged`'s fingerprint carries it as a change proxy.
func TestCompactHistoryStillRepairsAMemoryThatWasSupersededAfterwards(t *testing.T) {
	ctx := context.Background()
	// Every phase that files a version and moves NO live memory's updated_at, taken
	// as allHistoryPhases minus stampMovingPhases minus the one phase no live memory
	// can hold (anchorCoveredPhases) rather than as a hand-written list, so a phase
	// added to the schema has to be classified in one place or two and a phase moved
	// into the stamp-moving set without a writer behind it fails here instead of
	// quietly passing a fixture that no longer covers it.
	for _, phase := range anchorCoveredPhases() {
		t.Run(phase, func(t *testing.T) {
			staged := inertPhaseRows[phase]
			s := testStore(t)
			id := createCompactMemory(t, s, compactFirstText)

			// A prologue, for the one phase whose writer cannot be staged in place:
			// an unresolve clears resolved_at, so the row has to be holding a
			// resolved_at to clear for its version to record a change. The real
			// writer files the version from the live row, and every row above the
			// clear has to say the memory was resolved, or the two reflect versions
			// would restate the update and there would be no damage to repair.
			if phase == phaseUnresolve {
				setResolvedAt(t, s, id, "2025-12-01 09:00:00")
			}
			// A save, a real edit above it, then TWO pre-#727 no-op reflect versions,
			// then the inert event above those.
			stampHistoryRow(t, s, id, phaseSave, "2026-01-01 10:00:00")
			// The edit moves importance rather than content, for the one reason the
			// prologue above needs: UpdateMemory clears resolved_at when the CONTENT
			// changes, so a content edit would undo the resolve before the unresolve
			// had anything to do. importance is a recorded column, so this is still a
			// genuine state change and the anchor is still this version.
			raised := float32(0.6)
			if err := s.UpdateMemory(ctx, testProject, id, nil, nil, &raised, nil); err != nil {
				t.Fatalf("UpdateMemory: %v", err)
			}
			stampHistoryRow(t, s, id, phaseUpdate, "2026-02-01 10:00:00")
			appendVerbatimVersion(t, s, id)
			appendVerbatimVersion(t, s, id)
			// The event, recorded above the damage. `related` and `merged` are the
			// thread columns its phase sets, and `edits` is the recorded state its
			// writer's own UPDATE or INSERT leaves behind — the two reflect versions
			// above it were copied from the live row BEFORE any of it, so an override
			// here is a genuine difference from its predecessor rather than a
			// restatement of it.
			appendVersionRow(t, s, id, phase, "2026-03-01 10:00:00",
				staged.related, staged.merged, staged.edits)
			setUpdatedAt(t, s, id, anchorDamageTo)

			// The two preconditions, read rather than assumed. The reflect versions
			// have to be redundant or the delete never runs, and the event has to be
			// ABOVE them, so that finding it transparent is what opens the gate.
			if n := redundantVersionCount(t, s, id); n < 2 {
				t.Fatalf("%d state-identical versions, want at least 2: the two reflect versions copy "+
					"the live row, and the edit above them is the only thing that moved", n)
			}
			if _, err := firstVersionPhase(t, s, id); err != nil {
				t.Fatal(err)
			}
			newest, err := lastVersionPhase(t, s, id)
			if err != nil {
				t.Fatal(err)
			}
			if newest != phase {
				t.Fatalf("the memory's newest version is a %s, want the %s the case is about", newest, phase)
			}
			if !stampGateIsOpen(t, s, id, reflectNoOpCutoff) {
				t.Fatal("the gate is closed before the repair runs, so the fixture is not staging " +
					"the damage")
			}

			res, err := s.CompactHistory(ctx, testProject,
				HistoryCompactOptions{Apply: true, FixUpdatedAt: true})
			if err != nil {
				t.Fatalf("CompactHistory: %v", err)
			}
			if res.Removed != 2 {
				t.Errorf("Removed = %d, want 2: BOTH byte-identical reflect versions go — the inert "+
					"event above them is not what spares them, the newest-version guard is, because "+
					"the event is the newest row", res.Removed)
			}
			if res.UpdatedAt != 1 {
				t.Errorf("UpdatedAt = %d, want 1: a %s version moves no live memory's updated_at, so "+
					"it cannot be the thing that moved the stamp, and treating it as the last "+
					"change leaves a memory's updated_at reading as a reflect run's time forever",
					res.UpdatedAt, phase)
			}
			if got := readUpdatedAt(t, s, id); got != "2026-02-01 10:00:00" {
				t.Errorf("updated_at = %q, want the last recorded change's own 2026-02-01 10:00:00", got)
			}
		})
	}
}

// inertPhaseRows is how each phase outside stampMovingPhases records itself, as its
// own writer files it: the thread columns it sets, and the recorded state its
// writer's UPDATE or INSERT leaves behind.
//
// It is a map keyed by phase and the loop above indexes it without checking, so a
// phase added to allHistoryPhases and left out of this map stages the zero value —
// a byte-identical row with no thread columns, which is what the fixture used to do
// for every phase and what four of these writers do not produce. That shows up as a
// failure in the run that adds the phase, which is the moment to classify it.
var inertPhaseRows = map[string]struct {
	related, merged string
	edits           map[string]any
}{
	// CreateLink writes no memories row at all, and links.go says in as many words
	// that re-writing a live edge deliberately records nothing, so a supersede
	// version restates its predecessor and names the memory whose edge makes the
	// claim.
	phaseSupersede:   {related: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"},
	phaseUnsupersede: {related: "BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB"},
	// The fold strengthens the target and files the wording it dropped, so a merge
	// version carries BOTH. A merge recorded without the importance move is the
	// saturated case — MIN(1.0, …) landing back on the stored value — which changes
	// no recorded column and is a different row.
	phaseMerge: {merged: compactFirstText, edits: map[string]any{"importance": 0.9}},
	// The resolve writers stamp resolved_at and say in as many words that they
	// leave updated_at alone, so their versions are genuine state changes — the
	// pair TestCompactHistoryFixUpdatedAtAnchorsOnAStampMoveNotOnAStateChange drives
	// through SetResolved and ClearResolved themselves.
	phaseResolve:   {edits: map[string]any{"resolved_at": anchorChangeAt}},
	phaseUnresolve: {edits: map[string]any{"resolved_at": nil}},
	// A restore puts the snapshot's own text back, so its version records a state
	// its predecessor did not hold.
	phaseRestore: {edits: map[string]any{"content": "a text the snapshot held before the edit"}},
	// An import and a baseline are the memory's FIRST version by construction — an
	// artifact inserting a row, and the state a memory held before this build ever
	// recorded anything — so neither has a predecessor to differ from and a
	// mid-history row of either is a shape no writer produces. They are staged
	// because the phase list has to be covered either way, and what the case turns
	// on is the phase rather than the row.
	phaseImport:   {},
	phaseBaseline: {},
}

// nonStampMovingPhases is every phase the schema knows about that is NOT one whose
// writer moves a live memory's updated_at, in the order allHistoryPhases gives. It
// is the coverage list for the anchor rule: a phase absent from stampMovingPhases
// has to be transparent to the anchor, and a phase present in it has to have a
// writer that moves the column in the same statement.
func nonStampMovingPhases() []string {
	moving := make(map[string]bool, len(stampMovingPhases))
	for _, p := range stampMovingPhases {
		moving[p] = true
	}
	var out []string
	for _, p := range allHistoryPhases() {
		if !moving[p] {
			out = append(out, p)
		}
	}
	return out
}

// anchorCoveredPhases is nonStampMovingPhases without phaseDelete, and the
// exception is structural rather than an omission.
//
// Delete REMOVES the memories row, so a `delete` version that is a memory's newest
// version belongs to a memory the stamp repair cannot reach — the repair joins
// memories and a retired memory is not in it. The one shape in which a live memory's
// newest version IS a tombstone is a hand-built one, and that shape is now outside
// the repair entirely by name: TestCompactHistoryLeavesADeletedMemorysHistoryAlone
// is its test, and it is a case about the tombstone guard rather than about the
// anchor. Listing it as anchor-transparent would be asserting something about a
// memory no caller can ask this pass about.
func anchorCoveredPhases() []string {
	var out []string
	for _, p := range nonStampMovingPhases() {
		if p != phaseDelete {
			out = append(out, p)
		}
	}
	return out
}

// TestStampMovingPhasesNamesOnlyPhasesWithAWriter pins the set itself, because it
// is the one list in this file that is a CLAIM about code rather than a rule about
// rows, and a claim about code is the kind that goes stale silently.
//
// What can be checked is not "these three" — a fourth phase with a writer that moves
// updated_at would be a correct addition, and the audit that backs membership is
// written out on the var, one writer site per phase, because prose is not checkable.
// What is checkable is that every phase in the set is a phase the schema has, that
// every phase the schema has is either in the set or staged for the loop above, and
// that the two lists do not overlap. The last is the one a typo in a phase constant
// would break, and it is the one that makes a row both name another memory and move
// a live stamp — a shape the review that produced the anchor rule found does not
// exist.
func TestStampMovingPhasesNamesOnlyPhasesWithAWriter(t *testing.T) {
	known := make(map[string]bool, len(allHistoryPhases()))
	for _, p := range allHistoryPhases() {
		known[p] = true
		// A partition into three, and the third is the whole point of writing it
		// this way: every phase is either a stamp-moving writer, or staged by
		// inertPhaseRows so the loop above drives it, or phaseDelete, which no live
		// memory can hold as its newest version. A new phase belongs in one of the
		// three or it is unclassified, and the unclassified case is the one where
		// nothing fails today and a writer that moves no stamp quietly anchors a
		// repair on it.
		_, staged := inertPhaseRows[p]
		switch {
		case containsPhase(stampMovingPhases, p):
		case staged:
		case p == phaseDelete:
		default:
			t.Errorf("phase %q is in neither stampMovingPhases nor inertPhaseRows, so nothing here "+
				"pins whether its writer moves a live memory's updated_at", p)
		}
	}
	for _, p := range stampMovingPhases {
		if !known[p] {
			t.Errorf("stampMovingPhases names %q, which is not a phase of the schema: the list says "+
				"which writers move the stamp, and a phase that does not exist names no writer", p)
		}
		if _, staged := inertPhaseRows[p]; staged {
			t.Errorf("phase %q is both a stamp-moving writer and staged as an inert event, so the two "+
				"lists say opposite things about whether it may be an anchor", p)
		}
	}
	for _, p := range nonStampMovingPhases() {
		if containsPhase(stampMovingPhases, p) {
			t.Errorf("phase %q is in stampMovingPhases and outside it, so the anchor is a set with no "+
				"answer for it", p)
		}
	}
}

func containsPhase(phases []string, want string) bool {
	for _, p := range phases {
		if p == want {
			return true
		}
	}
	return false
}
