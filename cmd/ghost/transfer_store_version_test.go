package main

import (
	"context"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// dbDirAtVersion returns a data directory holding a current-schema database
// whose user_version was then rewritten to version (a binary upgraded but not yet
// opened against its own database).
func dbDirAtVersion(t *testing.T, version int) string {
	t.Helper()
	dir := t.TempDir()
	db, err := memory.OpenDB(filepath.Join(dir, "ghost.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	if version > 0 {
		if _, err := db.Exec(`PRAGMA user_version = ` + strconv.Itoa(version)); err != nil {
			t.Fatalf("set user_version: %v", err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return dir
}

// TestOpenReadOnlyTransferStoreRefusesAnUnmigratedDatabase: OpenReadDB runs no
// migrations, so a database behind the current version used to fail on its first
// query with a raw "no such column" (projects.repo_remote, memories.scope). The
// refusal must say which side is behind and how to migrate.
func TestOpenReadOnlyTransferStoreRefusesAnUnmigratedDatabase(t *testing.T) {
	store, err := openReadOnlyTransferStore(dbDirAtVersion(t, 3), exportOperation)
	if err == nil {
		store.Close() //nolint:errcheck
		t.Fatal("a read-only transfer store opened a database behind the current schema")
	}
	for _, want := range []string{"v3", "v" + strconv.Itoa(memory.SchemaVersion()), "start a session"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v, want it to contain %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "no such column") {
		t.Errorf("error = %v, want a migration message rather than a raw SQLite column error", err)
	}
}

// TestOpenReadOnlyTransferStoreAcceptsACurrentDatabase: a store at exactly the
// version this Ghost writes is never refused.
func TestOpenReadOnlyTransferStoreAcceptsACurrentDatabase(t *testing.T) {
	store, err := openReadOnlyTransferStore(dbDirAtVersion(t, 0), exportOperation)
	if err != nil {
		t.Fatalf("openReadOnlyTransferStore on a current database: %v", err)
	}
	defer store.Close() //nolint:errcheck
	if _, err := store.ListProjects(context.Background()); err != nil {
		t.Errorf("a current store could not be read: %v", err)
	}
}

// TestReadOnlyTransferStoreIsStrictAboutTheSchemaVersionNotAFloor pins the
// deliberate strictness of requireMigratedSchema: a v15 store has every column
// the readers select (so a floor would let it through), but strict equality is
// self-maintaining, where a floor hardcodes which columns exist at which version
// and silently reproduces "no such column" the day a reader selects a newer one.
// The refusal names the version and the migrate remedy and, for export and
// dry-run import, keeps its documented wording to the character.
func TestReadOnlyTransferStoreIsStrictAboutTheSchemaVersionNotAFloor(t *testing.T) {
	dir := dbDirAtVersion(t, 15)
	probe, _, _, err := openReadOnlyTransferStoreUnchecked(dir)
	if err != nil {
		t.Fatalf("the premise does not hold; the store could not be opened at all: %v", err)
	}
	_, listErr := probe.ListProjects(context.Background())
	probe.Close() //nolint:errcheck
	if listErr != nil {
		t.Fatalf("the premise does not hold; a v15 store is not queryable: %v", listErr)
	}

	_, err = openReadOnlyTransferStore(dir, exportOperation)
	if err == nil {
		t.Fatal("a v15 store was accepted; the check is a floor, not strict equality")
	}
	const wantSuffix = "to migrate it before exporting it or previewing an import into it"
	if !strings.Contains(err.Error(), "v15") || !strings.Contains(err.Error(), "migrate it") || !strings.HasSuffix(err.Error(), wantSuffix) {
		t.Errorf("error = %v, want the version, the remedy, and the suffix %q", err, wantSuffix)
	}
}

// TestReadOnlyTransferStoreStillRefusesAWrongVersionDirection: a database from a
// NEWER Ghost must not be told to "migrate it" (OpenDB refuses migrating
// backwards); the absence of the remedy is what is asserted.
func TestReadOnlyTransferStoreStillRefusesAWrongVersionDirection(t *testing.T) {
	_, err := openReadOnlyTransferStore(dbDirAtVersion(t, 1000), exportOperation)
	if err == nil || !strings.Contains(err.Error(), "newer") || !strings.Contains(err.Error(), "upgrade Ghost") {
		t.Fatalf("error = %v, want a newer-Ghost refusal pointing at upgrading", err)
	}
	for _, wrong := range []string{"migrate", "start a session", "ghost mcp init"} {
		if strings.Contains(err.Error(), wrong) {
			t.Errorf("error = %v, must not offer %q for a store from a newer Ghost", err, wrong)
		}
	}
}
