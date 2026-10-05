package memory

// #646 part 3: the whole-store retrieval aggregate.
//
// The property under test is that this reader answers the SAME questions as reading
// both tables and counting in Go, because a second implementation of one figure is how
// this package already produced one bug — the degraded note's denominator disagreed
// with the health block's until both divided by Scored.
//
// So the equality is the test, and it is written against BuildReport's own arithmetic
// rather than against numbers chosen by hand: a hand-written expectation asserts what
// the author believed, which is exactly what went wrong before. These fixtures state a
// store, and every field is compared against the same store read the long way.

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// totalsStore is a store at a known path, so the fixtures can backdate recorded_at —
// SQLite stamps it at write time and no store API can reach it.
func totalsStore(t *testing.T) (*Store, string) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "ghost.db")
	db, err := OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return NewStore(db, slog.New(slog.NewTextHandler(io.Discard, nil))), dbPath
}

// totalsRecord writes one recorded call whose kept rows are the given ids, and returns
// ITS OWN ROWID.
//
// The rowid comes back from the write rather than from a later read, because #857's
// write guard files a verdict only against a call that kept THAT memory — so a fixture
// that reached for "the newest row" instead of "the row this call is" would have its
// verdict silently refused and would then assert on zeroes that agree for the wrong
// reason. The guard returns the refusals rather than erroring, so only the fixture can
// see it.
func totalsRecord(t *testing.T, s *Store, project, source string, kept ...string) int64 {
	t.Helper()
	verdicts := make([]RowVerdict, 0, len(kept))
	for _, id := range kept {
		verdicts = append(verdicts, RowVerdict{ID: id, Kept: true, Stage: "fit", Reason: "fit_response"})
	}
	if err := s.RecordRetrieval(context.Background(), RetrievalRecord{
		ProjectID: project, SessionID: "s1", Source: source, Outcome: "answerable", Verdicts: verdicts,
	}); err != nil {
		t.Fatalf("RecordRetrieval(%s/%s): %v", project, source, err)
	}
	return newestRowID(t, s, project)
}

// fileVerdict files one verdict and FAILS if the store refused it. A refusal is not an
// error — the rows simply are not there — and a fixture that ignores it asserts on a
// store missing the very rows the test is about.
func fileVerdict(t *testing.T, s *Store, row RetrievalAuditRow) {
	t.Helper()
	refused, err := s.RecordRetrievalAudits(context.Background(), []RetrievalAuditRow{row})
	if err != nil {
		t.Fatalf("RecordRetrievalAudits(%s): %v", row.MemoryID, err)
	}
	if len(refused) > 0 {
		t.Fatalf("the fixture's verdict on %s was REFUSED and never stored: %+v", row.MemoryID, refused[0])
	}
}

// totalsVerdict is the short form of fileVerdict.
func totalsVerdict(t *testing.T, s *Store, project, source, memoryID, outcome string, recordRowID int64, degraded string) {
	t.Helper()
	fileVerdict(t, s, RetrievalAuditRow{
		ProjectID: project, SessionID: "s1", Source: source, MemoryID: memoryID,
		Outcome: outcome, RecordRowID: recordRowID, Degraded: degraded,
	})
}

// newestRowID is the rowid of the call just written.
func newestRowID(t *testing.T, s *Store, project string) int64 {
	t.Helper()
	recs, err := s.RetrievalRecordsForProject(context.Background(), project, 0)
	if err != nil || len(recs) == 0 {
		t.Fatalf("RetrievalRecordsForProject(%s): %v (%d records)", project, err, len(recs))
	}
	return recs[0].RowID
}

