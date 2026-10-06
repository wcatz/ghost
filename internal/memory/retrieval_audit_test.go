package memory

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// auditRow is one verdict as a caller states it.
func auditRow(memoryID, source, outcome, signal string) RetrievalAuditRow {
	return RetrievalAuditRow{
		ProjectID: "p1",
		Source:    source,
		MemoryID:  memoryID,
		Outcome:   outcome,
		Signal:    signal,
	}
}

// auditStore opens a store with a project and a retrieval record whose verdicts
// name two memories, so a comparison has something to judge. It returns the
// store and its database, because two tests below need the raw handle to write a
// row the writer's own checks would refuse.
func auditStore(t *testing.T) (*Store, *sql.DB, context.Context) {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "ghost.db")
	db, err := OpenDB(path)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	store := NewStore(db, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := store.EnsureProject(ctx, "p1", "/tmp/audit-p1", "p1"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	if err := store.RecordRetrieval(ctx, RetrievalRecord{
		ProjectID: "p1",
		Source:    "search",
		Outcome:   "answerable",
		Verdicts: []RowVerdict{
			{ID: "MEM1", Kept: true, Stage: "fit", Reason: "fit_response"},
			{ID: "MEM2", Kept: true, Stage: "fit", Reason: "fit_response"},
		},
	}); err != nil {
		t.Fatalf("RecordRetrieval: %v", err)
	}
	return store, db, ctx
}

func readAuditRows(t *testing.T, store *Store) []RetrievalAuditRow {
	t.Helper()
	rows, err := store.RetrievalAudits(context.Background(), "", "")
	if err != nil {
		t.Fatalf("RetrievalAudits: %v", err)
	}
	return rows
}

// TestTheAuditWriteReportsTheVerdictsItRefused: a write that silently drops a row
// cannot be told from a write that stored it, and the only thing that makes the
// difference visible is the value this returns.
//
// The dropped row is a real product of the guard above — a verdict whose call has
// been purged — so the caller above has already counted it into every figure it
// will print. Returning nothing leaves that count describing a table it does not
// match, and the branch that wrote nothing is indistinguishable from the branch
// that wrote everything.
//
// It must be the refused ROWS and not a count, because the caller has to subtract
// them from per-source figures as well as the total, and a bare count cannot say
// which source or which outcome it came from.
func TestTheAuditWriteReportsTheVerdictsItRefused(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	live := recordCallKeeping(t, s, testProject, "MEM-A", 830)
	// No call holds this rowid and none ever did, which is the state a purge
	// leaves behind. Both rows name the SAME memory, so a refused set keyed on the
	// memory alone would report the live one too.
	const gone = 9999
	batch := []RetrievalAuditRow{
		{ProjectID: testProject, RecordRowID: live, SessionID: "s1", Source: "search",
			MemoryID: "MEM-A", Outcome: "used", Signal: "identifier"},
		{ProjectID: testProject, RecordRowID: gone, SessionID: "s1", Source: "session_start",
			MemoryID: "MEM-A", Outcome: "ignored"},
	}

	refused, err := s.RecordRetrievalAudits(ctx, batch)
	if err != nil {
		t.Fatalf("RecordRetrievalAudits: %v", err)
	}
	if len(refused) != 1 {
		t.Fatalf("the write reports %d refused row(s), want 1 — the row naming the rowid no call holds. "+
			"Reported: %+v", len(refused), refused)
	}
	if refused[0].RecordRowID != gone || refused[0].Source != "session_start" || refused[0].Outcome != "ignored" {
		t.Errorf("the refused row is %+v, want the one naming rowid %d under source session_start with outcome "+
			"ignored — the caller subtracts per source and per outcome, so those fields have to survive the "+
			"round trip", refused[0], gone)
	}
	if got := readAuditRows(t, s); len(got) != 1 || got[0].RecordRowID != live {
		t.Errorf("the table holds %+v, want only the live call's verdict at rowid %d", got, live)
	}

	// A batch with nothing to refuse reports nothing, and says so by being empty
	// rather than by a sentinel: a caller that prints "0 refused" and a caller that
	// prints nothing must not be different code paths.
	clean, err := s.RecordRetrievalAudits(ctx, batch[:1])
	if err != nil {
		t.Fatalf("RecordRetrievalAudits (clean): %v", err)
	}
	if len(clean) != 0 {
		t.Errorf("a batch the guard accepts reports %d refused row(s), want none: %+v", len(clean), clean)
	}
}

func TestRecordRetrievalAuditsRoundTrip(t *testing.T) {
	store, _, ctx := auditStore(t)
	recs, err := store.RetrievalRecordsForProject(ctx, "p1", 10)
	if err != nil {
		t.Fatalf("RetrievalRecordsForProject: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("got %d record(s), want 1", len(recs))
	}
	if recs[0].RowID == 0 {
		t.Error("RowID = 0; a verdict cannot name the call it belongs to without it")
	}
	recID := recs[0].RowID

	rows := []RetrievalAuditRow{
		{ProjectID: "p1", RecordRowID: recID, Source: "search", SessionID: "", MemoryID: "MEM1",
			Outcome: "used", Signal: "identifier"},
		{ProjectID: "p1", RecordRowID: recID, Source: "search", SessionID: "", MemoryID: "MEM2",
			Outcome: "ignored", Signal: ""},
	}
	if _, err := store.RecordRetrievalAudits(ctx, rows); err != nil {
		t.Fatalf("RecordRetrievalAudits: %v", err)
	}
	got := readAuditRows(t, store)
	if len(got) != 2 {
		t.Fatalf("stored %d verdict(s), want 2", len(got))
	}
	byID := map[string]RetrievalAuditRow{}
	for _, r := range got {
		byID[r.MemoryID] = r
	}
	if byID["MEM1"].Outcome != "used" || byID["MEM1"].Signal != "identifier" || byID["MEM1"].RecordRowID != recID {
		t.Errorf("MEM1 = %+v, want used/identifier against rowid %d", byID["MEM1"], recID)
	}
	if byID["MEM2"].Outcome != "ignored" || byID["MEM2"].Signal != "" {
		t.Errorf("MEM2 = %+v, want ignored with no signal", byID["MEM2"])
	}
}

// TestRecordRetrievalAuditsStoresTheContentHash: since schema v22 a verdict
// carries the hash of the content it judged, and UsefulnessByMemory withdraws a
// row whose stamp does not match what is stored now (#879). So the stamp has to
// SURVIVE the round trip byte for byte — a writer that dropped it would leave
// every row reading as a pre-v22 row, and every verdict on every store would
// quietly fall back to the legacy timestamp rule.
//
// The empty stamp is asserted beside the filled one for the mirror reason: an
// unstamped row is how a pre-v22 row reads, and the writer producing one for a
// caller that supplied a hash would be the same defect from the other side.
func TestRecordRetrievalAuditsStoresTheContentHash(t *testing.T) {
	store, _, ctx := auditStore(t)
	recs, err := store.RetrievalRecordsForProject(ctx, "p1", 10)
	if err != nil || len(recs) != 1 {
		t.Fatalf("RetrievalRecordsForProject: %v (%d records)", err, len(recs))
	}
	const judged = "the text the comparison judged"
	stamped := ContentHash(judged)
	rows := []RetrievalAuditRow{
		{ProjectID: "p1", RecordRowID: recs[0].RowID, Source: "search", MemoryID: "MEM1",
			Outcome: VerdictOutcomeContradicted, ContentHash: stamped},
		{ProjectID: "p1", Source: "search", MemoryID: "MEM2",
			Outcome: VerdictOutcomeContradicted},
	}
	if refused, err := store.RecordRetrievalAudits(ctx, rows); err != nil || len(refused) > 0 {
		t.Fatalf("RecordRetrievalAudits: %v (refused: %+v)", err, refused)
	}
	got := readAuditRows(t, store)
	if len(got) != 2 {
		t.Fatalf("stored %d verdict(s), want 2", len(got))
	}
	byID := map[string]RetrievalAuditRow{}
	for _, r := range got {
		byID[r.MemoryID] = r
	}
	if h := byID["MEM1"].ContentHash; h != stamped {
		t.Errorf("MEM1 content_hash = %q, want %q — a stamp that does not survive the round trip sends every "+
			"verdict back to the legacy rule", h, stamped)
	}
	if h := byID["MEM1"].ContentHash; len(h) != 64 {
		t.Errorf("MEM1 content_hash is %d bytes, want a 64-character sha256 hex digest", len(h))
	}
	if h := byID["MEM2"].ContentHash; h != "" {
		t.Errorf("MEM2 content_hash = %q, want empty: an unstamped row is how a pre-v22 verdict reads", h)
	}
}

// TestRetrievalAuditsFiltersByProjectAndOutcome is the read the report surface
// needs, and the reason the filters exist: a per-project, per-bucket figure is
// what the issue asked for, and pooling two projects' rows would be a number
// about neither.
func TestRetrievalAuditsFiltersByProjectAndOutcome(t *testing.T) {
	store, _, ctx := auditStore(t)
	if err := store.EnsureProject(ctx, "p2", "/tmp/audit-p2", "p2"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	rows := []RetrievalAuditRow{
		auditRow("MEM1", "search", "used", "identifier"),
		auditRow("MEM2", "search", "ignored", ""),
		{ProjectID: "p2", Source: "search", MemoryID: "MEM9", Outcome: "contradicted", Signal: "negation"},
	}
	if _, err := store.RecordRetrievalAudits(ctx, rows); err != nil {
		t.Fatalf("RecordRetrievalAudits: %v", err)
	}

	only, err := store.RetrievalAudits(ctx, "p1", "contradicted")
	if err != nil {
		t.Fatalf("RetrievalAudits: %v", err)
	}
	if len(only) != 0 {
		t.Errorf("p1/contradicted returned %d row(s), want 0", len(only))
	}
	all, err := store.RetrievalAudits(ctx, "p1", "")
	if err != nil {
		t.Fatalf("RetrievalAudits: %v", err)
	}
	if len(all) != 2 {
		t.Errorf("p1 returned %d row(s), want 2", len(all))
	}
	everywhere, err := store.RetrievalAudits(ctx, "", "")
	if err != nil {
		t.Fatalf("RetrievalAudits: %v", err)
	}
	if len(everywhere) != 3 {
		t.Errorf("an unfiltered read returned %d row(s), want 3", len(everywhere))
	}
}

// TestRecordRetrievalAuditsReplacesTheRecordsItJudges: the stop hook fires per
// turn, so the same recorded call is audited more than once. A second pass must
// REPLACE its verdicts, or every turn would add a row and the counts would grow
// with the length of the session rather than with the calls it made.
func TestRecordRetrievalAuditsReplacesTheRecordsItJudges(t *testing.T) {
	store, _, ctx := auditStore(t)
	recs, err := store.RetrievalRecordsForProject(ctx, "p1", 10)
	if err != nil {
		t.Fatalf("RetrievalRecordsForProject: %v", err)
	}
	recID := recs[0].RowID

	first := []RetrievalAuditRow{
		{ProjectID: "p1", RecordRowID: recID, Source: "search", MemoryID: "MEM1", Outcome: "ignored"},
		{ProjectID: "p1", RecordRowID: recID, Source: "search", MemoryID: "MEM2", Outcome: "ignored"},
	}
	if _, err := store.RecordRetrievalAudits(ctx, first); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	// The next turn's transcript knows better: MEM1 was read after all.
	second := []RetrievalAuditRow{
		{ProjectID: "p1", RecordRowID: recID, Source: "search", MemoryID: "MEM1", Outcome: "used", Signal: "identifier"},
	}
	if _, err := store.RecordRetrievalAudits(ctx, second); err != nil {
		t.Fatalf("second pass: %v", err)
	}
	got := readAuditRows(t, store)
	if len(got) != 1 {
		t.Fatalf("stored %d verdict(s) after two passes over one call, want 1", len(got))
	}
	if got[0].Outcome != "used" {
		t.Errorf("outcome = %q, want the second pass's %q", got[0].Outcome, "used")
	}
}

// TestRecordRetrievalAuditsRejectsARowWithNoMemory: a verdict is about a memory.
// A row naming none is a row nothing can ever be counted against, and it would
// sit in the table forever because nothing else would replace it.
func TestRecordRetrievalAuditsRejectsARowWithNoMemory(t *testing.T) {
	store, _, ctx := auditStore(t)
	_, err := store.RecordRetrievalAudits(ctx, []RetrievalAuditRow{
		{ProjectID: "p1", Source: "search", Outcome: "used"},
	})
	if err == nil {
		t.Fatal("RecordRetrievalAudits accepted a verdict naming no memory")
	}
}

// TestRecordRetrievalAuditsRejectsARowWithNoProject: the same refusal as the
// record write's — a verdict filed under nothing answers "which project's
// retrieval was this" as "all of them", which is a claim about no call.
func TestRecordRetrievalAuditsRejectsARowWithNoProject(t *testing.T) {
	store, _, ctx := auditStore(t)
	_, err := store.RecordRetrievalAudits(ctx, []RetrievalAuditRow{
		{MemoryID: "MEM1", Source: "search", Outcome: "used"},
	})
	if err == nil {
		t.Fatal("RecordRetrievalAudits accepted a verdict naming no project")
	}
}

// TestRecordRetrievalAuditsEvictsOldestOverCap: the table is bounded, and the
// bound evicts the OLDEST rows — the evidence an operator is looking at is the
// recent past, so losing the far end is what keeps the window useful.
//
// Two more calls are recorded before the cap is lowered, so each pass below files
// against a call the store actually holds. That is the shape a pass has in
// production and it is the only shape the write accepts: a verdict naming a rowid
// no call owns is refused (see
// TestAVerdictForACallThatHasGoneAwayIsDroppedRatherThanInherited), so a fixture
// that invented rowids 2 and 3 out of a single recorded call would now store
// nothing and this test would be measuring the refusal instead of the eviction.
func TestRecordRetrievalAuditsEvictsOldestOverCap(t *testing.T) {
	store, _, ctx := auditStore(t)
	// Two more calls, each KEEPING the memory the pass below files against it —
	// the fixture's own call (rowid 1) kept MEM1 and MEM2. A call that kept
	// nothing cannot be given a verdict: the write files one only against a call
	// that kept that memory, so this fixture would otherwise be measuring the
	// refusal instead of the eviction.
	for i, memory := range []string{"MEM2", "MEM3"} {
		if err := store.RecordRetrieval(ctx, RetrievalRecord{
			ProjectID: "p1", Source: "search", QueryHash: digest(600 + i),
			Outcome: "answerable",
			Verdicts: []RowVerdict{
				{ID: memory, Kept: true, Stage: "fit", Reason: "fit_response"},
			},
		}); err != nil {
			t.Fatalf("RecordRetrieval %d: %v", i, err)
		}
	}
	restore := retrievalAuditRowsCap
	retrievalAuditRowsCap = 2
	t.Cleanup(func() { retrievalAuditRowsCap = restore })

	for i := 1; i <= 3; i++ {
		rows := []RetrievalAuditRow{{
			ProjectID: "p1", RecordRowID: int64(i), Source: "search",
			MemoryID: "MEM" + string(rune('0'+i)), Outcome: "ignored",
		}}
		if _, err := store.RecordRetrievalAudits(ctx, rows); err != nil {
			t.Fatalf("pass %d: %v", i, err)
		}
	}
	got := readAuditRows(t, store)
	if len(got) != 2 {
		t.Fatalf("stored %d row(s) at a cap of 2", len(got))
	}
	if got[0].MemoryID != "MEM2" || got[1].MemoryID != "MEM3" {
		t.Errorf("surviving memories = %q,%q — the oldest row must be the one evicted", got[0].MemoryID, got[1].MemoryID)
	}
}

// TestRecordRetrievalAuditsStoresNoText is the constraint the whole feature is
// built on, asserted the only way it can be: on the bytes. A column that could
// hold a sentence would let a future writer put one there and every reader here
// would keep working.
func TestRecordRetrievalAuditsStoresNoText(t *testing.T) {
	store, db, ctx := auditStore(t)
	const (
		transcriptPhrase = "mangoes are the only durable fruit here"
		queryPhrase      = "how do I configure the transcript sweep"
	)
	if _, err := store.CreateWithID(ctx, "p1", "MEM1", Memory{
		Content: transcriptPhrase, Category: "gotcha", Source: "manual",
	}); err != nil {
		t.Fatalf("CreateWithID: %v", err)
	}
	if _, err := store.RecordRetrievalAudits(ctx, []RetrievalAuditRow{
		{ProjectID: "p1", RecordRowID: 1, Source: "search", MemoryID: "MEM1", Outcome: "used", Signal: "identifier"},
	}); err != nil {
		t.Fatalf("RecordRetrievalAudits: %v", err)
	}

	rows, err := db.Query(`SELECT project_id, record_rowid, session_id, source, memory_id, outcome, signal
		FROM retrieval_audit`)
	if err != nil {
		t.Fatalf("read the raw table: %v", err)
	}
	defer rows.Close() //nolint:errcheck
	var columns []string
	for rows.Next() {
		var projectID, sessionID, source, memoryID, outcome, signal string
		var rowid int64
		if err := rows.Scan(&projectID, &rowid, &sessionID, &source, &memoryID, &outcome, &signal); err != nil {
			t.Fatalf("scan: %v", err)
		}
		columns = append(columns, projectID, sessionID, source, memoryID, outcome, signal)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	joined := strings.Join(columns, "|")
	for _, phrase := range []string{transcriptPhrase, "mangoes", "durable", "fruit", queryPhrase, "configure"} {
		if strings.Contains(joined, phrase) {
			t.Errorf("retrieval_audit holds %q: the table is ids and a fixed vocabulary", phrase)
		}
	}
}

// TestHistoryPurgeRemovesTheVerdictsNamingAMemory: a purge is asked to remove a
// NAME, and this table keeps one — the same reason it deletes memory_provenance
// and the rows of retrieval_record that judge the memory.
func TestHistoryPurgeRemovesTheVerdictsNamingAMemory(t *testing.T) {
	store, _, ctx := auditStore(t)
	// The purge needs the memories to EXIST: it refuses to delete a name it cannot
	// find, and refusing is the right behaviour — it is how a purge of a memory
	// another process already deleted stays from reporting a success it did not
	// have. The verdict rows are what this test is about, so the memories are
	// created here rather than in the shared fixture.
	for _, id := range []string{"MEM1", "MEM2"} {
		if _, err := store.CreateWithID(ctx, "p1", id, Memory{
			Content: "a fixture memory " + id, Category: "gotcha", Source: "manual",
		}); err != nil {
			t.Fatalf("CreateWithID %s: %v", id, err)
		}
	}
	if _, err := store.RecordRetrievalAudits(ctx, []RetrievalAuditRow{
		auditRow("MEM1", "search", "used", "identifier"),
		auditRow("MEM2", "search", "ignored", ""),
	}); err != nil {
		t.Fatalf("RecordRetrievalAudits: %v", err)
	}
	if err := store.DeleteWithOptions(ctx, "MEM1", DeleteOptions{PurgeHistory: true}); err != nil {
		t.Fatalf("purge: %v", err)
	}
	got := readAuditRows(t, store)
	if len(got) != 1 || got[0].MemoryID != "MEM2" {
		t.Errorf("verdicts after the purge = %+v, want only MEM2's", got)
	}
}

// TestAPurgeDoesNotLeaveAVerdictThatTheNextCallCanInherit: #852. A call admits
// MEM1 and MEM2, so the audit holds (rowid, MEM1) and (rowid, MEM2). Purging
// MEM1 takes the record row — its verdicts name MEM1 — and with it the
// (rowid, MEM1) verdict, which is right. The (rowid, MEM2) verdict is NOT the
// purged memory's, so nothing about the purge's own predicate reaches it, and it
// survives pointing at a row that no longer exists.
//
// That is only half the damage, and the half that is bounded: the orphan is
// counted by RetrievalAudits, the report's denominator, against a call nobody
// can read. The other half is that retrieval_record has no AUTOINCREMENT, so the
// freed rowid is handed to the NEXT call — and the orphan is silently
// re-attributed to a call that never admitted MEM2.
//
// The assertion is made BEFORE the second call, because that is the state the
// bug is in: an orphan is already wrong the moment its call is gone, and the
// reuse only decides which wrong call is blamed for it. The reuse is then
// checked as a PRECONDITION rather than as a hope — if a later build stops
// reusing freed rowids, an orphan stops being inherited and stops being
// misattributed, and this test must be re-read rather than left passing for the
// wrong reason.
func TestAPurgeDoesNotLeaveAVerdictThatTheNextCallCanInherit(t *testing.T) {
	store, db, ctx := auditStore(t)
	recs, err := store.RetrievalRecordsForProject(ctx, "p1", 10)
	if err != nil {
		t.Fatalf("RetrievalRecordsForProject: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("got %d record(s), want the fixture's 1", len(recs))
	}
	freed := recs[0].RowID

	// The purge needs the memories to EXIST — it refuses to delete a name it
	// cannot find, and refusing is right, so the verdict rows are not what this
	// fixture has to arrange.
	for _, id := range []string{"MEM1", "MEM2"} {
		if _, err := store.CreateWithID(ctx, "p1", id, Memory{
			Content: "a fixture memory " + id, Category: "gotcha", Source: "manual",
		}); err != nil {
			t.Fatalf("CreateWithID %s: %v", id, err)
		}
	}
	// Filed against the CALL's rowid, which is the shape a row has in production.
	if _, err := store.RecordRetrievalAudits(ctx, []RetrievalAuditRow{
		{ProjectID: "p1", RecordRowID: freed, Source: "search", MemoryID: "MEM1",
			Outcome: "used", Signal: "identifier"},
		{ProjectID: "p1", RecordRowID: freed, Source: "search", MemoryID: "MEM2", Outcome: "ignored"},
	}); err != nil {
		t.Fatalf("RecordRetrievalAudits: %v", err)
	}

	if err := store.DeleteWithOptions(ctx, "MEM1", DeleteOptions{PurgeHistory: true}); err != nil {
		t.Fatalf("purge: %v", err)
	}

	// The record row is gone, so the rowid is free — the state the second half
	// of the test depends on.
	var records int
	if err := db.QueryRow(`SELECT count(*) FROM retrieval_record`).Scan(&records); err != nil {
		t.Fatalf("count retrieval records: %v", err)
	}
	if records != 0 {
		t.Fatalf("%d record row(s) survive the purge, want 0 — the premise is that rowid %d is free", records, freed)
	}
	for _, r := range readAuditRows(t, store) {
		if r.RecordRowID == freed {
			t.Errorf("the verdict about %s still names record rowid %d, which the purge removed: it is a verdict "+
				"about a call that no longer exists, and the next call to take that rowid inherits it", r.MemoryID, freed)
		}
	}

	// And the reuse, asserted rather than assumed: this is the step that turns an
	// orphan into a wrong report, so a build that stopped freeing rowids must not
	// let this test keep passing on the strength of a reuse that no longer happens.
	if err := store.RecordRetrieval(ctx, RetrievalRecord{
		ProjectID: "p1", Source: "search", Outcome: "answerable",
		Verdicts: []RowVerdict{{ID: "MEM2", Kept: true, Stage: "fit", Reason: "fit_response"}},
	}); err != nil {
		t.Fatalf("RecordRetrieval (the next call): %v", err)
	}
	next, err := store.RetrievalRecordsForProject(ctx, "p1", 10)
	if err != nil {
		t.Fatalf("RetrievalRecordsForProject: %v", err)
	}
	if len(next) != 1 {
		t.Fatalf("got %d record(s) after the second call, want 1", len(next))
	}
	if next[0].RowID != freed {
		t.Fatalf("the second call took rowid %d, not the freed %d — freed rowids are no longer reused, so this "+
			"test no longer demonstrates anything and must be re-read", next[0].RowID, freed)
	}
	// The call that now owns rowid `freed` has admitted MEM2 and been given no
	// verdict of its own, so nothing may be attributed to it.
	for _, r := range readAuditRows(t, store) {
		if r.RecordRowID == freed {
			t.Errorf("the new call at rowid %d is credited with a verdict about %s (%s, %s) — it is the call the "+
				"purge removed, not the one that just ran", freed, r.MemoryID, r.Outcome, r.Source)
		}
	}
}

// TestTheRecordCapPairsOnEveryEvictionNotOnlyTheFirst: the pairing is not a
// one-shot that fires when the table first crosses the bound.
//
// retrieval_record has no AUTOINCREMENT and its window never empties on its own,
// so its rowid grows monotonically and `rowid > retrievalRecordRowsCap` is true of
// EVERY insert from the cap-th onward — the steady state evicts exactly ONE record
// row per call. A review of this PR read the sweep as amortised over a bulk
// eviction and was right to: the shape that reads as amortised (one insert, many
// rows gone) happens once, when a store is seeded past the cap, and never again.
// So this walks three consecutive evictions and asks each one to take its own
// call's verdict — the second and third are the ones a first-crossing-only
// implementation would leave behind, and a sweep that deleted on a rising counter
// instead of on the eviction's own bound would pass the first test and fail here.
func TestTheRecordCapPairsOnEveryEvictionNotOnlyTheFirst(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	// Two audited calls at the plain rowids the arithmetic below is written
	// against, then a cap of one: every insert from the second onward crosses it,
	// so the second and third inserts are the steady state itself.
	calls := seedAuditedCalls(t, s, testProject, 2)
	restore := retrievalRecordRowsCap
	retrievalRecordRowsCap = 1
	t.Cleanup(func() { retrievalRecordRowsCap = restore })

	// judge records a call that KEPT memory and then files a verdict against the
	// rowid it was given, which is the order the assembler produces them in. It
	// returns that rowid rather than assuming one, so the assertions below name
	// the row the store actually wrote.
	judge := func(call int, memory string) int64 {
		t.Helper()
		if err := s.RecordRetrieval(ctx, RetrievalRecord{
			ProjectID: testProject, Source: "search", QueryHash: digest(call),
			Outcome: "answerable",
			Verdicts: []RowVerdict{
				{ID: memory, Kept: true, Stage: "fit", Reason: "fit_response"},
			},
		}); err != nil {
			t.Fatalf("RecordRetrieval (call %d): %v", call, err)
		}
		recs, err := s.RetrievalRecords(ctx, 1)
		if err != nil {
			t.Fatalf("RetrievalRecords: %v", err)
		}
		if len(recs) != 1 {
			t.Fatalf("RetrievalRecords returned %d rows, want 1", len(recs))
		}
		if _, err := s.RecordRetrievalAudits(ctx, []RetrievalAuditRow{{
			ProjectID: testProject, RecordRowID: recs[0].RowID, SessionID: "s1",
			Source: "search", MemoryID: memory, Outcome: "ignored",
		}}); err != nil {
			t.Fatalf("RecordRetrievalAudits (call %d): %v", call, err)
		}
		return recs[0].RowID
	}

	// assertOnly reads the WHOLE table, not a filtered view of it: an orphan
	// anywhere in it has to fail rather than being averaged away by the verdict
	// that should have survived.
	assertOnly := func(step, wantMemory string, wantRowID int64) {
		t.Helper()
		got := readAuditRows(t, s)
		if len(got) != 1 {
			t.Fatalf("%s: %d verdict(s) in the table, want 1 — a cap of one keeps one call, so at most one "+
				"verdict can belong to a call the store still holds (%+v)", step, len(got), got)
		}
		if got[0].RecordRowID != wantRowID || got[0].MemoryID != wantMemory {
			t.Errorf("%s: the surviving verdict is (%s, rowid %d), want (%s, rowid %d)", step,
				got[0].MemoryID, got[0].RecordRowID, wantMemory, wantRowID)
		}
	}

	// calls[0] is judged, then two more calls run. The first evicts calls[0] and
	// takes its verdict; the second evicts the call the first one wrote and takes
	// THAT call's verdict. The third call's write is the one a pairing that only
	// fires when the table FIRST crosses the bound would leave an orphan behind.
	if _, err := s.RecordRetrievalAudits(ctx, []RetrievalAuditRow{{
		ProjectID: testProject, RecordRowID: calls[0], SessionID: "s1", Source: "search",
		MemoryID: "MEM-1", Outcome: "ignored",
	}}); err != nil {
		t.Fatalf("RecordRetrievalAudits (the seeded call): %v", err)
	}
	second := judge(902, "MEM-2")
	assertOnly("after the second call", "MEM-2", second)

	third := judge(903, "MEM-3")
	assertOnly("after the third call", "MEM-3", third)

	// And the record table is where the steady state is stated as a fact rather
	// than inferred: one row in, one row out, forever.
	recs, err := s.RetrievalRecords(ctx, 10)
	if err != nil {
		t.Fatalf("RetrievalRecords: %v", err)
	}
	if len(recs) != 1 {
		t.Errorf("the record table holds %d rows at a cap of 1, want 1", len(recs))
	}
}

// TestTheRecordCapTakesTheVerdictsWithIt: #852's second delete path. The cap
// evicts the OLDEST recorded calls, and a verdict filed against an evicted call
// outlives it — the same orphan as the purge's, reached by a different predicate
// (a rowid bound, not a memory id) and therefore missed by a fix written only for
// the purge.
//
// The purge's reuse is what makes its orphan visible in a report; the cap's is
// not, and the difference is worth stating rather than leaving implied. The cap
// keeps `cap` rows, so it never frees the HIGHEST rowid, and the next insert is
// always max(rowid)+1 — so a cap orphan cannot be inherited by the next call. It
// is still wrong: a verdict counted in the denominator of a precision report
// belongs to a call the store no longer holds, and it is bounded only by the
// audit's own cap. That is why the assertion is "no verdict names an evicted
// rowid" and not "no verdict was inherited".
func TestTheRecordCapTakesTheVerdictsWithIt(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	// Three calls, each already judged, each with a verdict of its own.
	calls := seedAuditedCalls(t, s, testProject, 3)
	// One verdict per call, plus an UNATTRIBUTED one (record_rowid 0) — the second
	// half of this test's subject. Zero is the deliberate "not attributable to a
	// call" value, and the eviction's bound is a ROWID bound: a bare
	// `record_rowid <= ?` reads 0 as "at or below the bound" and takes every
	// unattributed verdict in the table the first time the cap evicts anything,
	// which no other seed here would catch because every other one names a real
	// call.
	rows := make([]RetrievalAuditRow, 0, len(calls)+1)
	for i, rowid := range calls {
		rows = append(rows, RetrievalAuditRow{
			ProjectID: testProject, RecordRowID: rowid, SessionID: "s1", Source: "search",
			MemoryID: "MEM-" + string(rune('1'+i)), Outcome: "ignored",
		})
	}
	rows = append(rows, RetrievalAuditRow{
		ProjectID: testProject, RecordRowID: 0, SessionID: "s1", Source: "search",
		MemoryID: "MEM-SESSION", Outcome: "ignored",
	})
	if _, err := s.RecordRetrievalAudits(ctx, rows); err != nil {
		t.Fatalf("RecordRetrievalAudits: %v", err)
	}

	// Lowered AFTER the seed so the fixture's own rowids are the plain 1,2,3 the
	// eviction's arithmetic is written against, and the fourth call is the one
	// that crosses the bound: at a cap of 2 it evicts everything at or below rowid
	// 4-2, which is calls[0] and calls[1].
	restore := retrievalRecordRowsCap
	retrievalRecordRowsCap = 2
	t.Cleanup(func() { retrievalRecordRowsCap = restore })
	if err := s.RecordRetrieval(ctx, RetrievalRecord{
		ProjectID: testProject, Source: "search", QueryHash: digest(999),
		Outcome: "answerable",
		Verdicts: []RowVerdict{
			{ID: "MEM-4", Kept: true, Stage: "fit", Reason: "fit_response"},
		},
	}); err != nil {
		t.Fatalf("RecordRetrieval (the call past the cap): %v", err)
	}

	survivors := readAuditRows(t, s)
	for _, r := range survivors {
		for _, evicted := range calls[:2] {
			if r.RecordRowID == evicted {
				t.Errorf("the verdict about %s still names record rowid %d, which the cap evicted: it is a verdict "+
					"about a call the store no longer holds", r.MemoryID, evicted)
			}
		}
		if r.RecordRowID != calls[2] && r.RecordRowID != 0 {
			t.Errorf("the surviving verdict names rowid %d, want only the call the cap kept (%d) or the "+
				"unattributed one (0)", r.RecordRowID, calls[2])
		}
	}
	if len(survivors) != 2 {
		t.Fatalf("%d verdict(s) survive the cap's eviction, want 2 (the kept call's and the unattributed one)",
			len(survivors))
	}
	// Named, not merely counted: the count above is also what a sweep that took
	// everything below the bound would leave if it took the kept call's verdict
	// instead, and the unattributed row is the one the sweep cannot be allowed to
	// take.
	kept, unattributed := 0, 0
	for _, r := range survivors {
		switch r.RecordRowID {
		case calls[2]:
			kept++
		case 0:
			unattributed++
		}
	}
	if kept != 1 || unattributed != 1 {
		t.Errorf("survivors are %d attributed to the kept call and %d unattributed, want 1 and 1 — an unattributed "+
			"verdict belongs to no call and the eviction must not reach it", kept, unattributed)
	}
}

// TestAVerdictForACallThatHasGoneAwayIsDroppedRatherThanInherited: #852's WRITE
// side, and the one door the delete-side pairing above leaves open.
//
// audit.Run reads the recent calls (RetrievalRecordsForProject), judges them —
// a GetByIDs and a comparison against the transcript — and only then files the
// verdicts, keyed by the rowids it read at the TOP of the run. A purge landing in
// that window deletes the call row and, with the pairing, takes its verdicts with
// it; the write then re-files them against a rowid the store has already freed.
// When the purged call is the newest, its rowid IS the table's maximum, so the
// very next RecordRetrieval takes it — and a verdict about what the purged call
// admitted becomes a verdict about what the NEXT call never saw. The cap's
// eviction is not reachable in that window (it evicts rowids far below
// CallWindow), so the purge is the live door.
//
// A hole is preferred to that, and the reason is worth stating rather than
// leaving to inference: the call the purge removed is gone, so the (call, memory)
// pair this verdict was about no longer exists and the report has lost ONE pair.
// A re-filed verdict is not a lost pair but a WRONG one, counted in the
// denominator under a call that never admitted the memory — and it is
// indistinguishable from a real one afterwards, because nothing about the row
// says which call it was read against.
//
// The batch is mixed on purpose. One call survives the purge and one does not,
// and a guard that refused the whole batch would satisfy "nothing was inherited"
// by storing nothing at all — which is why the surviving call's verdict is
// asserted present and NAMED, not merely counted.
func TestAVerdictForACallThatHasGoneAwayIsDroppedRatherThanInherited(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	// The purge needs the memories to EXIST — it refuses to delete a name it
	// cannot find — and each call must admit a DIFFERENT one, or purging either
	// would take both record rows and the surviving call this test needs would go
	// with them.
	for _, id := range []string{"MEM-A", "MEM-B"} {
		if _, err := s.CreateWithID(ctx, testProject, id, Memory{
			Content: "a fixture memory " + id, Category: "gotcha", Source: "manual",
		}); err != nil {
			t.Fatalf("CreateWithID %s: %v", id, err)
		}
	}
	// call records a call admitting exactly one memory and returns the rowid the
	// store gave it rather than assuming one, so the assertions below name the
	// row that exists.
	seq := 0
	call := func(memory string) int64 {
		t.Helper()
		seq++
		if err := s.RecordRetrieval(ctx, RetrievalRecord{
			ProjectID: testProject, Source: "search", QueryHash: digest(800 + seq),
			Outcome:  "answerable",
			Verdicts: []RowVerdict{{ID: memory, Kept: true, Stage: "fit", Reason: "fit_response"}},
		}); err != nil {
			t.Fatalf("RecordRetrieval (%s): %v", memory, err)
		}
		recs, err := s.RetrievalRecords(ctx, 1)
		if err != nil {
			t.Fatalf("RetrievalRecords: %v", err)
		}
		if len(recs) != 1 {
			t.Fatalf("RetrievalRecords returned %d rows, want 1", len(recs))
		}
		return recs[0].RowID
	}
	first := call("MEM-A")
	newest := call("MEM-B")

	// The pass that reads, judges and writes normally, before anything is purged.
	// Its verdicts are what the sweep below is expected to take.
	pass := []RetrievalAuditRow{
		{ProjectID: testProject, RecordRowID: first, SessionID: "s1", Source: "search",
			MemoryID: "MEM-A", Outcome: "used", Signal: "identifier"},
		{ProjectID: testProject, RecordRowID: newest, SessionID: "s1", Source: "search",
			MemoryID: "MEM-B", Outcome: "ignored"},
	}
	if _, err := s.RecordRetrievalAudits(ctx, pass); err != nil {
		t.Fatalf("RecordRetrievalAudits (the first pass): %v", err)
	}

	// The purge lands inside the second pass's read-judge-write window. MEM-B is
	// the memory only the NEWEST call admitted, so this frees `newest` and leaves
	// `first` alone — which is the state the assertions below are about.
	if err := s.DeleteWithOptions(ctx, "MEM-B", DeleteOptions{PurgeHistory: true}); err != nil {
		t.Fatalf("purge: %v", err)
	}
	recs, err := s.RetrievalRecords(ctx, 10)
	if err != nil {
		t.Fatalf("RetrievalRecords: %v", err)
	}
	if len(recs) != 1 || recs[0].RowID != first {
		t.Fatalf("the purge left %d record row(s) (%+v), want just the call at rowid %d — the premise is that "+
			"rowid %d is free and the one at %d is not", len(recs), recs, first, newest, first)
	}

	// The late write: the same pass, filing what it judged at the top of its run.
	// One of the two calls it read no longer exists, and the write has to say so
	// rather than store a verdict against a rowid the store has given away.
	if _, err := s.RecordRetrievalAudits(ctx, pass); err != nil {
		t.Fatalf("RecordRetrievalAudits (the late pass): %v", err)
	}

	got := readAuditRows(t, s)
	for _, r := range got {
		if r.RecordRowID == newest {
			t.Errorf("the late pass stored a verdict about %s (%s) under record rowid %d, which the purge freed: "+
				"the call that admitted it is gone, and the next call to take that rowid inherits a verdict about a "+
				"memory it never admitted", r.MemoryID, r.Outcome, r.RecordRowID)
		}
	}
	if len(got) != 1 {
		t.Fatalf("%d verdict(s) in the table after the late pass, want 1 — the surviving call's, because a guard "+
			"that dropped the whole batch would store nothing and still inherit nothing (%+v)", len(got), got)
	}
	if got[0].RecordRowID != first || got[0].MemoryID != "MEM-A" {
		t.Errorf("the surviving verdict is (%s, rowid %d), want (MEM-A, rowid %d) — a guard that dropped the whole "+
			"batch, or filed the dead call's, satisfies neither",
			got[0].MemoryID, got[0].RecordRowID, first)
	}

	// And the reuse, asserted rather than assumed: this is the step that turns a
	// stored orphan into a wrong report, so a build that stopped freeing rowids
	// must not let this test keep passing on the strength of a reuse that no
	// longer happens.
	taken := call("MEM-A")
	if taken != newest {
		t.Fatalf("the next call took rowid %d, not the freed %d — freed rowids are no longer reused, so this test "+
			"no longer demonstrates anything and must be re-read", taken, newest)
	}
	for _, r := range readAuditRows(t, s) {
		if r.RecordRowID == newest {
			t.Errorf("the call that now owns rowid %d is credited with a verdict about %s (%s) — it is not the call "+
				"that was purged, and it admitted nothing that was judged", newest, r.MemoryID, r.Outcome)
		}
	}
}

// TestTheAuditCapStillBoundsTheTableWhenAPassDropsARow: the write-side guard's
// second-order consequence, and the one a later reader of RecordRetrievalAudits
// gets wrong by accident. The eviction rides on the rowid of the last row the
// pass FILED, so a pass that drops a row must leave that bound alone: a dropped
// row takes no rowid, and a bound reset on the drop path silently stops the
// eviction — and the table then grows past its cap on every pass that races a
// purge, which is a slower and quieter failure than the orphan it was fixing.
//
// Nothing above asserts it, because a dropped row is only reachable through the
// window TestAVerdictForACallThatHasGoneAwayIsDroppedRatherThanInherited sets
// up, and that test never fills the table. The dropped row goes LAST on purpose:
// that is the position that decides the bound, so a guard that coped with a drop
// in the middle of a batch and not at the end would be a guard with a hole in the
// one place the cap reads it.
func TestTheAuditCapStillBoundsTheTableWhenAPassDropsARow(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	calls := seedAuditedCalls(t, s, testProject, 3)

	// Lowered after seeding, so the calls above are at the plain rowids 1, 2, 3
	// rather than at a cap's bound, and two of them are already judged: a table
	// seeded with two verdicts and a cap of two sits AT the cap, so the next row
	// filed is the one that makes the eviction live.
	restore := retrievalAuditRowsCap
	retrievalAuditRowsCap = 2
	t.Cleanup(func() { retrievalAuditRowsCap = restore })

	// The memory each seeded call kept: call i kept MEM-<i+1>, and a verdict may
	// only name a memory its own call kept.
	verdict := func(i int, record int64) RetrievalAuditRow {
		return RetrievalAuditRow{
			ProjectID: testProject, RecordRowID: record, SessionID: "s1",
			Source: "search", MemoryID: "MEM-" + string(rune('1'+i)), Outcome: "ignored",
		}
	}
	if _, err := s.RecordRetrievalAudits(ctx, []RetrievalAuditRow{verdict(0, calls[0]), verdict(1, calls[1])}); err != nil {
		t.Fatalf("RecordRetrievalAudits (the seeded pass): %v", err)
	}

	// A rowid beyond the table's, so nothing about the refusal is ambiguous: no
	// call holds it and none ever did, which is the state a purge leaves behind
	// and the state a future build that stops reusing rowids would leave a
	// verdict in.
	const gone = 9999
	if _, err := s.RecordRetrievalAudits(ctx, []RetrievalAuditRow{verdict(2, calls[2]), verdict(2, gone)}); err != nil {
		t.Fatalf("RecordRetrievalAudits (the pass that drops a row): %v", err)
	}

	got := readAuditRows(t, s)
	if len(got) != 2 {
		t.Fatalf("%d verdict(s) in the table at a cap of 2, want 2 — the drop must not stand in for a filed row "+
			"in the bound the eviction reads (%+v)", len(got), got)
	}
	// Named, not only counted: the count is also what a table holding the two
	// NEWEST verdicts and a stale oldest one adds up to, and which pair survived
	// is the whole of what the eviction is for.
	for i, r := range got {
		want := calls[i+1]
		if r.RecordRowID != want {
			t.Errorf("verdict %d names record rowid %d, want %d — at a cap of two the table keeps the two "+
				"newest, so the oldest is the row the eviction exists to take", i, r.RecordRowID, want)
		}
	}
}

// recordCallKeeping records one call that KEPT memory, and returns the rowid the
// store gave it rather than assuming one.
//
// "Kept" is the whole of what it takes, and it is why this exists instead of
// RecordRetrieval with a hand-built verdicts slice at each call site: a verdict is
// only filed against a call that admitted its memory, so a fixture whose call kept
// nothing is a fixture whose verdicts the writer is right to drop, and it would
// then be measuring the refusal rather than what the test is about.
//
// n seeds the query digest so repeated calls do not collide, and the memories are
// real rows because a purge refuses to delete a name it cannot find.
func recordCallKeeping(t *testing.T, s *Store, projectID, memory string, n int) int64 {
	t.Helper()
	ctx := context.Background()
	if _, err := s.CreateWithID(ctx, projectID, memory, Memory{
		Content: "a fixture memory " + memory, Category: "gotcha", Source: "manual",
	}); err != nil {
		t.Fatalf("CreateWithID %s: %v", memory, err)
	}
	if err := s.RecordRetrieval(ctx, RetrievalRecord{
		ProjectID: projectID, Source: "search", QueryHash: digest(n),
		Outcome: "answerable",
		Verdicts: []RowVerdict{
			{ID: memory, Kept: true, Stage: "fit", Reason: "fit_response"},
		},
	}); err != nil {
		t.Fatalf("RecordRetrieval (%s): %v", memory, err)
	}
	recs, err := s.RetrievalRecords(ctx, 1)
	if err != nil {
		t.Fatalf("RetrievalRecords: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("RetrievalRecords returned %d rows, want 1", len(recs))
	}
	return recs[0].RowID
}

// TestAVerdictForACallThatHasBeenReplacedIsNotInheritedByItsSuccessor: the guard
// the write needs is not "the rowid still exists" but "the call at this rowid is
// one that KEPT this memory".
//
// The gap it closes is the one an existence check cannot see. The window in
// audit.Run (read calls at run.go:127, judge at 179-201, file at 214/224) is not
// only wide enough for a purge to leave the rowid VACANT — it is wide enough for
// the purge to be followed by a new search, so the freed rowid is RE-LET before the
// pass writes. When the purged call is the newest, its rowid is the table's
// maximum and the very next RecordRetrieval takes it, so `EXISTS (SELECT 1 FROM
// retrieval_record WHERE rowid = ?)` answers about the successor and files the
// dead call's verdict under a call that never admitted that memory. The predicate
// checks the call's OWN kept set for the memory the verdict is about instead.
//
// The successor here deliberately admits a DIFFERENT memory, because a successor
// that admitted the same one is not a wrong pair — that verdict would be a true
// claim about a call that did admit its memory — and a test that let that case
// pass for the wrong reason would be claiming more than the guard guarantees.
func TestAVerdictForACallThatHasBeenReplacedIsNotInheritedByItsSuccessor(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	survivor := recordCallKeeping(t, s, testProject, "MEM-A", 810)
	doomed := recordCallKeeping(t, s, testProject, "MEM-B", 811)

	// The first pass, before anything is purged.
	pass := []RetrievalAuditRow{
		{ProjectID: testProject, RecordRowID: survivor, SessionID: "s1", Source: "search",
			MemoryID: "MEM-A", Outcome: "used", Signal: "identifier"},
		{ProjectID: testProject, RecordRowID: doomed, SessionID: "s1", Source: "search",
			MemoryID: "MEM-B", Outcome: "ignored"},
	}
	if _, err := s.RecordRetrievalAudits(ctx, pass); err != nil {
		t.Fatalf("RecordRetrievalAudits (the first pass): %v", err)
	}

	// MEM-B is the memory only the NEWEST call kept, so this frees `doomed` and
	// leaves `survivor` — the state both halves of the test need.
	if err := s.DeleteWithOptions(ctx, "MEM-B", DeleteOptions{PurgeHistory: true}); err != nil {
		t.Fatalf("purge: %v", err)
	}

	// And the rowid is RE-LET before the pass writes. This call admits MEM-C, so
	// the row sitting on `doomed` is a call that never kept MEM-B.
	relet := recordCallKeeping(t, s, testProject, "MEM-C", 812)
	if relet != doomed {
		t.Fatalf("the successor took rowid %d, not the freed %d — freed rowids are no longer reused, so this test "+
			"no longer demonstrates anything and must be re-read", relet, doomed)
	}

	// The late write, filing what the pass judged at the top of its run.
	if _, err := s.RecordRetrievalAudits(ctx, pass); err != nil {
		t.Fatalf("RecordRetrievalAudits (the late pass): %v", err)
	}

	for _, r := range readAuditRows(t, s) {
		if r.MemoryID == "MEM-B" {
			t.Errorf("the late pass stored a verdict about %s (%s) under record rowid %d, which now belongs to the "+
				"call that kept MEM-C: the row EXISTS, so an existence check passes, and the verdict is filed "+
				"against a call that never admitted its memory", r.MemoryID, r.Outcome, r.RecordRowID)
		}
	}
	got := readAuditRows(t, s)
	if len(got) != 1 || got[0].RecordRowID != survivor || got[0].MemoryID != "MEM-A" {
		t.Fatalf("the table holds %+v, want exactly the surviving call's (MEM-A, rowid %d) — a guard that dropped "+
			"the whole batch would store nothing, and one that filed the dead call's would store this", got, survivor)
	}
}

// TestAVerdictIsNotFiledAgainstACallThatConsideredItsMemoryAndDroppedIt: the arm
// that "does the rowid still exist" and even "does this call name the memory" both
// miss, and it is the one that keeps the guard from being a weaker claim than it
// looks.
//
// A record stores DROPPED verdicts as well as kept ones — a memory the call
// considered and did not admit is exactly what the drop stages record — so "this
// call names the memory" is not "this call showed the agent the memory". A
// successor that CONSIDERED the dead call's memory and dropped it names it, so an
// id-only guard files the verdict under a call that never put that memory in front
// of the agent, and the report claims a use for a call that could not have had one.
//
// It is reachable, not hypothetical. The doomed call kept MEM-A and MEM-B, so a
// purge of MEM-B frees its row while MEM-A survives; the successor's search then
// considers MEM-A — it is still a candidate, it was never purged — and drops it
// for its own reason. The late verdict about MEM-A belongs to the call that is
// gone, and the row now under it belongs to one that refused to admit it.
func TestAVerdictIsNotFiledAgainstACallThatConsideredItsMemoryAndDroppedIt(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	for _, id := range []string{"MEM-A", "MEM-B", "MEM-C"} {
		if _, err := s.CreateWithID(ctx, testProject, id, Memory{
			Content: "a fixture memory " + id, Category: "gotcha", Source: "manual",
		}); err != nil {
			t.Fatalf("CreateWithID %s: %v", id, err)
		}
	}
	// The doomed call kept BOTH MEM-A and MEM-B, so purging MEM-B frees its row
	// without touching MEM-A — which is what leaves a still-live memory for the
	// successor to consider.
	doomed := recordCallKeepingAll(t, s, []string{"MEM-A", "MEM-B"}, 820)
	if _, err := s.RecordRetrievalAudits(ctx, []RetrievalAuditRow{{
		ProjectID: testProject, RecordRowID: doomed, SessionID: "s1", Source: "search",
		MemoryID: "MEM-A", Outcome: "used", Signal: "identifier",
	}}); err != nil {
		t.Fatalf("RecordRetrievalAudits (the first pass): %v", err)
	}
	if err := s.DeleteWithOptions(ctx, "MEM-B", DeleteOptions{PurgeHistory: true}); err != nil {
		t.Fatalf("purge: %v", err)
	}

	// The successor CONSIDERS MEM-A and DROPS it. It is the half a guard written
	// as an id match cannot see, and the reason this is a separate test rather than
	// a second phase of the one above.
	relet := recordCallDropping(t, s, "MEM-A", 821)
	if relet != doomed {
		t.Fatalf("the successor took rowid %d, not the freed %d — freed rowids are no longer reused, so this test "+
			"no longer demonstrates anything and must be re-read", relet, doomed)
	}
	var namesIt bool
	if err := s.db.QueryRow(`SELECT EXISTS (SELECT 1 FROM retrieval_record WHERE rowid = ?
		AND EXISTS (SELECT 1 FROM json_each(retrieval_record.verdicts) WHERE value->>'id' = ?))`,
		doomed, "MEM-A").Scan(&namesIt); err != nil {
		t.Fatalf("read the successor's verdicts: %v", err)
	}
	if !namesIt {
		t.Fatal("the successor does not NAME MEM-A — this test's premise is that an id-only guard would pass it, " +
			"so if the fixture stops dropping the memory the test no longer demonstrates anything")
	}

	if _, err := s.RecordRetrievalAudits(ctx, []RetrievalAuditRow{{
		ProjectID: testProject, RecordRowID: doomed, SessionID: "s1", Source: "search",
		MemoryID: "MEM-A", Outcome: "used", Signal: "identifier",
	}}); err != nil {
		t.Fatalf("RecordRetrievalAudits (the late pass): %v", err)
	}

	for _, r := range readAuditRows(t, s) {
		t.Errorf("the late pass stored a verdict about %s (%s) under record rowid %d, which now belongs to a call "+
			"that considered that memory and DROPPED it — it never put it in front of the agent, so it cannot "+
			"have used it", r.MemoryID, r.Outcome, r.RecordRowID)
	}
}

// recordCallKeepingAll records one call that KEPT every memory named, and returns
// its rowid. Several memories is the shape a real call has, and the one a purge
// needs here: freeing the row by purging one of them leaves the others live.
func recordCallKeepingAll(t *testing.T, s *Store, memories []string, n int) int64 {
	t.Helper()
	ctx := context.Background()
	verdicts := make([]RowVerdict, 0, len(memories))
	for _, m := range memories {
		verdicts = append(verdicts, RowVerdict{ID: m, Kept: true, Stage: "fit", Reason: "fit_response"})
	}
	if err := s.RecordRetrieval(ctx, RetrievalRecord{
		ProjectID: testProject, Source: "search", QueryHash: digest(n),
		Outcome: "answerable", Verdicts: verdicts,
	}); err != nil {
		t.Fatalf("RecordRetrieval (%v): %v", memories, err)
	}
	recs, err := s.RetrievalRecords(ctx, 1)
	if err != nil {
		t.Fatalf("RetrievalRecords: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("RetrievalRecords returned %d rows, want 1", len(recs))
	}
	return recs[0].RowID
}

// recordCallDropping records one call that CONSIDERED memory and did not admit it,
// and returns its rowid. The stage and reason are a real drop reason rather than an
// arbitrary string, because the point is that this is a verdict an assembler
// produces, not a malformed row.
func recordCallDropping(t *testing.T, s *Store, memory string, n int) int64 {
	t.Helper()
	ctx := context.Background()
	if err := s.RecordRetrieval(ctx, RetrievalRecord{
		ProjectID: testProject, Source: "search", QueryHash: digest(n),
		Outcome: "answerable",
		Verdicts: []RowVerdict{
			{ID: memory, Kept: false, Stage: "window", Reason: "outside_window"},
		},
	}); err != nil {
		t.Fatalf("RecordRetrieval (dropping %s): %v", memory, err)
	}
	recs, err := s.RetrievalRecords(ctx, 1)
	if err != nil {
		t.Fatalf("RetrievalRecords: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("RetrievalRecords returned %d rows, want 1", len(recs))
	}
	return recs[0].RowID
}

// TestALatePassCannotWipeTheSuccessorsVerdicts: the guard gates the INSERT, and
// that leaves the DELETE half of the same write ungated — which is the same defect
// reached through the other statement.
//
// RecordRetrievalAudits replaces a call's verdicts by deleting every row naming
// that call's rowid, then inserting the batch's rows for it. The delete used to run
// for every distinct rowid in the batch, unconditionally and up front, so a batch
// carrying a row the guard would refuse still WIPED that rowid's existing verdicts
// first. In the re-let window that destroys a legitimate call's evidence:
//
//  1. call A is the newest, rowid R, keeping MEM-X; a pass P is in flight having
//     already read A.
//  2. a purge of MEM-X deletes A and frees R (the sweep takes A's verdicts too).
//  3. a new search records call B, which takes rowid R and keeps a different memory.
//  4. a later pass Q reads B and files (R, MEM-B). That row is in the table, and Q's
//     report says one verdict stored.
//  5. P writes its stale batch: the replace-delete removes (R, MEM-B), and only
//     THEN does the guard refuse P's (R, MEM-X) row because B never kept MEM-X.
//
// The table no longer holds B's verdict, nothing re-files it, and Q's already-printed
// report claims a figure the table does not hold — the wrong number
// Summary.Unfiled was added to prevent, reached through the delete rather than the
// insert. So a pass may only REPLACE a call's verdicts when it is going to file
// some: the guard's own outcome decides whether the batch has a claim on that
// rowid at all.
//
// This is a separate test from the one above rather than another phase of it,
// because the two pin different statements. That one is about what gets WRITTEN;
// this is about what gets ERASED, and a fix to the insert's guard leaves the delete
// exactly as exposed.
func TestALatePassCannotWipeTheSuccessorsVerdicts(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	// The doomed call is the newest, so its rowid is the table's maximum and the
	// one a successor takes. recordCallKeeping creates the memory it names, which
	// the purge below needs to exist.
	doomed := recordCallKeeping(t, s, testProject, "MEM-X", 840)
	stale := []RetrievalAuditRow{{
		ProjectID: testProject, RecordRowID: doomed, SessionID: "s1", Source: "search",
		MemoryID: "MEM-X", Outcome: "used", Signal: "identifier",
	}}
	if _, err := s.RecordRetrievalAudits(ctx, stale); err != nil {
		t.Fatalf("RecordRetrievalAudits (the first pass): %v", err)
	}
	if err := s.DeleteWithOptions(ctx, "MEM-X", DeleteOptions{PurgeHistory: true}); err != nil {
		t.Fatalf("purge: %v", err)
	}

	// The successor takes the freed rowid, keeping a different memory, and its own
	// pass files that verdict. This row is what the late pass must not destroy.
	successor := recordCallKeeping(t, s, testProject, "MEM-Y", 841)
	if successor != doomed {
		t.Fatalf("the successor took rowid %d, not the freed %d — freed rowids are no longer reused, so this "+
			"test no longer demonstrates anything and must be re-read", successor, doomed)
	}
	if _, err := s.RecordRetrievalAudits(ctx, []RetrievalAuditRow{{
		ProjectID: testProject, RecordRowID: successor, SessionID: "s2", Source: "search",
		MemoryID: "MEM-Y", Outcome: "used", Signal: "identifier",
	}}); err != nil {
		t.Fatalf("RecordRetrievalAudits (the successor's pass): %v", err)
	}
	if got := readAuditRows(t, s); len(got) != 1 {
		t.Fatalf("the table holds %+v before the late pass, want the successor's one verdict — the premise is "+
			"that it is there to be destroyed", got)
	}

	// The late pass, filing what it judged before the purge.
	if _, err := s.RecordRetrievalAudits(ctx, stale); err != nil {
		t.Fatalf("RecordRetrievalAudits (the late pass): %v", err)
	}

	got := readAuditRows(t, s)
	if len(got) != 1 || got[0].RecordRowID != successor || got[0].MemoryID != "MEM-Y" {
		t.Fatalf("the table holds %+v, want the successor's (MEM-Y, rowid %d) verdict still there. A refused "+
			"verdict has no claim on that rowid, so this pass must not delete what the successor's own pass "+
			"filed — and nothing re-files it, so the loss is permanent and the successor's printed report "+
			"already claimed it", got, successor)
	}
}

// TestAPartlyRefusedBatchStillKeepsTheRowsItFiled: the replacement delete runs
// immediately after the FIRST row the guard accepts for a call, so it has to
// exclude that row — and whether it can is decided by the order the batch happens
// to name memories in, which is the one thing a caller does not control.
//
// The reachable shape is a partly-refused batch. The doomed call kept two
// memories, so purging one frees its row; the successor takes the freed rowid and
// kept ONE of the same two. The stale pass then names both: the memory the
// successor dropped is refused, the one it kept is accepted. If the delete did not
// exclude the row it had just written, that accepted row would delete ITSELF and
// the table would end the pass holding nothing at all — a loss strictly worse than
// the refusal it was avoiding, and one that a batch naming its memories in the
// other order would not show.
//
// The order in the batch below is deliberate and load-bearing: the refused row
// comes FIRST, so the delete lands after the accepted one has been written.
func TestAPartlyRefusedBatchStillKeepsTheRowsItFiled(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	// The doomed call kept both memories, so purging one frees its row and leaves
	// the other live for the successor to consider. Both are real rows because the
	// purge below refuses to delete a name it cannot find.
	for _, id := range []string{"MEM-X", "MEM-Y"} {
		if _, err := s.CreateWithID(ctx, testProject, id, Memory{
			Content: "a fixture memory " + id, Category: "gotcha", Source: "manual",
		}); err != nil {
			t.Fatalf("CreateWithID %s: %v", id, err)
		}
	}
	doomed := recordCallKeepingAll(t, s, []string{"MEM-X", "MEM-Y"}, 850)
	stale := []RetrievalAuditRow{
		{ProjectID: testProject, RecordRowID: doomed, SessionID: "s1", Source: "search",
			MemoryID: "MEM-X", Outcome: "used", Signal: "identifier"},
		{ProjectID: testProject, RecordRowID: doomed, SessionID: "s1", Source: "search",
			MemoryID: "MEM-Y", Outcome: "ignored"},
	}
	if err := s.DeleteWithOptions(ctx, "MEM-X", DeleteOptions{PurgeHistory: true}); err != nil {
		t.Fatalf("purge: %v", err)
	}

	// The successor takes the freed rowid and kept only MEM-Y, so MEM-X is refused
	// and MEM-Y is not. recordCallDropping alone would name nothing the guard could
	// accept, so the successor's own kept set is MEM-Y and MEM-X is recorded as
	// considered-and-dropped alongside it.
	successor := recordCallKeepingAndDropping(t, s, []string{"MEM-Y", "MEM-X"}, 851)
	if successor != doomed {
		t.Fatalf("the successor took rowid %d, not the freed %d — freed rowids are no longer reused, so this "+
			"test no longer demonstrates anything and must be re-read", successor, doomed)
	}

	refused, err := s.RecordRetrievalAudits(ctx, stale)
	if err != nil {
		t.Fatalf("RecordRetrievalAudits (the late pass): %v", err)
	}
	if len(refused) != 1 || refused[0].MemoryID != "MEM-X" {
		t.Fatalf("the pass refused %+v, want exactly the MEM-X row — the successor kept MEM-Y, so this test's "+
			"premise is one refusal and one acceptance in that order", refused)
	}

	got := readAuditRows(t, s)
	if len(got) != 1 || got[0].MemoryID != "MEM-Y" {
		t.Fatalf("the table holds %+v, want the one row the guard accepted (MEM-Y). The replacement delete runs "+
			"just after that row is written, so without excluding it the accepted row erases itself and the "+
			"pass ends having stored nothing", got)
	}
}

// recordCallKeepingAndDropping records one call that KEPT the first memory and
// CONSIDERED-AND-DROPPED the rest, and returns its rowid. It is the mixed shape a
// successor takes over a freed rowid when the dead call and the new one share some
// of their memories.
func recordCallKeepingAndDropping(t *testing.T, s *Store, memories []string, n int) int64 {
	t.Helper()
	ctx := context.Background()
	verdicts := make([]RowVerdict, 0, len(memories))
	for i, m := range memories {
		v := RowVerdict{ID: m, Stage: "fit", Reason: "fit_response", Kept: true}
		if i > 0 {
			v = RowVerdict{ID: m, Stage: "window", Reason: "outside_window", Kept: false}
		}
		verdicts = append(verdicts, v)
	}
	if err := s.RecordRetrieval(ctx, RetrievalRecord{
		ProjectID: testProject, Source: "search", QueryHash: digest(n),
		Outcome: "answerable", Verdicts: verdicts,
	}); err != nil {
		t.Fatalf("RecordRetrieval (%v): %v", memories, err)
	}
	recs, err := s.RetrievalRecords(ctx, 1)
	if err != nil {
		t.Fatalf("RetrievalRecords: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("RetrievalRecords returned %d rows, want 1", len(recs))
	}
	return recs[0].RowID
}

// TestEveryWriterThatDeletesRetrievalRecordsTakesTheirVerdicts is the structural
// half of the two behavioural tests above, and it exists because the gap was
// invisible to the tests that were here: two delete paths, each correct about the
// memory it was asked about and each silent about the calls they removed.
//
// The rule it checks is PER FUNCTION — the function that DELETEs from
// retrieval_record must also DELETE from retrieval_audit — because a file-level
// check would pass a file in which one path does this and another forgets. It
// cannot see a WRONG predicate, which is why the two tests above exist and are
// the ones that fail on the bug: the purge deleted verdicts by memory id, so this
// scan was already satisfied before the fix. What it buys is the next path: a new
// delete of recorded calls that reaches no verdicts at all is caught here, before
// a report has counted one.
func TestEveryWriterThatDeletesRetrievalRecordsTakesTheirVerdicts(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read the package: %v", err)
	}
	const (
		deletesRecords = "DELETE FROM retrieval_record"
		deletesAudits  = "DELETE FROM retrieval_audit"
	)
	checked := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		// Split into top-level declarations by the `func ` line each one starts on,
		// which is the coarsest split that still tells two paths apart. Each
		// declaration's body is the lines up to the next one, and comment lines
		// are dropped: a function that only EXPLAINS this rule must not be able to
		// satisfy it. Whole-line `//` is the only comment form stripped, and it is
		// enough here because every statement concerned opens its SQL with a
		// backtick on the line after its `if`.
		var fn, body string
		seen := 0
		flush := func() {
			if fn == "" || !strings.Contains(body, deletesRecords) {
				return
			}
			checked++
			seen++
			if !strings.Contains(body, deletesAudits) {
				t.Errorf("%s's %s deletes retrieval_record rows and no retrieval_audit rows: every call that goes "+
					"away takes the verdicts filed against it, or the report counts a verdict under a call that no "+
					"longer exists", name, fn)
			}
		}
		for _, line := range strings.Split(string(src), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "//") {
				continue
			}
			if strings.HasPrefix(line, "func ") {
				flush()
				fn = declaredName(strings.TrimPrefix(line, "func "))
				body = ""
			}
			body += line + "\n"
		}
		flush()
		// And the scan must not have walked PAST a delete it could not attribute.
		// A split that silently misses a path is the same failure as no check at
		// all, wearing the costume of one, so it is reported rather than skipped.
		if seen == 0 && strings.Contains(string(src), deletesRecords) {
			t.Errorf("%s holds a %s statement that no declaration in it was credited with — this scan did not "+
				"read that file correctly, so treat its silence above as unchecked", name, deletesRecords)
		}
	}
	if checked == 0 {
		t.Fatal("no function deletes retrieval_record rows — this test is not looking at what it claims to")
	}
}

// declaredName is the name a `func` line declares, with a method's receiver
// reduced to its type: `func (s *Store) RecordRetrieval(ctx ...) error` reads as
// `(*Store).RecordRetrieval`, so a failure names the path rather than a
// signature.
//
// The receiver is stripped FIRST and the name cut from what is left. Built the
// other way round — rewriting the signature and then cutting at the first `(` —
// it returns "" for every method, because the "(" of the receiver it just wrote
// is the first one it finds; and an empty name is indistinguishable from "not a
// declaration" to the scan that calls it, so a test that skips every method is a
// test that passes on two of the three paths it was written to cover. That is not
// hypothetical: it is what this function did the first time.
func declaredName(line string) string {
	name, recv := strings.TrimSpace(line), ""
	if strings.HasPrefix(name, "(") {
		if end := strings.Index(name, ") "); end >= 0 {
			recv = strings.TrimSpace(name[1:end])
			if i := strings.LastIndex(recv, " "); i >= 0 {
				recv = recv[i+1:]
			}
			recv = strings.TrimPrefix(recv, "*")
			name = name[end+2:]
		}
	}
	if end := strings.IndexAny(name, "(\t "); end >= 0 {
		name = name[:end]
	}
	if recv != "" {
		return "(*" + recv + ")." + name
	}
	return name
}

// TestRetrievalAuditsToleratesAnUnreadableRow: the reader must not fail the
// whole report because one row cannot be decoded — the state an operator is in
// is precisely the one where they need the report.
func TestRetrievalAuditsToleratesAnUnreadableRow(t *testing.T) {
	store, db, _ := auditStore(t)
	if _, err := db.Exec(`INSERT INTO retrieval_audit
		(project_id, record_rowid, source, memory_id, outcome, signal)
		VALUES ('p1', 1, 'search', 'MEM1', 'ignored', '')`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// outcome is NOT NULL, so the only way to write nonsense the reader must
	// survive is a value outside its vocabulary — which is what a table from a
	// LATER build looks like to this one.
	if _, err := db.Exec(`INSERT INTO retrieval_audit
		(project_id, record_rowid, source, memory_id, outcome, signal)
		VALUES ('p1', 2, 'search', 'MEM2', 'retired_by_a_newer_build', '')`); err != nil {
		t.Fatalf("seed the unreadable row: %v", err)
	}
	rows, err := store.RetrievalAudits(context.Background(), "p1", "")
	if err != nil {
		t.Fatalf("RetrievalAudits: %v", err)
	}
	if len(rows) != 2 {
		t.Errorf("got %d row(s), want 2 — an outcome this build does not name is still a row to count", len(rows))
	}
}

// TestRetrievalAuditsSkipsARowWithNoMemory keeps the read defensive against the
// one row shape that cannot be reported on at all.
func TestRetrievalAuditsSkipsARowWithNoMemory(t *testing.T) {
	store, db, ctx := auditStore(t)
	if _, err := db.Exec(`INSERT INTO retrieval_audit
		(project_id, record_rowid, source, memory_id, outcome, signal)
		VALUES ('p1', 1, 'search', '', 'ignored', '')`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	rows, err := store.RetrievalAudits(ctx, "p1", "")
	if err != nil {
		t.Fatalf("RetrievalAudits: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("got %d row(s), want 0: a verdict naming no memory is not reportable", len(rows))
	}
}

// TestMigrateV21MatchesTheFreshDatabaseSchema is the v20 test's own shape, for
// the verdict table: the migration path and the initSQL path must produce one
// table, or an upgraded store and a fresh one disagree on what a verdict is.
func TestMigrateV21MatchesTheFreshDatabaseSchema(t *testing.T) {
	newDB := func(t *testing.T) *sql.DB {
		t.Helper()
		db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "ghost.db"))
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		t.Cleanup(func() { _ = db.Close() })
		if _, err := db.Exec(initSQL); err != nil {
			t.Fatalf("initSQL: %v", err)
		}
		return db
	}

	fresh := newDB(t)
	migrated := newDB(t)
	if _, err := migrated.Exec(`DROP TABLE IF EXISTS retrieval_audit`); err != nil {
		t.Fatalf("drop the table so the step has work to do: %v", err)
	}
	tx, err := migrated.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := migrateV21(tx); err != nil {
		_ = tx.Rollback()
		t.Fatalf("migrateV21: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	// The arm above reproduces the v21 SHIP shape, and the comparison is against a
	// fresh database — which initSQL can only ever describe as it is NOW, holding
	// `content_hash` (schema v22). So the arm continues with the step that takes it
	// from that frozen shape to the current one; stopping at v21 would compare v21's
	// DDL with today's and report the difference as a defect in the v21 step.
	tx, err = migrated.Begin()
	if err != nil {
		t.Fatalf("begin (v22): %v", err)
	}
	if err := migrateV22(tx); err != nil {
		_ = tx.Rollback()
		t.Fatalf("migrateV22: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit (v22): %v", err)
	}

	freshCols, err := columnShapes(t, fresh, "retrieval_audit")
	if err != nil {
		t.Fatalf("read the fresh table: %v", err)
	}
	if len(freshCols) == 0 {
		t.Fatal("a fresh database has no retrieval_audit — the initSQL path is missing the table")
	}
	migratedCols, err := columnShapes(t, migrated, "retrieval_audit")
	if err != nil {
		t.Fatalf("read the migrated table: %v", err)
	}
	if diff := columnShapeDiff(freshCols, migratedCols); diff != "" {
		t.Errorf("the migration path and initSQL disagree on the table: %s", diff)
	}
	var idx string
	if err := migrated.QueryRow(
		`SELECT name FROM sqlite_master WHERE type='index' AND name='idx_retrieval_audit_project'`,
	).Scan(&idx); err != nil {
		t.Errorf("migrateV21 did not create the project index: %v", err)
	}
}

// TestMigrateV21RefusesAForeignRetrievalAuditTable: `CREATE TABLE IF NOT EXISTS`
// is a silent no-op against a table somebody else owns, so the step would stamp
// v21 over a shape every later write fails on. The error must name the remedy.
func TestMigrateV21RefusesAForeignRetrievalAuditTable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ghost.db")
	db, err := OpenDB(path)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	if _, err := db.Exec(`DROP TABLE retrieval_audit`); err != nil {
		t.Fatalf("drop: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE retrieval_audit (something_else TEXT)`); err != nil {
		t.Fatalf("seed the foreign table: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	_, err = OpenDB(path)
	if err == nil {
		t.Fatal("OpenDB opened a store holding somebody else's retrieval_audit")
	}
	for _, want := range []string{"retrieval_audit", "DROP TABLE"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

// TestMigrateV21LeavesAnEarlierStoreReadable: the step is additive, so a store
// at v20 with rows keeps them, and nothing is backfilled.
func TestMigrateV21LeavesAnEarlierStoreReadable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ghost.db")
	db, err := OpenDB(path)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO projects (id, path, name) VALUES ('p1', '/tmp/v21-p1', 'p1')`); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO retrieval_record (project_id, source, outcome) VALUES ('p1', 'search', 'answerable')`,
	); err != nil {
		t.Fatalf("seed record: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	reopened, err := OpenDB(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close() //nolint:errcheck
	var n int
	if err := reopened.QueryRow(`SELECT count(*) FROM retrieval_record`).Scan(&n); err != nil {
		t.Fatalf("count records: %v", err)
	}
	if n != 1 {
		t.Errorf("the migration dropped a recorded call: %d row(s) left", n)
	}
	// Nothing is backfilled. A verdict is a judgement ABOUT a transcript, and
	// there is no transcript to judge: a synthesised row would say every call
	// before this version was ignored, which is a claim nobody made.
	if err := reopened.QueryRow(`SELECT count(*) FROM retrieval_audit`).Scan(&n); err != nil {
		t.Fatalf("count verdicts: %v", err)
	}
	if n != 0 {
		t.Errorf("the migration backfilled %d verdict(s) — none of them were judged", n)
	}
}

// TestMigrateV22AddsContentHashToExistingVerdicts: the step adds the column the
// reader now keys on (#879), and it adds it to a store that already HOLDS
// verdicts — so the two properties that matter are that the row survives and
// that it comes through with the empty stamp rather than an invented one. An
// empty stamp is precisely what the reader's named legacy rule is for, so a
// backfill here would be a fabricated claim ("this verdict judged THIS text")
// about rows whose judged text nothing recorded.
//
// The shape is compared against a fresh database for the reason the v21 test
// compares its own: an upgraded store and a fresh one must be one schema, or the
// same SELECT reads different columns on each path.
func TestMigrateV22AddsContentHashToExistingVerdicts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ghost.db")
	db, err := OpenDB(path)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	// Rewind to the state the v21 step leaves behind: the current table dropped and
	// rebuilt under v21's own frozen DDL, with user_version stamped back to 21.
	if _, err := db.Exec(`DROP TABLE retrieval_audit`); err != nil {
		t.Fatalf("drop the current table: %v", err)
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := migrateV21(tx); err != nil {
		_ = tx.Rollback()
		t.Fatalf("migrateV21: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO projects (id, path, name) VALUES ('p1', '/tmp/v22-p1', 'p1')`); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO retrieval_audit
		(project_id, record_rowid, source, memory_id, outcome) VALUES ('p1', 1, 'search', 'MEM1', 'contradicted')`); err != nil {
		t.Fatalf("seed the v21 verdict: %v", err)
	}
	if _, err := db.Exec(`PRAGMA user_version = 21`); err != nil {
		t.Fatalf("stamp v21: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	reopened, err := OpenDB(path)
	if err != nil {
		t.Fatalf("reopen at v21: %v", err)
	}
	defer reopened.Close() //nolint:errcheck

	var version int
	if err := reopened.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatalf("read user_version: %v", err)
	}
	if version != SchemaVersion() {
		t.Errorf("user_version = %d, want %d — the step did not run, or ran without stamping", version, SchemaVersion())
	}
	var hash string
	if err := reopened.QueryRow(`SELECT content_hash FROM retrieval_audit WHERE memory_id = 'MEM1'`).Scan(&hash); err != nil {
		t.Fatalf("read the migrated verdict's stamp: %v", err)
	}
	if hash != "" {
		t.Errorf("the pre-v22 verdict reads content_hash = %q, want empty — nothing is backfilled, and an "+
			"unstamped row is what the legacy rule exists for", hash)
	}

	fresh, err := OpenDB(filepath.Join(t.TempDir(), "fresh.db"))
	if err != nil {
		t.Fatalf("OpenDB (fresh): %v", err)
	}
	defer fresh.Close() //nolint:errcheck
	freshCols, err := columnShapes(t, fresh, "retrieval_audit")
	if err != nil {
		t.Fatalf("read the fresh table: %v", err)
	}
	migratedCols, err := columnShapes(t, reopened, "retrieval_audit")
	if err != nil {
		t.Fatalf("read the migrated table: %v", err)
	}
	if diff := columnShapeDiff(freshCols, migratedCols); diff != "" {
		t.Errorf("an upgraded store and a fresh one disagree on retrieval_audit: %s", diff)
	}
}

// TestMigrateV22RefusesAForeignRetrievalAuditTable: the step is an ALTER, and
// unlike CREATE TABLE IF NOT EXISTS an ALTER SUCCEEDS against any table holding
// that name — so without the guard inside the step, a `retrieval_audit` somebody
// else owns would be given a column and the store would be stamped current over
// a shape nothing here can write. The guard names the remedy instead.
func TestMigrateV22RefusesAForeignRetrievalAuditTable(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "ghost.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(initSQL); err != nil {
		t.Fatalf("initSQL: %v", err)
	}
	if _, err := db.Exec(`DROP TABLE retrieval_audit`); err != nil {
		t.Fatalf("drop: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE retrieval_audit (something_else TEXT)`); err != nil {
		t.Fatalf("seed the foreign table: %v", err)
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := migrateV22(tx); err == nil {
		t.Fatal("migrateV22 added a column to a retrieval_audit that is not Ghost's")
	} else {
		for _, want := range []string{"retrieval_audit", "DROP TABLE"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not mention %q", err, want)
			}
		}
	}
}

// TestRetrievalAuditsCarryOneIndex: every index on this table is a b-tree insert
// inside the audit's write, and the report's own read is a full scan of a capped
// table — so a second index is cost with no reader behind it.
func TestRetrievalAuditsCarryOneIndex(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ghost.db")
	db, err := OpenDB(path)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer db.Close() //nolint:errcheck
	var stray int
	if err := db.QueryRow(`
		SELECT count(*) FROM sqlite_master WHERE type='index' AND tbl_name='retrieval_audit'
		 AND name NOT LIKE 'sqlite_autoindex%' AND name <> 'idx_retrieval_audit_project'`,
	).Scan(&stray); err != nil {
		t.Fatalf("count retrieval_audit indexes: %v", err)
	}
	if stray != 0 {
		t.Errorf("retrieval_audit carries %d index(es) nothing reads", stray)
	}
}

// TestRetrievalAuditTableIsNotExported: the verdicts are derived operational data
// about calls made on this machine, describing memories a portable artifact is
// not obliged to carry — the same call retrieval_record already makes.
func TestRetrievalAuditTableIsNotExported(t *testing.T) {
	raw, err := os.ReadFile("portable.go")
	if err != nil {
		if _, statErr := os.Stat("portable.go"); statErr != nil {
			t.Skip("no portable.go in this package")
		}
		t.Fatalf("read portable.go: %v", err)
	}
	if strings.Contains(string(raw), "retrieval_audit") {
		t.Error("retrieval_audit appears in the export path; it must stay out, like retrieval_record")
	}
}

// seedAuditedCalls writes n recorded calls for a project, each already judged
// into verdicts, and returns their rowids. The audits are filed against a real
// call's rowid rather than a made-up one, because that is the shape a row has in
// production — and because a fixture that could not be produced by the writer is
// a fixture that can drift away from it unnoticed.
// seedAuditedCalls records n calls in projectID and returns their rowids. Call i
// KEEPS the memory `MEM-<i+1>`, and no other: the write only files a verdict
// against a call that kept that memory, so a seeder that gave every call the same
// memory would let a test file verdicts no call ever admitted and watch the
// writer — correctly — refuse them.
//
// The memories are ids and nothing more. `RecordRetrieval` does not require one to
// exist, and these tests are about rowids, caps and project ids rather than about
// what is in the store, so nothing here creates a memory row; only the tests whose
// subject is a PURGE need one, and they say so where they make it.
func seedAuditedCalls(t *testing.T, s *Store, projectID string, n int) []int64 {
	t.Helper()
	ctx := context.Background()
	var rowids []int64
	for i := range n {
		memory := "MEM-" + string(rune('1'+i))
		rec := RetrievalRecord{
			ProjectID: projectID,
			Source:    "search",
			QueryHash: digest(700 + i),
			Outcome:   "answerable",
			Verdicts: []RowVerdict{
				{ID: memory, Kept: true, Stage: "fit", Reason: "fit_response"},
			},
		}
		if err := s.RecordRetrieval(ctx, rec); err != nil {
			t.Fatalf("RecordRetrieval %d: %v", i, err)
		}
		recs, err := s.RetrievalRecords(ctx, 1)
		if err != nil {
			t.Fatalf("RetrievalRecords %d: %v", i, err)
		}
		if len(recs) != 1 {
			t.Fatalf("RetrievalRecords %d returned %d rows, want 1", i, len(recs))
		}
		rowids = append(rowids, recs[0].RowID)
	}
	return rowids
}

// TestDeleteProjectTakesTheAuditsWithIt: a verdict keeps a memory's NAME (that
// is the whole of what it stores — see TestRecordRetrievalAuditsStoresNoText),
// and DeleteProject removes the memories. An audit row left behind would name a
// memory that no longer exists under a project that no longer exists, in a table
// no report would ever purge, because the report reads by project and the purge
// reads by memory.
//
// So the delete must take the audits, count them in the dry run — the summary is
// documented as covering every table the command touches, and the dry run is what
// a reader decides from — and take ONLY this project's rows. A delete with no
// predicate would satisfy "the audits are gone" and is exactly the bug the second
// project's rows below exist to catch.
func TestDeleteProjectTakesTheAuditsWithIt(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	// A second project whose audits must survive, and a memory in it that the
	// delete must not reach into.
	if err := s.EnsureProject(ctx, "other-project", "/tmp/other", "other"); err != nil {
		t.Fatalf("EnsureProject (other): %v", err)
	}
	otherCalls := seedAuditedCalls(t, s, "other-project", 1)
	// The seeded call kept MEM-1, so that is the only memory its verdict can
	// name: the write files a verdict against a call that KEPT the memory, so a
	// verdict about anything else is refused and this fixture would be measuring
	// the refusal rather than which project's rows the delete takes.
	otherRows := []RetrievalAuditRow{{
		ProjectID:   "other-project",
		RecordRowID: otherCalls[0],
		SessionID:   "s-other",
		Source:      "search",
		MemoryID:    "MEM-1",
		Outcome:     "ignored",
	}}
	if _, err := s.RecordRetrievalAudits(ctx, otherRows); err != nil {
		t.Fatalf("RecordRetrievalAudits (other): %v", err)
	}

	calls := seedAuditedCalls(t, s, testProject, 2)
	rows := []RetrievalAuditRow{
		{ProjectID: testProject, RecordRowID: calls[0], SessionID: "s1", Source: "search",
			MemoryID: "MEM-1", Outcome: "used", Signal: "identifier"},
		{ProjectID: testProject, RecordRowID: calls[1], SessionID: "s1", Source: "search",
			MemoryID: "MEM-2", Outcome: "ignored", Signal: ""},
	}
	if _, err := s.RecordRetrievalAudits(ctx, rows); err != nil {
		t.Fatalf("RecordRetrievalAudits: %v", err)
	}

	summary, err := s.DeleteProject(ctx, testProject, false)
	if err != nil {
		t.Fatalf("DeleteProject (dry run): %v", err)
	}
	if summary.RetrievalAudits != 2 {
		t.Errorf("the dry run reports %d retrieval audits, want 2 — the dry run is what a reader decides whether to delete from",
			summary.RetrievalAudits)
	}

	applied, err := s.DeleteProject(ctx, testProject, true)
	if err != nil {
		t.Fatalf("DeleteProject (apply): %v", err)
	}
	if applied.RetrievalAudits != summary.RetrievalAudits {
		t.Errorf("the apply path removed/counted %d audits, the dry run counted %d",
			applied.RetrievalAudits, summary.RetrievalAudits)
	}

	var left int
	if err := s.db.QueryRow(`SELECT count(*) FROM retrieval_audit`).Scan(&left); err != nil {
		t.Fatalf("count remaining audits: %v", err)
	}
	if left != 1 {
		t.Errorf("%d audit row(s) survive the project delete, want 1 (the other project's)", left)
	}
	survivors := readAuditRows(t, s)
	if len(survivors) != 1 || survivors[0].ProjectID != "other-project" {
		t.Errorf("the surviving audit rows are %+v, want exactly the other project's", survivors)
	}
}

// TestMergeProjectReassignsTheAudits is the behavioural half of
// TestBothMergeStatementListsReassignTheAudits, and it is deliberately NOT a
// substitute for the source scan. It can only see ONE of the two copies:
// MergeProject goes mergeProjectLocked → mergeProjectWithRepoTx → the free
// function mergeProjectTx, which reads the package-level projectMergeStatements.
// The method's own list has exactly two callers, both bind-recovery paths
// (store.go:955, 1365), so deleting the method's copy leaves this test GREEN —
// which is measured, not assumed: removing that line fails only the source scan.
// So the scan is the sole check on the second copy, and this test exists for the
// half a scan cannot see at all: that the statement, once present in both, moves
// THESE rows on the path a real merge takes. A list entry naming the right table
// but reaching it wrongly — the wrong predicate, the wrong column — passes every
// source scan and orphans every verdict in production.
func TestMergeProjectReassignsTheAudits(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	const old, survivor = "old-project", "survivor-project"
	for _, p := range []string{old, survivor} {
		if err := s.EnsureProject(ctx, p, "/tmp/"+p, p); err != nil {
			t.Fatalf("EnsureProject %s: %v", p, err)
		}
	}

	oldCalls := seedAuditedCalls(t, s, old, 2)
	newCalls := seedAuditedCalls(t, s, survivor, 1)
	// Each verdict names the memory its own call kept — oldCalls[0] kept MEM-1,
	// oldCalls[1] kept MEM-2, and newCalls[0] is the survivor's FIRST call so it
	// kept MEM-1 too. Anything else is refused by the write, which files a
	// verdict only against a call that kept its memory.
	rows := []RetrievalAuditRow{
		{ProjectID: old, RecordRowID: oldCalls[0], Source: "search", MemoryID: "MEM-1", Outcome: "used", Signal: "identifier"},
		{ProjectID: old, RecordRowID: oldCalls[1], Source: "search", MemoryID: "MEM-2", Outcome: "ignored"},
		{ProjectID: survivor, RecordRowID: newCalls[0], Source: "search", MemoryID: "MEM-1", Outcome: "superseded_in_session"},
	}
	if _, err := s.RecordRetrievalAudits(ctx, rows); err != nil {
		t.Fatalf("RecordRetrievalAudits: %v", err)
	}

	if err := s.MergeProject(ctx, old, survivor); err != nil {
		t.Fatalf("MergeProject: %v", err)
	}

	kept := readAuditRows(t, s)
	if len(kept) != 3 {
		t.Fatalf("%d audit row(s) survive the merge, want 3 — the merge DELETEs the outgoing projects row and "+
			"this table's project_id is deliberately not a foreign key, so a row missing from the reassignment "+
			"is lost with it", len(kept))
	}
	for _, r := range kept {
		if r.ProjectID == old {
			t.Errorf("audit for memory %s still names the merged-away project %q — it is misattributed in a report "+
				"and unreachable by a later delete of that id", r.MemoryID, old)
		}
	}
	// And the counts each side now reads as, so a swap or a doubling cannot pass.
	gotOld, err := s.RetrievalAudits(ctx, old, "")
	if err != nil {
		t.Fatalf("RetrievalAudits(old): %v", err)
	}
	if len(gotOld) != 0 {
		t.Errorf("%d audit row(s) still name %q after the merge", len(gotOld), old)
	}
	gotNew, err := s.RetrievalAudits(ctx, survivor, "")
	if err != nil {
		t.Fatalf("RetrievalAudits(survivor): %v", err)
	}
	if len(gotNew) != 3 {
		t.Errorf("the survivor holds %d audit row(s), want 3 (its own 1 plus the merged 2)", len(gotNew))
	}
}

// TestBothMergeStatementListsReassignTheAudits: the merge reassignment exists
// in TWO copies — projectMergeStatements for the merge/migration paths and the
// list inside (*Store).mergeProjectTx for the bind-recovery ones — and both
// lists' own comments say they are kept in step on purpose. That is a promise a
// comment cannot keep, and it has already been broken on this file: a table in
// one list and not the other is reassigned on the happy path and orphaned on the
// recovery one, which no test that only calls MergeProject can see.
//
// The two lists are the same statements, so the comparison is exact and a
// divergence in either direction fails here.
func TestBothMergeStatementListsReassignTheAudits(t *testing.T) {
	src, err := os.ReadFile("store.go")
	if err != nil {
		t.Fatalf("read store.go: %v", err)
	}
	const stmt = "UPDATE retrieval_audit SET project_id = ? WHERE project_id = ?"
	if n := strings.Count(string(src), stmt); n != 2 {
		t.Errorf("the audit merge reassignment appears %d time(s) in store.go, want 2 — the two lists are documented as "+
			"deliberately duplicated, and a table in one and not the other is reassigned on the merge path and "+
			"orphaned on the bind-recovery path", n)
	}
}

// TestEveryRendererOfTheDeleteSummaryNamesTheAudits: DeleteProjectSummary has
// THREE renderers, and this table's counts would be a fourth omission of the
// kind that happened twice on retrieval_record — a field present in one renderer
// and absent in another is not cosmetic, because the dry run is what a reader
// decides from and the log line is the durable record of what was removed.
//
// This test asserts the FIELD is named in each source, which is what keeps the
// three in step: a test asserting the rendered text of one surface cannot see the
// other two drift, and cannot see a fourth renderer be added without naming it.
func TestEveryRendererOfTheDeleteSummaryNamesTheAudits(t *testing.T) {
	for _, c := range []struct{ path, needle, why string }{
		{"../../cmd/ghost/project.go", "RetrievalAudits", "CLI's delete report"},
		{"../mcpserver/mcpserver.go", "RetrievalAudits", "MCP tool's identical report"},
		{"store.go", "retrieval_audits", "durable delete log line"},
	} {
		src, err := os.ReadFile(c.path)
		if err != nil {
			t.Fatalf("read %s: %v", c.path, err)
		}
		if !strings.Contains(string(src), c.needle) {
			t.Errorf("%s never reads DeleteProjectSummary.%s — a count the %s does not show is a count the reader cannot see",
				c.path, c.needle, c.why)
		}
	}
}
