package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// TestOpenImportStoreOpensADryRunReadOnly: a dry run has to be a way of looking
// at what an import would do, and bootstrap() is a read-write open. That is not
// a neutral way to look: memory.OpenDB runs migrations and stamps user_version,
// and bootstrap's caller then seeds the builtin global rows. So a dry run reached
// through it could migrate a database whose schema is behind and insert rows
// while reporting "nothing written" — a preview that changes the thing it is
// previewing.
//
// The assertion is that the connection refuses a write, which is the property
// rather than the spelling of it: an open that cannot write cannot migrate,
// cannot seed, and cannot have written anything.
func TestOpenImportStoreOpensADryRunReadOnly(t *testing.T) {
	// A temp data dir standing in for the real one, so this test can neither
	// read nor write the developer's store.
	dir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "config"))

	// A database that exists, so the read-only open has something to open. It is
	// built through the read-write path, which is the only way to create one.
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

	// The project has to exist first, or the write below would fail on a foreign
	// key and prove nothing about the connection's mode.
	seed, err := memory.OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	seedStore := memory.NewStore(seed, nil)
	if err := seedStore.EnsureProject(context.Background(), "p1", "/src/p1", "p1"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	if err := seedStore.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	store, err := openImportStore(false)
	if err != nil {
		t.Fatalf("openImportStore(dry run): %v", err)
	}
	defer store.Close() //nolint:errcheck

	ctx := context.Background()
	// A write that would otherwise succeed: a valid project, a valid memory.
	// The only thing that can stop it is the connection's own mode.
	if _, err := store.Create(ctx, "p1", memory.Memory{
		Category: "fact", Content: "a dry run must not be able to write this", Source: "mcp",
	}); err == nil {
		t.Error("a dry-run store accepted a write: the import preview can change the store it previews")
	} else if !strings.Contains(strings.ToLower(err.Error()), "readonly") &&
		!strings.Contains(strings.ToLower(err.Error()), "read-only") {
		t.Errorf("the write failed with %v, which is not the read-only refusal the test is about", err)
	}

	// And the read the dry run does need still works, so "read-only" is not
	// "broken".
	if _, err := store.ListProjects(ctx); err != nil {
		t.Errorf("a dry-run store could not read: %v", err)
	}
}

// TestOpenImportStoreNamesAMissingDatabase: a read-only open cannot create the
// file, so a dry run on a machine with no store has to say so in the actionable
// way rather than reporting a SQLite open error.
func TestOpenImportStoreNamesAMissingDatabase(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "config"))

	_, err := openImportStore(false)
	if err == nil {
		t.Fatal("a dry run against a machine with no database must report it")
	}
	if !strings.Contains(err.Error(), "ghost.db") || !strings.Contains(err.Error(), "start a session") {
		t.Errorf("error = %v, want it to name the missing database and the next step", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "ghost", "ghost.db")); statErr == nil {
		t.Error("a dry run created the database it was asked to preview an import into")
	}
}