// auditPlant opens a second handle on the store's file, for the rows no write path can
// produce — a verdict naming a rowid no call owns, and a raw id the write API mints.
//
// A separate handle because the store holds the single pooled connection the test itself
// reads through, and because the point of these fixtures is to write a document or a
// rowid the API refuses.
func auditPlant(t *testing.T, dbPath string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(dbPath)+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open %s: %v", dbPath, err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// totalsFor is the fixture's own lookup. A source the aggregate omits entirely comes
// back as the zero value rather than a failure, because ABSENT is one of the states
// under test — a window that excludes every row must produce no entry, and that is the
// window working, not a missing fixture.
func totalsFor(got []RetrievalSourceTotals, source string) RetrievalSourceTotals {
	for _, x := range got {
		if x.Source == source {
			return x
		}
	}
	return RetrievalSourceTotals{Source: source}
}

// TestRetrievalSourceTotalsCountsEveryShapeTheTablesHold: one source, one call
// keeping two memories, one call keeping none, and a verdict against each kept pair.
//
// The duplicate kept id is the case worth having in the fixture: two stages of ONE call
// keeping the same memory is one (call, memory) pair, which is the table's grain, so
// kept is 2 and not 3. This is the shape a single-project reader gets right by
// de-duplicating in Go and an aggregate gets wrong by counting rows.
func TestRetrievalSourceTotalsCountsEveryShapeTheTablesHold(t *testing.T) {
	s, _ := totalsStore(t)
	ctx := context.Background()
	if err := s.EnsureProject(ctx, "p1", "/tmp/totals-p1", "p1"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	for _, id := range []string{"M1", "M2"} {
		if _, err := s.CreateWithID(ctx, "p1", id, Memory{Content: "content for " + id, Category: "fact", Source: "manual"}); err != nil {
			t.Fatalf("CreateWithID(%s): %v", id, err)
		}
	}
	// One call keeping the same memory twice (two stages of one call) plus a second,
	// and one call keeping nothing. Each kept pair gets its OWN call, because
	// RecordRetrievalAudits replaces a call's verdicts rather than adding to them —
	// which is itself worth being here, so the fixture cannot be read as "two verdicts
	// on one call are two rows".
	first := totalsRecord(t, s, "p1", "search", "M1", "M1", "M2")
	second := totalsRecord(t, s, "p1", "search", "M3")
	totalsRecord(t, s, "p1", "search")
	totalsVerdict(t, s, "p1", "search", "M1", "used", first, "")
	totalsVerdict(t, s, "p1", "search", "M3", "ignored", second, "")

	got, err := s.RetrievalSourceTotals(ctx, time.Time{})
	if err != nil {
		t.Fatalf("RetrievalSourceTotals: %v", err)
	}
	// Kept is 3 and not 4: the call that kept M1 twice kept it ONCE, because two
	// stages of one call admitting the same memory is one (call, memory) pair. The
	// other two calls contributed one pair and none.
	want := RetrievalSourceTotals{
		Source: "search", Calls: 3, Kept: 3, KeptNothing: 1,
		Scored: 2, Used: 1, Ignored: 1,
	}
	if diff := diffTotals(want, totalsFor(got, "search")); diff != "" {
		t.Errorf("the aggregate disagrees with the rows it summarizes:\n%s", diff)
	}
}

// TestRetrievalSourceTotalsIsNotConfusedByAnUnreadableVerdictsColumn: the
// verdicts column is TEXT and nothing constrains what is in it, so a row can hold a
// document this build cannot read — and json_each ERRORS on a malformed one rather
// than returning nothing. Unguarded, one such row would fail the whole health call.
//
// The unreadable shapes are all real: a malformed document (hand-edited), a JSON
// object, and a bare JSON string — all valid json_each inputs that yield no `kept`
// rows. All must read as "this call kept nothing", which is what the Go reader's
// failed Unmarshal into []RowVerdict yields.
func TestRetrievalSourceTotalsIsNotConfusedByAnUnreadableVerdictsColumn(t *testing.T) {
	s, dbPath := totalsStore(t)
	ctx := context.Background()
	if err := s.EnsureProject(ctx, "p1", "/tmp/totals-p1", "p1"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	only := totalsRecord(t, s, "p1", "search", "M1")

	// Reached through a second handle, because no store API writes a raw document
	// into that column and writing one by hand is the point.
	db := auditPlant(t, dbPath)
	// NULL is not among the shapes: retrieval_record.verdicts is NOT NULL, which is a
	// fact this test discovered the hard way (the write refuses it), so a NULL stamp is
	// impossible here however malformed the document is.
	for i, doc := range []any{`{not json at all`, `{"kept":true}`, `"a bare string"`} {
		if _, err := db.Exec(`UPDATE retrieval_record SET verdicts = ? WHERE rowid = ?`, doc, only); err != nil {
			t.Fatalf("plant shape %d: %v", i, err)
		}
		got, err := s.RetrievalSourceTotals(ctx, time.Time{})
		if err != nil {
			t.Fatalf("shape %d (%v) failed the aggregate: %v", i, doc, err)
		}
		search := totalsFor(got, "search")
		if search.Calls != 1 || search.Kept != 0 || search.KeptNothing != 1 {
			t.Errorf("shape %d (%v) read as %+v, want one call that kept nothing", i, doc, search)
		}
	}
}

// TestRetrievalSourceTotalsSplitsTheVerdictsByWhatTheyName: the three attributions, on
// one source, because the aggregate's verdict half is a decision and this is the
// decision — a verdict counts only if a counted call owns its rowid, and the other two
// are counted and NAMED rather than dropped.
//
// Detached is the one that regresses silently: with the membership test missing, a
// verdict whose call the report does not count lands in the figures and a precision is
// reported over two populations.
//
// Detached is also the one shape the CURRENT WRITER cannot produce: #857 refuses a
// verdict naming a call that did not keep that memory, so a store written by this build
// alone has none. It is real all the same — the call cap holds 5000 rows while the
// verdict cap holds 50000, so a call evicted under pressure takes no verdicts with it,
// and a store written before #857 has whatever its writer filed. So the fixture plants
// it through SQL, which is the only way to reach it, and says so where a reader would
// otherwise go looking for the missing write call.
func TestRetrievalSourceTotalsSplitsTheVerdictsByWhatTheyName(t *testing.T) {
	s, dbPath := totalsStore(t)
	ctx := context.Background()
	if err := s.EnsureProject(ctx, "p1", "/tmp/totals-p1", "p1"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	call := totalsRecord(t, s, "p1", "search", "M1")
	totalsVerdict(t, s, "p1", "search", "M1", "used", call, "")

	// A verdict naming no call: the shape the write accepts for a verdict about a
	// session rather than about one call.
	totalsVerdict(t, s, "p1", "search", "M2", "ignored", 0, "")

	// A verdict naming a rowid no row owns — what the call cap's eviction leaves
	// behind, and what a pre-#857 store may hold. It must be DETACHED and counted,
	// never dropped: a report that quietly loses real verdicts is indistinguishable
	// from one over a store where they were never written.
	db := auditPlant(t, dbPath)
	if _, err := db.Exec(`INSERT INTO retrieval_audit
		(project_id, record_rowid, session_id, source, memory_id, outcome, signal, degraded)
		VALUES ('p1', 999999, 's9', 'search', 'M3', 'ignored', '', '')`); err != nil {
		t.Fatalf("plant the detached verdict: %v", err)
	}

	got, err := s.RetrievalSourceTotals(ctx, time.Time{})
	if err != nil {
		t.Fatalf("RetrievalSourceTotals: %v", err)
	}
	want := RetrievalSourceTotals{
		Source: "search", Calls: 1, Kept: 1,
		Scored: 1, Used: 1, Ignored: 0,
		Unattributed: 1, Detached: 1,
	}
	if diff := diffTotals(want, totalsFor(got, "search")); diff != "" {
		t.Errorf("the attribution split disagrees with the rows:\n%s", diff)
	}
}

// TestRetrievalSourceTotalsKeepsAnUnreadableStamp: a row whose recorded_at cannot be
// parsed is KEPT, because a report is not the operator's request to lose anything. A
// window filter that dropped it would silently shrink a figure on exactly the stores
// most likely to hold hand-edited rows.
func TestRetrievalSourceTotalsKeepsAnUnreadableStamp(t *testing.T) {
	s, dbPath := totalsStore(t)
	ctx := context.Background()
	if err := s.EnsureProject(ctx, "p1", "/tmp/totals-p1", "p1"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	totalsRecord(t, s, "p1", "search", "M1")

	db := auditPlant(t, dbPath)
	if _, err := db.Exec(`UPDATE retrieval_record SET recorded_at = 'not a timestamp' WHERE project_id = 'p1'`); err != nil {
		t.Fatalf("backdate to an unreadable stamp: %v", err)
	}

	// A floor that excludes everything written in the last day.
	floor := time.Now().Add(-24 * time.Hour)
	got, err := s.RetrievalSourceTotals(ctx, floor)
	if err != nil {
		t.Fatalf("RetrievalSourceTotals: %v", err)
	}
	if search := totalsFor(got, "search"); search.Calls != 1 {
		t.Errorf("a window dropped the row whose stamp it could not read: %+v", search)
	}

	// And a row WITH a readable stamp outside the window is dropped, so the test above
	// is not passing because the window does nothing.
	if _, err := db.Exec(`UPDATE retrieval_record SET recorded_at = ? WHERE project_id = 'p1'`,
		time.Now().Add(-72*time.Hour).UTC().Format(StoredStampLayout)); err != nil {
		t.Fatalf("backdate out of the window: %v", err)
	}
	got, err = s.RetrievalSourceTotals(ctx, floor)
	if err != nil {
		t.Fatalf("RetrievalSourceTotals: %v", err)
	}
	if search := totalsFor(got, "search"); search.Calls != 0 {
		t.Errorf("the window admitted a row three days old against a 24h floor: %+v", search)
	}
}

// TestRetrievalSourceTotalsCountsBothProjectsInOneSource: the store-wide report pools
// PROJECTS within a source, which is the whole point of it and the reason it may exist
// at all — a search is a search whichever project ran it. This is the property
// MergeProjects held, asserted on the reader that replaces the merge.
func TestRetrievalSourceTotalsCountsBothProjectsInOneSource(t *testing.T) {
	s, _ := totalsStore(t)
	ctx := context.Background()
	for _, p := range []string{"p1", "p2"} {
		if err := s.EnsureProject(ctx, p, "/tmp/totals-"+p, p); err != nil {
			t.Fatalf("EnsureProject(%s): %v", p, err)
		}
		call := totalsRecord(t, s, p, "search", "M"+p)
		totalsVerdict(t, s, p, "search", "M"+p, "used", call, "")
	}
	// A second source in one project, which must stay its own line.
	totalsRecord(t, s, "p1", "session_start", "Ms")

	got, err := s.RetrievalSourceTotals(ctx, time.Time{})
	if err != nil {
		t.Fatalf("RetrievalSourceTotals: %v", err)
	}
	if diff := diffTotals(RetrievalSourceTotals{Source: "search", Calls: 2, Kept: 2, Scored: 2, Used: 2}, totalsFor(got, "search")); diff != "" {
		t.Errorf("two projects' searches were not pooled into one line:\n%s", diff)
	}
	if diff := diffTotals(RetrievalSourceTotals{Source: "session_start", Calls: 1, Kept: 1}, totalsFor(got, "session_start")); diff != "" {
		t.Errorf("a source was pooled into another:\n%s", diff)
	}
}

// TestRetrievalSourceTotalsNamesEveryReasonAndId: the two lists the renderers print.
// Distinct and sorted, because a reason repeated per row is a list of every verdict
// rather than of every reason, and an unsorted list is a different list on every run.
func TestRetrievalSourceTotalsNamesEveryReasonAndId(t *testing.T) {
	s, _ := totalsStore(t)
	ctx := context.Background()
	if err := s.EnsureProject(ctx, "p1", "/tmp/totals-p1", "p1"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	// Three calls, so each verdict is filed against its own call: the write REPLACES a
	// call's verdicts, so three verdicts on one call would be one row and the fixture
	// would assert nothing about distinctness.
	keptBoth := totalsRecord(t, s, "p1", "search", "M1", "M2", "M1")
	keptM1 := totalsRecord(t, s, "p1", "search", "M1")
	keptM2 := totalsRecord(t, s, "p1", "search", "M2")
	// The SAME memory contradicted by two different calls, under two different reasons,
	// and one reason repeated: the lists must name each thing once while the COUNTS
	// stay per verdict.
	totalsVerdict(t, s, "p1", "search", "M1", "contradicted", keptM1, "scan transcript: truncated")
	totalsVerdict(t, s, "p1", "search", "M1", "contradicted", keptBoth, "scan transcript: read failure")
	totalsVerdict(t, s, "p1", "search", "M2", "contradicted", keptM2, "scan transcript: truncated")

	got, err := s.RetrievalSourceTotals(ctx, time.Time{})
	if err != nil {
		t.Fatalf("RetrievalSourceTotals: %v", err)
	}
	search := totalsFor(got, "search")
	if want := []string{"M1", "M2"}; !reflect.DeepEqual(search.ContradictedIDs, want) {
		t.Errorf("ContradictedIDs = %v, want %v (distinct and sorted)", search.ContradictedIDs, want)
	}
	if want := []string{"scan transcript: read failure", "scan transcript: truncated"}; !reflect.DeepEqual(search.DegradedReasons, want) {
		t.Errorf("DegradedReasons = %v, want %v (distinct and sorted)", search.DegradedReasons, want)
	}
	if search.DegradedVerdicts != 3 {
		t.Errorf("DegradedVerdicts = %d, want 3 — a degraded verdict is counted, not collapsed", search.DegradedVerdicts)
	}
	if search.Contradicted != 3 || search.Scored != 3 {
		t.Errorf("the contradicted bucket does not count every verdict: %+v", search)
	}
}

// TestRetrievalSourceTotalsKeepsAContradictedIDHoldingAComma: a memory id is arbitrary
// stored text, not a 32-hex shape, so the id list must not be built by pasting ids
// together and cutting the paste apart again.
//
// The id here is one a real store can hold: retrieval_audit.memory_id deliberately has
// no shape CHECK ("forbidding any future build that moves to a different id shape"),
// CheckImportedID refuses only length, control characters, whitespace, a backtick and
// «», and `ghost import` writes an artifact's ids verbatim — so a comma survives every
// write path into this column. An id carrying one is then split into two ids that name
// no row, printed under `contradicted (search):` as if they were memories of this
// store, while the count beside them stays right. That is a wrong-but-plausible answer,
// which is the only kind this package exists to refuse.
//
// Two calls and two verdicts, so the two readings are distinguishable: the COUNT is per
// verdict (2) and the LIST names each memory once (1). A reader that has split the id
// reports two ids for one memory.
func TestRetrievalSourceTotalsKeepsAContradictedIDHoldingAComma(t *testing.T) {
	s, _ := totalsStore(t)
	ctx := context.Background()
	if err := s.EnsureProject(ctx, "p1", "/tmp/totals-p1", "p1"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	const commaID = "M1,M2"
	if _, err := s.CreateWithID(ctx, "p1", commaID, Memory{Content: "content for " + commaID, Category: "fact", Source: "manual"}); err != nil {
		t.Fatalf("CreateWithID(%q): %v", commaID, err)
	}
	// Each verdict on its OWN call, because the write replaces a call's verdicts
	// rather than adding to them.
	first := totalsRecord(t, s, "p1", "search", commaID)
	second := totalsRecord(t, s, "p1", "search", commaID)
	totalsVerdict(t, s, "p1", "search", commaID, "contradicted", first, "")
	totalsVerdict(t, s, "p1", "search", commaID, "contradicted", second, "")

	got, err := s.RetrievalSourceTotals(ctx, time.Time{})
	if err != nil {
		t.Fatalf("RetrievalSourceTotals: %v", err)
	}
	search := totalsFor(got, "search")
	if want := []string{commaID}; !reflect.DeepEqual(search.ContradictedIDs, want) {
		t.Errorf("ContradictedIDs = %q, want %q — an id is stored text and a separator that occurs inside it is not a separator",
			search.ContradictedIDs, want)
	}
	if search.Contradicted != 2 || search.Scored != 2 {
		t.Errorf("the contradicted bucket does not count every verdict: %+v", search)
	}
}

// TestRetrievalSourceTotalsNamesOnlyTheContradictedIDsItCounted: the id list and the
// figures come from two statements now, and a list that ran wider than its figure would
// name a memory under a count that does not include it.
//
// So a contradiction naming no call, and one naming a call this report does not count,
// are counted in Unattributed and Detached and named by NEITHER — they are in no
// figure, and a memory listed under a figure it is not part of reads as a memory the
// source contradicted.
//
// The detached verdict is planted rather than written, because #857's guard refuses a
// verdict naming a call that did not keep the memory and no call holds that rowid. That
// is the same reason the attribution test plants it, and the same honest note: the shape
// is real in a store this build's writer did not produce.
func TestRetrievalSourceTotalsNamesOnlyTheContradictedIDsItCounted(t *testing.T) {
	s, dbPath := totalsStore(t)
	ctx := context.Background()
	if err := s.EnsureProject(ctx, "p1", "/tmp/totals-p1", "p1"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	counted := totalsRecord(t, s, "p1", "search", "M1")
	totalsVerdict(t, s, "p1", "search", "M1", "contradicted", counted, "")
	// A contradiction about a session rather than about a call: the shape the write
	// accepts for record_rowid 0.
	totalsVerdict(t, s, "p1", "search", "M2", "contradicted", 0, "")
	// A verdict that is neither out of population nor a contradiction, so the two
	// halves of the id statement's WHERE are separable: this memory is kept, judged
	// and counted, and is not a memory the source contradicted.
	used := totalsRecord(t, s, "p1", "search", "M4")
	totalsVerdict(t, s, "p1", "search", "M4", "used", used, "")

	db := auditPlant(t, dbPath)
	if _, err := db.Exec(`INSERT INTO retrieval_audit
		(project_id, record_rowid, session_id, source, memory_id, outcome, signal, degraded)
		VALUES ('p1', 999999, 's9', 'search', 'M3', 'contradicted', '', '')`); err != nil {
		t.Fatalf("plant the detached contradiction: %v", err)
	}

	got, err := s.RetrievalSourceTotals(ctx, time.Time{})
	if err != nil {
		t.Fatalf("RetrievalSourceTotals: %v", err)
	}
	search := totalsFor(got, "search")
	if want := []string{"M1"}; !reflect.DeepEqual(search.ContradictedIDs, want) {
		t.Errorf("ContradictedIDs = %q, want %q — a memory is named only under a figure that counts it",
			search.ContradictedIDs, want)
	}
	if search.Contradicted != 1 || search.Scored != 2 {
		t.Errorf("a contradiction in no figure reached the figures: %+v", search)
	}
	if search.Used != 1 {
		t.Errorf("the used verdict is not counted: %+v", search)
	}
	if search.Unattributed != 1 || search.Detached != 1 {
		t.Errorf("the two out-of-population contradictions are not counted as such: %+v", search)
	}
}

// TestRetrievalSourceTotalsWindowsTheIDsItNames: --since is a claim about a period, and
// the two statements must answer it identically. A list that ignored the window would
// name a memory from outside the period under a count computed inside it — the report
// would send a reader to a contradiction the period excluded, and the count beside it
// would still say the period held one thing.
//
// The state is the LATE VERDICT, and it is a real one: retrieval_audit is stamped when
// the detached lifecycle run JUDGED the call, so a verdict can land hours or days after
// the call it is about. Backdating the verdict alone is the only arrangement that tests
// this statement's own window — backdate the CALL as well and the membership test
// reports the verdict as detached, which the id statement filters anyway, so the
// window would go untested behind an unrelated guard.
//
// The two halves are asserted together on purpose: a list right for the wrong period is
// a different bug from a count right for the wrong period, and a test that checked only
// the count would pass with the list wide open.
func TestRetrievalSourceTotalsWindowsTheIDsItNames(t *testing.T) {
	s, dbPath := totalsStore(t)
	ctx := context.Background()
	if err := s.EnsureProject(ctx, "p1", "/tmp/totals-p1", "p1"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	// Both calls are inside the window and stay there, so the membership test passes for
	// both verdicts and the only thing that can exclude the second is the window.
	first := totalsRecord(t, s, "p1", "search", "M1")
	totalsVerdict(t, s, "p1", "search", "M1", "contradicted", first, "")
	second := totalsRecord(t, s, "p1", "search", "M2")
	totalsVerdict(t, s, "p1", "search", "M2", "contradicted", second, "")

	db := auditPlant(t, dbPath)
	if _, err := db.Exec(`UPDATE retrieval_audit SET recorded_at = ? WHERE memory_id = 'M2'`,
		time.Now().Add(-72*time.Hour).UTC().Format(StoredStampLayout)); err != nil {
		t.Fatalf("backdate the verdict out of the window: %v", err)
	}

	// A floor that excludes everything written in the last day.
	got, err := s.RetrievalSourceTotals(ctx, time.Now().Add(-24*time.Hour))
	if err != nil {
		t.Fatalf("RetrievalSourceTotals: %v", err)
	}
	search := totalsFor(got, "search")
	if want := []string{"M1"}; !reflect.DeepEqual(search.ContradictedIDs, want) {
		t.Errorf("ContradictedIDs = %q, want %q — the id list answered a different period than the figures",
			search.ContradictedIDs, want)
	}
	if search.Contradicted != 1 || search.Scored != 1 {
		t.Errorf("the window did not reach the contradicted bucket: %+v", search)
	}
	// And the two calls are still counted: only the late verdict left the period, so a
	// test that quietly lost the call would read the same list.
	if search.Calls != 2 || search.Kept != 2 {
		t.Errorf("the window dropped calls it should have kept: %+v", search)
	}
}

// TestRetrievalSourceTotalsIsSortedBySource: the aggregate's order must not depend on
// SQLite's grouping, or two runs of the same store render two different blocks.
func TestRetrievalSourceTotalsIsSortedBySource(t *testing.T) {
	s, _ := totalsStore(t)
	ctx := context.Background()
	if err := s.EnsureProject(ctx, "p1", "/tmp/totals-p1", "p1"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	// Seeded in reverse-alphabetical order, so an unsorted reader fails here.
	for _, src := range []string{"zeta", "mid", "alpha"} {
		totalsRecord(t, s, "p1", src, "M1")
	}

	got, err := s.RetrievalSourceTotals(ctx, time.Time{})
	if err != nil {
		t.Fatalf("RetrievalSourceTotals: %v", err)
	}
	var names []string
	for _, x := range got {
		names = append(names, x.Source)
	}
	if want := []string{"alpha", "mid", "zeta"}; !reflect.DeepEqual(names, want) {
		t.Errorf("sources came back as %v, want %v", names, want)
	}
}

// diffTotals renders the fields that disagree, or "" when they do not.
func diffTotals(want, got RetrievalSourceTotals) string {
	var b []string
	note := func(field string, w, g any) {
		if fmt.Sprint(w) != fmt.Sprint(g) {
			b = append(b, fmt.Sprintf("%s: want %v, got %v", field, w, g))
		}
	}
	note("Source", want.Source, got.Source)
	note("Calls", want.Calls, got.Calls)
	note("Kept", want.Kept, got.Kept)
	note("KeptNothing", want.KeptNothing, got.KeptNothing)
	note("Scored", want.Scored, got.Scored)
	note("Used", want.Used, got.Used)
	note("Ignored", want.Ignored, got.Ignored)
	note("Superseded", want.Superseded, got.Superseded)
	note("Contradicted", want.Contradicted, got.Contradicted)
	note("DegradedVerdicts", want.DegradedVerdicts, got.DegradedVerdicts)
	note("Unattributed", want.Unattributed, got.Unattributed)
	note("Detached", want.Detached, got.Detached)
	note("ContradictedIDs", want.ContradictedIDs, got.ContradictedIDs)
	note("DegradedReasons", want.DegradedReasons, got.DegradedReasons)
	return joinLines(b)
}

func joinLines(b []string) string {
	out := ""
	for _, s := range b {
		out += "\n  " + s
	}
	return out
}
