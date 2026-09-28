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
// The fixtures build the damaged history DIRECTLY rather than through writers.
// That is not a shortcut: the damage is defined as rows a pre-#727 build wrote
// and no current writer produces any more (reuseChangesNothing makes a
// re-emission that changed nothing a no-op), so there is no call left to make
// and a test that drove one would assert the fix rather than the repair.

// firstVersionText and secondVersionText are the two states the shared fixture
// moves between, so an assertion can name which version survived rather than
// counting rows and hoping.
const (
	compactFirstText  = "the relay listens on port 2222 in staging"
	compactSecondText = "the relay listens on port 2222 in production"
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
// used to leave behind for every memory it kept.
func appendVerbatimVersion(t *testing.T, s *Store, memoryID string) {
	t.Helper()
	appendVersionRow(t, s, memoryID, phaseReflect, nil, "", "", nil)
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
		appendVersionRow(t, s, id, phaseReflect, nil, "", "", nil)
		appendVersionRow(t, s, id, c.phase, nil, c.related, c.merged, nil)
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

			appendVersionRow(t, s, id, phaseReflect, nil, "", "", nil)
			appendVersionRow(t, s, id, phaseReflect, nil, "", "", map[string]any{tc.column: tc.edited})
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

// TestCompactHistoryFixUpdatedAtRestoresTheLastRealChange: the second half of the
// repair. updated_at has to say when the memory last really changed — the resolve,
// the last row that changed state — and not when a reflection that changed nothing
// looked at it.
//
// The fixture is seedStampedHistory, not the issue's own ordering, and the
// difference is load-bearing: for a stamp to be PAST the last real change the
// reflect run has to come after that change, so a run that happened before it never
// produced damage to repair and the repair correctly declines to invent any.
func TestCompactHistoryFixUpdatedAtRestoresTheLastRealChange(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	id, _, _, resolvedAt, runTo := seedStampedHistory(t, s, 5)
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
	if got := readUpdatedAt(t, s, id); got != resolvedAt {
		t.Errorf("updated_at = %q, want the last state-changing row's recorded_at (%q)", got, resolvedAt)
	}
	if res.StampsUnreadable != 0 {
		t.Errorf("StampsUnreadable = %d, want 0: every fixture stamp is readable", res.StampsUnreadable)
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
	appendVersionRow(t, s, id, phaseSave, "2026-01-01 10:00:00", "", "", nil)
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
	if got := historyRowCount(t, s, id); got != 2 {
		t.Errorf("a deleted memory kept %d history rows, want 2 (the save and its tombstone)", got)
	}
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
	id, _, _, resolvedAt, runTo := seedStampedHistory(t, s, 5)
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
	if got := readUpdatedAt(t, s, id); got != resolvedAt {
		t.Errorf("after the apply updated_at = %q, want %q", got, resolvedAt)
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

// TestCompactHistoryKeepsTheRestatementACurrentWriterStillMakes is #730's review
// finding, reproduced against the real writer rather than a fixture.
//
// ReplaceNonManual's reusePreservesAge branch (#623) re-tags a reused row and
// records a version of it, and #727 was RIGHT that this is a real change: a re-tag
// is a change to what the memory says about itself, and TestReplacedNonManual…
// EveryRecordedFieldBreaksTheNoOp pins that the version is appended. But the
// version restates content, category, importance, resolved_at and source byte for
// byte, because this table has no column for tags or scope — so from here it is
// indistinguishable from the pre-#727 damage this command removes.
//
// A compaction that deleted it would re-grow the rows the repair just pruned on
// every lifecycle pass, and --fix-updated-at would move the stamp back over the
// bump the same branch made. The newest-version guard is what prevents both, and
// this drives the real pass to prove it: one re-tag pass, then a full apply, and
// both the version and the stamp it produced survive.
//
// And the convergence, because "survives once" is not the same as "does not
// re-grow": a SECOND pass over the same unchanged re-tag makes the first version
// non-newest, and that is exactly the row the flood is made of.
func TestCompactHistoryKeepsTheRestatementACurrentWriterStillMakes(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	ids := seedVerbatimCorpus(t, s)
	target := ids[0] // the one carrying tags
	before := memoryUpdatedAt(t, s, target)

	// One pass that changes the tags and nothing else: the reusePreservesAge
	// branch, since content and category match byte for byte.
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

	// The precondition the whole finding rests on, asserted rather than assumed:
	// the pass DID write a version, and that version restates the state byte for
	// byte. If either stopped being true the finding would be moot and this test
	// would be passing for the wrong reason.
	if n := historyRowCount(t, s, target); n != 2 {
		t.Fatalf("the re-tag pass left %d history rows, want 2 (the save and the re-tag's version)", n)
	}
	if !sameStoredState(t, s, target) {
		t.Fatal("the re-tag's version is not byte-identical over the recorded state, so this is no longer the case the finding is about")
	}
	if memoryUpdatedAt(t, s, target) == before {
		t.Fatal("the re-tag pass did not move updated_at, so there is no stamp for --fix-updated-at to take back")
	}

	res, err := s.CompactHistory(ctx, testProject, HistoryCompactOptions{Apply: true, FixUpdatedAt: true})
	if err != nil {
		t.Fatalf("CompactHistory: %v", err)
	}
	if res.Removed != 0 {
		t.Errorf("Removed = %d, want 0: the re-tag's version is this memory's newest, and nothing removes that", res.Removed)
	}
	if res.UpdatedAt != 0 {
		t.Errorf("UpdatedAt = %d, want 0: there is no removable version above the last change, so the repair recognises no damage", res.UpdatedAt)
	}
	if n := historyRowCount(t, s, target); n != 2 {
		t.Errorf("history rows = %d, want 2: the compaction removed a version the store deliberately recorded", n)
	}

	// And the convergence: a second identical pass makes the first version
	// removable, and the compaction takes it — the flood, once there is one.
	// A DIFFERENT tag set, because a pass that re-states the tags the row already
	// has is the #727 no-op and writes nothing at all.
	retag("kes", "bp", "rotation", "verified")
	rowsAfterSecond := historyRowCount(t, s, target)
	if rowsAfterSecond != 3 {
		t.Fatalf("the second re-tag pass left %d history rows, want 3", rowsAfterSecond)
	}
	second, err := s.CompactHistory(ctx, testProject, HistoryCompactOptions{Apply: true})
	if err != nil {
		t.Fatalf("second CompactHistory: %v", err)
	}
	if second.Removed != 1 {
		t.Errorf("Removed = %d, want 1: the FIRST re-tag's version is no longer the newest and is exactly the redundant row", second.Removed)
	}
	if n := historyRowCount(t, s, target); n != 2 {
		t.Errorf("history rows = %d, want 2: the compaction left the table growing", n)
	}
}

// sameStoredState reports whether a memory holds a version that records exactly
// what the row before it records, over every column a version stores. It is the
// finding's precondition written as a check, in the compaction's own spelling, so
// the test above fails loudly if a future change makes the re-tag visible to the
// table — at which point the case it guards stops existing and the test would
// otherwise be passing for the wrong reason.
func sameStoredState(t *testing.T, s *Store, memoryID string) bool {
	t.Helper()
	var n int
	if err := s.db.QueryRow(
		`SELECT count(*) FROM memory_history cur
		 WHERE cur.memory_id = ? AND `+historyEqualPredecessorSQL("cur"),
		memoryID).Scan(&n); err != nil {
		t.Fatalf("read a version's predecessor comparison: %v", err)
	}
	return n > 0
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
