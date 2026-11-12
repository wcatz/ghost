package memory

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
)

const schemaVersion = 31

func SchemaVersion() int { return schemaVersion }

// DBUserVersion reads PRAGMA user_version off an open database, which is the
// schema version a store is actually at.
func DBUserVersion(db *sql.DB) (int, error) { return dbUserVersion(db) }

func dbUserVersion(q Queryer) (int, error) {
	rows, err := q.QueryContext(context.Background(), `PRAGMA user_version`)
	if err != nil {
		return 0, fmt.Errorf("read schema version: %w", err)
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return 0, fmt.Errorf("read schema version: %w", err)
		}
		return 0, errors.New("read schema version: PRAGMA user_version returned no row")
	}
	var v int
	if err := rows.Scan(&v); err != nil {
		return 0, fmt.Errorf("read schema version: %w", err)
	}
	return v, nil
}

var migrations = []func(*sql.Tx) error{
	migrateV1,
	migrateV2,
	migrateV3,
	migrateV4,
	migrateV5,
	migrateV6,
	migrateV7,
	migrateV8,
	migrateV9,
	migrateV10,
	migrateV11,
	migrateV12,
	migrateV13,
	migrateV14,
	migrateV15,
	migrateV16,
	migrateV17,
	migrateV18,
	migrateV19,
	migrateV20,
	migrateV21,
	migrateV22,
	migrateV23,
	migrateV24,
	migrateV25,
	migrateV26,
	migrateV27,
	migrateV28,
	migrateV29,
	migrateV30,
	migrateV31,
}

func migrate(db *sql.DB, from int) error {
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("pin connection: %w", err)
	}
	defer conn.Close() //nolint:errcheck
	if _, err := conn.ExecContext(ctx, "PRAGMA foreign_keys=OFF"); err != nil {
		return fmt.Errorf("foreign keys off: %w", err)
	}
	if err := repairPreExistingFKOrphans(ctx, conn); err != nil {
		_, _ = conn.ExecContext(ctx, "PRAGMA foreign_keys=ON")
		return err
	}

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
			stale, err := fkViolations(tx)
			if err != nil {
				return fmt.Errorf("foreign_key_check: %w", err)
			}
			if len(stale) > 0 {
				return fmt.Errorf("migration %d introduced %d foreign-key violation(s): %v",
					v+1, len(stale), stale)
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

var cacheFKWhitelist = map[string]bool{
	"memory_embeddings": true,
	"link_scans":        true,
	"supersede_checked": true,
	"memory_links":      true,
	"memory_provenance": true,
}

func repairPreExistingFKOrphans(ctx context.Context, conn *sql.Conn) error {
	violations, err := fkViolations(conn)
	if err != nil {
		return fmt.Errorf("foreign_key_check: %w", err)
	}
	if len(violations) == 0 {
		return nil
	}
	var blocked []string
	for table, rowids := range violations {
		if !cacheFKWhitelist[table] {
			rows := make([]string, 0, len(rowids))
			for _, rowid := range rowids {
				rows = append(rows, fmt.Sprintf("DELETE FROM %s WHERE rowid = %d;", table, rowid))
			}
			blocked = append(blocked, fmt.Sprintf("%s (%d row(s)): %s", table, len(rowids), strings.Join(rows, " ")))
			continue
		}
		for _, rowid := range rowids {
			if _, err := conn.ExecContext(ctx,
				fmt.Sprintf("DELETE FROM %s WHERE rowid = ?", table), rowid,
			); err != nil {
				return fmt.Errorf("delete orphaned %s rowid %d: %w", table, rowid, err)
			}
		}
		slog.Warn("migration: deleted pre-existing orphaned rows",
			"table", table, "rows", len(rowids))
	}
	if len(blocked) > 0 {
		return fmt.Errorf("aborting migration: pre-existing foreign-key orphans in user-content tables: %s",
			strings.Join(blocked, " "))
	}
	return nil
}

func fkViolations(run interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}) (map[string][]int64, error) {
	rows, err := run.QueryContext(context.Background(), "PRAGMA foreign_key_check")
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck
	out := map[string][]int64{}
	for rows.Next() {
		var table string
		var rowid int64
		var parent string
		var fkid int
		if err := rows.Scan(&table, &rowid, &parent, &fkid); err != nil {
			return nil, err
		}
		out[table] = append(out[table], rowid)
	}
	return out, rows.Err()
}

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

func migrateV2(tx *sql.Tx) error {
	exists, err := columnExists(tx, "memories", "resolved_at")
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	if _, err := tx.Exec(`ALTER TABLE memories ADD COLUMN resolved_at TEXT`); err != nil {
		return fmt.Errorf("add memories.resolved_at: %w", err)
	}
	return nil
}

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

