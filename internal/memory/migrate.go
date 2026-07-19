package memory

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// schemaVersion is the current schema version, stamped into PRAGMA user_version.
// Bump it and append to migrations whenever initSQL changes in a way that
// CREATE TABLE IF NOT EXISTS cannot deliver to existing databases (new columns,
// CHECK values, foreign keys, dropped tables).
const schemaVersion = 1

// migrations[i] upgrades a database from user_version i to i+1. Each step is
// frozen in time — it must keep working against the schema as it existed when
// the step was written, so it carries its own DDL copies rather than reusing
// initSQL (which keeps moving). Steps introspect sqlite_master and skip work
// that is already done, so a hand-migrated database is stamped without harm.
var migrations = []func(*sql.Tx) error{
	migrateV1,
}

// migrate brings an existing database up to schemaVersion. Fresh databases
// (detected by the caller) are stamped directly and never pass through here.
// Table rebuilds require foreign_keys=OFF, which is a no-op inside a
// transaction — so the pragma is toggled on a pinned connection around each
// step, and foreign_key_check runs before the version stamp is committed.
func migrate(db *sql.DB, from int) error {
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("pin connection: %w", err)
	}
	defer conn.Close() //nolint:errcheck

	for v := from; v < schemaVersion; v++ {
		if _, err := conn.ExecContext(ctx, "PRAGMA foreign_keys=OFF"); err != nil {
			return fmt.Errorf("migration %d: foreign_keys off: %w", v+1, err)
		}
		err := func() error {
			tx, err := conn.BeginTx(ctx, nil)
			if err != nil {
				return fmt.Errorf("begin: %w", err)
			}
			defer tx.Rollback() //nolint:errcheck
			if err := migrations[v](tx); err != nil {
				return err
			}
			var table, rowid, parent, fkid any
			if err := tx.QueryRow("PRAGMA foreign_key_check").Scan(&table, &rowid, &parent, &fkid); err != sql.ErrNoRows {
				return fmt.Errorf("foreign_key_check failed: table=%v rowid=%v parent=%v (%v)", table, rowid, parent, err)
			}
			if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", v+1)); err != nil {
				return fmt.Errorf("stamp user_version: %w", err)
			}
			return tx.Commit()
		}()
		if _, ferr := conn.ExecContext(ctx, "PRAGMA foreign_keys=ON"); ferr != nil && err == nil {
			err = fmt.Errorf("foreign_keys on: %w", ferr)
		}
		if err != nil {
			return fmt.Errorf("migration %d: %w", v+1, err)
		}
	}
	return nil
}

// migrateV1 fixes drift accumulated before versioning existed:
//  1. memories.source CHECK gains 'onboarding' and 'decision_log' — without
//     them every decision-log memory mirror insert fails the constraint.
//  2. memory_snapshots.project_id gains REFERENCES projects(id) ON DELETE CASCADE.
//  3. Pre-v0.8.0 assistant-era tables (notifications, reminders, scheduled_jobs)
//     are dropped.
//
// SQLite cannot ALTER a CHECK constraint or add a foreign key, so both fixes
// are full table rebuilds. The memories rebuild preserves rowids (the FTS
// external-content index is keyed on them) and rebuilds the FTS index after.
func migrateV1(tx *sql.Tx) error {
	stale, err := tableDDLLacks(tx, "memories", "'decision_log'")
	if err != nil {
		return err
	}
	if stale {
		if err := rebuildMemoriesV1(tx); err != nil {
			return fmt.Errorf("rebuild memories: %w", err)
		}
	}

	stale, err = tableDDLLacks(tx, "memory_snapshots", "REFERENCES projects")
	if err != nil {
		return err
	}
	if stale {
		if err := rebuildSnapshotsV1(tx); err != nil {
			return fmt.Errorf("rebuild memory_snapshots: %w", err)
		}
	}

	for _, t := range []string{"notifications", "reminders", "scheduled_jobs"} {
		if _, err := tx.Exec("DROP TABLE IF EXISTS " + t); err != nil {
			return fmt.Errorf("drop %s: %w", t, err)
		}
	}
	return nil
}

// tableDDLLacks reports whether the named table exists and its stored DDL does
// NOT contain marker — i.e. the table needs rebuilding. A missing table needs
// no rebuild: initSQL has already created it in current form.
func tableDDLLacks(tx *sql.Tx, table, marker string) (bool, error) {
	var ddl string
	err := tx.QueryRow(
		`SELECT sql FROM sqlite_master WHERE type='table' AND name=?`, table,
	).Scan(&ddl)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read %s DDL: %w", table, err)
	}
	return !strings.Contains(ddl, marker), nil
}

