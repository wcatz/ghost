package memory

// #648 slice 2's schema: a new append-only table, a frozen migration step, and
// the two ways a same-named table that is not Ghost's would otherwise be
// silently adopted.

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// TestMigrateV23MatchesTheFreshDatabaseSchema: the migration path and the initSQL
// path must produce the same table, or a store that reached v23 by upgrading
// holds a different shape from one created fresh at the same version.
//
// Both spellings of the DDL exist — initSQL cannot deliver a new table to a
// database that already exists — so nothing but this comparison keeps them in
// step. The step is RUN DIRECTLY rather than reached through OpenDB, for the
// reason TestMigrateV20MatchesTheFreshDatabaseSchema states: OpenDB executes
// initSQL BEFORE the migrations, so a database that had the table dropped would
// have it back again by the time the step ran and the step would be a no-op.
func TestMigrateV23MatchesTheFreshDatabaseSchema(t *testing.T) {
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
	if _, err := migrated.Exec(`DROP TABLE IF EXISTS memory_flags`); err != nil {
		t.Fatalf("drop the table so the step has work to do: %v", err)
	}
	tx, err := migrated.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := migrateV23(tx); err != nil {
		_ = tx.Rollback()
		t.Fatalf("migrateV23: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	freshCols, err := columnShapes(t, fresh, "memory_flags")
	if err != nil {
		t.Fatalf("read the fresh table: %v", err)
	}
	if len(freshCols) == 0 {
		t.Fatal("a fresh database has no memory_flags — the initSQL path is missing the table")
	}
	migratedCols, err := columnShapes(t, migrated, "memory_flags")
	if err != nil {
		t.Fatalf("read the migrated table: %v", err)
	}
	if diff := columnShapeDiff(freshCols, migratedCols); diff != "" {
		t.Errorf("the migration path and initSQL disagree on the table: %s", diff)
	}
	// And the index: a migrated store without it reads the whole table for the
	// one query that filters by project.
	var idx string
	if err := migrated.QueryRow(
		`SELECT name FROM sqlite_master WHERE type='index' AND name='idx_memory_flags_project'`,
	).Scan(&idx); err != nil {
		t.Errorf("migrateV23 did not create the project index: %v", err)
	}
}

// TestTheFlagTableRefusesWhatTheWriterRefuses: the table's own constraints, on a
// FRESH database.
//
// columnShapes does not compare CHECKs — pragma_table_info reports a column's
// type, nullability and default, not its predicate — so the two constraints this
// table carries are probed by writing against them. Both are belt to the
// writer's braces: FlagMemory refuses an unknown kind and a reason that is not
// text, and a raw INSERT (an import, a future writer) must not be able to file a
// claim nobody can read back as one.
func TestTheFlagTableRefusesWhatTheWriterRefuses(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "ghost.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(initSQL); err != nil {
		t.Fatalf("initSQL: %v", err)
	}
	for _, s := range []string{
		`INSERT INTO projects (id, path, name) VALUES ('p1', '/tmp/flags-shape', 'p1')`,
		`INSERT INTO memories (id, project_id, category, content, source)
		 VALUES ('M1', 'p1', 'fact', 'the claim', 'mcp')`,
	} {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("seed (%s): %v", s, err)
		}
	}

	const okFlag = `INSERT INTO memory_flags (project_id, memory_id, kind, reason, content_hash, agent, session_id)
		VALUES ('p1', 'M1', 'stale', 'the queue moved', ?, 'codex', 'ses_1')`
	hash := ContentHash("the claim")

	// The two shapes the table must take.
	if _, err := db.Exec(okFlag, hash); err != nil {
		t.Errorf("a well-formed flag was refused by the table: %v", err)
	}
	if _, err := db.Exec(okFlag, ""); err != nil {
		t.Errorf("a flag with an empty content_hash was refused: %v — '' is the value a writer that has no "+
			"hash to stamp would produce, and the reader has a named rule for it", err)
	}

	// The kind vocabulary, which is what makes "wrong" and "stale" the two
	// things an agent can say rather than two of infinitely many.
	for _, tc := range []struct{ name, kind string }{
		{"an unknown kind", "useful"},
		{"an empty kind", ""},
	} {
		if _, err := db.Exec(
			`INSERT INTO memory_flags (project_id, memory_id, kind, reason, content_hash)
			 VALUES ('p1', 'M1', ?, 'reason', '')`, tc.kind,
		); err == nil {
			t.Errorf("the table accepted %s (%q); the CHECK on kind is not there", tc.name, tc.kind)
		}
	}

	// The hash shape: 64 hex characters or empty, never a partial digest. A
	// truncated stamp would match nothing and quietly withdraw every flag on the
	// row, which is the failure the column exists to avoid.
	if _, err := db.Exec(okFlag, "abc123"); err == nil {
		t.Error("the table accepted a truncated content_hash, so a stamp that matches nothing is a legal row")
	}
}

