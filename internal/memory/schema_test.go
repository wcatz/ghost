package memory

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

// TestOpenDBHostilePath: OpenDB must open the database at exactly the path it
// was given even when the path contains URI-special characters ('?' or '#',
// legal in $XDG_DATA_HOME/$HOME). A naive DSN concatenation parses those as
// query/fragment separators and silently opens a truncated path instead.
func TestOpenDBHostilePath(t *testing.T) {
	for _, dirName := range []string{"we?rd", "we#rd", "with space"} {
		t.Run(dirName, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), dirName)
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
			dbPath := filepath.Join(dir, "ghost.db")

			db, err := OpenDB(dbPath)
			if err != nil {
				t.Fatalf("OpenDB(%q): %v", dbPath, err)
			}
			store := NewStore(db, slog.New(slog.NewTextHandler(os.Stderr, nil)))
			defer func() { _ = store.Close() }()

			ctx := context.Background()
			if err := store.EnsureProject(ctx, "p", "/tmp/p", "p"); err != nil {
				t.Fatalf("EnsureProject: %v", err)
			}
			id, err := store.Create(ctx, "p", Memory{Category: "fact", Content: "roundtrip", Importance: 0.5, Source: "mcp"})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			got, err := store.GetByIDs(ctx, []string{id})
			if err != nil || len(got) != 1 || got[0].Content != "roundtrip" {
				t.Fatalf("roundtrip failed: %v %v", got, err)
			}

			// The database landed at the intended path — not a truncated one.
			if _, err := os.Stat(dbPath); err != nil {
				t.Errorf("database not at intended path %q: %v", dbPath, err)
			}
		})
	}
}

// TestOpenDBInMemory: the ":memory:" special path must keep its bare form —
// wrapping it in a file: URI would change its per-connection semantics.
func TestOpenDBInMemory(t *testing.T) {
	db, err := OpenDB(":memory:")
	if err != nil {
		t.Fatalf("OpenDB(:memory:): %v", err)
	}
	defer func() { _ = db.Close() }()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM projects`).Scan(&n); err != nil {
		t.Fatalf("schema not initialized on :memory:: %v", err)
	}
}

// TestOpenDBRefusesNewerSchema: a database stamped with a newer schema must be
// refused, not opened read-write against a schema the binary cannot interpret.
func TestOpenDBRefusesNewerSchema(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "ghost.db")
	db, err := OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	if _, err := db.Exec(fmt.Sprintf("PRAGMA user_version = %d", schemaVersion+1)); err != nil {
		t.Fatalf("bump user_version: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := OpenDB(dbPath); err == nil {
		t.Fatal("expected OpenDB to refuse a database newer than the binary")
	}
}

// TestBackupBeforeMigrate: the pre-migration copy must exist and contain the
// data as it was before any migration step.
func TestBackupBeforeMigrate(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "ghost.db")
	db, err := OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer func() { _ = db.Close() }()

	if _, err := db.Exec(`INSERT INTO projects (id, name, path) VALUES ('p', 'p', '/p')`); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	if err := backupBeforeMigrate(db, dbPath); err != nil {
		t.Fatalf("backupBeforeMigrate: %v", err)
	}

	matches, _ := filepath.Glob(dbPath + ".pre-migrate-*")
	if len(matches) != 1 {
		t.Fatalf("expected exactly one backup file, got %v", matches)
	}

	bdb, err := sql.Open("sqlite", "file:"+matches[0])
	if err != nil {
		t.Fatalf("open backup: %v", err)
	}
	defer func() { _ = bdb.Close() }()
	var n int
	if err := bdb.QueryRow(`SELECT count(*) FROM projects WHERE id = 'p'`).Scan(&n); err != nil {
		t.Fatalf("query backup: %v", err)
	}
	if n != 1 {
		t.Errorf("backup does not contain the pre-migration row: count=%d", n)
	}
}

func TestOpenDBRunsRetentionAfterSuccessfulMigration(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("GHOST_RETENTION_BACKUP_COUNT", "1")

	dbPath := newLegacyDB(t)
	old := dbPath + ".pre-migrate-1"
	if err := os.WriteFile(old, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}

	db, err := OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer func() { _ = db.Close() }()
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatalf("configured retention left old backup in place after migration: %v", err)
	}
	matches, _ := filepath.Glob(dbPath + ".pre-migrate-*")
	if len(matches) != 1 {
		t.Fatalf("retention kept %d backups, want 1: %v", len(matches), matches)
	}
}
