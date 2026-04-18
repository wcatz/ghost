package mcpinit

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
	_ "modernc.org/sqlite"
)

// openTestDB creates an in-memory SQLite DB with the ghost schema.
func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := memory.OpenDB(":memory:")
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// insertProject inserts a project row directly (bypasses EnsureProject uniqueness logic).
func insertProject(t *testing.T, db *sql.DB, id, path, name string) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO projects (id, path, name) VALUES (?, ?, ?)`, id, path, name)
	if err != nil {
		t.Fatalf("insertProject: %v", err)
	}
}

func TestLookupProject_ExactMatch(t *testing.T) {
	db := openTestDB(t)
	insertProject(t, db, "abc123", "/home/wayne/git/ghost", "ghost")

	id, name := lookupProject(db, "/home/wayne/git/ghost")
	if id != "abc123" || name != "ghost" {
		t.Errorf("exact match: got id=%q name=%q, want abc123/ghost", id, name)
	}
}

func TestLookupProject_SubdirMatch(t *testing.T) {
	db := openTestDB(t)
	insertProject(t, db, "abc123", "/home/wayne/git/ghost", "ghost")

	id, name := lookupProject(db, "/home/wayne/git/ghost/internal/mcpinit")
	if id != "abc123" || name != "ghost" {
		t.Errorf("subdir match: got id=%q name=%q, want abc123/ghost", id, name)
	}
}

// TestLookupProject_NoPrefixFalseMatch is the regression test for the bug:
// a CWD of /home/wayne/git/ghost-extra must NOT match a project at /home/wayne/git/ghost.
func TestLookupProject_NoPrefixFalseMatch(t *testing.T) {
	db := openTestDB(t)
	insertProject(t, db, "abc123", "/home/wayne/git/ghost", "ghost")

	id, name := lookupProject(db, "/home/wayne/git/ghost-extra")
	if id != "" || name != "" {
		t.Errorf("prefix false match: /home/wayne/git/ghost-extra should NOT match /home/wayne/git/ghost, got id=%q name=%q", id, name)
	}
}

func TestLookupProject_LongestPathWins(t *testing.T) {
	db := openTestDB(t)
	insertProject(t, db, "parent", "/home/wayne/git", "parent")
	insertProject(t, db, "child", "/home/wayne/git/ghost", "ghost")

	// CWD inside ghost should match the longer (more specific) path.
	id, _ := lookupProject(db, "/home/wayne/git/ghost/cmd")
	if id != "child" {
		t.Errorf("longest path should win, got %q, want %q", id, "child")
	}
}

func TestLookupProject_NameFallback(t *testing.T) {
	db := openTestDB(t)
	// Use a short path so path prefix matching won't trigger (LENGTH(path) > 10 guard).
	// The name fallback fires when cwd basename matches a project name.
	insertProject(t, db, "abc123", "/x/ghost", "ghost")

	// cwd basename is "ghost" — should match by name even when path doesn't match.
	id, name := lookupProject(db, filepath.Join("/some/unrelated/path", "ghost"))
	if id != "abc123" || name != "ghost" {
		t.Errorf("name fallback: got id=%q name=%q, want abc123/ghost", id, name)
	}
}

func TestLookupProject_NoMatch(t *testing.T) {
	db := openTestDB(t)
	insertProject(t, db, "abc123", "/home/wayne/git/ghost", "ghost")

	id, name := lookupProject(db, "/home/user/other-project")
	if id != "" || name != "" {
		t.Errorf("no match: expected empty, got id=%q name=%q", id, name)
	}
}