func rebuildMemoriesV1(tx *sql.Tx) error {
	stmts := []string{
		`DROP TRIGGER IF EXISTS memories_ai`,
		`DROP TRIGGER IF EXISTS memories_ad`,
		`DROP TRIGGER IF EXISTS memories_au`,
		`DROP TABLE IF EXISTS memories_fts`,
		`CREATE TABLE memories_v1_new (
    id            TEXT PRIMARY KEY DEFAULT (hex(randomblob(16))),
    project_id    TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    category      TEXT NOT NULL DEFAULT 'fact'
                  CHECK (category IN (
                      'architecture', 'decision', 'pattern', 'convention',
                      'gotcha', 'dependency', 'preference', 'fact'
                  )),
    content       TEXT NOT NULL,
    importance    REAL NOT NULL DEFAULT 0.5,
    access_count  INTEGER NOT NULL DEFAULT 0,
    last_accessed TEXT,
    source        TEXT NOT NULL DEFAULT 'reflection'
                  CHECK (source IN ('reflection', 'chat', 'manual', 'tool', 'mcp', 'onboarding', 'decision_log')),
    tags          TEXT DEFAULT '[]',
    pinned        INTEGER NOT NULL DEFAULT 0,
    created_at    TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at    TEXT NOT NULL DEFAULT (datetime('now'))
)`,
		`INSERT INTO memories_v1_new (rowid, id, project_id, category, content, importance,
    access_count, last_accessed, source, tags, pinned, created_at, updated_at)
SELECT rowid, id, project_id, category, content, importance,
    access_count, last_accessed, source, tags, pinned, created_at, updated_at
FROM memories`,
		`DROP TABLE memories`,
		`ALTER TABLE memories_v1_new RENAME TO memories`,
		`CREATE VIRTUAL TABLE memories_fts USING fts5(
    content,
    content=memories,
    content_rowid=rowid,
    tokenize='porter unicode61'
)`,
		`CREATE TRIGGER memories_ai AFTER INSERT ON memories BEGIN
    INSERT INTO memories_fts(rowid, content) VALUES (new.rowid, new.content);
END`,
		`CREATE TRIGGER memories_ad AFTER DELETE ON memories BEGIN
    INSERT INTO memories_fts(memories_fts, rowid, content) VALUES('delete', old.rowid, old.content);
END`,
		`CREATE TRIGGER memories_au AFTER UPDATE ON memories BEGIN
    INSERT INTO memories_fts(memories_fts, rowid, content) VALUES('delete', old.rowid, old.content);
    INSERT INTO memories_fts(rowid, content) VALUES (new.rowid, new.content);
END`,
		`INSERT INTO memories_fts(memories_fts) VALUES('rebuild')`,
		`CREATE INDEX IF NOT EXISTS idx_memories_project_cat ON memories(project_id, category)`,
		`CREATE INDEX IF NOT EXISTS idx_memories_project_imp ON memories(project_id, importance DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_memories_project_source ON memories(project_id, source)`,
	}
	for _, s := range stmts {
		if _, err := tx.Exec(s); err != nil {
			return fmt.Errorf("%q: %w", s[:min(40, len(s))], err)
		}
	}
	return nil
}

func rebuildSnapshotsV1(tx *sql.Tx) error {
	stmts := []string{
		`CREATE TABLE memory_snapshots_v1_new (
    id            TEXT PRIMARY KEY DEFAULT (hex(randomblob(16))),
    snapshot_id   TEXT NOT NULL,
    project_id    TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    category      TEXT NOT NULL,
    content       TEXT NOT NULL,
    importance    REAL NOT NULL,
    source        TEXT NOT NULL,
    tags          TEXT DEFAULT '[]',
    created_at    TEXT NOT NULL DEFAULT (datetime('now'))
)`,
		`INSERT INTO memory_snapshots_v1_new (id, snapshot_id, project_id, category,
    content, importance, source, tags, created_at)
SELECT id, snapshot_id, project_id, category,
    content, importance, source, tags, created_at
FROM memory_snapshots`,
		`DROP TABLE memory_snapshots`,
		`ALTER TABLE memory_snapshots_v1_new RENAME TO memory_snapshots`,
		`CREATE INDEX IF NOT EXISTS idx_snapshots_project ON memory_snapshots(project_id, snapshot_id)`,
	}
	for _, s := range stmts {
		if _, err := tx.Exec(s); err != nil {
			return fmt.Errorf("%q: %w", s[:min(40, len(s))], err)
		}
	}
	return nil
}
