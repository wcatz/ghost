package memory

// #1031's schema: a one-row counter and the triggers that bump it, delivered by
// a frozen migration step.

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// vectorEpochTriggers are the six triggers of the v24 step, in the order
// migrateV24 creates them. The fresh-vs-v24 comparison iterates exactly these,
// so migrateV25's validity trigger — which a fresh database also has — does not
// enter a comparison it was never part of.
var vectorEpochTriggers = []string{
	"vector_epoch_emb_ai", "vector_epoch_emb_au", "vector_epoch_emb_ad",
	"vector_epoch_mem_au", "vector_epoch_mem_ad", "vector_epoch_mem_ai",
}

// allVectorEpochTriggers is every trigger of the current schema, v24's six plus
// migrateV25's validity arm. dropVectorEpoch iterates this, so shaping a
// database back to a pre-vector-epoch build removes the validity trigger too —
// otherwise a "v23" fixture would still bump its epoch on a validity edit and
// the upgrade test would pass for the wrong reason.
var allVectorEpochTriggers = append(append([]string{}, vectorEpochTriggers...), "vector_epoch_mem_validity_au")

// dropVectorEpoch removes the counter and its triggers, leaving the database as
// a v23 build would have.
func dropVectorEpoch(t *testing.T, db *sql.DB) {
	t.Helper()
	for _, trg := range allVectorEpochTriggers {
		if _, err := db.Exec(`DROP TRIGGER IF EXISTS ` + trg); err != nil {
			t.Fatalf("drop %s: %v", trg, err)
		}
	}
	if _, err := db.Exec(`DROP TABLE IF EXISTS vector_epoch`); err != nil {
		t.Fatalf("drop vector_epoch: %v", err)
	}
}

func triggerSQL(t *testing.T, db *sql.DB, name string) string {
	t.Helper()
	var ddl string
	if err := db.QueryRow(`SELECT sql FROM sqlite_master WHERE type='trigger' AND name=?`, name).Scan(&ddl); err != nil {
		t.Fatalf("trigger %s is missing: %v", name, err)
	}
	return strings.Join(strings.Fields(ddl), " ")
}

func rawEpoch(t *testing.T, db *sql.DB) int64 {
	t.Helper()
	var e int64
	if err := db.QueryRow(`SELECT COALESCE((SELECT epoch FROM vector_epoch WHERE id = 1), 0)`).Scan(&e); err != nil {
		t.Fatalf("read vector_epoch: %v", err)
	}
	return e
}

// seedOneEmbeddedMemory writes a project, a memory and (when withVector) its
// embedding through raw SQL, so the triggers are what any bump is attributed to.
func seedOneEmbeddedMemory(t *testing.T, db *sql.DB, withVector bool) {
	t.Helper()
	for _, s := range []string{
		`INSERT INTO projects (id, path, name) VALUES ('p1', '/tmp/epoch', 'p1')`,
		`INSERT INTO memories (id, project_id, category, content, source) VALUES ('M1', 'p1', 'fact', 'c', 'mcp')`,
	} {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("seed (%s): %v", s, err)
		}
	}
	if withVector {
		if _, err := db.Exec(`INSERT INTO memory_embeddings (memory_id, embedding, model) VALUES ('M1', x'00000000', 'm')`); err != nil {
			t.Fatalf("seed embedding: %v", err)
		}
	}
}

// assertEpochTriggersFire exercises every trigger once, and one write that must
// NOT move the counter, against a database that has M1 embedded.
func assertEpochTriggersFire(t *testing.T, db *sql.DB) {
	t.Helper()
	steps := []struct {
		name string
		sql  string
		bump bool
	}{
		{"embedding update", `UPDATE memory_embeddings SET embedding = x'01000000' WHERE memory_id = 'M1'`, true},
		{"embedding rewritten to the same bytes", `UPDATE memory_embeddings SET embedding = x'01000000' WHERE memory_id = 'M1'`, false},
		{"resolve", `UPDATE memories SET resolved_at = datetime('now') WHERE id = 'M1'`, true},
		{"scope", `UPDATE memories SET scope = '{"a":"b"}' WHERE id = 'M1'`, true},
		{"project", `INSERT INTO projects (id, path, name) VALUES ('p2', '/tmp/epoch2', 'p2')`, false},
		{"project move", `UPDATE memories SET project_id = 'p2' WHERE id = 'M1'`, true},
		{"access count", `UPDATE memories SET access_count = access_count + 1 WHERE id = 'M1'`, false},
		{"tags", `UPDATE memories SET tags = '["x"]', resolved_at = resolved_at WHERE id = 'M1'`, false},
		{"embedding delete", `DELETE FROM memory_embeddings WHERE memory_id = 'M1'`, true},
		{"embedding insert", `INSERT INTO memory_embeddings (memory_id, embedding, model) VALUES ('M1', x'02000000', 'm')`, true},
		{"memory delete", `DELETE FROM memories WHERE id = 'M1'`, true},
	}
	for _, st := range steps {
		before := rawEpoch(t, db)
		if _, err := db.Exec(st.sql); err != nil {
			t.Fatalf("%s: %v", st.name, err)
		}
		if moved := rawEpoch(t, db) > before; moved != st.bump {
			t.Errorf("%s: vector_epoch moved = %v, want %v", st.name, moved, st.bump)
		}
	}
}

