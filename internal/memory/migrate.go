package memory

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"strings"
)

// schemaVersion is the current schema version, stamped into PRAGMA user_version.
// Bump it and append to migrations whenever initSQL changes in a way that
// CREATE TABLE IF NOT EXISTS cannot deliver to existing databases (new columns,
// CHECK values, foreign keys, dropped tables).
const schemaVersion = 15

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
}

// migrate brings an existing database up to schemaVersion. Fresh databases
// (detected by the caller) are stamped directly and never pass through here.
// Table rebuilds require foreign_keys=OFF, which is a no-op inside a
// transaction — so the pragma is toggled on a pinned connection around each
// step, and foreign_key_check runs before the version stamp is committed.
//
// A database can carry PRE-EXISTING foreign-key orphans — child rows whose
// parent (a memories or projects row) is gone. The migration steps did not
// create them, but a naive foreign_key_check after a step would abort on them
// and brick every newer binary (the real v5 DB aborted v0.30.10+ this way).
// migrate() therefore repairs derived-cache orphans (embedding vectors, link
// scan markers, supersede verdicts, the auto link graph) up front with a loud
// warning, and refuses to guess on user-content rows (memories, tasks,
// decisions, ghost_state, snapshots) — those abort with copy-pasteable DELETE
// statements instead of silently dropping user data.
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
			// Pre-existing orphans were already repaired above, so any
			// violation here means the migration step itself left the schema
			// inconsistent — a step bug that must fail loudly, not be masked.
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

// cacheFKWhitelist names the tables whose rows are derived caches: embedding
// vectors, linking scan markers, supersede verdicts, and the auto link graph.
// A row in one of these whose parent row is gone is meaningless debris — no user
// content, recomputable by the workers — so a pre-existing orphan there must
// not abort the migration (which would brick every newer binary). It is deleted
// with a loud warning instead. Anything NOT in this list is treated as
// user-content and never auto-deleted.
var cacheFKWhitelist = map[string]bool{
	"memory_embeddings": true,
	"link_scans":        true,
	"supersede_checked": true,
	"memory_links":      true,
}

// repairPreExistingFKOrphans deletes derived-cache rows whose parent row is
// gone — e.g. a memory_embeddings row for a deleted memory — so a pre-existing
// orphan cannot abort every migration step and brick the new binary. A
// violation in a user-content table (memories, tasks, decisions, ghost_state,
// memory_snapshots) is NOT deleted: migrate() returns an error carrying
// copy-pasteable DELETE statements so the operator decides. Either way the
// pre-migration backup (backupBeforeMigrate, run by OpenDB) is already on disk
// as a last resort.
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

// fkViolations scans PRAGMA foreign_key_check and returns a table→rowids map.
// The pragma reports violations regardless of the foreign_keys setting, so it
// works both before migration (repair pass) and inside a step transaction
// (post-step gate). run accepts a *sql.Conn or *sql.Tx.
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

// migrateV5 drops the orphaned conversation/message tables (#347). Both date
// from the assistant-era chat subsystem stripped in 9aff8a7 (v0.8.0, #149):
// nothing has written to them since v0.8.0 — reflection's recent-exchanges
// feed has been returning empty ever since. migrateV1 dropped the other
// assistant-era tables (notifications, reminders, scheduled_jobs) but left
// these two because reflect still read them; with their writers gone and the
// read path dead in practice, they are now dead weight — 76 conversations /
// 645 messages of orphaned rows in the production DB as of 2026-08-24.
// Dropping a table drops its indices (idx_conversations_project,
// idx_messages_conv) with it. Messages is dropped before its parent
// conversations for FK hygiene, though migrate() already runs each step with
// foreign_keys=OFF.
func migrateV5(tx *sql.Tx) error {
	for _, t := range []string{"messages", "conversations"} {
		if _, err := tx.Exec("DROP TABLE IF EXISTS " + t); err != nil {
			return fmt.Errorf("drop %s: %w", t, err)
		}
	}
	return nil
}

// migrateV6 adds ghost_state.reflect_input_sig: the fingerprint of the memory
// set that produced the last applied consolidation (reflection.InputSignature).
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

// migrateV7 adds memories.resolve_kept_hash: the content hash recorded when
// resolve last judged a memory KEEP, so a converged pass can skip re-asking
// the classifier. A nullable column would work too, but NOT NULL with an empty
// string default matches the column's meaning — empty means "never judged
// KEEP" — and needs no special-casing in the store.
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

// migrateV8 adds the supersede_checked table: the content-keyed NEITHER cache
// for `ghost supersede`, so a converged project skips re-classifying candidate
// pairs whose endpoints have not changed since they were judged NEITHER. The
// cascading FKs keep the table derived state — replacing or deleting a memory
// (as reflection's consolidation does) takes its checked rows with it. New
// tables copy their DDL here rather than reusing initSQL, per the migrations
// comment; the guards make the step safe to re-run against a hand-migrated DB.
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

// migrateV9 adds the maintenance_runs table: scratch-hygiene events (a budget
// check that fired before a harness spawn — over budget, reaped, maybe warned)
// recorded so `ghost maintenance status` can show scratch bytes and reaped
// counts. Like migrateV8, the DDL is copied here rather than shared with
// initSQL (steps are frozen in time) and the CREATE IF NOT EXISTS guards make
// the step safe to re-run against a hand-migrated database. The table is
// deliberately project-less — hygiene events belong to the whole instance, not
// one project — so it carries no foreign keys and needs no orphan handling.
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

