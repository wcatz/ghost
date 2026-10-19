package memory

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// v27FTSSQL is the full-text index a v27 store carries: one unbucketed index
// keyed by the memory's rowid, content=memories, as initSQL spelled it before
// migrateV28.
const v27FTSSQL = `
CREATE VIRTUAL TABLE memories_fts USING fts5(
    content,
    content=memories,
    content_rowid=rowid,
    tokenize='porter unicode61'
);

CREATE TRIGGER memories_ai AFTER INSERT ON memories BEGIN
    INSERT INTO memories_fts(rowid, content) VALUES (new.rowid, new.content);
END;

CREATE TRIGGER memories_ad AFTER DELETE ON memories BEGIN
    INSERT INTO memories_fts(memories_fts, rowid, content) VALUES('delete', old.rowid, old.content);
END;

CREATE TRIGGER memories_au AFTER UPDATE ON memories WHEN old.content != new.content BEGIN
    INSERT INTO memories_fts(memories_fts, rowid, content) VALUES('delete', old.rowid, old.content);
    INSERT INTO memories_fts(rowid, content) VALUES (new.rowid, new.content);
END;
`

// v27SearchSQL and v27DedupSQL are the keyword search and the same-category
// duplicate lookup exactly as a v27 build ran them, so the migrated store's
// answers can be compared with the answers the unmigrated store gave.
const v27SearchSQL = `
		SELECT m.id
		FROM memories m
		JOIN memories_fts f ON f.rowid = m.rowid
		WHERE (m.project_id = ? OR m.project_id = '_global') AND memories_fts MATCH ?
		ORDER BY rank, m.importance DESC
		LIMIT ?`

const v27DedupSQL = `
		SELECT m.id
		FROM memories m
		JOIN memories_fts f ON f.rowid = m.rowid
		WHERE m.project_id = ?
		  AND m.category = ?
		  AND memories_fts MATCH ?
		ORDER BY rank, m.importance DESC
		LIMIT 15`

// ftsSchemaObjects returns the stored DDL of every object the full-text index
// is made of, by name.
func ftsSchemaObjects(t *testing.T, db *sql.DB) map[string]string {
	t.Helper()
	rows, err := db.Query(`SELECT name, sql FROM sqlite_master
		WHERE name IN ('memories_fts', 'memories_fts_source', 'memories_fts_projects', 'memories_ai', 'memories_ad', 'memories_au')`)
	if err != nil {
		t.Fatalf("read schema: %v", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]string{}
	for rows.Next() {
		var name, ddl string
		if err := rows.Scan(&name, &ddl); err != nil {
			t.Fatalf("scan schema: %v", err)
		}
		out[name] = ddl
	}
	return out
}

type ftsAnswers map[string][]string

// collectFTSAnswers records every comparison query's answer through the given
// statements, keyed by the query, the project and the category.
func collectFTSAnswers(t *testing.T, db *sql.DB, search func(project, match string) (string, []any), dedup func(project, category, match string) (string, []any)) ftsAnswers {
	t.Helper()
	out := ftsAnswers{}
	run := func(key, q string, args []any) {
		rows, err := db.Query(q, args...)
		if err != nil {
			t.Fatalf("%s: %v", key, err)
		}
		var ids []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				t.Fatalf("%s: scan: %v", key, err)
			}
			ids = append(ids, id)
		}
		_ = rows.Close()
		out[key] = ids
	}
	for _, raw := range []string{"the deploy is to staging", "staging cache", "nightly", "zebra"} {
		for _, p := range []string{"alpha", "beta", GlobalProjectID} {
			q, a := search(p, sanitizeFTS(raw))
			run(fmt.Sprintf("search|%s|%s", raw, p), q, a)
			for _, c := range []string{"fact", "gotcha"} {
				q, a := dedup(p, c, sanitizeFTSN(raw, 30))
				run(fmt.Sprintf("dedup|%s|%s|%s", raw, p, c), q, a)
			}
		}
	}
	return out
}

func v27Search(project, match string) (string, []any) {
	return v27SearchSQL, []any{project, match, 50}
}

func v27Dedup(project, category, match string) (string, []any) {
	return v27DedupSQL, []any{project, category, match}
}

func v28Search(project, match string) (string, []any) {
	q, a := ftsSearchStatement(project, match, 50, time.Time{}, false)
	return "SELECT id FROM (" + q + ")", a
}

func v28Dedup(project, category, match string) (string, []any) {
	q, a := dedupLookupStatement(false, "", nil, project, category, match)
	return "SELECT id FROM (" + q + ")", a
}