// TestMigrateV23RefusesAForeignFlagsTable: the guard that stops the step
// stamping v23 over a table somebody else owns.
//
// `CREATE TABLE IF NOT EXISTS` is a silent no-op against an existing table, so
// without this the step would report success over a shape it cannot write, and
// the failure would arrive later as "no such column: reason" on the first flag
// an agent filed. The remedy is named rather than logged because the condition
// is permanent: nothing converts another project's table into Ghost's.
func TestMigrateV23RefusesAForeignFlagsTable(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "ghost.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() }) //nolint:errcheck

	if _, err := db.Exec(initSQL); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	if _, err := db.Exec(`DROP TABLE memory_flags`); err != nil {
		t.Fatalf("drop memory_flags: %v", err)
	}
	// Somebody else's table of the same name: it carries project_id (so the index
	// a foreign table without it would break on is not the thing being tested
	// here) but none of the columns this feature writes.
	if _, err := db.Exec(`
		CREATE TABLE memory_flags (
			project_id TEXT NOT NULL,
			note       TEXT NOT NULL
		)`); err != nil {
		t.Fatalf("create the foreign table: %v", err)
	}
	for _, s := range []string{
		`INSERT INTO projects (id, path, name) VALUES ('p1', '/tmp/v23-foreign', 'p1')`,
		`PRAGMA user_version = 22`,
	} {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("seed (%s): %v", s, err)
		}
	}

	err = migrate(db, 22)
	if err == nil {
		t.Fatal("migrate accepted a memory_flags table that is not Ghost's flag table")
	}
	msg := err.Error()
	for _, want := range []string{"memory_flags", "kind", "DROP TABLE memory_flags"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error = %q, want it to name %q", msg, want)
		}
	}
	// A refusal is a refusal: the version is not stamped, so the store is exactly
	// as recoverable as it was before the upgrade was attempted.
	if v := schemaVersionOf(t, db); v != 22 {
		t.Errorf("user_version = %d after a refused migration, want 22", v)
	}
}

// TestOpenDBRefusesAForeignFlagsTableBeforeItsIndexRuns: the SAME guard at the
// other call site, and the reason it has to be there.
//
// initSQL's `CREATE INDEX ... ON memory_flags(project_id)` is the statement that
// fails against a foreign table without that column, and it fails with "no such
// column: project_id" — an error that names neither the table nor the way out.
// The guard runs BEFORE the DDL and therefore before the pre-migration backup,
// for the reason OpenDB gives for the two guards beside it: the condition is
// permanent, so an open that had already written a copy would write one again on
// every retry.
func TestOpenDBRefusesAForeignFlagsTableBeforeItsIndexRuns(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "ghost.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	// No project_id column: this is the shape whose failure is initSQL's index
	// rather than the step's create.
	if _, err := db.Exec(`CREATE TABLE memory_flags (note TEXT NOT NULL)`); err != nil {
		t.Fatalf("create the foreign table: %v", err)
	}
	if _, err := db.Exec(`PRAGMA user_version = 22`); err != nil {
		t.Fatalf("stamp the version: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	opened, err := OpenDB(dbPath)
	if err == nil {
		_ = opened.Close()
		t.Fatal("OpenDB adopted a memory_flags table that is not Ghost's flag table")
	}
	msg := err.Error()
	for _, want := range []string{"memory_flags", "DROP TABLE memory_flags"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error = %q, want it to name %q — the refusal must say which table and how to clear it, "+
				"not surface initSQL's index error", msg, want)
		}
	}
}
