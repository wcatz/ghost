package memory

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func sampleRetrievalRecord() RetrievalRecord {
	return RetrievalRecord{
		ProjectID: testProject, Source: "search", QueryHash: digest(1),
		Outcome: "answerable", Reason: "floor_met",
		Verdicts: []RowVerdict{{ID: "MEM-1", Kept: true, Stage: "validity", Reason: "valid"}, {ID: "MEM-2", Kept: false, Stage: "validity", Reason: "expired"}},
	}
}
func digest(n int) string { return fmt.Sprintf("%064x", n) }

func TestRecordRetrievalStoresOneRowPerCall(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	for _, rec := range []RetrievalRecord{
		sampleRetrievalRecord(),
		{ProjectID: testProject, Source: "search", QueryHash: digest(2), Outcome: "empty", Reason: "no_candidates"},
		{ProjectID: testProject, Source: "session_start", QueryHash: "", Outcome: "answerable", Reason: "floor_met", Verdicts: []RowVerdict{{ID: "MEM-3", Kept: true, Stage: "validity", Reason: "valid"}}},
	} {
		if err := s.RecordRetrieval(ctx, rec); err != nil {
			t.Fatalf("RecordRetrieval: %v", err)
		}
	}
	var n int
	if err := s.db.QueryRow(`SELECT count(*) FROM retrieval_record`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 3 {
		t.Errorf("rows=%d want 3", n)
	}
}

func TestRecordRetrievalStoresVerdictsAndNotText(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	const query = "how does the k3s ingress controller rotate its certificate"
	const content = "the ingress controller reloads its certificate on SIGHUP only"
	if err := s.RecordRetrieval(ctx, sampleRetrievalRecord()); err != nil {
		t.Fatalf("RecordRetrieval: %v", err)
	}
	for _, text := range []string{query, content} {
		leaked, err := countOccurrences(t, s.db, text)
		if err != nil {
			t.Fatalf("scan: %v", err)
		}
		if leaked != 0 {
			t.Errorf("%q leaked in %d rows", text, leaked)
		}
	}
}

func TestRecordRetrievalRoundTripsEveryVerdict(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	want := sampleRetrievalRecord()
	want.SessionID, want.AsOf = "sess-abc", "2026-09-01T00:00:00Z"
	if err := s.RecordRetrieval(ctx, want); err != nil {
		t.Fatalf("RecordRetrieval: %v", err)
	}
	got, err := s.RetrievalRecords(ctx, 10)
	if err != nil || len(got) != 1 {
		t.Fatalf("RetrievalRecords: %v %d", err, len(got))
	}
	r := got[0]
	if r.ProjectID != want.ProjectID || r.Source != want.Source || r.QueryHash != want.QueryHash || r.SessionID != want.SessionID || r.AsOf != want.AsOf || r.Outcome != want.Outcome || r.Reason != want.Reason {
		t.Errorf("header/verdict mismatch: %+v", r)
	}
	if len(r.Verdicts) != len(want.Verdicts) {
		t.Fatalf("verdicts=%d want %d", len(r.Verdicts), len(want.Verdicts))
	}
	for i := range want.Verdicts {
		if r.Verdicts[i] != want.Verdicts[i] {
			t.Errorf("verdict %d = %+v want %+v", i, r.Verdicts[i], want.Verdicts[i])
		}
	}
	if r.RecordedAt == "" {
		t.Error("recorded_at empty")
	}
}

func digestOf(hash string) int {
	var n int
	_, _ = fmt.Sscanf(hash, "%x", &n)
	return n
}

func TestRetrievalRecordsAreNewestFirst(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	for i := range 3 {
		rec := sampleRetrievalRecord()
		rec.QueryHash = digest(i)
		if err := s.RecordRetrieval(ctx, rec); err != nil {
			t.Fatalf("RecordRetrieval %d: %v", i, err)
		}
	}
	got, err := s.RetrievalRecords(ctx, 2)
	if err != nil || len(got) != 2 {
		t.Fatalf("RetrievalRecords: %v %d", err, len(got))
	}
	if digestOf(got[0].QueryHash) != 2 || digestOf(got[1].QueryHash) != 1 {
		t.Errorf("order=%d,%d want 2,1", digestOf(got[0].QueryHash), digestOf(got[1].QueryHash))
	}
}

func TestRecordRetrievalCapsTheTableOldestFirst(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	restore := retrievalRecordRowsCap
	retrievalRecordRowsCap = 10
	t.Cleanup(func() { retrievalRecordRowsCap = restore })
	for i := range 25 {
		rec := sampleRetrievalRecord()
		rec.QueryHash = digest(i)
		if err := s.RecordRetrieval(ctx, rec); err != nil {
			t.Fatalf("RecordRetrieval %d: %v", i, err)
		}
		var n int
		if err := s.db.QueryRow(`SELECT count(*) FROM retrieval_record`).Scan(&n); err != nil {
			t.Fatalf("count: %v", err)
		}
		if n > retrievalRecordRowsCap {
			t.Fatalf("after %d rows=%d cap=%d", i, n, retrievalRecordRowsCap)
		}
	}
	var n int
	if err := s.db.QueryRow(`SELECT count(*) FROM retrieval_record`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != retrievalRecordRowsCap {
		t.Errorf("rows=%d want %d", n, retrievalRecordRowsCap)
	}
	got, err := s.RetrievalRecords(ctx, retrievalRecordRowsCap)
	if err != nil {
		t.Fatalf("RetrievalRecords: %v", err)
	}
	if digestOf(got[0].QueryHash) != 24 || digestOf(got[len(got)-1].QueryHash) != 15 {
		t.Errorf("window newest=%d oldest=%d", digestOf(got[0].QueryHash), digestOf(got[len(got)-1].QueryHash))
	}
}

func TestRecordRetrievalIsRefusedOnAStoreANewerGhostOwns(t *testing.T) {
	s, path := fileBackedStore(t)
	ctx := context.Background()
	if err := s.RecordRetrieval(ctx, sampleRetrievalRecord()); err != nil {
		t.Fatalf("RecordRetrieval: %v", err)
	}
	stampFromAnotherHandle(t, path, schemaVersion+1)
	err := s.RecordRetrieval(ctx, sampleRetrievalRecord())
	if !errors.Is(err, ErrStoreNewer) {
		t.Fatalf("err=%v want ErrStoreNewer", err)
	}
	var n int
	if err := s.db.QueryRow(`SELECT count(*) FROM retrieval_record`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("rows=%d want 1", n)
	}
}

func TestPurgeRemovesTheRetrievalRecordsNamingTheMemory(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	rec := sampleRetrievalRecord()
	rec.Verdicts = []RowVerdict{{ID: "SECRET-MEMORY-ID", Kept: true, Stage: "validity", Reason: "valid"}, {ID: "OTHER-MEMORY-ID", Kept: true, Stage: "validity", Reason: "valid"}}
	if err := s.RecordRetrieval(ctx, rec); err != nil {
		t.Fatalf("RecordRetrieval: %v", err)
	}
	if err := s.RecordRetrieval(ctx, RetrievalRecord{ProjectID: testProject, Source: "search", QueryHash: digest(3), Outcome: "answerable", Reason: "floor_met", Verdicts: []RowVerdict{{ID: "UNRELATED-MEMORY-ID", Kept: true, Stage: "validity", Reason: "valid"}}}); err != nil {
		t.Fatalf("RecordRetrieval: %v", err)
	}
	if _, err := s.PurgeMemoryHistory(ctx, "SECRET-MEMORY-ID"); err != nil {
		t.Fatalf("PurgeMemoryHistory: %v", err)
	}
	var n int
	if err := s.db.QueryRow(`SELECT count(*) FROM retrieval_record WHERE EXISTS (SELECT 1 FROM json_each(retrieval_record.verdicts) WHERE value->>'id' = ?)`, "SECRET-MEMORY-ID").Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("secret still named %d times", n)
	}
	if err := s.db.QueryRow(`SELECT count(*) FROM retrieval_record`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("rows=%d err=%v want 1", n, err)
	}
}

func TestPurgeToleratesARowWhoseVerdictsAreNotJSON(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	for _, bad := range []string{`not json at all`, `{"id":"X"}`} {
		if _, err := s.db.Exec(`INSERT INTO retrieval_record (project_id, source, query_hash, outcome, reason, verdicts) VALUES (?, 'search', ?, 'answerable', 'floor_met', ?)`, testProject, digest(4), bad); err != nil {
			t.Fatalf("insert bad: %v", err)
		}
	}
	rec := sampleRetrievalRecord()
	rec.Verdicts = []RowVerdict{{ID: "SECRET-MEMORY-ID", Kept: true, Stage: "validity", Reason: "valid"}}
	if err := s.RecordRetrieval(ctx, rec); err != nil {
		t.Fatalf("RecordRetrieval: %v", err)
	}
	if _, err := s.PurgeMemoryHistory(ctx, "SECRET-MEMORY-ID"); err != nil {
		t.Fatalf("PurgeMemoryHistory: %v", err)
	}
	var n int
	if err := s.db.QueryRow(`SELECT count(*) FROM retrieval_record WHERE verdicts LIKE '%SECRET-MEMORY-ID%'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("secret left n=%d err=%v", n, err)
	}
	if err := s.db.QueryRow(`SELECT count(*) FROM retrieval_record`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("rows=%d err=%v want 2", n, err)
	}
}

func TestPurgeReachesARowWhoseVerdictsNameTheMemoryButCannotBeRead(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if _, err := s.db.Exec(`INSERT INTO retrieval_record (project_id, source, query_hash, outcome, reason, verdicts) VALUES (?, 'search', ?, 'answerable', 'floor_met', ?)`, testProject, digest(9), `[{"id":"SECRET-MEMORY-ID","kept":true} truncated`); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if _, err := s.db.Exec(`INSERT INTO retrieval_record (project_id, source, query_hash, outcome, reason, verdicts) VALUES (?, 'search', ?, 'answerable', 'floor_met', 'also not json')`, testProject, digest(10)); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if _, err := s.PurgeMemoryHistory(ctx, "SECRET-MEMORY-ID"); err != nil {
		t.Fatalf("PurgeMemoryHistory: %v", err)
	}
	var left int
	if err := s.db.QueryRow(`SELECT count(*) FROM retrieval_record WHERE verdicts LIKE '%SECRET-MEMORY-ID%'`).Scan(&left); err != nil || left != 0 {
		t.Fatalf("secret left=%d err=%v", left, err)
	}
	if err := s.db.QueryRow(`SELECT count(*) FROM retrieval_record`).Scan(&left); err != nil || left != 1 {
		t.Fatalf("rows=%d err=%v want 1", left, err)
	}
}

func TestPurgeDoesNotTreatAnIdAsAPattern(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	const id = "A_1%done"
	for _, mid := range []string{id, "Ax1%done", "A_1anything"} {
		if err := s.RecordRetrieval(ctx, RetrievalRecord{ProjectID: testProject, Source: "search", QueryHash: digest(11), Outcome: "answerable", Reason: "floor_met", Verdicts: []RowVerdict{{ID: mid, Kept: true, Stage: "validity", Reason: "valid"}}}); err != nil {
			t.Fatalf("RecordRetrieval %q: %v", mid, err)
		}
	}
	if _, err := s.PurgeMemoryHistory(ctx, id); err != nil {
		t.Fatalf("PurgeMemoryHistory: %v", err)
	}
	var n, left int
	if err := s.db.QueryRow(`SELECT count(*) FROM retrieval_record`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("rows=%d err=%v want 2", n, err)
	}
	if err := s.db.QueryRow(`SELECT count(*) FROM retrieval_record WHERE instr(verdicts, ?) > 0`, id).Scan(&left); err != nil || left != 0 {
		t.Fatalf("target left=%d err=%v", left, err)
	}
}

func TestPurgeRefusesAnEmptyMemoryID(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	for i := range 3 {
		if err := s.RecordRetrieval(ctx, RetrievalRecord{ProjectID: testProject, Source: "search", QueryHash: digest(i + 20), Outcome: "answerable", Reason: "floor_met", Verdicts: []RowVerdict{{ID: "M" + digest(i), Kept: true}}}); err != nil {
			t.Fatalf("RecordRetrieval: %v", err)
		}
	}
	if _, err := s.PurgeMemoryHistory(ctx, ""); err == nil {
		t.Fatal("expected refusal")
	}
	var n int
	if err := s.db.QueryRow(`SELECT count(*) FROM retrieval_record`).Scan(&n); err != nil || n != 3 {
		t.Fatalf("rows=%d err=%v want 3", n, err)
	}
}

func TestMigrateV20RefusesATableThatIsNotGhosts(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "ghost.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close() //nolint:errcheck
	if _, err := db.Exec(initSQL); err != nil {
		t.Fatalf("initSQL: %v", err)
	}
	if _, err := db.Exec(`DROP TABLE retrieval_record`); err != nil {
		t.Fatalf("drop: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE retrieval_record (id TEXT PRIMARY KEY, note TEXT)`); err != nil {
		t.Fatalf("create: %v", err)
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	err = migrateV20(tx)
	_ = tx.Rollback() //nolint:errcheck
	if err == nil {
		t.Fatal("expected refusal")
	}
	for _, want := range []string{"retrieval_record", "DROP TABLE retrieval_record"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q: %v", want, err)
		}
	}
}

func TestOpenDBRefusesAForeignRetrievalRecordBeforeItBacksUp(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "ghost.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := db.Exec(initSQL); err != nil {
		t.Fatalf("initSQL: %v", err)
	}
	for _, stmt := range []string{`DROP TABLE retrieval_record`, `CREATE TABLE retrieval_record (id TEXT PRIMARY KEY, note TEXT)`, `DROP INDEX IF EXISTS idx_retrieval_record_project`, fmt.Sprintf(`PRAGMA user_version = %d`, schemaVersion-1)} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	opened, err := OpenDB(dbPath)
	if err == nil {
		_ = opened.Close()
		t.Fatal("expected refusal")
	}
	if !strings.Contains(err.Error(), "retrieval_record") {
		t.Errorf("error: %v", err)
	}
	backups, err := filepath.Glob(dbPath + ".pre-migrate-*")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(backups) != 0 {
		t.Errorf("wrote backups: %v", backups)
	}
}

func TestDeleteProjectTakesTheRecordsWithIt(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if err := s.RecordRetrieval(ctx, sampleRetrievalRecord()); err != nil {
		t.Fatalf("RecordRetrieval: %v", err)
	}
	if err := s.EnsureProject(ctx, "other-project", "/tmp/test-other", "other"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	if err := s.RecordRetrieval(ctx, RetrievalRecord{ProjectID: "other-project", Source: "search", QueryHash: digest(30), Outcome: "answerable", Reason: "floor_met", Verdicts: []RowVerdict{{ID: "OTHER", Kept: true}}}); err != nil {
		t.Fatalf("RecordRetrieval: %v", err)
	}
	if _, err := s.DeleteProject(ctx, testProject, true); err != nil {
		t.Fatalf("DeleteProject: %v", err)
	}
	var left int
	if err := s.db.QueryRow(`SELECT count(*) FROM retrieval_record WHERE project_id = ?`, testProject).Scan(&left); err != nil || left != 0 {
		t.Fatalf("deleted project rows=%d err=%v", left, err)
	}
	if err := s.db.QueryRow(`SELECT count(*) FROM retrieval_record`).Scan(&left); err != nil || left != 1 {
		t.Fatalf("remaining=%d err=%v want 1", left, err)
	}
}

func TestAForeignRetrievalRecordCheckIgnoresAColumnThisBuildDoesNotKnow(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "ghost.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := db.Exec(initSQL); err != nil {
		t.Fatalf("initSQL: %v", err)
	}
	if _, err := db.Exec(`ALTER TABLE retrieval_record ADD COLUMN scope TEXT`); err != nil {
		t.Fatalf("alter: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	opened, err := OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer opened.Close() //nolint:errcheck
	s := NewStore(opened, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := s.RecordRetrieval(context.Background(), sampleRetrievalRecord()); err != nil {
		t.Fatalf("RecordRetrieval: %v", err)
	}
}

func TestMergeProjectCarriesTheRecordsWithIt(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	for _, p := range []struct{ id, path, name string }{{"keep-project", "/tmp/test-keep", "keep"}, {"other-project", "/tmp/test-other", "other"}} {
		if err := s.EnsureProject(ctx, p.id, p.path, p.name); err != nil {
			t.Fatalf("EnsureProject: %v", err)
		}
	}
	for i, pid := range []string{testProject, "keep-project", "other-project"} {
		rec := sampleRetrievalRecord()
		rec.ProjectID = pid
		rec.QueryHash = digest(40 + i)
		if err := s.RecordRetrieval(ctx, rec); err != nil {
			t.Fatalf("RecordRetrieval %s: %v", pid, err)
		}
	}
	if err := s.MergeProject(ctx, "keep-project", testProject); err != nil {
		t.Fatalf("MergeProject: %v", err)
	}
	for _, c := range []struct {
		project string
		want    int
	}{{"keep-project", 0}, {testProject, 2}, {"other-project", 1}} {
		var n int
		if err := s.db.QueryRow(`SELECT count(*) FROM retrieval_record WHERE project_id = ?`, c.project).Scan(&n); err != nil || n != c.want {
			t.Fatalf("%s rows=%d err=%v want %d", c.project, n, err, c.want)
		}
	}
}

func TestDeleteProjectSummaryCountsTheRecords(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	for i := range 2 {
		rec := sampleRetrievalRecord()
		rec.QueryHash = digest(i + 50)
		if err := s.RecordRetrieval(ctx, rec); err != nil {
			t.Fatalf("RecordRetrieval: %v", err)
		}
	}
	summary, err := s.DeleteProject(ctx, testProject, false)
	if err != nil || summary.RetrievalRecords != 2 {
		t.Fatalf("dry run: %+v err=%v", summary, err)
	}
	applied, err := s.DeleteProject(ctx, testProject, true)
	if err != nil || applied.RetrievalRecords != summary.RetrievalRecords {
		t.Fatalf("apply: %+v err=%v", applied, err)
	}
	var left int
	if err := s.db.QueryRow(`SELECT count(*) FROM retrieval_record`).Scan(&left); err != nil || left != 0 {
		t.Fatalf("left=%d err=%v", left, err)
	}
}

func TestBothMergeStatementListsReassignTheRecords(t *testing.T) {
	src, err := os.ReadFile("store.go")
	if err != nil {
		t.Fatalf("read store.go: %v", err)
	}
	const stmt = "UPDATE retrieval_record SET project_id = ? WHERE project_id = ?"
	if n := strings.Count(string(src), stmt); n != 2 {
		t.Errorf("stmt appears %d times, want 2", n)
	}
}

func TestEveryRendererOfTheDeleteSummaryNamesTheRecords(t *testing.T) {
	for _, c := range []struct{ path, needle string }{
		{"../../cmd/ghost/project.go", "RetrievalRecords"},
		{"../mcpserver/mcpserver.go", "RetrievalRecords"},
		{"store.go", "retrieval_records"},
	} {
		src, err := os.ReadFile(c.path)
		if err != nil {
			t.Fatalf("read %s: %v", c.path, err)
		}
		if !strings.Contains(string(src), c.needle) {
			t.Errorf("%s missing %s", c.path, c.needle)
		}
	}
}

func TestRecordRetrievalRejectsAnEmptyProject(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	err := s.RecordRetrieval(ctx, RetrievalRecord{Source: "search", QueryHash: digest(1), Outcome: "answerable"})
	if err == nil {
		t.Fatal("expected error")
	}
	var n int
	if err := s.db.QueryRow(`SELECT count(*) FROM retrieval_record`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("rows=%d err=%v", n, err)
	}
}

func TestRetrievalRecordsSurvivesAMalformedVerdictColumn(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if err := s.RecordRetrieval(ctx, sampleRetrievalRecord()); err != nil {
		t.Fatalf("RecordRetrieval: %v", err)
	}
	if _, err := s.db.Exec(`INSERT INTO retrieval_record (project_id, source, query_hash, outcome, reason, verdicts) VALUES (?, 'search', ?, 'answerable', 'floor_met', 'not json')`, testProject, digest(5)); err != nil {
		t.Fatalf("insert: %v", err)
	}
	got, err := s.RetrievalRecords(ctx, 10)
	if err != nil || len(got) != 2 {
		t.Fatalf("RetrievalRecords: %v %d", err, len(got))
	}
	if len(got[0].Verdicts) != 0 || len(got[1].Verdicts) != 2 {
		t.Errorf("verdict lens=%d,%d", len(got[0].Verdicts), len(got[1].Verdicts))
	}
}

func TestThePurgeDoesNotFuzzyMatchAWellFormedRecord(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	const purgeID, neighbour = "A_1", "A_1%done"
	if err := s.RecordRetrieval(ctx, RetrievalRecord{ProjectID: testProject, Source: "search", QueryHash: digest(70), Outcome: "answerable", Reason: "floor_met", Verdicts: []RowVerdict{{ID: neighbour, Kept: true, Stage: "validity", Reason: "valid"}}}); err != nil {
		t.Fatalf("RecordRetrieval: %v", err)
	}
	if err := s.RecordRetrieval(ctx, RetrievalRecord{ProjectID: testProject, Source: "search", QueryHash: digest(71), Outcome: "answerable", Reason: "floor_met", Verdicts: []RowVerdict{{ID: purgeID, Kept: true, Stage: "validity", Reason: "valid"}}}); err != nil {
		t.Fatalf("RecordRetrieval: %v", err)
	}
	if _, err := s.PurgeMemoryHistory(ctx, purgeID); err != nil {
		t.Fatalf("PurgeMemoryHistory: %v", err)
	}
	recs, err := s.RetrievalRecords(ctx, 10)
	if err != nil || len(recs) != 1 || len(recs[0].Verdicts) != 1 || recs[0].Verdicts[0].ID != neighbour {
		t.Fatalf("recs=%+v err=%v", recs, err)
	}
}

func TestAPurgeIdThatIsAJSONKeywordCannotWipeTheTable(t *testing.T) {
	for _, id := range []string{"id", "stage", "true", "reason", "valid", "validity", "floor_met", "mem"} {
		t.Run(id, func(t *testing.T) {
			s := testStore(t)
			ctx := context.Background()
			if err := s.RecordRetrieval(ctx, sampleRetrievalRecord()); err != nil {
				t.Fatalf("RecordRetrieval: %v", err)
			}
			if err := s.RecordRetrieval(ctx, RetrievalRecord{ProjectID: testProject, Source: "search", QueryHash: digest(80), Outcome: "answerable", Reason: "floor_met", Verdicts: []RowVerdict{{ID: "MEM-9", Kept: true}}}); err != nil {
				t.Fatalf("RecordRetrieval: %v", err)
			}
			if _, err := s.db.Exec(`INSERT INTO retrieval_record (project_id, source, query_hash, outcome, reason, verdicts) VALUES (?, 'search', ?, 'answerable', 'floor_met', ?)`, testProject, digest(81), `[{"id":"MEM-8","kept":true,"stage":"validity","reason":"valid"},{"id":"MEM-9","kept":false,"stage":"budget","reason":"expired"}`); err != nil {
				t.Fatalf("insert: %v", err)
			}
			if _, err := s.PurgeMemoryHistory(ctx, id); err != nil {
				t.Fatalf("PurgeMemoryHistory: %v", err)
			}
			var left int
			if err := s.db.QueryRow(`SELECT count(*) FROM retrieval_record`).Scan(&left); err != nil || left != 3 {
				t.Fatalf("left=%d err=%v want 3", left, err)
			}
		})
	}
}

func TestTheLeakScanCatchesTextHiddenInsideALongerValue(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	const query = "how does the k3s ingress controller rotate its certificate"
	const content = "the ingress controller reloads its certificate on SIGHUP only"
	if err := s.RecordRetrieval(ctx, RetrievalRecord{ProjectID: testProject, Source: "search", QueryHash: digest(60), Outcome: "answerable", Reason: "the caller asked: " + query, Verdicts: []RowVerdict{{ID: "MEM-1", Kept: true, Stage: "validity", Reason: "valid"}, {ID: "MEM-2", Kept: false, Stage: "budget", Reason: "dropped because: " + content}}}); err != nil {
		t.Fatalf("RecordRetrieval: %v", err)
	}
	for _, text := range []string{query, content} {
		n, err := countOccurrences(t, s.db, text)
		if err != nil {
			t.Fatalf("scan: %v", err)
		}
		if n == 0 {
			t.Errorf("text %q not found", text)
		}
	}
}

func TestRowVerdictJSONShape(t *testing.T) {
	b, err := json.Marshal(RowVerdict{ID: "M", Kept: true, Stage: "validity", Reason: "valid"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if got, want := string(b), `{"id":"M","kept":true,"stage":"validity","reason":"valid"}`; got != want {
		t.Errorf("got %s want %s", got, want)
	}
}

func TestARecordWriteGivesUpOnAHeldStoreLockRatherThanWaiting(t *testing.T) {
	s := testStore(t)
	s.mu.Lock()
	held := make(chan struct{})
	go func() { defer close(held); time.Sleep(time.Second); s.mu.Unlock() }()
	t.Cleanup(func() { <-held })
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := s.RecordRetrieval(ctx, sampleRetrievalRecord())
	elapsed := time.Since(start)
	if err == nil {
		t.Error("expected error")
	}
	if elapsed > 900*time.Millisecond {
		t.Errorf("elapsed=%v", elapsed)
	}
	var left int
	if qErr := s.db.QueryRow(`SELECT count(*) FROM retrieval_record`).Scan(&left); qErr != nil || left != 0 {
		t.Fatalf("left=%d err=%v", left, qErr)
	}
}

func TestARecordWriteIsBoundedWhenAnotherProcessHoldsTheWriteLock(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "ghost.db")
	db, err := OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	s := NewStore(db, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := s.EnsureProject(context.Background(), testProject, "/tmp/test", "test"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	blocker, err := sql.Open("sqlite", "file:"+dbPath+"?_txlock=immediate&_pragma=journal_mode(WAL)&_pragma=busy_timeout(0)")
	if err != nil {
		t.Fatalf("open blocker: %v", err)
	}
	t.Cleanup(func() { _ = blocker.Close() })
	blocker.SetMaxOpenConns(1)
	held, err := blocker.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("begin blocker: %v", err)
	}
	t.Cleanup(func() { _ = held.Rollback() })
	if _, err := held.Exec(`CREATE TABLE hold (id INTEGER)`); err != nil {
		t.Fatalf("hold: %v", err)
	}
	probe, err := sql.Open("sqlite", "file:"+dbPath+"?_txlock=immediate&_pragma=journal_mode(WAL)&_pragma=busy_timeout(0)")
	if err != nil {
		t.Fatalf("open probe: %v", err)
	}
	t.Cleanup(func() { _ = probe.Close() })
	probe.SetMaxOpenConns(1)
	if probeTx, pErr := probe.BeginTx(context.Background(), nil); pErr == nil {
		_ = probeTx.Rollback()
		t.Fatal("probe acquired lock")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	start := time.Now()
	err = s.RecordRetrieval(ctx, sampleRetrievalRecord())
	elapsed := time.Since(start)
	if err == nil {
		t.Error("expected error")
	}
	if elapsed > 2*time.Second {
		t.Errorf("elapsed=%v", elapsed)
	}
}

func TestAScopedRecordWritePutsTheStoreBusyTimeoutBack(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "ghost.db")
	db, err := OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	s := NewStore(db, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := s.EnsureProject(context.Background(), testProject, "/tmp/test", "test"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	read := func() int {
		t.Helper()
		var got int
		if err := db.QueryRow(`PRAGMA busy_timeout`).Scan(&got); err != nil {
			t.Fatalf("read: %v", err)
		}
		return got
	}
	original := read()
	if original <= 0 {
		t.Fatalf("busy_timeout=%d", original)
	}
	if err := s.RecordRetrieval(context.Background(), sampleRetrievalRecord()); err != nil {
		t.Fatalf("RecordRetrieval: %v", err)
	}
	if got := read(); got != original {
		t.Errorf("after success got=%d want=%d", got, original)
	}
	blocker, err := sql.Open("sqlite", "file:"+dbPath+"?_txlock=immediate&_pragma=journal_mode(WAL)&_pragma=busy_timeout(0)")
	if err != nil {
		t.Fatalf("open blocker: %v", err)
	}
	t.Cleanup(func() { _ = blocker.Close() })
	blocker.SetMaxOpenConns(1)
	held, err := blocker.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("begin blocker: %v", err)
	}
	t.Cleanup(func() { _ = held.Rollback() })
	if _, err := held.Exec(`CREATE TABLE hold (id INTEGER)`); err != nil {
		t.Fatalf("hold: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if err := s.RecordRetrieval(ctx, sampleRetrievalRecord()); err == nil {
		t.Fatal("expected error")
	}
	if got := read(); got != original {
		t.Errorf("after refusal got=%d want=%d", got, original)
	}
}
