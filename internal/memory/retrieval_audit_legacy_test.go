package memory

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// retrieval_audit is a legacy table: nothing reads it and nothing writes it.
// The statements that still touch it (the migrations, the purge, the record
// cap's orphan sweep, the project merge, the project delete) act on rows an
// older build left behind, so these tests seed it with raw INSERTs.

type legacyAudit struct {
	project  string
	rowid    int64
	memoryID string
}

// seedLegacyAudit inserts one verdict row the way an older build stored it.
func seedLegacyAudit(t *testing.T, s *Store, project string, recordRowID int64, memoryID string) {
	t.Helper()
	if _, err := s.db.ExecContext(context.Background(),
		`INSERT INTO retrieval_audit (project_id, record_rowid, session_id, source, memory_id, outcome)
		 VALUES (?, ?, 's1', 'search', ?, 'ignored')`,
		project, recordRowID, memoryID); err != nil {
		t.Fatalf("seed retrieval_audit: %v", err)
	}
}

// readLegacyAudits returns the whole table, so an orphan anywhere fails a test.
func readLegacyAudits(t *testing.T, s *Store) []legacyAudit {
	t.Helper()
	rows, err := s.db.QueryContext(context.Background(),
		`SELECT project_id, record_rowid, memory_id FROM retrieval_audit ORDER BY rowid`)
	if err != nil {
		t.Fatalf("read retrieval_audit: %v", err)
	}
	defer rows.Close() //nolint:errcheck
	var out []legacyAudit
	for rows.Next() {
		var r legacyAudit
		if err := rows.Scan(&r.project, &r.rowid, &r.memoryID); err != nil {
			t.Fatalf("scan retrieval_audit: %v", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate retrieval_audit: %v", err)
	}
	return out
}

// seedCalls records n calls, each keeping one memory, and returns their rowids.
func seedCalls(t *testing.T, s *Store, projectID string, n int) []int64 {
	t.Helper()
	ctx := context.Background()
	var rowids []int64
	for i := range n {
		if err := s.RecordRetrieval(ctx, RetrievalRecord{
			ProjectID: projectID, Source: "search", QueryHash: digest(700 + i), Outcome: "answerable",
			Verdicts: []RowVerdict{{ID: "MEM-" + string(rune('1'+i)), Kept: true, Stage: "fit", Reason: "fit_response"}},
		}); err != nil {
			t.Fatalf("RecordRetrieval %d: %v", i, err)
		}
		recs, err := s.RetrievalRecords(ctx, 1)
		if err != nil || len(recs) != 1 {
			t.Fatalf("RetrievalRecords %d: %v (%d rows)", i, err, len(recs))
		}
		rowids = append(rowids, recs[0].RowID)
	}
	return rowids
}

// TestHistoryPurgeRemovesTheVerdictsNamingAMemory: the purge removes the
// verdicts of the calls it deletes and the verdicts naming the memory whose call
// survives, and leaves every other verdict alone.
func TestHistoryPurgeRemovesTheVerdictsNamingAMemory(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	for _, id := range []string{"MEM1", "MEM2"} {
		if _, err := s.CreateWithID(ctx, testProject, id, Memory{
			Content: "a fixture memory " + id, Category: "gotcha", Source: "manual",
		}); err != nil {
			t.Fatalf("CreateWithID %s: %v", id, err)
		}
	}
	if err := s.RecordRetrieval(ctx, RetrievalRecord{
		ProjectID: testProject, Source: "search", Outcome: "answerable",
		Verdicts: []RowVerdict{
			{ID: "MEM1", Kept: true, Stage: "fit", Reason: "fit_response"},
			{ID: "MEM2", Kept: true, Stage: "fit", Reason: "fit_response"},
		},
	}); err != nil {
		t.Fatalf("RecordRetrieval: %v", err)
	}
	recs, err := s.RetrievalRecords(ctx, 1)
	if err != nil || len(recs) != 1 {
		t.Fatalf("RetrievalRecords: %v (%d rows)", err, len(recs))
	}
	call := recs[0].RowID
	seedLegacyAudit(t, s, testProject, call, "MEM1")
	seedLegacyAudit(t, s, testProject, call, "MEM2") // the call goes away with MEM1
	seedLegacyAudit(t, s, testProject, 0, "MEM1")    // unattributed, names MEM1
	seedLegacyAudit(t, s, testProject, 0, "MEM2")    // unattributed, names MEM2

	if err := s.DeleteWithOptions(ctx, "MEM1", DeleteOptions{PurgeHistory: true}); err != nil {
		t.Fatalf("purge: %v", err)
	}
	got := readLegacyAudits(t, s)
	if len(got) != 1 || got[0].memoryID != "MEM2" || got[0].rowid != 0 {
		t.Errorf("verdicts after the purge = %+v, want only the unattributed MEM2 row", got)
	}
}

// TestTheRecordCapPairsOnEveryEvictionNotOnlyTheFirst: retrieval_record has no
// AUTOINCREMENT, so past the cap every insert evicts one call, and each
// eviction takes that call's verdicts.
func TestTheRecordCapPairsOnEveryEvictionNotOnlyTheFirst(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	calls := seedCalls(t, s, testProject, 2)
	restore := retrievalRecordRowsCap
	retrievalRecordRowsCap = 1
	t.Cleanup(func() { retrievalRecordRowsCap = restore })

	judge := func(call int, memory string) int64 {
		t.Helper()
		if err := s.RecordRetrieval(ctx, RetrievalRecord{
			ProjectID: testProject, Source: "search", QueryHash: digest(call), Outcome: "answerable",
			Verdicts: []RowVerdict{{ID: memory, Kept: true, Stage: "fit", Reason: "fit_response"}},
		}); err != nil {
			t.Fatalf("RecordRetrieval (call %d): %v", call, err)
		}
		recs, err := s.RetrievalRecords(ctx, 1)
		if err != nil || len(recs) != 1 {
			t.Fatalf("RetrievalRecords: %v (%d rows)", err, len(recs))
		}
		seedLegacyAudit(t, s, testProject, recs[0].RowID, memory)
		return recs[0].RowID
	}
	assertOnly := func(step, wantMemory string, wantRowID int64) {
		t.Helper()
		got := readLegacyAudits(t, s)
		if len(got) != 1 {
			t.Fatalf("%s: %d verdict(s) in the table, want 1 (%+v)", step, len(got), got)
		}
		if got[0].rowid != wantRowID || got[0].memoryID != wantMemory {
			t.Errorf("%s: the surviving verdict is (%s, rowid %d), want (%s, rowid %d)", step,
				got[0].memoryID, got[0].rowid, wantMemory, wantRowID)
		}
	}

	seedLegacyAudit(t, s, testProject, calls[0], "MEM-1")
	second := judge(902, "MEM-2")
	assertOnly("after the second call", "MEM-2", second)
	third := judge(903, "MEM-3")
	assertOnly("after the third call", "MEM-3", third)
}

// TestTheRecordCapTakesTheVerdictsWithIt: the cap's eviction takes the verdicts
// of the evicted calls and spares the unattributed one (record_rowid 0).
func TestTheRecordCapTakesTheVerdictsWithIt(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	calls := seedCalls(t, s, testProject, 3)
	for i, rowid := range calls {
		seedLegacyAudit(t, s, testProject, rowid, "MEM-"+string(rune('1'+i)))
	}
	seedLegacyAudit(t, s, testProject, 0, "MEM-SESSION")

	restore := retrievalRecordRowsCap
	retrievalRecordRowsCap = 2
	t.Cleanup(func() { retrievalRecordRowsCap = restore })
	if err := s.RecordRetrieval(ctx, RetrievalRecord{
		ProjectID: testProject, Source: "search", QueryHash: digest(999), Outcome: "answerable",
		Verdicts: []RowVerdict{{ID: "MEM-4", Kept: true, Stage: "fit", Reason: "fit_response"}},
	}); err != nil {
		t.Fatalf("RecordRetrieval (the call past the cap): %v", err)
	}

	survivors := readLegacyAudits(t, s)
	if len(survivors) != 2 {
		t.Fatalf("%d verdict(s) survive the eviction, want 2 (the kept call's and the unattributed one): %+v",
			len(survivors), survivors)
	}
	kept, unattributed := 0, 0
	for _, r := range survivors {
		switch r.rowid {
		case calls[2]:
			kept++
		case 0:
			unattributed++
		default:
			t.Errorf("verdict about %s still names rowid %d, which the cap evicted", r.memoryID, r.rowid)
		}
	}
	if kept != 1 || unattributed != 1 {
		t.Errorf("survivors: %d for the kept call and %d unattributed, want 1 and 1", kept, unattributed)
	}
}

// TestMergeProjectReassignsTheLegacyVerdicts: the project merge moves a legacy
// verdict with its project, because project_id is not a foreign key.
func TestMergeProjectReassignsTheLegacyVerdicts(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	const old = "old-project"
	if err := s.EnsureProject(ctx, old, "/tmp/"+old, old); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	seedLegacyAudit(t, s, old, 1, "MEM-1")
	seedLegacyAudit(t, s, old, 2, "MEM-2")
	seedLegacyAudit(t, s, testProject, 3, "MEM-3")

	if err := s.MergeProject(ctx, old, testProject); err != nil {
		t.Fatalf("MergeProject: %v", err)
	}
	got := readLegacyAudits(t, s)
	if len(got) != 3 {
		t.Fatalf("%d verdict(s) survive the merge, want 3", len(got))
	}
	for _, r := range got {
		if r.project != testProject {
			t.Errorf("verdict about %s names %q after the merge, want %q", r.memoryID, r.project, testProject)
		}
	}
}

// TestRepoClaimMergeReassignsTheLegacyVerdicts: the bind-recovery merge is the
// second copy of the reassignment list, reached by a save from a directory
// whose remote another project owns.
func TestRepoClaimMergeReassignsTheLegacyVerdicts(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	const remote = "git@github.com:acme/gizmos.git"
	if err := s.EnsureProjectWithRepo(ctx, "owner", "/tmp/owner", "owner", remote); err != nil {
		t.Fatalf("EnsureProjectWithRepo: %v", err)
	}
	if err := s.EnsureProject(ctx, "named", "/tmp/named", "named"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	seedLegacyAudit(t, s, "named", 1, "MEM-1")

	resolved, _, err := s.ResolveOrCreateRepoProject(ctx, "named", "named", "named", "/tmp/named", "named", remote)
	if err != nil {
		t.Fatalf("ResolveOrCreateRepoProject: %v", err)
	}
	if resolved != "owner" {
		t.Fatalf("resolved project = %q, want owner: the merge this test needs did not happen", resolved)
	}
	got := readLegacyAudits(t, s)
	if len(got) != 1 || got[0].project != "owner" {
		t.Errorf("verdicts after the repository-claim merge = %+v, want one naming owner", got)
	}
}

// TestPromoteToGlobalLeavesTheLegacyVerdictWithItsCall: a promotion does not
// move a verdict; it belongs to the project of the call it was filed against.
func TestPromoteToGlobalLeavesTheLegacyVerdictWithItsCall(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	id, err := s.Create(ctx, testProject, Memory{
		Category: "fact", Content: "the verdict stays with the call that filed it", Source: "mcp", Importance: 0.5,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	seedLegacyAudit(t, s, testProject, 1, id)

	if err := s.PromoteToGlobal(ctx, testProject, id); err != nil {
		t.Fatalf("PromoteToGlobal: %v", err)
	}
	got := readLegacyAudits(t, s)
	if len(got) != 1 || got[0].project != testProject || got[0].memoryID != id {
		t.Errorf("verdicts after the promotion = %+v, want one still naming %q", got, testProject)
	}
}

// TestBothMergeStatementListsReassignTheLegacyVerdicts: the reassignment lives
// in two lists that are kept in step on purpose.
func TestBothMergeStatementListsReassignTheLegacyVerdicts(t *testing.T) {
	src, err := os.ReadFile("store.go")
	if err != nil {
		t.Fatalf("read store.go: %v", err)
	}
	const stmt = "UPDATE retrieval_audit SET project_id = ? WHERE project_id = ?"
	if n := strings.Count(string(src), stmt); n != 2 {
		t.Errorf("the reassignment appears %d time(s) in store.go, want 2 — one per merge list", n)
	}
}

// TestEveryWriterThatDeletesRetrievalRecordsTakesTheirVerdicts: every function
// that deletes retrieval_record rows also deletes the retrieval_audit rows filed
// against them, so no path leaves a verdict naming a call that is gone.
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
					"away takes the verdicts filed against it, or a legacy verdict is left naming a call that no "+
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