func migrateV4(tx *sql.Tx) error {
	stale, err := triggerDDLLacks(tx, "memories_au", "old.content != new.content")
	if err != nil {
		return err
	}
	if !stale {
		return nil
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

func migrateV5(tx *sql.Tx) error {
	for _, t := range []string{"messages", "conversations"} {
		if _, err := tx.Exec("DROP TABLE IF EXISTS " + t); err != nil {
			return fmt.Errorf("drop %s: %w", t, err)
		}
	}
	return nil
}

func migrateV6(tx *sql.Tx) error {
	exists, err := columnExists(tx, "ghost_state", "reflect_input_sig")
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	if _, err := tx.Exec(`ALTER TABLE ghost_state ADD COLUMN reflect_input_sig TEXT NOT NULL DEFAULT ''`); err != nil {
		return fmt.Errorf("add ghost_state.reflect_input_sig: %w", err)
	}
	return nil
}

func migrateV7(tx *sql.Tx) error {
	exists, err := columnExists(tx, "memories", "resolve_kept_hash")
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	if _, err := tx.Exec(`ALTER TABLE memories ADD COLUMN resolve_kept_hash TEXT NOT NULL DEFAULT ''`); err != nil {
		return fmt.Errorf("add memories.resolve_kept_hash: %w", err)
	}
	return nil
}

func migrateV8(tx *sql.Tx) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS supersede_checked (
    newer_id    TEXT NOT NULL REFERENCES memories(id) ON DELETE CASCADE,
    older_id    TEXT NOT NULL REFERENCES memories(id) ON DELETE CASCADE,
    project_id  TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    newer_hash  TEXT NOT NULL,
    older_hash  TEXT NOT NULL,
    checked_at  TEXT NOT NULL DEFAULT (datetime('now')),
    PRIMARY KEY (newer_id, older_id)
)`,
		`CREATE INDEX IF NOT EXISTS idx_supersede_checked_project ON supersede_checked(project_id)`,
	}
	for _, s := range stmts {
		if _, err := tx.Exec(s); err != nil {
			return fmt.Errorf("%q: %w", s[:min(40, len(s))], err)
		}
	}
	return nil
}

func migrateV9(tx *sql.Tx) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS maintenance_runs (
    id                   TEXT PRIMARY KEY DEFAULT (hex(randomblob(16))),
    kind                 TEXT NOT NULL,
    recorded_at          TEXT NOT NULL DEFAULT (datetime('now')),
    scratch_bytes        INTEGER NOT NULL DEFAULT 0,
    scratch_reaped_bytes INTEGER NOT NULL DEFAULT 0,
    scratch_reaped_count INTEGER NOT NULL DEFAULT 0,
    note                 TEXT NOT NULL DEFAULT ''
)`,
		`CREATE INDEX IF NOT EXISTS idx_maintenance_runs_at ON maintenance_runs(recorded_at DESC)`,
	}
	for _, s := range stmts {
		if _, err := tx.Exec(s); err != nil {
			return fmt.Errorf("%q: %w", s[:min(40, len(s))], err)
		}
	}
	return nil
}

func migrateV10(tx *sql.Tx) error {
	for _, c := range phase1aProvenanceColumns {
		exists, err := columnExists(tx, "memories", c.name)
		if err != nil {
			return err
		}
		if exists {
			continue
		}
		q := fmt.Sprintf("ALTER TABLE memories ADD COLUMN %s %s", c.name, c.typ)
		if _, err := tx.Exec(q); err != nil {
			return fmt.Errorf("add memories.%s: %w", c.name, err)
		}
	}
	return nil
}

func migrateV11(tx *sql.Tx) error {
	exists, err := columnExists(tx, "projects", "repo_remote")
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	if _, err := tx.Exec(`ALTER TABLE projects ADD COLUMN repo_remote TEXT`); err != nil {
		return fmt.Errorf("add projects.repo_remote: %w", err)
	}
	return nil
}

func migrateV12(tx *sql.Tx) error {
	exists, err := columnExists(tx, "memories", "scope")
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	if _, err := tx.Exec(`ALTER TABLE memories ADD COLUMN scope TEXT`); err != nil {
		return fmt.Errorf("add memories.scope: %w", err)
	}
	return nil
}

func migrateV13(tx *sql.Tx) error {
	cols := []struct{ name, ddl string }{
		{"memory_id", `TEXT`},
		{"access_count", `INTEGER NOT NULL DEFAULT 0`},
		{"last_accessed", `TEXT`},
		{"agent", `TEXT`},
		{"session_id", `TEXT`},
		{"source_ref", `TEXT`},
		{"confidence", `REAL`},
		{"valid_from", `TEXT`},
		{"valid_until", `TEXT`},
		{"verified_at", `TEXT`},
	}
	for _, c := range cols {
		exists, err := columnExists(tx, "memory_snapshots", c.name)
		if err != nil {
			return err
		}
		if exists {
			continue
		}
		if _, err := tx.Exec(fmt.Sprintf(`ALTER TABLE memory_snapshots ADD COLUMN %s %s`, c.name, c.ddl)); err != nil {
			return fmt.Errorf("add memory_snapshots.%s: %w", c.name, err)
		}
	}
	return nil
}

