package memory

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// indexSQL returns the stored DDL for a named index.
func indexSQL(t *testing.T, db *sql.DB, name string) string {
	t.Helper()
	var sqlStr string
	err := db.QueryRow(
		`SELECT sql FROM sqlite_master WHERE type='index' AND name=?`, name).Scan(&sqlStr)
	if err != nil {
		t.Fatalf("read index %s: %v", name, err)
	}
	return sqlStr
}

// TestOpenDBMigratesAV26StoreToTheTieOrderedIndexes: a v26 store holding data
// is migrated to v27 and gains both tie-ordered indexes; running the migration
// a second time is harmless (idempotent).
func TestOpenDBMigratesAV26StoreToTheTieOrderedIndexes(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "v26.db")

	// Build a v26-shaped store: take a fresh database, drop the widened indexes,
	// recreate the narrow one that a v26 build shipped, stamp it as v26, and add
	// a data row so the migration has something to preserve.
	seed, err := OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB (seed): %v", err)
	}
	if _, err := seed.Exec(`DROP INDEX IF EXISTS idx_memories_project_imp`); err != nil {
		t.Fatalf("drop idx: %v", err)
	}
	if _, err := seed.Exec(`DROP INDEX IF EXISTS idx_memories_project_imp_updated`); err != nil {
		t.Fatalf("drop idx updated: %v", err)
	}
	if _, err := seed.Exec(`CREATE INDEX idx_memories_project_imp ON memories(project_id, importance DESC)`); err != nil {
		t.Fatalf("create narrow idx: %v", err)
	}
	if _, err := seed.Exec(`INSERT INTO projects (id, name, path) VALUES ('proj','proj','/tmp/proj')`); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	if _, err := seed.Exec(`INSERT INTO memories (id, project_id, category, content, source, importance, pinned, created_at, updated_at) VALUES ('m1','proj','fact','x','manual',0.7,0,'2024-01-01 00:00:00','2024-01-01 00:00:00')`); err != nil {
		t.Fatalf("insert data: %v", err)
	}
	if _, err := seed.Exec(`PRAGMA user_version = 26`); err != nil {
		t.Fatalf("stamp v26: %v", err)
	}
	_ = seed.Close()

	// Migrate to v27.
	db, err := OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB (migrate): %v", err)
	}
	defer db.Close()
	var v int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		t.Fatalf("read user_version: %v", err)
	}
	if v != schemaVersion {
		t.Fatalf("user_version = %d, want %d", v, schemaVersion)
	}
	// both indexes present with the tie-break id column.
	for _, name := range []string{"idx_memories_project_imp", "idx_memories_project_imp_updated"} {
		sqlStr := indexSQL(t, db, name)
		want := "created_at DESC, id"
		if name == "idx_memories_project_imp_updated" {
			want = "updated_at DESC, id"
		}
		if !strings.Contains(sqlStr, want) {
			t.Errorf("index %s missing id tie-break %q: %s", name, want, sqlStr)
		}
	}
	// data preserved.
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM memories WHERE id='m1'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("data not preserved: n=%d err=%v", n, err)
	}
	// keep db open for the idempotency check

	// Run the migration a second time directly, which is what "harmless to run
	// twice" means: migrateV27 must be idempotent on an already-v27 database.
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin second migration: %v", err)
	}
	if err := migrateV27(tx); err != nil {
		_ = tx.Rollback()
		t.Fatalf("migrateV27 (second): %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit second migration: %v", err)
	}
	db3 := db
	var v3 int
	if err := db3.QueryRow(`PRAGMA user_version`).Scan(&v3); err != nil || v3 != schemaVersion {
		t.Fatalf("user_version after second migration = %d, want %d (err=%v)", v3, schemaVersion, err)
	}
	for _, name := range []string{"idx_memories_project_imp", "idx_memories_project_imp_updated"} {
		sqlStr := indexSQL(t, db3, name)
		want := "created_at DESC, id"
		if name == "idx_memories_project_imp_updated" {
			want = "updated_at DESC, id"
		}
		if !strings.Contains(sqlStr, want) {
			t.Errorf("index %s missing id tie-break %q after second migration: %s", name, want, sqlStr)
		}
	}
}
