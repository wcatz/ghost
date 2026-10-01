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
	if err := store.RecordRetrievalAudits(ctx, rows); err != nil {
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
	if err := store.RecordRetrievalAudits(ctx, rows); err != nil {
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
	if err := store.RecordRetrievalAudits(ctx, first); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	// The next turn's transcript knows better: MEM1 was read after all.
	second := []RetrievalAuditRow{
		{ProjectID: "p1", RecordRowID: recID, Source: "search", MemoryID: "MEM1", Outcome: "used", Signal: "identifier"},
	}
	if err := store.RecordRetrievalAudits(ctx, second); err != nil {
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
	err := store.RecordRetrievalAudits(ctx, []RetrievalAuditRow{
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
	err := store.RecordRetrievalAudits(ctx, []RetrievalAuditRow{
		{MemoryID: "MEM1", Source: "search", Outcome: "used"},
	})
	if err == nil {
		t.Fatal("RecordRetrievalAudits accepted a verdict naming no project")
	}
}

// TestRecordRetrievalAuditsEvictsOldestOverCap: the table is bounded, and the
// bound evicts the OLDEST rows — the evidence an operator is looking at is the
// recent past, so losing the far end is what keeps the window useful.
func TestRecordRetrievalAuditsEvictsOldestOverCap(t *testing.T) {
	store, _, ctx := auditStore(t)
	restore := retrievalAuditRowsCap
	retrievalAuditRowsCap = 2
	t.Cleanup(func() { retrievalAuditRowsCap = restore })

	for i := 1; i <= 3; i++ {
		rows := []RetrievalAuditRow{{
			ProjectID: "p1", RecordRowID: int64(i), Source: "search",
			MemoryID: "MEM" + string(rune('0'+i)), Outcome: "ignored",
		}}
		if err := store.RecordRetrievalAudits(ctx, rows); err != nil {
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
	if err := store.RecordRetrievalAudits(ctx, []RetrievalAuditRow{
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
	if err := store.RecordRetrievalAudits(ctx, []RetrievalAuditRow{
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
func seedAuditedCalls(t *testing.T, s *Store, projectID string, n int) []int64 {
	t.Helper()
	ctx := context.Background()
	var rowids []int64
	for i := range n {
		rec := RetrievalRecord{
			ProjectID: projectID,
			Source:    "search",
			QueryHash: digest(700 + i),
			Outcome:   "answerable",
			Verdicts: []RowVerdict{
				{ID: "MEM-1", Kept: true, Stage: "fit", Reason: "fit_response"},
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
	otherRows := []RetrievalAuditRow{{
		ProjectID:   "other-project",
		RecordRowID: otherCalls[0],
		SessionID:   "s-other",
		Source:      "search",
		MemoryID:    "OTHER-MEM",
		Outcome:     "ignored",
	}}
	if err := s.RecordRetrievalAudits(ctx, otherRows); err != nil {
		t.Fatalf("RecordRetrievalAudits (other): %v", err)
	}

	calls := seedAuditedCalls(t, s, testProject, 2)
	rows := []RetrievalAuditRow{
		{ProjectID: testProject, RecordRowID: calls[0], SessionID: "s1", Source: "search",
			MemoryID: "MEM-1", Outcome: "used", Signal: "identifier"},
		{ProjectID: testProject, RecordRowID: calls[1], SessionID: "s1", Source: "search",
			MemoryID: "MEM-1", Outcome: "ignored", Signal: ""},
	}
	if err := s.RecordRetrievalAudits(ctx, rows); err != nil {
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
	rows := []RetrievalAuditRow{
		{ProjectID: old, RecordRowID: oldCalls[0], Source: "search", MemoryID: "MEM-1", Outcome: "used", Signal: "identifier"},
		{ProjectID: old, RecordRowID: oldCalls[1], Source: "search", MemoryID: "MEM-2", Outcome: "ignored"},
		{ProjectID: survivor, RecordRowID: newCalls[0], Source: "search", MemoryID: "MEM-3", Outcome: "superseded_in_session"},
	}
	if err := s.RecordRetrievalAudits(ctx, rows); err != nil {
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
