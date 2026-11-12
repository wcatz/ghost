package memory

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type legacyAudit struct {
	project  string
	rowid    int64
	memoryID string
}

func seedLegacyAudit(t *testing.T, s *Store, project string, recordRowID int64, memoryID string) {
	t.Helper()
	if _, err := s.db.ExecContext(context.Background(),
		`INSERT INTO retrieval_audit (project_id, record_rowid, session_id, source, memory_id, outcome)
		 VALUES (?, ?, 's1', 'search', ?, 'ignored')`,
		project, recordRowID, memoryID); err != nil {
		t.Fatalf("seed retrieval_audit: %v", err)
	}
}
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
		if seen == 0 && strings.Contains(string(src), deletesRecords) {
			t.Errorf("%s holds a %s statement that no declaration in it was credited with — this scan did not "+
				"read that file correctly, so treat its silence above as unchecked", name, deletesRecords)
		}
	}
	if checked == 0 {
		t.Fatal("no function deletes retrieval_record rows — this test is not looking at what it claims to")
	}
}
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

func TestMigrateV22AddsContentHashToExistingVerdicts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ghost.db")
	db, err := OpenDB(path)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
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