// The migration path and the fresh-database path (initSQL, then vectorEpochSQL)
// produce the same counter and the same triggers. The step is run directly for the reason
// TestMigrateV23MatchesTheFreshDatabaseSchema gives: OpenDB runs initSQL first,
// which would put the objects back before the step could be seen to do it.
func TestMigrateV24MatchesTheFreshDatabaseSchema(t *testing.T) {
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
		if _, err := db.Exec(vectorEpochSQL); err != nil {
			t.Fatalf("vectorEpochSQL: %v", err)
		}
		return db
	}
	fresh, migrated := newDB(t), newDB(t)
	dropVectorEpoch(t, migrated)
	tx, err := migrated.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := migrateV24(tx); err != nil {
		_ = tx.Rollback()
		t.Fatalf("migrateV24: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	freshCols, err := columnShapes(t, fresh, "vector_epoch")
	if err != nil || len(freshCols) == 0 {
		t.Fatalf("a fresh database has no vector_epoch (%v, %v)", freshCols, err)
	}
	migratedCols, err := columnShapes(t, migrated, "vector_epoch")
	if err != nil {
		t.Fatal(err)
	}
	if diff := columnShapeDiff(freshCols, migratedCols); diff != "" {
		t.Errorf("the migration path and initSQL disagree on the table: %s", diff)
	}
	if got := rawEpoch(t, migrated); got != 0 {
		t.Errorf("the migrated counter starts at %d, want 0", got)
	}
	var rowsBefore int
	if err := migrated.QueryRow(`SELECT count(*) FROM vector_epoch`).Scan(&rowsBefore); err != nil || rowsBefore != 0 {
		t.Errorf("the step wrote %d rows into the counter (%v): the first bump creates it, so opening a database writes nothing", rowsBefore, err)
	}
	for _, trg := range vectorEpochTriggers {
		if f, m := triggerSQL(t, fresh, trg), triggerSQL(t, migrated, trg); f != m {
			t.Errorf("trigger %s differs between the paths:\n fresh    %s\n migrated %s", trg, f, m)
		}
	}
	// The migrated database's triggers do the work, not only exist.
	seedOneEmbeddedMemory(t, migrated, true)
	assertEpochTriggersFire(t, migrated)
}

// An old-schema database upgrades: the counter exists, the triggers fire, and
// the data it already held is untouched. The database is shaped as a v23 build
// left it (the table and triggers dropped, the stamp set back), which the
// migration step then brings forward.
func TestOpenDBUpgradesAV23DatabaseToTheVectorEpoch(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "v23.db")
	seed, err := OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	seedOneEmbeddedMemory(t, seed, true)
	dropVectorEpoch(t, seed)
	if _, err := seed.Exec(`PRAGMA user_version = 23`); err != nil {
		t.Fatal(err)
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}

	db, err := OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB on a v23 database: %v", err)
	}
	defer db.Close() //nolint:errcheck
	if v := schemaVersionOf(t, db); v != schemaVersion || v < 24 {
		t.Errorf("user_version = %d, want %d (at least 24)", v, schemaVersion)
	}
	if got := rawEpoch(t, db); got != 0 {
		t.Errorf("the upgraded counter = %d, want 0", got)
	}
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM memory_embeddings WHERE memory_id = 'M1'`).Scan(&n); err != nil || n != 1 {
		t.Errorf("the embedding the old database held is gone (%d, %v)", n, err)
	}
	// The v25 arm before assertEpochTriggersFire, which deletes M1 as its last
	// step: a validity update afterwards would touch no row and prove nothing.
	assertValidityEpochTriggerFires(t, db)
	assertEpochTriggersFire(t, db)

	// Reopening is idempotent and does not reset the counter.
	after := rawEpoch(t, db)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	again, err := OpenDB(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close() //nolint:errcheck
	if got := rawEpoch(t, again); got != after {
		t.Errorf("reopening changed the counter: %d -> %d", after, got)
	}
}

// A database stamped newer than this build is still refused, and the refusal
// does not put the counter back: a refusal that has already written is not one.
func TestOpenDBRefusesANewerSchemaWithoutRecreatingTheVectorEpoch(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "newer.db")
	seed, err := OpenDB(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	dropVectorEpoch(t, seed)
	if _, err := seed.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, schemaVersion+1)); err != nil {
		t.Fatal(err)
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := OpenDB(dbPath); err == nil || !strings.Contains(err.Error(), "newer") {
		t.Fatalf("OpenDB error = %v, want the newer-schema refusal", err)
	}
	ro, err := OpenReadDB(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close() //nolint:errcheck
	var n int
	if err := ro.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name = 'vector_epoch' OR name LIKE 'vector_epoch_%'`).Scan(&n); err != nil || n != 0 {
		t.Errorf("a refused open recreated %d vector_epoch objects (%v)", n, err)
	}
}

// A database opened read-only without the counter (a schema older than v24 that
// a preview cannot migrate) is not an error for the cache: it is "do not cache",
// and the search is answered from the rows as before.
func TestVectorSearchWorksWithoutTheEpochCounter(t *testing.T) {
	s, _ := cacheStore(t)
	cacheCorpus(t, s)
	dropVectorEpoch(t, s.db)
	if _, ok := readVectorEpoch(t.Context(), s.db); ok {
		t.Fatal("readVectorEpoch succeeded without the table")
	}
	res, err := s.SearchVector(t.Context(), cacheProj, cacheVec(1), 5)
	if err != nil || len(res) != 5 {
		t.Fatalf("search without the counter = %d rows, %v", len(res), err)
	}
	if _, ok := s.vcache.held(); ok {
		t.Error("a copy was cached with no counter to validate it")
	}
}
