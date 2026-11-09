package memory

import (
	"database/sql"
	"path/filepath"
	"reflect"
	"testing"
)

// historyColumns returns PRAGMA table_info for memory_history as comparable
// rows (name, type, notnull, default, pk).
func historyColumns(t *testing.T, db *sql.DB) [][5]string {
	t.Helper()
	rows, err := db.Query(`SELECT name, type, "notnull", COALESCE(dflt_value, '<nil>'), pk FROM pragma_table_info('memory_history') ORDER BY cid`)
	if err != nil {
		t.Fatalf("table_info: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var out [][5]string
	for rows.Next() {
		var n, ty, d string
		var nn, pk int
		if err := rows.Scan(&n, &ty, &nn, &d, &pk); err != nil {
			t.Fatal(err)
		}
		out = append(out, [5]string{n, ty, string(rune('0' + nn)), d, string(rune('0' + pk))})
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestMigrateV30ToV31WidensMemoryHistory(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "v30.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`PRAGMA foreign_keys = ON`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(initSQL); err != nil {
		t.Fatal(err)
	}
	if err := ensurePostMigrationIndexes(db); err != nil {
		t.Fatal(err)
	}
	// Put memory_history back into its v30 shape: no 'move' phase and no
	// from/to columns.
	for _, s := range []string{
		`DROP TABLE memory_history`,
		`CREATE TABLE memory_history (
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
		`CREATE INDEX idx_history_memory ON memory_history(memory_id, recorded_at)`,
		`INSERT INTO projects (id, path, name) VALUES ('p1', '/tmp/p1', 'p1')`,
		`INSERT INTO memory_history (id, memory_id, project_id, recorded_at, phase, agent, session_id, related_id, merged_content, content, category, importance, resolved_at, source) VALUES
		 ('h1', 'm1', 'p1', '2026-01-01 00:00:00', 'save', 'claude-code', 's1', NULL, NULL, 'first', 'fact', 0.7, NULL, 'mcp'),
		 ('h2', 'm1', 'p1', '2026-01-02 00:00:00', 'update', NULL, NULL, NULL, NULL, 'second', 'fact', 0.8, NULL, 'cli'),
		 ('h3', 'm2', 'p1', '2026-01-03 00:00:00', 'merge', 'opencode', NULL, 'm1', 'folded text', 'merged', 'gotcha', 0.5, '2026-01-04 00:00:00', 'reflection'),
		 ('h4', 'm1', 'p1', '2026-01-05 00:00:00', 'delete', NULL, NULL, 'm3', NULL, 'last live', 'fact', 0.8, NULL, NULL)`,
		`PRAGMA user_version = 30`,
	} {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("%.40s: %v", s, err)
		}
	}
	type histRow struct {
		id, memoryID, projectID, recordedAt, phase                 string
		agent, session, related, merged, content, category, source sql.NullString
		importance                                                 sql.NullFloat64
		resolvedAt                                                 sql.NullString
	}
	const sel = `SELECT id, memory_id, project_id, recorded_at, phase, agent, session_id, related_id, merged_content, content, category, importance, resolved_at, source FROM memory_history ORDER BY id`
	readAll := func(db *sql.DB) []histRow {
		rows, err := db.Query(sel)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = rows.Close() }()
		var out []histRow
		for rows.Next() {
			var r histRow
			if err := rows.Scan(&r.id, &r.memoryID, &r.projectID, &r.recordedAt, &r.phase, &r.agent, &r.session, &r.related, &r.merged, &r.content, &r.category, &r.importance, &r.resolvedAt, &r.source); err != nil {
				t.Fatal(err)
			}
			out = append(out, r)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return out
	}
	before := readAll(db)
	if len(before) != 4 {
		t.Fatalf("seeded %d rows, want 4", len(before))
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	migrated, err := OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer func() { _ = migrated.Close() }()

	if v := schemaVersionOf(t, migrated); v != schemaVersion || v < 31 {
		t.Fatalf("user_version = %d, want %d", v, schemaVersion)
	}
	if after := readAll(migrated); !reflect.DeepEqual(before, after) {
		t.Fatalf("rows changed by migration:\n before %+v\n after  %+v", before, after)
	}
	var nonNull int
	if err := migrated.QueryRow(`SELECT count(*) FROM memory_history WHERE from_project_id IS NOT NULL OR to_project_id IS NOT NULL`).Scan(&nonNull); err != nil || nonNull != 0 {
		t.Fatalf("from/to not NULL on old rows: n=%d err=%v", nonNull, err)
	}
	var idx string
	if err := migrated.QueryRow(`SELECT name FROM sqlite_master WHERE type='index' AND name='idx_history_memory' AND tbl_name='memory_history'`).Scan(&idx); err != nil {
		t.Fatalf("idx_history_memory missing: %v", err)
	}
	if _, err := migrated.Exec(`INSERT INTO memory_history (memory_id, project_id, phase, from_project_id, to_project_id) VALUES ('m1', 'p1', 'move', 'p1', '_global')`); err != nil {
		t.Fatalf("move phase insert: %v", err)
	}
	if _, err := migrated.Exec(`INSERT INTO memory_history (memory_id, project_id, phase) VALUES ('m1', 'p1', 'bogus')`); err == nil {
		t.Fatal("bogus phase accepted; CHECK constraint lost")
	}

	// The DDL copied into migrateV31 must not drift from schema.go.
	fresh, err := OpenDB(filepath.Join(dir, "fresh.db"))
	if err != nil {
		t.Fatalf("fresh OpenDB: %v", err)
	}
	defer func() { _ = fresh.Close() }()
	got, want := historyColumns(t, migrated), historyColumns(t, fresh)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("migrated memory_history columns differ from a fresh store:\n migrated %v\n fresh    %v", got, want)
	}
}
