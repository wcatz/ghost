package memory

import (
	"database/sql"
	"fmt"

	_ "modernc.org/sqlite"
)

// initSQL is the schema for the ghost database.
// Kept as a Go constant rather than go:embed because embed paths cannot use "..".
// The canonical SQL file is also maintained at migrations/001_init.sql for reference.
const initSQL = `
CREATE TABLE IF NOT EXISTS projects (
    id          TEXT PRIMARY KEY,
    path        TEXT NOT NULL UNIQUE,
    name        TEXT NOT NULL,
    created_at  TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at  TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE IF NOT EXISTS memories (
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
                  CHECK (source IN ('reflection', 'chat', 'manual', 'tool', 'mcp')),
    tags          TEXT DEFAULT '[]',
    pinned        INTEGER NOT NULL DEFAULT 0,
    created_at    TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at    TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE VIRTUAL TABLE IF NOT EXISTS memories_fts USING fts5(
    content,
    content=memories,
    content_rowid=rowid,
    tokenize='porter unicode61'
);

CREATE TRIGGER IF NOT EXISTS memories_ai AFTER INSERT ON memories BEGIN
    INSERT INTO memories_fts(rowid, content) VALUES (new.rowid, new.content);
END;

CREATE TRIGGER IF NOT EXISTS memories_ad AFTER DELETE ON memories BEGIN
    INSERT INTO memories_fts(memories_fts, rowid, content) VALUES('delete', old.rowid, old.content);
END;

CREATE TRIGGER IF NOT EXISTS memories_au AFTER UPDATE ON memories BEGIN
    INSERT INTO memories_fts(memories_fts, rowid, content) VALUES('delete', old.rowid, old.content);
    INSERT INTO memories_fts(rowid, content) VALUES (new.rowid, new.content);
END;

CREATE INDEX IF NOT EXISTS idx_memories_project_cat ON memories(project_id, category);
CREATE INDEX IF NOT EXISTS idx_memories_project_imp ON memories(project_id, importance DESC);
CREATE INDEX IF NOT EXISTS idx_memories_project_source ON memories(project_id, source);

CREATE TABLE IF NOT EXISTS conversations (
    id          TEXT PRIMARY KEY DEFAULT (hex(randomblob(16))),
    project_id  TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    mode        TEXT NOT NULL DEFAULT 'code',
    title       TEXT DEFAULT '',
    created_at  TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at  TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE IF NOT EXISTS messages (
    id              TEXT PRIMARY KEY DEFAULT (hex(randomblob(16))),
    conversation_id TEXT NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
    role            TEXT NOT NULL CHECK (role IN ('user', 'assistant', 'tool_use', 'tool_result')),
    content         TEXT NOT NULL,
    tool_name       TEXT,
    tool_use_id     TEXT,
    created_at      TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE INDEX IF NOT EXISTS idx_conversations_project ON conversations(project_id, updated_at DESC);
CREATE INDEX IF NOT EXISTS idx_messages_conv ON messages(conversation_id, created_at);

CREATE TABLE IF NOT EXISTS ghost_state (
    project_id          TEXT PRIMARY KEY REFERENCES projects(id) ON DELETE CASCADE,
    interaction_count   INTEGER NOT NULL DEFAULT 0,
    learned_context     TEXT DEFAULT '',
    last_reflection_at  TEXT,
    reflection_summary  TEXT DEFAULT '',
    updated_at          TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE IF NOT EXISTS token_usage (
    id              TEXT PRIMARY KEY DEFAULT (hex(randomblob(16))),
    project_id      TEXT NOT NULL,
    model           TEXT NOT NULL,
    input_tokens    INTEGER NOT NULL DEFAULT 0,
    output_tokens   INTEGER NOT NULL DEFAULT 0,
    cache_creation  INTEGER NOT NULL DEFAULT 0,
    cache_read      INTEGER NOT NULL DEFAULT 0,
    cost_usd        REAL NOT NULL DEFAULT 0,
    created_at      TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE INDEX IF NOT EXISTS idx_usage_project ON token_usage(project_id, created_at DESC);

CREATE TABLE IF NOT EXISTS audit_log (
    id          TEXT PRIMARY KEY DEFAULT (hex(randomblob(16))),
    timestamp   TEXT NOT NULL DEFAULT (datetime('now')),
    action      TEXT NOT NULL,
    project_id  TEXT,
    user        TEXT DEFAULT '',
    details     TEXT DEFAULT '{}',
    tokens      INTEGER DEFAULT 0,
    cost_usd    REAL DEFAULT 0,
    duration_ms INTEGER DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_audit_project ON audit_log(project_id, timestamp DESC);
CREATE INDEX IF NOT EXISTS idx_audit_action ON audit_log(action, timestamp DESC);

CREATE TABLE IF NOT EXISTS memory_embeddings (
    memory_id   TEXT PRIMARY KEY REFERENCES memories(id) ON DELETE CASCADE,
    embedding   BLOB NOT NULL,
    model       TEXT NOT NULL DEFAULT 'nomic-embed-text',
    created_at  TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE IF NOT EXISTS notifications (
    id              TEXT PRIMARY KEY,
    github_id       TEXT NOT NULL UNIQUE,
    repo_full_name  TEXT NOT NULL,
    subject_title   TEXT NOT NULL,
    subject_type    TEXT NOT NULL,
    subject_url     TEXT DEFAULT '',
    reason          TEXT NOT NULL,
    priority        INTEGER NOT NULL DEFAULT 4,
    unread          INTEGER NOT NULL DEFAULT 1,
    updated_at      TEXT NOT NULL,
    created_at      TEXT NOT NULL DEFAULT (datetime('now')),
    dismissed_at    TEXT
);

CREATE INDEX IF NOT EXISTS idx_notif_priority ON notifications(priority, updated_at DESC);
CREATE INDEX IF NOT EXISTS idx_notif_repo ON notifications(repo_full_name, updated_at DESC);
CREATE INDEX IF NOT EXISTS idx_notif_unread ON notifications(unread, priority);

CREATE TABLE IF NOT EXISTS scheduled_jobs (
    id          TEXT PRIMARY KEY DEFAULT (hex(randomblob(16))),
    name        TEXT NOT NULL,
    schedule    TEXT NOT NULL,
    payload     TEXT DEFAULT '{}',
    enabled     INTEGER NOT NULL DEFAULT 1,
    last_run    TEXT,
    next_run    TEXT,
    created_at  TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE IF NOT EXISTS reminders (
    id          TEXT PRIMARY KEY DEFAULT (hex(randomblob(16))),
    message     TEXT NOT NULL,
    due_at      TEXT NOT NULL,
    fired       INTEGER NOT NULL DEFAULT 0,
    created_at  TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE INDEX IF NOT EXISTS idx_reminders_due ON reminders(fired, due_at);
`

// OpenDB opens or creates the SQLite database and runs migrations.
func OpenDB(dbPath string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", dbPath+"?_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}

	if _, err := db.Exec(initSQL); err != nil {
		db.Close()
		return nil, fmt.Errorf("init schema: %w", err)
	}

	return db, nil
}
