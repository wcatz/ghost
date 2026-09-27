//go:build e2e

package e2e

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// v16Fixture is a database as a v16 release left it: the current schema minus
// everything the v17 step adds, stamped at user_version 16.
//
// The fixture is built by asking the BUILT binary to create a store and then
// undoing v17 on it, rather than by carrying a copy of the v16 DDL here. That is
// deliberate in both directions. A frozen copy would drift the moment a
// migration touched a table v16 also has, and the drift would be invisible: the
// fixture would still stamp 16 and still migrate, on a schema no release ever
// shipped. Deriving it from the current schema and removing exactly what the
// v17 step adds is the same construction internal/memory's own migration test
// uses (TestMigrateV17AddsMemoryHistory), and it fails loudly the moment the two
// disagree about what v17 created.
//
// What is verified as a consequence: the store the binary migrates has the
// shape the migration expects, because a fresh store is the product's own.
type v16Fixture struct {
	// memories is the seeded content, keyed by id, so the test can prove each
	// one survived the migration.
	memories map[string]string
	// projects is the project count the fixture created.
	projects int
}

// newV16Store builds a v16 store in a sandbox and returns the sandbox. The
// content is written through the product's own MCP surface first, so the rows
// are the ones a real release would have written rather than hand-inserted ones
// with column defaults nothing else uses.
func newV16Store(t *testing.T, contents []string) *sandbox {
	t.Helper()
	s := newSandbox(t)
	cs := s.mcpSession(t)
	want := map[string]string{}
	for _, content := range contents {
		id := parseID(t, call(t, cs, "ghost_memory_save", map[string]any{
			"project_id": e2eProject,
			"content":    content,
			"category":   "architecture",
			"importance": 0.6,
		}))
		want[id] = content
	}
	// A decision and a task, so the migration has to carry more than memories.
	call(t, cs, "ghost_decision_record", map[string]any{
		"project_id": e2eProject,
		"title":      "A decision the migration must carry",
		"decision":   "the store keeps the history table",
		"rationale":  "an edit destroys the text it replaces",
	})
	call(t, cs, "ghost_task_create", map[string]any{
		"project_id": e2eProject,
		"title":      "a task the migration must carry",
	})

	// Take the history out of the picture and stamp the version back. The
	// history table is what v17 creates, so dropping it is what makes this
	// store a v16 one; the DROP INDEX lines are there because a stale index over
	// a dropped table would make the v17 step's CREATE INDEX a no-op on paper
	// and leave the absence assertion below passing for the wrong reason.
	downgrade(s, t)

	// Read the content back THROUGH the store, and check it is what was written.
	// A fixture that had already lost a row would make the migration assertion
	// below pass for the wrong reason.
	for id, content := range want {
		rows := s.queryStrings(t, `SELECT content FROM memories WHERE id = ?`, id)
		if len(rows) != 1 {
			t.Fatalf("the v16 fixture lost memory %s while being built", id)
		}
		if rows[0] != content {
			t.Fatalf("the v16 fixture changed memory %s: %q", id, rows[0])
		}
	}
	if v := s.userVersion(t); v != 16 {
		t.Fatalf("the v16 fixture is at user_version %d, want 16", v)
	}
	if n := s.queryInt(t, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='memory_history'`); n != 0 {
		t.Fatalf("the v16 fixture still has a memory_history table")
	}
	// The memory count is read here, from the store itself, and not assumed from
	// the seed: the decision above writes a COMPANION memory as well as a
	// decisions row, so "four saves" is not "four memories" and a count derived
	// from the seed would be wrong for a reason that has nothing to do with the
	// migration.
	if n := s.queryInt(t, `SELECT COUNT(*) FROM memories WHERE project_id = ?`, e2eProject); n < len(contents) {
		t.Fatalf("the v16 fixture holds %d project memories, want at least %d", n, len(contents))
	}
	return s
}

// downgrade removes the v17 additions and stamps user_version 16.
func downgrade(s *sandbox, t *testing.T) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(s.dbPath()))
	if err != nil {
		t.Fatalf("open the fixture write-write: %v", err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close() //nolint:errcheck
	for _, stmt := range []string{
		`DROP INDEX IF EXISTS idx_history_memory`,
		`DROP INDEX IF EXISTS idx_history_recorded`,
		`DROP INDEX IF EXISTS idx_provenance_recorded`,
		`DROP TABLE IF EXISTS memory_history`,
		`PRAGMA user_version = 16`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("downgrade the fixture (%s): %v", stmt, err)
		}
	}
	// Every history row went with the table, which is what a v16 store looks
	// like: the change log did not exist yet.
	if n := countInFile(t, s.dbPath(), "SELECT COUNT(*) FROM memories"); n == 0 {
		t.Fatalf("the downgrade emptied the store")
	}
}

// userVersion reads the store's schema version. It is read from the file rather
// than through the product, because the whole point is to see the stamp the
// product wrote without the product interpreting it first.
func (s *sandbox) userVersion(t *testing.T) int {
	t.Helper()
	var v int
	if err := s.openDB(t).QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		t.Fatalf("read user_version: %v", err)
	}
	return v
}

// TestUpgradeFromV16 runs the built binary against a v16 store and checks the
// whole upgrade: the pre-migration backup, the migration itself, and that every
// memory survived it.
func TestUpgradeFromV16(t *testing.T) {
	contents := []string{
		"the relay listens on port 2222 in staging",
		"the sqlite store pins MaxOpenConns to one connection",
		"the release process signs the tag before publishing",
		"the sqlite store pins the connection pool to a single connection",
	}
	s := newV16Store(t, contents)

	before := map[string]string{}
	for _, row := range s.queryStrings(t, `SELECT id || '|' || content FROM memories WHERE project_id = ? ORDER BY id`, e2eProject) {
		id, content, _ := strings.Cut(row, "|")
		before[id] = content
	}
	if len(before) < len(contents) {
		t.Fatalf("the v16 store holds %d project memories, want at least %d", len(before), len(contents))
	}

	// A read-write open migrates. `ghost history` is the cheapest one that goes
	// through the ordinary bootstrap, so the migration under test is the one
	// every command takes and not a special path.
	hist := s.mustRun("history", sortedKeys(before)[0])
	mustMatch(t, "history against the migrated store", hist.stdout+hist.stderr, "(?i)memory|history|version")

	if v := s.userVersion(t); v != 17 {
		t.Fatalf("after the open the store is at user_version %d, want 17 (the current schema)", v)
	}

	t.Run("every memory survived", func(t *testing.T) {
		after := map[string]string{}
		for _, row := range s.queryStrings(t, `SELECT id || '|' || content FROM memories WHERE project_id = ? ORDER BY id`, e2eProject) {
			id, content, _ := strings.Cut(row, "|")
			after[id] = content
		}
		if len(after) != len(before) {
			t.Fatalf("the store holds %d memories after the migration, want %d", len(after), len(before))
		}
		for id, content := range before {
			if after[id] != content {
				t.Fatalf("memory %s is %q after the migration, want %q", id, after[id], content)
			}
		}
		// And the rows are still reachable through the product's own surfaces,
		// which is the only way to know the migration left them queryable rather
		// than merely present.
		out := call(t, s.mcpSession(t), "ghost_project_context", map[string]any{"project_id": e2eProject})
		for _, content := range contents {
			if !strings.Contains(out, content) {
				t.Fatalf("the migrated memory %q is not in the project context", content)
			}
		}
	})

	t.Run("the other record types survived", func(t *testing.T) {
		if n := s.queryInt(t, `SELECT COUNT(*) FROM tasks WHERE project_id = ?`, e2eProject); n != 1 {
			t.Fatalf("the migrated store holds %d tasks, want 1", n)
		}
		if n := s.queryInt(t, `SELECT COUNT(*) FROM decisions WHERE project_id = ?`, e2eProject); n != 1 {
			t.Fatalf("the migrated store holds %d decisions, want 1", n)
		}
		if n := s.queryInt(t, `SELECT COUNT(*) FROM projects WHERE id = ?`, e2eProject); n != 1 {
			t.Fatalf("the migrated store holds %d project rows, want 1", n)
		}
	})

	t.Run("the pre-migration backup was taken", func(t *testing.T) {
		// A store behind is backed up before it is migrated, so a migration that
		// loses data is recoverable. The backup is the user's copy of the
		// pre-upgrade database, and without it the upgrade is irreversible.
		backups := backupsIn(t, s.dataDir())
		if len(backups) == 0 {
			t.Fatalf("the migration took no pre-migration backup in %s", s.dataDir())
		}
		// And the backup is a real database a restore could use: every memory,
		// with the text it held before the migration.
		//
		// The backup is taken after initSQL has run and before migrate() has,
		// so it already carries the tables initSQL creates — including
		// memory_history, which arrives empty. That is a property of the
		// ordering, not a defect: initSQL is CREATE ... IF NOT EXISTS
		// throughout, so it cannot have altered any row, and the copy is a
		// faithful restore point either way. The assertion is therefore on the
		// DATA, which is what a rollback actually needs, and not on the table
		// list.
		b := backups[len(backups)-1]
		if n := countInFileWhere(t, b, "SELECT COUNT(*) FROM memories WHERE project_id = ?", e2eProject); n < len(contents) {
			t.Fatalf("the pre-migration backup holds %d project memories, want at least %d", n, len(contents))
		}
		restored := stringsInFile(t, b, `SELECT content FROM memories WHERE project_id = ?`, e2eProject)
		for _, content := range contents {
			found := false
			for _, text := range restored {
				if text == content {
					found = true
				}
			}
			if !found {
				t.Fatalf("the pre-migration backup does not hold %q; it holds %v", content, restored)
			}
		}
		// The copy is openable, so it is a database and not a truncated file.
		if n := countInFile(t, b, "SELECT COUNT(*) FROM projects"); n < 2 {
			t.Fatalf("the pre-migration backup holds %d project rows, want the seeded one plus _global", n)
		}
	})

	t.Run("the history table works after the migration", func(t *testing.T) {
		// A v16 memory arrives with no history row, so the first write that
		// touches one records a baseline of what it read. Without that baseline
		// the first edit would leave the old wording unrecoverable, which is the
		// whole reason the table exists — so it is asserted, not assumed.
		// The memory is named by what it SAYS, not by the first id in sort
		// order: the assertion below is about a specific pre-edit text, and
		// editing whichever row happened to sort first would make that text
		// depend on hex ordering.
		id := memoryWithContent(t, s, "port 2222 in staging")
		call(t, s.mcpSession(t), "ghost_memory_update", map[string]any{
			"project_id": e2eProject,
			"memory_id":  id,
			"content":    "the relay listens on port 2222 in production",
		})
		phases := s.queryStrings(t, `SELECT phase FROM memory_history WHERE memory_id = ? ORDER BY recorded_at, rowid`, id)
		if len(phases) == 0 {
			t.Fatalf("the first edit of a migrated memory recorded no history")
		}
		if phases[0] != "baseline" {
			t.Fatalf("the first history phase is %q, want baseline — without it the pre-edit text is unrecoverable", phases[0])
		}
		// The baseline has to hold the ORIGINAL text, which is the whole point.
		texts := s.queryStrings(t, `SELECT COALESCE(content, '') FROM memory_history WHERE memory_id = ?`, id)
		found := false
		for _, text := range texts {
			if strings.Contains(text, "staging") {
				found = true
			}
		}
		if !found {
			t.Fatalf("no history row holds the pre-edit text: %v", texts)
		}
		// And the history command reads it back.
		hist := s.mustRun("history", id)
		mustContain(t, "history after the migration", hist.stdout, "staging")
		mustContain(t, "history after the migration", hist.stdout, "production")
	})

	t.Run("a second open is a no-op", func(t *testing.T) {
		// The migration is idempotent, so a second run neither re-migrates nor
		// takes a second backup — the store is current and stays that way.
		countBefore := len(backupsIn(t, s.dataDir()))
		s.mustRun("history", sortedKeys(before)[0])
		if v := s.userVersion(t); v != 17 {
			t.Fatalf("the second open left the store at user_version %d", v)
		}
		if got := len(backupsIn(t, s.dataDir())); got != countBefore {
			t.Fatalf("a second open took another migration backup (%d -> %d)", countBefore, got)
		}
	})
}

// TestUpgradeRefusesANewerStore covers the other direction: a store from a
// build that knows more than this one must be refused BEFORE any DDL runs
// against it. initSQL is CREATE ... IF NOT EXISTS, so on a newer store it is
// nearly a no-op — which is exactly what hid the ordering — but any object the
// newer build renamed or dropped would be recreated in a store this build then
// declares unreadable. A refusal that has already written is not a refusal.
func TestUpgradeRefusesANewerStore(t *testing.T) {
	s := newSandbox(t)
	call(t, s.mcpSession(t), "ghost_memory_save", map[string]any{
		"project_id": e2eProject,
		"content":    "a memory in a store from the future",
	})

	// Stamp a version this build does not know, and add an object a future
	// build might have dropped — so a refusal that ran the DDL would leave
	// evidence behind.
	future := 9999
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(s.dbPath()))
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	if _, err := db.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, future)); err != nil {
		t.Fatalf("stamp the future version: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE future_only_table (id TEXT PRIMARY KEY)`); err != nil {
		t.Fatalf("create the future-only table: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Every READ-WRITE open has to refuse it. One would be enough to prove the
	// rule; both are here because they take different routes into the store, and
	// a route that reached OpenDB without the check would be a real hole.
	//
	// A read-only command is deliberately absent from this list. `ghost obsidian
	// export` opens its own mode=ro handle rather than going through OpenDB, so
	// it reads a future store happily — which is correct, and is asserted below:
	// refusing to SHOW a user their own data because this build cannot write it
	// would be a worse failure than the one the rule prevents.
	for _, args := range [][]string{
		{"history", "whatever"},
		{"backup"},
	} {
		r := s.mustFail(args...)
		mustMatch(t, "refusing a newer store via `ghost "+strings.Join(args, " ")+"`",
			r.stderr, "(?i)newer|version")
	}

	// The refusal wrote nothing: the future-only table is still there, and the
	// version is still the one the future build stamped.
	if n := countInFile(t, s.dbPath(),
		"SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='future_only_table'"); n != 1 {
		t.Fatalf("the refusal dropped the future-only table — it ran DDL against a store it cannot read")
	}
	if v := s.userVersion(t); v != future {
		t.Fatalf("the refusal re-stamped the version to %d, want the untouched %d", v, future)
	}
	// Scoped to the project, because a store also holds the builtin global
	// memories the seed writes and counting those would make this assertion
	// about Ghost's own rows.
	if n := countInFileWhere(t, s.dbPath(),
		"SELECT COUNT(*) FROM memories WHERE project_id = ?", e2eProject); n != 1 {
		t.Fatalf("the refusal disturbed the store's memories")
	}

	// The read-only paths refuse it too, and the refusal is SPECIFIC: the
	// transfer readers select columns whose existence is tied to the schema
	// version, so a store from a future build may not have them, and
	// requireMigratedSchema is strict on both sides — equal and only equal
	// passes. What matters here is that the message tells the user which
	// situation they are in. The remedy sentence ("migrate") belongs to the
	// BEHIND case, because a store from a newer Ghost is not fixed by
	// migrating this binary; saying "run a session" to someone whose problem is
	// the opposite would send them the wrong way.
	export := s.mustFail("export", "--out", filepath.Join(s.t.TempDir(), "a.jsonl"))
	mustMatch(t, "export from a newer store", export.stderr, "(?i)newer|version")
	mustNotContain(t, "the newer-store refusal", export.stderr, "migrate ghost")

	// The vault mirror takes its own mode=ro handle and does NOT consult the
	// version, so it reads a future store happily. That is asserted rather than
	// left implicit, because it is a real difference between two read-only
	// surfaces and the next person to touch the version check needs to know it:
	// the mirror's queries are hand-written in internal/mcpinit and select
	// columns per host-event path, not the transfer readers' version-checked
	// set, so there is nothing there for requireMigratedSchema to be right
	// about. It reads what it selects and reports the store as it finds it.
	mirror := s.mustRun("obsidian", "export", "--out", filepath.Join(s.t.TempDir(), "vault"), "--project", e2eProject)
	mustContain(t, "obsidian export from a newer store", mirror.stdout, "vault")
}

// backupsIn returns the pre-migration backup files in a data directory, oldest
// first. The naming convention is the product's, so a change to it would make
// this return nothing — which fails the test rather than quietly passing on an
// empty list, because the caller compares lengths.
func backupsIn(t *testing.T, dataDir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dataDir)
	if err != nil {
		t.Fatalf("read %s: %v", dataDir, err)
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		// The product's own naming: <db>.pre-migrate-<unix>. Matching on
		// "backup" alone would find nothing, and matching loosely enough to catch
		// it by accident would also catch an unrelated file — so the marker is
		// the prefix the migration writes.
		if !strings.Contains(name, ".pre-migrate-") {
			continue
		}
		// The -wal and -shm siblings of a live database are not backups.
		if strings.HasSuffix(name, "-wal") || strings.HasSuffix(name, "-shm") {
			continue
		}
		out = append(out, filepath.Join(dataDir, name))
	}
	sort.Strings(out)
	return out
}