func rebuildMemoriesV15(tx *sql.Tx) error {
	stmts := []string{
		`DROP TRIGGER IF EXISTS memories_ai`,
		`DROP TRIGGER IF EXISTS memories_ad`,
		`DROP TRIGGER IF EXISTS memories_au`,
		`DROP TABLE IF EXISTS memories_fts`,
		`CREATE TABLE memories_v14_new (
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
                  CHECK (source IN ('reflection', 'chat', 'manual', 'tool', 'mcp', 'onboarding', 'decision_log', 'builtin')),
    tags          TEXT DEFAULT '[]',
    pinned        INTEGER NOT NULL DEFAULT 0,
    created_at    TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at    TEXT NOT NULL DEFAULT (datetime('now')),
    resolved_at   TEXT,
    resolve_kept_hash TEXT NOT NULL DEFAULT '',
    valid_from    TEXT,
    valid_until   TEXT,
    verified_at   TEXT,
    agent         TEXT,
    session_id    TEXT,
    source_ref    TEXT,
    confidence    REAL,
    scope         TEXT
)`,
		`INSERT INTO memories_v14_new (
    rowid, id, project_id, category, content, importance, access_count,
    last_accessed, source, tags, pinned, created_at, updated_at, resolved_at,
    resolve_kept_hash, valid_from, valid_until, verified_at, agent, session_id,
    source_ref, confidence, scope
)
SELECT rowid, id, project_id, category, content, importance, access_count,
       last_accessed,
       CASE WHEN project_id = '_global' AND source = 'manual'
                 AND content = 'NEVER add Co-Authored-By or any AI attribution to commit messages. All commits belong to the user.'
            THEN 'builtin' ELSE source END,
       tags, pinned, created_at, updated_at, resolved_at, resolve_kept_hash,
       valid_from, valid_until, verified_at, agent, session_id, source_ref,
       confidence, scope
FROM memories`,
		`DROP TABLE memories`,
		`ALTER TABLE memories_v14_new RENAME TO memories`,
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
		`CREATE TRIGGER memories_au AFTER UPDATE ON memories WHEN old.content != new.content BEGIN
    INSERT INTO memories_fts(memories_fts, rowid, content) VALUES('delete', old.rowid, old.content);
    INSERT INTO memories_fts(rowid, content) VALUES (new.rowid, new.content);
END`,
		`INSERT INTO memories_fts(memories_fts) VALUES('rebuild')`,
		`CREATE INDEX IF NOT EXISTS idx_memories_project_cat ON memories(project_id, category)`,
		`CREATE INDEX IF NOT EXISTS idx_memories_project_imp ON memories(project_id, importance DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_memories_project_source ON memories(project_id, source)`,
	}
	for _, stmt := range stmts {
		if _, err := tx.Exec(stmt); err != nil {
			return fmt.Errorf("%q: %w", stmt[:min(40, len(stmt))], err)
		}
	}
	return nil
}

func migrateV15(tx *sql.Tx) error {
	stale, err := tableDDLLacks(tx, "memories", "'builtin'")
	if err != nil {
		return err
	}
	if stale {
		if err := rebuildMemoriesV15(tx); err != nil {
			return fmt.Errorf("rebuild memories for builtin source: %w", err)
		}
	}

	if _, err := tx.Exec(`
		UPDATE memories
		SET source = 'builtin'
		WHERE project_id = '_global'
		  AND source = 'manual'
		  AND content = 'NEVER add Co-Authored-By or any AI attribution to commit messages. All commits belong to the user.'
	`); err != nil {
		return fmt.Errorf("relabel builtin seed: %w", err)
	}
	return nil
}

func migrateV16(tx *sql.Tx) error {
	exists, err := columnExists(tx, "projects", "repo_remote")
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}
	if _, err := tx.Exec(`
		UPDATE projects SET repo_remote = NULL
		WHERE id = '_global' AND repo_remote IS NOT NULL AND repo_remote <> ''
	`); err != nil {
		return fmt.Errorf("clear _global repository identity: %w", err)
	}
	rows, err := tx.Query(`
		SELECT repo_remote, group_concat(id, '	')
		FROM projects
		WHERE repo_remote IS NOT NULL AND repo_remote <> ''
		GROUP BY repo_remote HAVING count(*) > 1
		ORDER BY repo_remote
	`)
	if err != nil {
		return fmt.Errorf("find duplicate repository identities: %w", err)
	}
	var globalRemote string
	for rows.Next() {
		var remote, ids string
		if err := rows.Scan(&remote, &ids); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan duplicate repository identity: %w", err)
		}
		for _, id := range strings.Split(ids, "	") {
			if id == "_global" {
				globalRemote = remote
			}
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("iterate duplicate repository identities: %w", err)
	}
	_ = rows.Close()
	if globalRemote != "" {
		return fmt.Errorf("%w: repository %q is recorded on the _global project as well as on a regular project; "+
			"clear _global's repo_remote before upgrading, since _global is the bucket global injection reads from and cannot be merged",
			ErrAmbiguousProject, globalRemote)
	}

	rows, err = tx.Query(`
		SELECT repo_remote FROM projects
		WHERE repo_remote IS NOT NULL AND repo_remote <> ''
		GROUP BY repo_remote HAVING count(*) > 1
		ORDER BY repo_remote
	`)
	if err != nil {
		return fmt.Errorf("find duplicate repository identities: %w", err)
	}
	var remotes []string
	for rows.Next() {
		var remote string
		if err := rows.Scan(&remote); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan duplicate repository identity: %w", err)
		}
		remotes = append(remotes, remote)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("iterate duplicate repository identities: %w", err)
	}
	_ = rows.Close()

	for _, remote := range remotes {
		rows, err := tx.Query(`SELECT id FROM projects WHERE repo_remote = ? ORDER BY id`, remote)
		if err != nil {
			return fmt.Errorf("list duplicate repository identity %q: %w", remote, err)
		}
		var ids []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				_ = rows.Close()
				return fmt.Errorf("scan duplicate repository project: %w", err)
			}
			ids = append(ids, id)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return fmt.Errorf("iterate duplicate repository projects: %w", err)
		}
		_ = rows.Close()
		for _, oldID := range ids[1:] {
			if err := mergeProjectTx(context.Background(), tx, oldID, ids[0]); err != nil {
				return fmt.Errorf("merge duplicate repository identity %q: %w", remote, err)
			}
		}
	}
	if _, err := tx.Exec(`
		CREATE UNIQUE INDEX IF NOT EXISTS idx_projects_repo_remote
		ON projects(repo_remote) WHERE repo_remote IS NOT NULL AND repo_remote <> ''
	`); err != nil {
		return fmt.Errorf("create unique repository identity index: %w", err)
	}
	return nil
}

