package mcpinit

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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
	t.Cleanup(func() { _ = db.Close() })
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

// openFileTestDB creates a real on-disk SQLite DB (needed for mode=ro callers like loadGlobalMemories).
func openFileTestDB(t *testing.T) (db *sql.DB, path string) {
	t.Helper()
	dir := t.TempDir()
	path = filepath.Join(dir, "ghost.db")
	db, err := memory.OpenDB(path)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, path
}

func TestLoadGlobalMemories(t *testing.T) {
	db, dbPath := openFileTestDB(t)

	// Seed the _global project row (FK required by memories table).
	_, err := db.Exec(`INSERT INTO projects (id, path, name) VALUES ('_global', '_global', 'global')`)
	if err != nil {
		t.Fatalf("insert _global project: %v", err)
	}
	_, err = db.Exec(
		`INSERT INTO memories (id, project_id, category, content, source) VALUES ('testid01', '_global', 'preference', 'never push to main', 'manual')`,
	)
	if err != nil {
		t.Fatalf("insert global memory: %v", err)
	}

	globals := loadGlobalMemories(dbPath)
	if len(globals) != 1 {
		t.Fatalf("expected 1 global memory, got %d", len(globals))
	}
	if globals[0][0] != "preference" {
		t.Errorf("category: got %q, want preference", globals[0][0])
	}
	if globals[0][1] != "never push to main" {
		t.Errorf("content: got %q, want 'never push to main'", globals[0][1])
	}
}

// TestLoadGlobalMemories_MissingDBNoPhantom verifies the session hook never
// creates an empty ghost.db when none exists (the bare-path mode=ro DSN used
// to open read-write and materialize a phantom file on first read).
func TestLoadGlobalMemories_MissingDBNoPhantom(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "ghost.db")
	if globals := loadGlobalMemories(dbPath); globals != nil {
		t.Errorf("missing DB should yield no globals, got %v", globals)
	}
	if _, err := os.Stat(dbPath); !os.IsNotExist(err) {
		t.Errorf("loadGlobalMemories must not create %s (err=%v)", dbPath, err)
	}
}

func TestHandleSessionStartHook_GlobalsOnNoMatch(t *testing.T) {
	// config.DataDir() returns XDG_DATA_HOME/ghost — build that structure explicitly.
	xdgHome := t.TempDir()
	ghostDir := filepath.Join(xdgHome, "ghost")
	if err := os.MkdirAll(ghostDir, 0o700); err != nil {
		t.Fatalf("mkdir ghostDir: %v", err)
	}
	dbPath := filepath.Join(ghostDir, "ghost.db")

	db, err := memory.OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	_, err = db.Exec(`INSERT INTO projects (id, path, name) VALUES ('_global', '_global', 'global')`)
	if err != nil {
		t.Fatalf("insert _global project: %v", err)
	}
	_, err = db.Exec(
		`INSERT INTO memories (id, project_id, category, content, source) VALUES ('testid02', '_global', 'convention', 'sign all commits with DCO', 'manual')`,
	)
	if err != nil {
		t.Fatalf("insert global memory: %v", err)
	}
	_ = db.Close()

	t.Setenv("XDG_DATA_HOME", xdgHome)

	input, _ := json.Marshal(map[string]string{"cwd": "/tmp/no-project-here"})
	var out strings.Builder
	HandleSessionStartHook(strings.NewReader(string(input)), &out)

	result := out.String()
	if !strings.Contains(result, "Global (applies to all projects)") {
		t.Errorf("globals section missing from no-match output; got:\n%s", result)
	}
	if !strings.Contains(result, "sign all commits with DCO") {
		t.Errorf("global memory content missing from no-match output; got:\n%s", result)
	}
}

// TestSessionCounterIncrements: each hook invocation bumps the project's
// session counter — the one deliberate write the hook makes — and the emitted
// "Session #N" reflects the post-increment count.
func TestSessionCounterIncrements(t *testing.T) {
	xdgHome := t.TempDir()
	ghostDir := filepath.Join(xdgHome, "ghost")
	if err := os.MkdirAll(ghostDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	dbPath := filepath.Join(ghostDir, "ghost.db")

	projDir := filepath.Join(t.TempDir(), "myproj")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatalf("mkdir proj: %v", err)
	}
	// EvalSymlinks in the hook canonicalizes cwd; store the canonical path.
	canonical, err := filepath.EvalSymlinks(projDir)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}

	db, err := memory.OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO projects (id, path, name) VALUES ('p1', ?, 'myproj')`, canonical); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	_ = db.Close()

	t.Setenv("XDG_DATA_HOME", xdgHome)

	run := func() string {
		input, _ := json.Marshal(map[string]string{"cwd": projDir})
		var out strings.Builder
		HandleSessionStartHook(strings.NewReader(string(input)), &out)
		return out.String()
	}
	if got := run(); !strings.Contains(got, "Session #1") {
		t.Errorf("first run should show Session #1; got:\n%s", got)
	}
	if got := run(); !strings.Contains(got, "Session #2") {
		t.Errorf("second run should show Session #2; got:\n%s", got)
	}
}

// TestBumpSessionCountNoPhantomDB: the counter write path must never create a
// database that isn't there.
func TestBumpSessionCountNoPhantomDB(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "ghost.db")
	if n := bumpSessionCount(dbPath, "p1"); n != 0 {
		t.Errorf("bump on missing DB returned %d, want 0", n)
	}
	if _, err := os.Stat(dbPath); !os.IsNotExist(err) {
		t.Errorf("bumpSessionCount must not create %s", dbPath)
	}
}