// migrateV10 adds temporal-validity and provenance columns to memories.
//
// Temporal validity answers "was this true then / is it true now", which
// decay alone cannot: decay only lowers a stale memory's rank, it never says
// the memory stopped being applicable. Provenance answers "why believe it" —
// a fact read out of a Helmfile is not the same evidence as an agent's
// inference, and `source` already distinguishes how a memory arrived but not
// who or what produced its content, in which session, against which
// reference, or how much it was trusted.
//
// All seven columns are nullable and deliberately get NO default. Ghost has
// no record of the agent, session, or reference behind memories written
// before v10, and a fabricated value would be an assertion about provenance
// that nobody made: NULL reads as "unknown", which is true. confidence in
// particular stays NULL rather than 0.5 — a stored number implies a
// measured belief, and an invented midpoint would launder into evidence the
// moment anything ranked by it.
//
// Purely additive: no existing column, constraint, trigger, or FTS index
// changes, so there is no table rebuild and no writer or reader needs to
// change for the migration to be correct.
func migrateV10(tx *sql.Tx) error {
	for _, c := range phase1aProvenanceColumns {
		exists, err := columnExists(tx, "memories", c.name)
		if err != nil {
			return err
		}
		if exists {
			continue // hand-migrated DB already carries this column
		}
		q := fmt.Sprintf("ALTER TABLE memories ADD COLUMN %s %s", c.name, c.typ)
		if _, err := tx.Exec(q); err != nil {
			return fmt.Errorf("add memories.%s: %w", c.name, err)
		}
	}
	return nil
}

// migrateV11 adds projects.repo_remote, the normalized remote URL of the
// repository a project's path points at.
//
// Path identity cannot recognise that two checkouts are one project:
// ~/src/ghost and ~/work/ghost are different strings, so an agent that changes
// working directory silently starts a second project and loses everything the
// first one knew. The remote is the only thing the two paths share that says
// they are the same repository.
//
// Additive and nullable: a project may have no repository, and existing rows
// have nothing to detect from here — git is consulted by the caller that owns
// the path, never by this package. No default is applied, because inventing a
// remote for a project that has none would merge unrelated work.
func migrateV11(tx *sql.Tx) error {
	exists, err := columnExists(tx, "projects", "repo_remote")
	if err != nil {
		return err
	}
	if exists {
		return nil // hand-migrated DB already has the column
	}
	if _, err := tx.Exec(`ALTER TABLE projects ADD COLUMN repo_remote TEXT`); err != nil {
		return fmt.Errorf("add projects.repo_remote: %w", err)
	}
	return nil
}

// migrateV12 adds memories.scope, a JSON object naming where a memory
// applies — {"environment":"production","component":"api"} — or NULL when it
// applies everywhere.
//
// Before this, scope was only ever implied by wording: "the database for
// development is SQLite" and "the database for production is PostgreSQL"
// differed because the sentence said so, so retrieval could only hope
// semantic similarity separated them. A machine-readable scope lets a query
// for production exclude a development row by construction rather than by
// resemblance.
//
// Additive and NULL by default. Existing memories were written without any
// notion of scope, so stamping them one would assert where knowledge applies
// that nobody asserted — NULL reads as "unspecified", which is true, and
// ScopeMatches treats it as eligible everywhere.
func migrateV12(tx *sql.Tx) error {
	exists, err := columnExists(tx, "memories", "scope")
	if err != nil {
		return err
	}
	if exists {
		return nil // hand-migrated DB already has the column
	}
	if _, err := tx.Exec(`ALTER TABLE memories ADD COLUMN scope TEXT`); err != nil {
		return fmt.Errorf("add memories.scope: %w", err)
	}
	return nil
}

// migrateV13 widens memory_snapshots so a restore returns the same memory
// rather than a lookalike.
//
// Before this the snapshot stored only the text, so a restore re-inserted
// every row with a fresh id. Because memory_embeddings and memory_links both
// reference memories(id) ON DELETE CASCADE, that destroyed the embedding and
// the row's entire link graph while the text came back looking fine, and the
// reset created_at/access_count made decay treat an old memory as new.
//
// Additive. Existing snapshot rows keep NULL memory_id — the ids were never
// recorded and cannot be recovered — so RestoreSnapshot matches those by
// content instead, which is idempotent and can only add a row that is
// missing, never overwrite a live one.
//
// pinned and resolved_at are not added: the snapshot predicate fixes both to
// 0/NULL, so storing them would record a constant. Restore re-reads them from
// the live row, where they are the value that can actually have changed.
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
			continue // hand-migrated DB already has it
		}
		if _, err := tx.Exec(fmt.Sprintf(`ALTER TABLE memory_snapshots ADD COLUMN %s %s`, c.name, c.ddl)); err != nil {
			return fmt.Errorf("add memory_snapshots.%s: %w", c.name, err)
		}
	}
	return nil
}

// migrateV15 adds the builtin source and relabels Ghost's shipped global
// seeds. A pinned manual row is still protected from reflection, but its
// source must not make SessionStart call a Ghost-authored rule the user's own
// preference.
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

	// Keep this data migration separate from the CHECK rebuild so databases
	// that were hand-migrated to the new CHECK still receive the provenance
	// correction. The content literal is frozen with this migration; changing
	// today's seed list must not rewrite historical migration behavior.
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

// the tests that assert it, so a column added to one and forgotten in the
// other cannot pass.
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

// migrateV14 adds memory_snapshots.scope and its scope_captured marker. v13
// gave the snapshot table the identity and provenance columns; scope arrived
// with the memories table in v12 and was simply never carried across, so a
// reflection replace could drop a memory's scope and the restore that should
// have undone it had nowhere to read it from (issue #572).
//
// The two columns are added separately on purpose. A database whose scope
// column an operator added by hand has rows that were never verified against a
// real snapshot, so scope_captured stays 0 for them: restore preserves the live
// scope rather than trusting a backfill, which is the recoverable direction.
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