var phase1aProvenanceColumns = []struct {
	name string
	typ  string
}{
	{"valid_from", "TEXT"},
	{"valid_until", "TEXT"},
	{"verified_at", "TEXT"},
	{"agent", "TEXT"},
	{"session_id", "TEXT"},
	{"source_ref", "TEXT"},
	{"confidence", "REAL"},
}

type tableInspector interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func columnExists(tx tableInspector, table, column string) (bool, error) {
	ctx := context.Background()
	rows, err := tx.QueryContext(ctx, fmt.Sprintf(`PRAGMA table_info(%s)`, table))
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

func migrateV17(tx *sql.Tx) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS memory_history (
    id          TEXT PRIMARY KEY DEFAULT (hex(randomblob(16))),
    memory_id   TEXT NOT NULL,
    project_id  TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    recorded_at TEXT NOT NULL DEFAULT (datetime('now')),
    phase       TEXT NOT NULL
                CHECK (phase IN (
                    'save', 'update', 'reflect', 'merge', 'resolve',
                    'unresolve', 'supersede', 'unsupersede', 'restore',
                    'import', 'delete', 'baseline'
                )),
    agent       TEXT,
    session_id  TEXT,
    related_id  TEXT,
    merged_content TEXT,
    content     TEXT,
    category    TEXT,
    importance  REAL,
    resolved_at TEXT,
    source      TEXT
)`,
		`CREATE INDEX IF NOT EXISTS idx_history_memory ON memory_history(memory_id, recorded_at)`,
	}
	for _, stmt := range stmts {
		if _, err := tx.Exec(stmt); err != nil {
			return fmt.Errorf("%q: %w", stmt[:min(40, len(stmt))], err)
		}
	}
	return nil
}

func migrateV18(tx *sql.Tx) error {
	// line this build has always documented, and it is stated rather than logged:
	if err := refuseForeignProvenanceTable(tx); err != nil {
		return err
	}

	stmts := []string{
		`CREATE TABLE IF NOT EXISTS memory_provenance (
    id          TEXT PRIMARY KEY DEFAULT (hex(randomblob(16))),
    memory_id   TEXT NOT NULL REFERENCES memories(id) ON DELETE CASCADE,
    kind        TEXT NOT NULL
                CHECK (kind IN ('observed', 'imported', 'verified', 'legacy')),
    agent       TEXT,
    session_id  TEXT,
    source_ref  TEXT,
    confidence  REAL,
    observed_at TEXT,
    verified_at TEXT,
    carried_from TEXT
)`,
		`CREATE INDEX IF NOT EXISTS idx_provenance_memory ON memory_provenance(memory_id)`,
		`CREATE TABLE IF NOT EXISTS memory_snapshot_evidence (
    snapshot_id  TEXT NOT NULL,
    memory_id    TEXT NOT NULL,
    kind         TEXT NOT NULL
                 CHECK (kind IN ('observed', 'imported', 'verified', 'legacy')),
    agent        TEXT,
    session_id   TEXT,
    source_ref   TEXT,
    confidence   REAL,
    observed_at  TEXT,
    verified_at  TEXT
)`,
		`CREATE INDEX IF NOT EXISTS idx_snapshot_evidence ON memory_snapshot_evidence(snapshot_id, memory_id)`,
		`INSERT INTO memory_provenance (memory_id, kind, agent, session_id, source_ref, confidence, verified_at)
SELECT id, 'legacy', agent, session_id, source_ref, confidence, verified_at
FROM memories
WHERE (agent IS NOT NULL OR session_id IS NOT NULL OR source_ref IS NOT NULL OR confidence IS NOT NULL)
  AND NOT EXISTS (SELECT 1 FROM memory_provenance p WHERE p.memory_id = memories.id)`,
	}
	for _, stmt := range stmts {
		if _, err := tx.Exec(stmt); err != nil {
			return fmt.Errorf("%q: %w", stmt[:min(40, len(stmt))], err)
		}
	}
	return nil
}

func migrateV19(tx *sql.Tx) error {
	hasRetention, err := columnExists(tx, "memories", "retention")
	if err != nil {
		return err
	}
	if !hasRetention {
		if _, err := tx.Exec(`ALTER TABLE memories ADD COLUMN retention TEXT NOT NULL DEFAULT 'project'
            CHECK (retention IN ('session', 'project', 'persistent'))`); err != nil {
			return fmt.Errorf("add memories.retention: %w", err)
		}
	}
	hasExpiry, err := columnExists(tx, "memories", "expires_at")
	if err != nil {
		return err
	}
	if !hasExpiry {
		if _, err := tx.Exec(`ALTER TABLE memories ADD COLUMN expires_at TEXT`); err != nil {
			return fmt.Errorf("add memories.expires_at: %w", err)
		}
	}
	if _, err := tx.Exec(`CREATE INDEX IF NOT EXISTS idx_memories_session_expiry
        ON memories(expires_at) WHERE retention = 'session'`); err != nil {
		return fmt.Errorf("create the session-expiry index: %w", err)
	}
	return nil
}

// migrateV20 adds the retrieval record: one row per retrieval call, holding what
// the call retrieved and what it kept (#646).
func migrateV20(tx *sql.Tx) error {
	if err := refuseForeignRetrievalRecordTable(tx); err != nil {
		return err
	}
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS retrieval_record (
    project_id TEXT NOT NULL,
    session_id TEXT NOT NULL DEFAULT '',
    source     TEXT NOT NULL,
    query_hash TEXT NOT NULL DEFAULT ''
               CHECK (query_hash = '' OR
                      (length(query_hash) = 64 AND query_hash NOT GLOB '*[^0-9a-f]*')),
    as_of      TEXT NOT NULL DEFAULT '',
    outcome    TEXT NOT NULL,
    reason     TEXT NOT NULL DEFAULT '',
    verdicts   TEXT NOT NULL DEFAULT '[]',
    recorded_at TEXT NOT NULL DEFAULT (datetime('now'))
)`,
		`CREATE INDEX IF NOT EXISTS idx_retrieval_record_project ON retrieval_record(project_id)`,
	}
	for _, stmt := range stmts {
		if _, err := tx.Exec(stmt); err != nil {
			return fmt.Errorf("v20 retrieval record: %w", err)
		}
	}
	return nil
}

