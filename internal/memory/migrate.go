package memory

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
)

// schemaVersion is the current schema version, stamped into PRAGMA user_version.
// Bump it and append to migrations whenever initSQL changes in a way that
// CREATE TABLE IF NOT EXISTS cannot deliver to existing databases (new columns,
// CHECK values, foreign keys, dropped tables).
const schemaVersion = 19

// SchemaVersion returns the schema version this build of Ghost expects, which is
// the value a fully migrated database carries in PRAGMA user_version.
//
// It is exported for the callers that open a store read-only and so cannot make
// it current: they need to know the floor the columns they select are at, and
// they cannot discover it without asking. See DBUserVersion for the other half.
func SchemaVersion() int { return schemaVersion }

// DBUserVersion reads PRAGMA user_version off an open database, which is the
// schema version a store is actually at.
//
// It is a plain read, so it works on the read-only connection a preview or an
// export holds, and it deliberately does not migrate: the caller opening
// read-only has already decided not to write, and a version check that repaired
// the store would be the read doing the write.
func DBUserVersion(db *sql.DB) (int, error) { return dbUserVersion(db) }

// dbUserVersion is DBUserVersion over a Queryer, for a caller that is INSIDE a read
// transaction. This is not a convenience: the pool is pinned at MaxOpenConns(1), so
// an open transaction holds the only connection and a PRAGMA issued on the pool
// while it is open waits for a connection that cannot be handed out — a deadlock
// rather than an error. A caller holding a snapshot therefore has to read the
// version through it, and that is the only way this can be asked at all.
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
	migrateV16,
	migrateV17,
	migrateV18,
	migrateV19,
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
//
// memory_provenance is in the list on this table's own definition rather than on
// convenience: its rows are claims ABOUT a memory, its foreign key cascades, and a
// live store therefore cannot hold one whose memory is gone. An orphan here is
// debris a correct delete would already have removed, and treating it as user
// content would mean every later migration aborts on it — bricking every build
// newer than the delete that left it, over rows that mean nothing.
var cacheFKWhitelist = map[string]bool{
	"memory_embeddings": true,
	"link_scans":        true,
	"supersede_checked": true,
	"memory_links":      true,
	"memory_provenance": true,
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
// NULL is also what a later write with nothing to say writes: the tools'
// validity and provenance arguments are all optional, and a caller that
// states no window, no reference and no confidence gets the same record a
// pre-v10 row has. That is the point of the columns being nullable — a
// migration is where a fabricated value would be least excusable, since
// there is no caller to have asked.
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

// migrateV16 makes normalized repository identity unique. Older databases may
// already contain duplicate remotes; merge those rows before creating the
// partial unique index so the invariant is established without losing project
// data.
func migrateV16(tx *sql.Tx) error {
	exists, err := columnExists(tx, "projects", "repo_remote")
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}
	// _global must not carry a repository identity, ever. Older builds wrote one
	// whenever EnsureProjectWithRepo was called for that id, so a real database
	// can hold it, and a lookup that matched would resolve a checkout to the
	// bucket injected into every project. Clearing is the recoverable direction:
	// it is one value on one row, and nothing reads it. Failing the migration
	// instead would brick a working install over a field that is about to be
	// ignored anyway.
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
	// A duplicate group that includes _global cannot be merged by this step: the
	// merge primitive refuses every merge involving _global, because it is the
	// bucket global injection reads from rather than a project. Refuse the
	// migration with the group named instead of picking a merge order that
	// happens to leave _global alone — the order is a guess, and the guess moves
	// somebody's whole corpus if it is wrong.
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

// phase1aProvenanceColumns is the v10 column set, shared by migrateV10 and
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

// tableInspector is the read surface a schema precondition can be checked
// against, satisfied by a migration's *sql.Tx and by the *sql.DB OpenDB holds
// before it migrates. It exists so ONE check can live in the step that owns the
// precondition and also run on the open path, where running it early is what
// makes the refusal cheap and legible — see refuseForeignProvenanceTable.
type tableInspector interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// columnExists reports whether table has a column named column, matching
// case-insensitively — SQLite itself treats column identifiers as
// case-insensitive, so a hand-migrated RESOLVED_AT column must be recognized
// as the same column resolved_at names, not trigger a duplicate ALTER that
// SQLite would then reject.
//
// The handle is a tableInspector rather than a *sql.Tx so the same check serves
// the open path; every existing caller passes a transaction and is unaffected.
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

// migrateV17 adds memory_history, the append-only per-memory change log
// (schema v17, issue #578). The step creates the table under this name, and the
// name is part of what it ships: `memory_provenance` is reserved for a different,
// later concept (evidence records — several per memory, answering "who or what
// supports this memory"), which this change log is not. v17 has never been
// released, so the step itself carries the right name rather than renaming the
// table after the fact.
//
// The DDL is CREATE ... IF NOT EXISTS throughout, so a database an operator has
// already created the table in is stamped without harm — the step has no data to
// correct, and no backfill is attempted.
//
// No backfill is a deliberate omission with a cost, not an oversight. Every
// pre-v17 memory reaches this version with NO history row, so the first write
// that touches one records a baseline of the state it read (see
// recordBaselineHistoryTx) rather than only the state it produced: an edit is the
// one write that destroys the text, and without the baseline a v16 memory's
// first edit leaves the old wording unrecoverable. A backfill here would be the
// other way to close that gap — one INSERT ... SELECT over memories — and was not
// done because it writes a claim ("this is what it said when v17 arrived") about
// rows whose real age and authorship nobody recorded, and because it would run on
// every store with a large corpus during the open that also takes the
// pre-migration backup.
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
		// One index, and the same one initSQL creates. A standalone
		// recorded_at index has no reader — the per-memory cap ranks by rowid and
		// an as_of read filters on memory_id — while every append would pay a
		// second b-tree insert to keep it current, inside the write transaction.
		// Creating it here anyway would have left every UPGRADED store with two
		// while every fresh one had one, which is the sort of difference only a
		// test comparing the two paths would catch.
		`CREATE INDEX IF NOT EXISTS idx_history_memory ON memory_history(memory_id, recorded_at)`,
	}
	for _, stmt := range stmts {
		if _, err := tx.Exec(stmt); err != nil {
			return fmt.Errorf("%q: %w", stmt[:min(40, len(stmt))], err)
		}
	}
	return nil
}

