package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// TestOpenImportStoreOpensADryRunReadOnly: a dry run must not go through
// bootstrap()'s read-write open (migrations, user_version, seeded rows), or the
// preview changes the store it previews. The property is that the connection
// refuses a write; the project exists first so a foreign key cannot be the reason.
func TestOpenImportStoreOpensADryRunReadOnly(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "config"))
	dataDir := filepath.Join(dir, "ghost")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatalf("mkdir data dir: %v", err)
	}
	seed, err := memory.OpenDB(filepath.Join(dataDir, "ghost.db"))
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
	_, err = store.Create(ctx, "p1", memory.Memory{Category: "fact", Content: "a dry run must not be able to write this", Source: "mcp"})
	if msg := strings.ToLower(fmt.Sprint(err)); err == nil || !strings.Contains(msg, "readonly") && !strings.Contains(msg, "read-only") {
		t.Errorf("the write = %v, want the read-only refusal", err)
	}
	if _, err := store.ListProjects(ctx); err != nil {
		t.Errorf("a dry-run store could not read: %v", err)
	}
}

// TestOpenImportStoreNamesAMissingDatabase: a read-only open cannot create the
// file, so a dry run with no store says so actionably instead of a SQLite error.
func TestOpenImportStoreNamesAMissingDatabase(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "config"))
	_, err := openImportStore(false)
	if err == nil || !strings.Contains(err.Error(), "ghost.db") || !strings.Contains(err.Error(), "start a session") {
		t.Errorf("error = %v, want it to name the missing database and the next step", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "ghost", "ghost.db")); statErr == nil {
		t.Error("a dry run created the database it was asked to preview an import into")
	}
}
