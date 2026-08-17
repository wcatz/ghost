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
const schemaVersion = 4

// migrations[i] upgrades a database from user_version i to i+1. Each step is
// frozen in time — it must keep working against the schema as it existed when
// the step was written, so it carries its own DDL copies rather than reusing
// initSQL (which keeps moving). Steps introspect sqlite_master and skip work
// that is already done, so a hand-migrated database is stamped without harm.
var migrations = []func(*sql.Tx) error{
	migrateV1,
	migrateV2,
	migrateV3,
	migrateV4,
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

// migrateV2 adds the nullable memories.resolved_at column used by the
// resolution classifier to drop resolved-evidence memories from the ranked
// injection surface (NULL = active/unknown; set = classified resolved). A
// nullable column add needs no table rebuild — it leaves the FTS
// external-content index untouched — so this is a guarded ALTER, not the
// rebuild dance migrateV1 needed for its CHECK-constraint change.
func migrateV2(tx *sql.Tx) error {
	exists, err := columnExists(tx, "memories", "resolved_at")
	if err != nil {
		return err
	}
	if exists {
		return nil // hand-migrated DB already has the column
	}
	if _, err := tx.Exec(`ALTER TABLE memories ADD COLUMN resolved_at TEXT`); err != nil {
		return fmt.Errorf("add memories.resolved_at: %w", err)
	}
	return nil
}

// migrateV3 adds 'duplicate' to memory_links.relation's CHECK — Upsert no
// longer destructively overwrites content on a near-duplicate match; it links
// the new row to the existing one instead, and the CHECK must accept that
// relation value. SQLite cannot ALTER a CHECK constraint, so this is a table
// rebuild like migrateV1's, but simpler: memory_links carries no FTS index or
// rowid dependency, so it's a straight copy under the new DDL. A database that
// never had memory_links at all (pre-dates the link graph entirely) needs no
// rebuild — initSQL's CREATE TABLE IF NOT EXISTS will have already created it
// fresh with this CHECK, same as migrateV1's pattern for tables that don't
// exist yet.
func migrateV3(tx *sql.Tx) error {
	stale, err := tableDDLLacks(tx, "memory_links", "'duplicate'")
	if err != nil {
		return err
	}
	if !stale {
		return nil
	}
	stmts := []string{
		`CREATE TABLE memory_links_v3_new (
    source_id      TEXT NOT NULL REFERENCES memories(id) ON DELETE CASCADE,
    target_id      TEXT NOT NULL REFERENCES memories(id) ON DELETE CASCADE,
    relation       TEXT NOT NULL DEFAULT 'related'
                   CHECK (relation IN ('related', 'supersedes', 'contradicts', 'elaborates', 'causes', 'duplicate')),
    strength       REAL NOT NULL DEFAULT 0.5,
    source         TEXT NOT NULL DEFAULT 'auto'
                   CHECK (source IN ('auto', 'llm', 'manual')),
    created_at     TEXT NOT NULL DEFAULT (datetime('now')),
    invalidated_at TEXT,
    PRIMARY KEY (source_id, target_id, relation)
)`,
		`INSERT INTO memory_links_v3_new (source_id, target_id, relation, strength, source, created_at, invalidated_at)
SELECT source_id, target_id, relation, strength, source, created_at, invalidated_at
FROM memory_links`,
		`DROP TABLE memory_links`,
		`ALTER TABLE memory_links_v3_new RENAME TO memory_links`,
		`CREATE INDEX IF NOT EXISTS idx_links_source ON memory_links(source_id) WHERE invalidated_at IS NULL`,
		`CREATE INDEX IF NOT EXISTS idx_links_target ON memory_links(target_id) WHERE invalidated_at IS NULL`,
	}
	for _, s := range stmts {
		if _, err := tx.Exec(s); err != nil {
			return fmt.Errorf("%q: %w", s[:min(40, len(s))], err)
		}
	}
	return nil
}

// migrateV4 narrows memories_au to only fire when an UPDATE actually changes
// content. The trigger previously ran unconditionally on every UPDATE —
// including SET resolved_at, SET pinned, SET access_count — none of which
// touch content, so it did a harmless but wasteful FTS delete+re-insert on
// every such write (#286). SQLite has no ALTER TRIGGER, so this drops and
// recreates memories_au with a WHEN guard; unlike migrateV1/migrateV3 this
// needs no table rebuild — a trigger carries no rows or FK dependencies of
// its own, so replacing it is a plain DROP+CREATE.
func migrateV4(tx *sql.Tx) error {
	stale, err := triggerDDLLacks(tx, "memories_au", "old.content != new.content")
	if err != nil {
		return err
	}
	if !stale {
		return nil // hand-migrated DB (or a fresh initSQL create) already has the guard
	}
	stmts := []string{
		`DROP TRIGGER IF EXISTS memories_au`,
		`CREATE TRIGGER memories_au AFTER UPDATE ON memories WHEN old.content != new.content BEGIN
    INSERT INTO memories_fts(memories_fts, rowid, content) VALUES('delete', old.rowid, old.content);
    INSERT INTO memories_fts(rowid, content) VALUES (new.rowid, new.content);
END`,
	}
	for _, s := range stmts {
		if _, err := tx.Exec(s); err != nil {
			return fmt.Errorf("%q: %w", s[:min(40, len(s))], err)
		}
	}
	return nil
}

// columnExists reports whether table has a column named column, matching
// case-insensitively — SQLite itself treats column identifiers as
// case-insensitive, so a hand-migrated RESOLVED_AT column must be recognized
// as the same column resolved_at names, not trigger a duplicate ALTER that
// SQLite would then reject.
func columnExists(tx *sql.Tx, table, column string) (bool, error) {
	rows, err := tx.Query(fmt.Sprintf(`PRAGMA table_info(%s)`, table))
	if err != nil {
		return false, fmt.Errorf("read %s columns: %w", table, err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt any
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return false, fmt.Errorf("scan %s column info: %w", table, err)
		}
		if strings.EqualFold(name, column) {
			return true, nil
		}
	}
	return false, rows.Err()
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

// triggerDDLLacks reports whether the named trigger exists and its stored DDL
// does NOT contain marker — mirrors tableDDLLacks but queries sqlite_master
// for a trigger instead of a table. A missing trigger needs no recreate here:
// initSQL's CREATE TRIGGER IF NOT EXISTS will already have created it in
// current form before migrate() ever runs.
func triggerDDLLacks(tx *sql.Tx, trigger, marker string) (bool, error) {
	var ddl string
	err := tx.QueryRow(
		`SELECT sql FROM sqlite_master WHERE type='trigger' AND name=?`, trigger,
	).Scan(&ddl)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read %s DDL: %w", trigger, err)
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
