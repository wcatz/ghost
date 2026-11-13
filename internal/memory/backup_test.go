package memory

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// backupTestStore opens a database in a temp dir and returns a store over it.
// The path is a real file rather than :memory: because a backup is a file
// operation — an in-memory database has no file for VACUUM INTO to read.
func backupTestStore(t *testing.T) *Store {
	t.Helper()
	return backupTestStoreAt(t, filepath.Join(t.TempDir(), "ghost.db"))
}

func backupTestStoreAt(t *testing.T, dbPath string) *Store {
	t.Helper()
	db, err := OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return NewStore(db, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))
}

// seedBackupFixture gives the store one project, four memories (one resolved,
// one pinned with provenance and scope, one companion to the decision), a
// link, a task and a decision, so the backup's counts have to come from rows
// that are not all the same kind.
func seedBackupFixture(t *testing.T, s *Store) {
	t.Helper()
	ctx := context.Background()
	if err := s.EnsureProjectWithRepo(ctx, "p1", "/src/p1", "p1", "git@github.com:wcatz/p1.git"); err != nil {
		t.Fatalf("EnsureProjectWithRepo: %v", err)
	}
	conf := 0.75
	first, err := s.Create(ctx, "p1", Memory{Category: "gotcha", Content: "first", Importance: 0.5, Source: "manual"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// Create writes the provenance and scope columns but not the pin, so the
	// pin is set the way a user sets it — through the pin API — which is also
	// what makes this fixture's pinned row a real one.
	second, err := s.Create(ctx, "p1", Memory{
		Category:   "architecture",
		Content:    "second",
		Importance: 0.9,
		Source:     "mcp",
		Tags:       []string{"one", "two"},
		Agent:      "opencode",
		SessionID:  "sess-1",
		SourceRef:  "ref-1",
		Confidence: &conf,
		Scope:      map[string]string{"environment": "production"},
	})
	if err != nil {
		t.Fatalf("Create second: %v", err)
	}
	if err := s.TogglePin(ctx, second, true); err != nil {
		t.Fatalf("TogglePin: %v", err)
	}
	if err := s.CreateLink(ctx, first, second, "related", 0.5, "auto"); err != nil {
		t.Fatalf("CreateLink: %v", err)
	}
	if _, err := s.CreateTask(ctx, "p1", "task", "desc", 2); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	// RecordDecision also writes a companion memory, so the fixture ends with
	// four: the two created above, the resolved one, and this companion. The
	// count is asserted rather than computed so a change to what the backup
	// counts is visible here instead of being silently absorbed.
	if _, _, _, err := s.RecordDecision(ctx, "p1", "title", "decision", "why", nil, nil); err != nil {
		t.Fatalf("RecordDecision: %v", err)
	}
	// A resolved memory, so the backup carries resolved_at rather than only
	// live rows.
	resolved, err := s.Create(ctx, "p1", Memory{Category: "fact", Content: "resolved", Source: "chat"})
	if err != nil {
		t.Fatalf("Create resolved: %v", err)
	}
	if _, err := s.SetResolved(ctx, []string{resolved}); err != nil {
		t.Fatalf("SetResolved: %v", err)
	}
}

// TestStoreBackupMissingParentIsAnError: a path whose directory does not exist
// has to fail loudly rather than leave the user believing a snapshot exists,
// and the error has to name the directory — SQLite's own "unable to open
// database file" reads as a problem with the database.
func TestStoreBackupMissingParentIsAnError(t *testing.T) {
	store := backupTestStore(t)
	missing := filepath.Join(t.TempDir(), "no-such-dir")
	dest := filepath.Join(missing, "snapshot.db")
	_, err := store.Backup(context.Background(), dest)
	if err == nil {
		t.Fatal("Backup must fail when the destination directory does not exist")
	}
	if !strings.Contains(err.Error(), missing) {
		t.Errorf("error = %v, want it to name the missing directory", err)
	}
	// "does not exist", not "directory": the OS's own message for a failed
	// create is "no such file or directory", so a word match on "directory"
	// would pass whether or not the explicit check ran.
	if !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("error = %v, want it to say the directory does not exist", err)
	}
}