// migrateV18 adds memory_provenance, the append-only EVIDENCE table (schema v18,
// issue #673). The step creates the table under the name that was reserved for
// exactly this concept: several rows per memory, one per observation, answering
// "who or what supports this memory" — which memory_history, the change log v17
// added, does not answer at all.
//
// The seed is one INSERT ... SELECT over memories, and it is deliberately
// conditional. A memory that recorded none of agent, session_id, source_ref or
// confidence gets NO row: an evidence row with nothing in it still says "this
// was observed", and for a memory nobody ever attributed there was no
// observation to record. Backfilling a row for it would make every memory in a
// store report one observation, which is the same fabrication as inventing a
// session id — a count that means nothing, made to look like support.
//
// verified_at comes across when the memory holds one, because a memory a human
// verified by hand is corroborated and dropping the stamp would understate what
// the store already knows. It does not on its own justify a row: the stamp is
// still on the memory, and the row exists to carry a provenance claim.
//
// observed_at is left NULL by the seed and is nullable for the same reason
// memories' own provenance columns are (migrateV10): the migration can say what
// the columns hold now, and cannot say when the fact was first observed. Stamping
// the migration's own clock would be a claim about the past that nobody made.
//
// The seed is idempotent through NOT EXISTS, so a database an operator has
// already created and partly populated the table in is stamped without doubling
// its rows. Like every step it runs with foreign_keys=OFF, so the seed does not
// depend on the pragma for its own consistency — and the cascade it installs is
// the store's, not the migration's.
func migrateV18(tx *sql.Tx) error {
	// A memory_provenance table can already exist under this name, from a build
	// between two commits that used the name for the CHANGE LOG (#664's pre-rename
	// shape). initSQL's CREATE INDEX is a no-op against it — the change log's
	// memory_id is there too — so this check is what stands between that table and
	// an INSERT ... SELECT that fails with SQLite's "no such column: kind", which
	// rolls the step back and leaves the operator with a store that will not open
	// and an error that does not name the way out.
	//
	// It REFUSES rather than adapts. The rows in such a table are a dev build's
	// change log under the wrong name; there is no shape to convert them into, no
	// release ever wrote one, and the pre-migration backup OpenDB has already taken
	// is the safety net a conversion would be guessing past. The remedy is the one
	// line this build has always documented, and it is stated rather than logged:
	// a store that cannot be opened is the operator's to fix, and a warning they
	// may not see would leave every writer failing on the same missing column.
	//
	// What it checks is IDENTITY, not version -- see evidenceTableIdentity -- so an
	// older shape of the real table is left alone with its evidence intact, and the
	// column it lacks is reported at the statement that reads it: a repairable
	// message about one column, rather than a DROP of a table full of records.
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
		// One index, for the reason initSQL's says: the reads filter on
		// memory_id, and a carried_from index would be a b-tree insert on every
		// consolidation carry that no query asks for.
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

// migrateV19 adds the retention tier and its expiry (schema v19, issue #587).
//
// Both columns arrive with their DEFAULTs and no UPDATE touches a single row,
// which is the property this step is judged on: a store that has been running
// for a year has rows whose created_at, importance and pin state are load-bearing
// for decay, for the reflection drop guard and for the session-start ordering,
// and the only value this migration may have an opinion about is the one nothing
// has ever set. Adding retention NOT NULL DEFAULT 'project' gives every existing
// row the tier it has in fact always had — a memory that persisted until
// somebody resolved it was a project memory — without rewriting it, and the
// DEFAULT covers every writer that never mentions the column at all.
//
// The alternative, backfilling an expiry for rows nobody classified, would be
// writing a claim ("this stops being wanted on the 28th") about a conversation
// that is not this build's, and the only writer of expires_at is the save that
// stores a session row.
//
// The CHECK is spelled here rather than read from initSQL for the reason every
// step carries its own DDL: this step is frozen, and initSQL keeps moving. The
// index is created here too, and for the same reason the v17 step creates its
// own: a store that already had rows at this version must not end up with a
// different schema from a fresh install, and a partial index is what keeps a
// prune off a full scan of a large corpus.
//
// Idempotent through the column probes, so a database an operator has already
// added one of these columns to is stamped rather than failing the open.
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

// refuseForeignProvenanceTable returns an error naming the remedy when the
// database already holds a `memory_provenance` table that is NOT Ghost's evidence
// table: a development build between two commits used the reserved name for the
// change log (#664's pre-rename shape), and its table has a `memory_id` like the
// evidence table does — so `initSQL`'s `CREATE INDEX` succeeds against the wrong
// table and nothing before the seed catches it. Left alone, the seed fails with
// SQLite's "no such column: kind": a rolled-back step and a store that will not
// open.
//
// It is called from TWO places, and the second is the point:
//
//   - from `migrateV18`, so the step that owns the precondition is safe on its
//     own and does not depend on its caller having checked;
//   - from `OpenDB`, BEFORE `backupBeforeMigrate`. A refusal that arrives after a
//     full `VACUUM INTO` copy is a refusal that costs a copy every time, because
//     the condition is permanent (nothing converts that table; the operator drops
//     it) and leaves `user_version` behind, so every later open re-enters the
//     backup path — and two opens in the same wall-clock second collide on the
//     copy's name, so the retry an operator is guaranteed to make (the hook and
//     the MCP server both open on session start) reports "refusing to overwrite an
//     existing file" and names neither the table nor the remedy. That is the
//     diagnosability this check exists to provide, defeated on the most likely
//     retry.
//
// It refuses rather than adapts. The rows in such a table are a dev build's
// change log under the wrong name, there is no shape to convert them into, no
// release ever wrote one, and the pre-migration copy is the net a conversion would
// be guessing past.
//
// It checks IDENTITY rather than version, so an older shape of the real table is
// left alone with its evidence intact — see evidenceTableIdentity for why the
// column list is deliberately not the whole set.
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

// evidenceTableIdentity is the smallest column set that says "this is Ghost's
// evidence table", and it is all refuseForeignProvenanceTable checks.
//
// `kind` alone settles it: no shape of the change log has a kind column, and that
// is the only other thing this name has ever been used for. The list is
// deliberately NOT the full column set, because a probe that compared the full set
// would be a VERSION check wearing an identity check's clothes — the next schema
// change that adds a column to this table would then refuse every store already
// at that version, and the remedy that refusal names is `DROP TABLE
// memory_provenance`, a command that destroys real evidence records. A build must
// never answer "that is the wrong table" about a table it merely knows an older
// shape of; a column it lacks is its own error, reported by the statement that
// reads it, which is a repairable message about one column.
var evidenceTableIdentity = []string{"id", "memory_id", "kind"}

// tableExists reports whether a table of that name is in the schema.
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