var retrievalRecordIdentity = []string{"project_id", "source", "verdicts"}

func migrateV21(tx *sql.Tx) error {
	if err := refuseForeignRetrievalAuditTable(tx); err != nil {
		return err
	}
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS retrieval_audit (
    project_id   TEXT NOT NULL,
    record_rowid INTEGER NOT NULL DEFAULT 0,
    session_id   TEXT NOT NULL DEFAULT '',
    source       TEXT NOT NULL,
    memory_id    TEXT NOT NULL,
    outcome      TEXT NOT NULL,
    signal       TEXT NOT NULL DEFAULT '',
    degraded     TEXT NOT NULL DEFAULT '',
    recorded_at  TEXT NOT NULL DEFAULT (datetime('now'))
)`,
		`CREATE INDEX IF NOT EXISTS idx_retrieval_audit_project ON retrieval_audit(project_id)`,
	}
	for _, stmt := range stmts {
		if _, err := tx.Exec(stmt); err != nil {
			return fmt.Errorf("v21 retrieval audit: %w", err)
		}
	}
	return nil
}

var retrievalAuditIdentity = []string{"project_id", "record_rowid", "outcome"}

func refuseForeignRetrievalAuditTable(tx tableInspector) error {
	present, err := tableExists(tx, "retrieval_audit")
	if err != nil {
		return err
	}
	if !present {
		return nil
	}
	for _, c := range retrievalAuditIdentity {
		has, err := columnExists(tx, "retrieval_audit", c)
		if err != nil {
			return err
		}
		if !has {
			return fmt.Errorf(
				"this database already holds a retrieval_audit table without a %q column, so it is not Ghost's "+
					"retrieval audit — a same-named table makes CREATE TABLE IF NOT EXISTS a silent no-op and lets "+
					"ALTER TABLE ADD COLUMN through, so the migration would stamp its version over a shape every "+
					"later write would fail on. Drop it and reopen: "+
					"sqlite3 <db> 'DROP TABLE retrieval_audit'", c)
		}
	}
	return nil
}

func migrateV22(tx *sql.Tx) error {
	if err := refuseForeignRetrievalAuditTable(tx); err != nil {
		return err
	}
	has, err := columnExists(tx, "retrieval_audit", "content_hash")
	if err != nil {
		return err
	}
	if has {
		return nil
	}
	stmt := `ALTER TABLE retrieval_audit ADD COLUMN content_hash TEXT NOT NULL DEFAULT ''`
	if _, err := tx.Exec(stmt); err != nil {
		return fmt.Errorf("v22 retrieval audit content hash: %w", err)
	}
	return nil
}

func migrateV23(tx *sql.Tx) error {
	if err := refuseForeignMemoryFlagsTable(tx); err != nil {
		return err
	}
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS memory_flags (
    project_id   TEXT NOT NULL,
    memory_id    TEXT NOT NULL REFERENCES memories(id) ON DELETE CASCADE,
    kind         TEXT NOT NULL CHECK (kind IN ('wrong','stale')),
    reason       TEXT NOT NULL,
    content_hash TEXT NOT NULL DEFAULT ''
                CHECK (content_hash = '' OR length(content_hash) = 64),
    agent        TEXT NOT NULL DEFAULT '',
    session_id   TEXT NOT NULL DEFAULT '',
    recorded_at  TEXT NOT NULL DEFAULT (datetime('now'))
)`,
		`CREATE INDEX IF NOT EXISTS idx_memory_flags_project ON memory_flags(project_id)`,
	}
	for _, stmt := range stmts {
		if _, err := tx.Exec(stmt); err != nil {
			return fmt.Errorf("v23 memory flags: %w", err)
		}
	}
	return nil
}

