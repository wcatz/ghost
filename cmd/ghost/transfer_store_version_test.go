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

// TestOpenReadOnlyTransferStoreAcceptsACurrentDatabase: the version check must
// not refuse a store this Ghost would have created, and must not refuse one a
// little behind either if the columns the readers need are present — the
// version is a floor, not a gate on equality.
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
		t.Errorf("error = %v, want it to say the store is from a newer Ghost rather than ask to migrate", err)
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
