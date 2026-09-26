package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// TestOpenReadOnlyTransferStoreRefusesAnUnmigratedDatabase: OpenDBReadOnly runs
// no migrations, and a read-only transfer store used to read no PRAGMA
// user_version, so a database behind the current version failed on its very
// first query with a raw SQLite message naming a column the schema did not have
// yet — projects.repo_remote for v11, memories.scope for v12. `ghost export`
// then said "export projects: no such column: repo_remote" while
// `ghost import --apply`, which goes through bootstrap() and migrates, worked
// fine. `ghost backup` on the same store also worked, because VACUUM INTO reads
// no columns, so a user comparing the three saw the "safe copy" succeed and the
// export fail for no stated reason.
//
// The message is the point of the test, not just the refusal: a raw SQLite
// error names a column, which reads as a Ghost bug rather than as "your store
// is older than this Ghost". So the check must say which side is behind and
// what to do about it.
func TestOpenReadOnlyTransferStoreRefusesAnUnmigratedDatabase(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "config"))

	dataDir := filepath.Join(dir, "ghost")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatalf("mkdir data dir: %v", err)
	}
	dbPath := filepath.Join(dataDir, "ghost.db")

	// A database stamped as an older schema. Built by opening read-write first
	// (the only way to create one) and then rewinding user_version, which is
	// exactly the state a binary upgraded but not yet opened against its own
	// database is in.
	db, err := memory.OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	if _, err := db.Exec(`PRAGMA user_version = 3`); err != nil {
		t.Fatalf("rewind user_version: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	store, err := openReadOnlyTransferStore(dataDir)
	if err == nil {
		store.Close() //nolint:errcheck
		t.Fatal("a read-only transfer store opened a database behind the current schema")
	}
	msg := err.Error()
	// The wrong side is named: the database is behind, not this Ghost ahead of
	// it. "too new" is a different, also-real condition with a different remedy.
	if !strings.Contains(msg, "v3") {
		t.Errorf("error = %v, want it to name the schema version the store is at", err)
	}
	if !strings.Contains(msg, "v"+itoa(memory.SchemaVersion())) {
		t.Errorf("error = %v, want it to name the version this Ghost needs", err)
	}
	// A raw SQLite error would say "no such column"; this must not.
	if strings.Contains(msg, "no such column") {
		t.Errorf("error = %v, want a migration message rather than a raw SQLite column error", err)
	}
	// The remedy, in the shape the missing-database error already uses.
	if !strings.Contains(msg, "start a session") {
		t.Errorf("error = %v, want it to say how to migrate the store", err)
	}
}

// TestOpenReadOnlyTransferStoreAcceptsACurrentDatabase: the check must not
// refuse a store at exactly the version this Ghost writes, which is the one
// case where refusing would be wrong.
func TestOpenReadOnlyTransferStoreAcceptsACurrentDatabase(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "config"))
	dataDir := filepath.Join(dir, "ghost")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatalf("mkdir data dir: %v", err)
	}
	dbPath := filepath.Join(dataDir, "ghost.db")
	db, err := memory.OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	store, err := openReadOnlyTransferStore(dataDir)
	if err != nil {
		t.Fatalf("openReadOnlyTransferStore on a current database: %v", err)
	}
	defer store.Close() //nolint:errcheck
	if _, err := store.ListProjects(context.Background()); err != nil {
		t.Errorf("a current store could not be read: %v", err)
	}
}