var memoryFlagsIdentity = []string{"project_id", "kind"}

func refuseForeignMemoryFlagsTable(tx tableInspector) error {
	present, err := tableExists(tx, "memory_flags")
	if err != nil {
		return err
	}
	if !present {
		return nil
	}
	for _, c := range memoryFlagsIdentity {
		has, err := columnExists(tx, "memory_flags", c)
		if err != nil {
			return err
		}
		if !has {
			return fmt.Errorf(
				"this database already holds a memory_flags table without a %q column, so it is not Ghost's "+
					"memory-flag table — a same-named table makes CREATE TABLE IF NOT EXISTS a silent no-op and lets "+
					"the migration stamp its version over a shape every later flag write would fail on. Drop it and "+
					"reopen: sqlite3 <db> 'DROP TABLE memory_flags'", c)
		}
	}
	return nil
}

func refuseForeignRetrievalRecordTable(tx tableInspector) error {
	present, err := tableExists(tx, "retrieval_record")
	if err != nil {
		return err
	}
	if !present {
		return nil
	}
	for _, c := range retrievalRecordIdentity {
		has, err := columnExists(tx, "retrieval_record", c)
		if err != nil {
			return err
		}
		if !has {
			return fmt.Errorf(
				"this database already holds a retrieval_record table without a %q column, so it is not Ghost's "+
					"retrieval record — a same-named table makes CREATE TABLE IF NOT EXISTS a silent no-op, so the "+
					"migration would stamp v20 over a shape every later write would fail on. Drop it and reopen: "+
					"sqlite3 <db> 'DROP TABLE retrieval_record'", c)
		}
	}
	return nil
}

func refuseForeignProvenanceTable(run tableInspector) error {
	present, err := tableExists(run, "memory_provenance")
	if err != nil {
		return err
	}
	if !present {
		return nil
	}
	for _, c := range evidenceTableIdentity {
		has, err := columnExists(run, "memory_provenance", c)
		if err != nil {
			return err
		}
		if !has {
			return fmt.Errorf(
				"this database already holds a memory_provenance table without a %q column, so it is not Ghost's evidence table "+
					"— it is a development build that used the reserved name for something else, and no release ever wrote one. "+
					"Drop it and reopen: sqlite3 <db> 'DROP TABLE memory_provenance'",
				c)
		}
	}
	return nil
}

var evidenceTableIdentity = []string{"id", "memory_id", "kind"}

func tableExists(run tableInspector, table string) (bool, error) {
	var n int
	err := run.QueryRowContext(context.Background(),
		`SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`, table,
	).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("look up table %s: %w", table, err)
	}
	return n > 0, nil
}

func migrateV14(tx *sql.Tx) error {
	hasScope, err := columnExists(tx, "memory_snapshots", "scope")
	if err != nil {
		return err
	}
	if !hasScope {
		if _, err := tx.Exec(`ALTER TABLE memory_snapshots ADD COLUMN scope TEXT`); err != nil {
			return fmt.Errorf("add memory_snapshots.scope: %w", err)
		}
	}
	hasCaptured, err := columnExists(tx, "memory_snapshots", "scope_captured")
	if err != nil {
		return err
	}
	if !hasCaptured {
		if _, err := tx.Exec(`ALTER TABLE memory_snapshots ADD COLUMN scope_captured INTEGER NOT NULL DEFAULT 0`); err != nil {
			return fmt.Errorf("add memory_snapshots.scope_captured: %w", err)
		}
	}
	return nil
}

func migrateV24(tx *sql.Tx) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS vector_epoch (
    id    INTEGER PRIMARY KEY CHECK (id = 1),
    epoch INTEGER NOT NULL DEFAULT 0
)`,
		`CREATE TRIGGER IF NOT EXISTS vector_epoch_emb_ai AFTER INSERT ON memory_embeddings BEGIN
    INSERT INTO vector_epoch (id, epoch) VALUES (1, 1) ON CONFLICT (id) DO UPDATE SET epoch = epoch + 1;
END`,
		`CREATE TRIGGER IF NOT EXISTS vector_epoch_emb_au AFTER UPDATE ON memory_embeddings
    WHEN old.memory_id IS NOT new.memory_id OR old.embedding IS NOT new.embedding OR old.model IS NOT new.model BEGIN
    INSERT INTO vector_epoch (id, epoch) VALUES (1, 1) ON CONFLICT (id) DO UPDATE SET epoch = epoch + 1;
END`,
		`CREATE TRIGGER IF NOT EXISTS vector_epoch_emb_ad AFTER DELETE ON memory_embeddings BEGIN
    INSERT INTO vector_epoch (id, epoch) VALUES (1, 1) ON CONFLICT (id) DO UPDATE SET epoch = epoch + 1;
