package mcpinit

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// storeAtOlderSchema builds a store and stamps it behind this build, so an
// ordinary read-WRITE open would MIGRATE it — take a pre-migration backup and
// rewrite the schema. A read-only run must leave it exactly as it is, and that
// is what the tests below measure rather than assert from prose.
func storeAtOlderSchema(t *testing.T) (dataHome, dataDir, dbPath string, before []byte) {
	t.Helper()
	root := t.TempDir()
	dataHome = filepath.Join(root, "data")
	dataDir = filepath.Join(dataHome, "ghost")
	dbPath = storeAt(t, dataDir)
	stampToV17(t, dbPath)
	before, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatalf("read the store: %v", err)
	}
	return dataHome, dataDir, dbPath, before
}

// assertStoreUnmigrated is the property both commands must have: the schema
// stamp and the file bytes are exactly what they were, and no pre-migration
// copy appeared beside the store.
func assertStoreUnmigrated(t *testing.T, dbPath, dataDir string, before []byte, wantVersion int) {
	t.Helper()
	after, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatalf("re-read the store: %v", err)
	}
	if !bytes.Equal(after, before) {
		t.Error("the store's bytes changed: the command migrated it")
	}
	db, err := memory.OpenReadDB(dbPath)
	if err != nil {
		t.Fatalf("open the store read-only: %v", err)
	}
	defer db.Close() //nolint:errcheck
	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatalf("read user_version: %v", err)
	}
	if version != wantVersion {
		t.Errorf("user_version = %d, want %d (the command migrated the store)", version, wantVersion)
	}
	if got := preMigrateBackupsIn(t, dataDir); len(got) != 0 {
		t.Errorf("the command wrote pre-migration backups: %v", got)
	}
}

// TestDryRunImportLeavesAnOlderStoreUnmigrated pins the issue's first half: a
// preview that prints "No changes made" must not run the migrations an
// ordinary open runs, or the preview is the irreversible write it promised not
// to be.
func TestDryRunImportLeavesAnOlderStoreUnmigrated(t *testing.T) {
	dataHome, dataDir, dbPath, before := storeAtOlderSchema(t)
	t.Setenv("XDG_DATA_HOME", dataHome)

	var out bytes.Buffer
	if _, err := importMemories(&out, true); err != nil {
		t.Fatalf("importMemories dry run: %v", err)
	}
	assertStoreUnmigrated(t, dbPath, dataDir, before, 17)
	if !strings.Contains(out.String(), "does not migrate") {
		t.Errorf("the dry run did not say it skipped the migration; it printed:\n%s", out.String())
	}
}

// TestStatusLeavesAnOlderStoreUnmigrated pins the second half: a status run is
// a diagnostic, so it reports on the store through a read-only handle and never
// upgrades what it is reporting on.
func TestStatusLeavesAnOlderStoreUnmigrated(t *testing.T) {
	statusEnv(t)
	dataHome, dataDir, dbPath, before := storeAtOlderSchema(t)
	t.Setenv("XDG_DATA_HOME", dataHome)

	var out bytes.Buffer
	if _, err := Status(&out); err != nil {
		t.Fatalf("Status = %v, want a report rather than an error", err)
	}
	assertStoreUnmigrated(t, dbPath, dataDir, before, 17)
}

// TestDryRunImportCreatesNoDataDirWithoutAStore is the no-store half: resolving
// the data directory for a preview must not be the thing that creates it.
func TestDryRunImportCreatesNoDataDirWithoutAStore(t *testing.T) {
	dataHome := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dataHome)
	dataDir := filepath.Join(dataHome, "ghost")

	var out bytes.Buffer
	if _, err := importMemories(&out, true); err != nil {
		t.Fatalf("importMemories dry run: %v", err)
	}
	if _, err := os.Stat(dataDir); !os.IsNotExist(err) {
		t.Errorf("a dry-run import created the data directory (stat error: %v)", err)
	}
}

// TestStatusCreatesNoDataDirWithoutAStore is the same property for the
// diagnostic: a status run on a machine with no store reports that and leaves
// nothing behind — not even the directory.
func TestStatusCreatesNoDataDirWithoutAStore(t *testing.T) {
	statusEnv(t)
	dataHome := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dataHome)
	dataDir := filepath.Join(dataHome, "ghost")

	var out bytes.Buffer
	if _, err := Status(&out); err != nil {
		t.Fatalf("Status = %v, want a report rather than an error", err)
	}
	if _, err := os.Stat(dataDir); !os.IsNotExist(err) {
		t.Errorf("a status run created the data directory (stat error: %v)", err)
	}
	if !strings.Contains(out.String(), "no Ghost database") {
		t.Errorf("status did not report the absent database; it printed:\n%s", out.String())
	}
}