// TestReadOnlyTransferStoreIsStrictAboutTheSchemaVersionNotAFloor pins the
// deliberate strictness of requireMigratedSchema.
//
// A floor would be defensible on the numbers: the only columns the transfer
// readers select that were added after v10 are projects.repo_remote (v11) and
// memories.scope (v12); v13 through v16 touch memory_snapshots, the memories
// CHECK list and a repo_remote index, and the task and decision readers select
// only base columns. So a v12-v15 store is fully queryable, `ghost export` on one
// worked before this PR, and a floor would keep it working.
//
// Strict equality is chosen anyway, and the reason is maintenance, not
// conservatism. A floor hardcodes an assumption about which columns exist at
// which version, nothing enforces that assumption, and the day a reader selects a
// column added in v17 the floor would let a v12 store through and reproduce
// exactly the "no such column" error this check exists to replace — silently,
// because nothing in the test suite would be asserting about v12. Strict
// equality is self-maintaining: the store is refused unless it provably has
// every column this build's readers select.
//
// The cost is stated rather than hidden: a v12-v15 user must run one read-write
// open before `ghost export` or a dry-run import will work on their store, where
// before this PR it did — `ghost mcp init`, any session, or `ghost backup`, which
// is reached through bootstrap() and so migrates and seeds rather than reading
// the store, and is refused for a store from a newer Ghost exactly as OpenDB
// refuses it. `ghost backup` is a way to pay this cost, not a way around it. The
// error names the remedy, so the user is not left guessing which command they
// need.
//
// This test is the one that makes the choice visible: it builds a store at v15
// whose every column is present, so the *only* reason to refuse it is the
// version, and it would pass under a floor. If someone later decides a floor is
// right, this fails and the decision gets made on purpose rather than by drift.
func TestReadOnlyTransferStoreIsStrictAboutTheSchemaVersionNotAFloor(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "ghost.db")
	db, err := memory.OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	// v15 on a v16 schema: every column the readers select exists, so the only
	// thing wrong with this store is the number in user_version.
	if _, err := db.Exec(`PRAGMA user_version = 15`); err != nil {
		t.Fatalf("set user_version: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Prove the premise: the store really is queryable, so a floor would let it
	// through and this refusal really is about the version.
	probe, _, _, err := openReadOnlyTransferStoreUnchecked(dir)
	if err != nil {
		t.Fatalf("the premise does not hold — the store could not be opened at all: %v", err)
	}
	if _, err := probe.ListProjects(context.Background()); err != nil {
		probe.Close() //nolint:errcheck
		t.Fatalf("the premise does not hold — a v15 store is not queryable, so a floor would be no safer than equality: %v", err)
	}
	probe.Close() //nolint:errcheck

	_, err = openReadOnlyTransferStore(dir)
	if err == nil {
		t.Fatal("a v15 store was accepted; the check is a floor, not strict equality — if that is intended, this test is the thing to delete deliberately")
	}
	if !strings.Contains(err.Error(), "v15") {
		t.Errorf("error = %v, want it to name the version the store is at", err)
	}
	if !strings.Contains(err.Error(), "migrate it") {
		t.Errorf("error = %v, want it to give the read-write-open remedy", err)
	}
}

// TestReadOnlyTransferStoreStillRefusesAWrongVersionDirection: a database from a
// NEWER Ghost is a different condition and must not be described as "migrate
// it" — migrating backwards is not what the user should do, and OpenDB already
// refuses it.
func TestReadOnlyTransferStoreStillRefusesAWrongVersionDirection(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "ghost.db")
	db, err := memory.OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	if _, err := db.Exec(`PRAGMA user_version = 1000`); err != nil {
		t.Fatalf("bump user_version: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	_, err = openReadOnlyTransferStore(dir)
	if err == nil {
		t.Fatal("a read-only transfer store opened a database from a newer Ghost")
	}
	if !strings.Contains(err.Error(), "newer") {
		t.Errorf("error = %v, want it to say the store is from a newer Ghost", err)
	}
	// Asserted on the ABSENCE of the remedy, not just on the word "newer": the
	// sentence already contains "newer" in "which is newer than this Ghost", so a
	// positive match on it passes whether or not the wrong advice is still there.
	// The remedy is the thing being tested — migrating backwards is not the fix
	// for a user whose store is ahead, and OpenDB refuses that case for them.
	for _, wrong := range []string{"migrate", "start a session", "ghost mcp init"} {
		if strings.Contains(err.Error(), wrong) {
			t.Errorf("error = %v, must not offer %q for a store from a newer Ghost", err, wrong)
		}
	}
	if !strings.Contains(err.Error(), "upgrade Ghost") {
		t.Errorf("error = %v, want it to point at upgrading Ghost", err)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