END`,
		`CREATE TRIGGER IF NOT EXISTS vector_epoch_mem_au AFTER UPDATE OF resolved_at, scope, project_id ON memories
    WHEN old.resolved_at IS NOT new.resolved_at OR old.scope IS NOT new.scope OR old.project_id IS NOT new.project_id BEGIN
    INSERT INTO vector_epoch (id, epoch) VALUES (1, 1) ON CONFLICT (id) DO UPDATE SET epoch = epoch + 1;
END`,
		`CREATE TRIGGER IF NOT EXISTS vector_epoch_mem_ad AFTER DELETE ON memories BEGIN
    INSERT INTO vector_epoch (id, epoch) VALUES (1, 1) ON CONFLICT (id) DO UPDATE SET epoch = epoch + 1;
END`,
		`CREATE TRIGGER IF NOT EXISTS vector_epoch_mem_ai AFTER INSERT ON memories
    WHEN EXISTS (SELECT 1 FROM memory_embeddings WHERE memory_id = new.id) BEGIN
    INSERT INTO vector_epoch (id, epoch) VALUES (1, 1) ON CONFLICT (id) DO UPDATE SET epoch = epoch + 1;
END`,
	}
	for i, stmt := range stmts {
		if table := vectorEpochStepTables[i]; table != "" {
			ok, err := tableExists(tx, table)
			if err != nil {
				return fmt.Errorf("v24 vector epoch: %w", err)
			}
			if !ok {
				continue
			}
		}
		if _, err := tx.Exec(stmt); err != nil {
			return fmt.Errorf("v24 vector epoch: %w", err)
		}
	}
	return nil
}

var vectorEpochStepTables = []string{"", "memory_embeddings", "memory_embeddings", "memory_embeddings", "memories", "memories", "memories"}

func migrateV25(tx *sql.Tx) error {
	ok, err := tableExists(tx, "memories")
	if err != nil {
		return fmt.Errorf("v25 vector epoch validity: %w", err)
	}
	if !ok {
		return nil
	}
	for _, stmt := range []string{
		`CREATE TABLE IF NOT EXISTS vector_epoch (
    id    INTEGER PRIMARY KEY CHECK (id = 1),
    epoch INTEGER NOT NULL DEFAULT 0
)`,
		`CREATE TRIGGER IF NOT EXISTS vector_epoch_mem_validity_au AFTER UPDATE OF valid_from, valid_until ON memories
    WHEN old.valid_from IS NOT new.valid_from OR old.valid_until IS NOT new.valid_until BEGIN
    INSERT INTO vector_epoch (id, epoch) VALUES (1, 1) ON CONFLICT (id) DO UPDATE SET epoch = epoch + 1;
END`,
	} {
		if _, err := tx.Exec(stmt); err != nil {
			return fmt.Errorf("v25 vector epoch validity: %w", err)
		}
	}
	return nil
}

func migrateV26(tx *sql.Tx) error {
	hasRetention, err := columnExists(tx, "memory_snapshots", "retention")
	if err != nil {
		return err
	}
	if !hasRetention {
		if _, err := tx.Exec(`ALTER TABLE memory_snapshots ADD COLUMN retention TEXT`); err != nil {
			return fmt.Errorf("add memory_snapshots.retention: %w", err)
		}
	}
	hasCaptured, err := columnExists(tx, "memory_snapshots", "retention_captured")
	if err != nil {
		return err
	}
	if !hasCaptured {
		if _, err := tx.Exec(`ALTER TABLE memory_snapshots ADD COLUMN retention_captured INTEGER NOT NULL DEFAULT 0`); err != nil {
			return fmt.Errorf("add memory_snapshots.retention_captured: %w", err)
		}
	}
	return nil
}

func migrateV27(tx *sql.Tx) error {
	stmts := []string{
		`DROP INDEX IF EXISTS idx_memories_project_imp`,
		`CREATE INDEX IF NOT EXISTS idx_memories_project_imp ON memories(project_id, importance DESC, created_at DESC, id)`,
		`CREATE INDEX IF NOT EXISTS idx_memories_project_imp_updated ON memories(project_id, importance DESC, updated_at DESC, id)`,
	}
	for _, stmt := range stmts {
		if _, err := tx.Exec(stmt); err != nil {
			return fmt.Errorf("migrateV27: %w", err)
		}
	}
	return nil
}

func migrateV28(tx *sql.Tx) error {
	ok, err := tableExists(tx, "memories")
	if err != nil {
		return fmt.Errorf("v28 fts project buckets: %w", err)
	}
	if !ok {
		return nil
	}
	stmts := []string{
		`DROP TRIGGER IF EXISTS memories_ai`,
		`DROP TRIGGER IF EXISTS memories_ad`,
		`DROP TRIGGER IF EXISTS memories_au`,
		`DROP TABLE IF EXISTS memories_fts`,
		`DROP VIEW IF EXISTS memories_fts_source`,
		`CREATE TABLE IF NOT EXISTS memories_fts_projects (
    ord        INTEGER PRIMARY KEY,
    project_id TEXT NOT NULL UNIQUE
)`,
		`DELETE FROM memories_fts_projects`,
		`INSERT INTO memories_fts_projects(project_id) SELECT DISTINCT project_id FROM memories ORDER BY project_id`,
		`CREATE VIEW IF NOT EXISTS memories_fts_source AS
    SELECT k.ord * 1099511627776 + m.rowid AS fts_rowid, m.content AS content
    FROM memories m JOIN memories_fts_projects k ON k.project_id = m.project_id`,
		`CREATE VIRTUAL TABLE IF NOT EXISTS memories_fts USING fts5(
    content,
    content=memories_fts_source,
    content_rowid=fts_rowid,
    tokenize='porter unicode61'
)`,
		`CREATE TRIGGER IF NOT EXISTS memories_ai AFTER INSERT ON memories BEGIN
    INSERT INTO memories_fts_projects(project_id) SELECT new.project_id
        WHERE NOT EXISTS (SELECT 1 FROM memories_fts_projects WHERE project_id = new.project_id);
    INSERT INTO memories_fts(rowid, content) VALUES (
        (SELECT ord FROM memories_fts_projects WHERE project_id = new.project_id) * 1099511627776 + new.rowid,
        new.content);