// TestOpenDBMigratesAV27StoreToTheBucketedIndex: a v27 store holding rows in
// several projects and _global — tied ranks inside a project and across a
// project and _global among them — is migrated to the bucketed index, and the
// migrated store answers every keyword search and duplicate lookup with the
// rows, and the order, the v27 statements gave on the unmigrated store. The
// index is in sync with its content afterwards, the schema is the one a fresh
// store gets, and running the step a second time changes nothing.
func TestOpenDBMigratesAV27StoreToTheBucketedIndex(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "v27.db")
	seed, err := OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB (seed): %v", err)
	}
	for _, stmt := range []string{
		`DROP TRIGGER memories_ai`, `DROP TRIGGER memories_ad`, `DROP TRIGGER memories_au`,
		`DROP TABLE memories_fts`, `DROP VIEW memories_fts_source`, `DROP TABLE memories_fts_projects`,
	} {
		if _, err := seed.Exec(stmt); err != nil {
			t.Fatalf("strip the v28 index: %s: %v", stmt, err)
		}
	}
	if _, err := seed.Exec(v27FTSSQL); err != nil {
		t.Fatalf("create the v27 index: %v", err)
	}
	for _, p := range []string{"alpha", "beta", "gamma", GlobalProjectID} {
		if _, err := seed.Exec(`INSERT INTO projects (id, name, path) VALUES (?, ?, ?) ON CONFLICT(id) DO NOTHING`, p, p, "/x/"+p); err != nil {
			t.Fatalf("insert project: %v", err)
		}
	}
	ins := func(project, category, content string, importance float64) {
		if _, err := seed.Exec(`INSERT INTO memories (project_id, category, content, source, importance) VALUES (?, ?, ?, 'manual', ?)`,
			project, category, content, importance); err != nil {
			t.Fatalf("insert memory: %v", err)
		}
	}
	for i, p := range []string{"alpha", GlobalProjectID, "beta", "alpha", GlobalProjectID, "gamma"} {
		ins(p, []string{"fact", "gotcha"}[i%2], "the deploy runs to staging nightly", 0.5)
	}
	for i := 0; i < 30; i++ {
		ins("beta", "fact", fmt.Sprintf("the deploy is to staging cluster %d", i), 0.4)
		ins("gamma", "gotcha", fmt.Sprintf("staging deploy breaks the cache %d", i), 0.6)
		if i%3 == 0 {
			ins("alpha", []string{"fact", "gotcha"}[i%2], fmt.Sprintf("the staging cache warmup step %d", i), 0.3+float64(i)/100)
		}
	}
	ins(GlobalProjectID, "fact", "zebra rule about staging deploy", 0.7)
	// A deleted row and an edited one, so the v27 index carries tombstones.
	if _, err := seed.Exec(`DELETE FROM memories WHERE content = 'the staging cache warmup step 0'`); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := seed.Exec(`UPDATE memories SET content = 'the staging cache warmup step rewritten' WHERE content = 'the staging cache warmup step 3'`); err != nil {
		t.Fatalf("edit: %v", err)
	}
	before := collectFTSAnswers(t, seed, v27Search, v27Dedup)
	if _, err := seed.Exec(`PRAGMA user_version = 27`); err != nil {
		t.Fatalf("stamp v27: %v", err)
	}
	_ = seed.Close()

	db, err := OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB (migrate): %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	var v int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil || v != schemaVersion {
		t.Fatalf("user_version = %d, want %d (err=%v)", v, schemaVersion, err)
	}
	s := NewStore(db, slog.New(slog.NewTextHandler(io.Discard, nil)))

	verify := func(stage string) {
		t.Helper()
		assertFTSInSync(t, s)
		after := collectFTSAnswers(t, db, v28Search, v28Dedup)
		for key, want := range before {
			if got := after[key]; strings.Join(got, ",") != strings.Join(want, ",") {
				t.Errorf("%s: %s\n got %v\nwant %v", stage, key, got, want)
			}
		}
		// And through the store's own reader, which is what a caller runs.
		got, err := s.SearchFTS(context.Background(), "alpha", "the deploy is to staging", 50)
		if err != nil {
			t.Fatalf("%s: SearchFTS: %v", stage, err)
		}
		if want := before["search|the deploy is to staging|alpha"]; strings.Join(memoryIDs(got), ",") != strings.Join(want, ",") {
			t.Errorf("%s: SearchFTS\n got %v\nwant %v", stage, memoryIDs(got), want)
		}
	}
	verify("migrated")

	// The migrated schema is the one a fresh store gets.
	fresh, err := OpenDB(filepath.Join(t.TempDir(), "fresh.db"))
	if err != nil {
		t.Fatalf("OpenDB (fresh): %v", err)
	}
	t.Cleanup(func() { _ = fresh.Close() })
	freshObjs, migratedObjs := ftsSchemaObjects(t, fresh), ftsSchemaObjects(t, db)
	if len(freshObjs) != 6 {
		t.Fatalf("a fresh store has %d of the six full-text objects: %v", len(freshObjs), freshObjs)
	}
	for name, ddl := range freshObjs {
		if migratedObjs[name] != ddl {
			t.Errorf("%s differs between a fresh and a migrated store:\nfresh:    %s\nmigrated: %s", name, ddl, migratedObjs[name])
		}
	}

	// Harmless to run twice.
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := migrateV28(tx); err != nil {
		_ = tx.Rollback()
		t.Fatalf("migrateV28 (second): %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	verify("migrated twice")

	// And the triggers it installed keep the index in sync for later writes.
	if _, err := s.db.Exec(`UPDATE memories SET project_id = '_global' WHERE content = 'the staging cache warmup step 6'`); err != nil {
		t.Fatalf("move: %v", err)
	}
	if _, err := s.db.Exec(`DELETE FROM memories WHERE project_id = 'gamma'`); err != nil {
		t.Fatalf("delete project rows: %v", err)
	}
	assertFTSInSync(t, s)
}