END`,
		`CREATE TRIGGER IF NOT EXISTS memories_ad AFTER DELETE ON memories BEGIN
    INSERT INTO memories_fts(memories_fts, rowid, content) VALUES('delete',
        (SELECT ord FROM memories_fts_projects WHERE project_id = old.project_id) * 1099511627776 + old.rowid,
        old.content);
    DELETE FROM memories_fts_projects WHERE project_id = old.project_id
        AND NOT EXISTS (SELECT 1 FROM memories WHERE project_id = old.project_id);
END`,
		`CREATE TRIGGER IF NOT EXISTS memories_au AFTER UPDATE ON memories
    WHEN old.content != new.content OR old.project_id != new.project_id BEGIN
    INSERT INTO memories_fts(memories_fts, rowid, content) VALUES('delete',
        (SELECT ord FROM memories_fts_projects WHERE project_id = old.project_id) * 1099511627776 + old.rowid,
        old.content);
    INSERT INTO memories_fts_projects(project_id) SELECT new.project_id
        WHERE NOT EXISTS (SELECT 1 FROM memories_fts_projects WHERE project_id = new.project_id);
    INSERT INTO memories_fts(rowid, content) VALUES (
        (SELECT ord FROM memories_fts_projects WHERE project_id = new.project_id) * 1099511627776 + new.rowid,
        new.content);
    DELETE FROM memories_fts_projects WHERE project_id = old.project_id
        AND NOT EXISTS (SELECT 1 FROM memories WHERE project_id = old.project_id);
END`,
		`INSERT INTO memories_fts(memories_fts) VALUES('rebuild')`,
	}
	for _, stmt := range stmts {
		if _, err := tx.Exec(stmt); err != nil {
			return fmt.Errorf("v28 fts project buckets: %q: %w", stmt[:min(40, len(stmt))], err)
		}
	}
	return nil
}

func migrateV29(tx *sql.Tx) error {
	has, err := columnExists(tx, "memory_snapshots", "updated_at")
	if err != nil {
		return err
	}
	if !has {
		if _, err := tx.Exec(`ALTER TABLE memory_snapshots ADD COLUMN updated_at TEXT`); err != nil {
			return fmt.Errorf("add memory_snapshots.updated_at: %w", err)
		}
	}
	return nil
}

func migrateV30(tx *sql.Tx) error {
	if _, err := tx.Exec(`CREATE INDEX IF NOT EXISTS idx_memories_project_created ON memories(project_id, created_at DESC)`); err != nil {
		return fmt.Errorf("create idx_memories_project_created: %w", err)
	}
	return nil
}

func migrateV31(tx *sql.Tx) error {
	const historyV31DDL = `(
    id          TEXT PRIMARY KEY DEFAULT (hex(randomblob(16))),
    memory_id   TEXT NOT NULL,
    project_id  TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    recorded_at TEXT NOT NULL DEFAULT (datetime('now')),
    phase       TEXT NOT NULL
                CHECK (phase IN (
                    'save', 'update', 'reflect', 'merge', 'resolve',
                    'unresolve', 'supersede', 'unsupersede', 'restore',
                    'import', 'delete', 'baseline', 'move'
                )),
    agent       TEXT,
    session_id  TEXT,
    related_id  TEXT,
    merged_content TEXT,
    from_project_id TEXT,
    to_project_id   TEXT,
    content     TEXT,
    category    TEXT,
    importance  REAL,
    resolved_at TEXT,
    source      TEXT
)`
	copyCols := `id, memory_id, project_id, recorded_at, phase, agent, session_id, related_id, merged_content,
		     content, category, importance, resolved_at, source`
	stmts := []string{
		`CREATE TABLE memory_history_move_v31 ` + historyV31DDL,
		`INSERT INTO memory_history_move_v31 (` + copyCols + `)
		SELECT ` + copyCols + `
		FROM memory_history`,
		`DROP TABLE memory_history`,
		`CREATE TABLE memory_history ` + historyV31DDL,
		`INSERT INTO memory_history (` + copyCols + `)
		SELECT ` + copyCols + `
		FROM memory_history_move_v31`,
		`DROP TABLE memory_history_move_v31`,
		`CREATE INDEX IF NOT EXISTS idx_history_memory ON memory_history(memory_id, recorded_at)`,
	}
	for _, s := range stmts {
		if _, err := tx.Exec(s); err != nil {
			return fmt.Errorf("%q: %w", s[:min(40, len(s))], err)
		}
	}
	return nil
}
