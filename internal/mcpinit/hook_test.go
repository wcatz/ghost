package mcpinit

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
	_ "modernc.org/sqlite"
)

// testStore wraps db in a memory.Store with a discard logger, matching the
// package's existing convention (see init.go, status.go).
func testStore(db *sql.DB) *memory.Store {
	return memory.NewStore(db, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

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

	id, name, err := testStore(db).ResolveProject(context.Background(), "/home/wayne/git/ghost")
	if err != nil {
		t.Fatalf("ResolveProject: %v", err)
	}
	if id != "abc123" || name != "ghost" {
		t.Errorf("exact match: got id=%q name=%q, want abc123/ghost", id, name)
	}
}

func TestLookupProject_SubdirMatch(t *testing.T) {
	db := openTestDB(t)
	insertProject(t, db, "abc123", "/home/wayne/git/ghost", "ghost")

	id, name, err := testStore(db).ResolveProject(context.Background(), "/home/wayne/git/ghost/internal/mcpinit")
	if err != nil {
		t.Fatalf("ResolveProject: %v", err)
	}
	if id != "abc123" || name != "ghost" {
		t.Errorf("subdir match: got id=%q name=%q, want abc123/ghost", id, name)
	}
}

// TestLookupProject_NoPrefixFalseMatch is the regression test for the bug:
// a CWD of /home/wayne/git/ghost-extra must NOT match a project at /home/wayne/git/ghost.
func TestLookupProject_NoPrefixFalseMatch(t *testing.T) {
	db := openTestDB(t)
	insertProject(t, db, "abc123", "/home/wayne/git/ghost", "ghost")

	id, name, err := testStore(db).ResolveProject(context.Background(), "/home/wayne/git/ghost-extra")
	if err != nil {
		t.Fatalf("ResolveProject: %v", err)
	}
	if id != "" || name != "" {
		t.Errorf("prefix false match: /home/wayne/git/ghost-extra should NOT match /home/wayne/git/ghost, got id=%q name=%q", id, name)
	}
}

func TestLookupProject_LongestPathWins(t *testing.T) {
	db := openTestDB(t)
	insertProject(t, db, "parent", "/home/wayne/git", "parent")
	insertProject(t, db, "child", "/home/wayne/git/ghost", "ghost")

	// CWD inside ghost should match the longer (more specific) path.
	id, _, err := testStore(db).ResolveProject(context.Background(), "/home/wayne/git/ghost/cmd")
	if err != nil {
		t.Fatalf("ResolveProject: %v", err)
	}
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
	id, name, err := testStore(db).ResolveProject(context.Background(), filepath.Join("/some/unrelated/path", "ghost"))
	if err != nil {
		t.Fatalf("ResolveProject: %v", err)
	}
	if id != "abc123" || name != "ghost" {
		t.Errorf("name fallback: got id=%q name=%q, want abc123/ghost", id, name)
	}
}

func TestLookupProject_NoMatch(t *testing.T) {
	db := openTestDB(t)
	insertProject(t, db, "abc123", "/home/wayne/git/ghost", "ghost")

	id, name, err := testStore(db).ResolveProject(context.Background(), "/home/user/other-project")
	if err != nil {
		t.Fatalf("ResolveProject: %v", err)
	}
	if id != "" || name != "" {
		t.Errorf("no match: expected empty, got id=%q name=%q", id, name)
	}
}

// TestLookupProject_PathWithLikeWildcards is the regression test for a project
// path containing a literal '%' or '_': those must not act as unintended LIKE
// wildcards and false-match an unrelated sibling directory.
func TestLookupProject_PathWithLikeWildcards(t *testing.T) {
	db := openTestDB(t)
	insertProject(t, db, "abc123", "/home/wayne/git/foo_bar", "foo_bar")

	// Without escaping, "foo_bar/%" as a LIKE pattern would also match
	// "fooXbar/anything" since '_' matches any single character.
	id, name, err := testStore(db).ResolveProject(context.Background(), "/home/wayne/git/fooXbar/sub")
	if err != nil {
		t.Fatalf("ResolveProject: %v", err)
	}
	if id != "" || name != "" {
		t.Errorf("unescaped underscore false match: got id=%q name=%q, want no match", id, name)
	}

	// The real subdirectory should still match correctly.
	id, name, err = testStore(db).ResolveProject(context.Background(), "/home/wayne/git/foo_bar/sub")
	if err != nil {
		t.Fatalf("ResolveProject: %v", err)
	}
	if id != "abc123" || name != "foo_bar" {
		t.Errorf("exact underscore path: got id=%q name=%q, want abc123/foo_bar", id, name)
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

	globals, total, totalKnown := loadGlobalMemories(dbPath)
	if len(globals) != 1 {
		t.Fatalf("expected 1 global memory, got %d", len(globals))
	}
	if globals[0].Category != "preference" {
		t.Errorf("category: got %q, want preference", globals[0].Category)
	}
	if globals[0].Content != "never push to main" {
		t.Errorf("content: got %q, want 'never push to main'", globals[0].Content)
	}
	if !totalKnown || total != 1 {
		t.Errorf("total: got known=%v total=%d, want known=true total=1", totalKnown, total)
	}
}

// TestLoadGlobalMemories_MissingDBNoPhantom verifies the session hook never
// creates an empty ghost.db when none exists (the bare-path mode=ro DSN used
// to open read-write and materialize a phantom file on first read).
func TestLoadGlobalMemories_MissingDBNoPhantom(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "ghost.db")
	globals, total, totalKnown := loadGlobalMemories(dbPath)
	if globals != nil || total != 0 || totalKnown {
		t.Errorf("missing DB should yield no globals, got globals=%v total=%d known=%v", globals, total, totalKnown)
	}
	if _, err := os.Stat(dbPath); !os.IsNotExist(err) {
		t.Errorf("loadGlobalMemories must not create %s (err=%v)", dbPath, err)
	}
}

// TestLoadGlobalMemories_DedupsNearDuplicates: two near-duplicate globals
// linked above the globals demotion threshold (0.85) must not both survive,
// even though 0.8857 is below the general DefaultDemotionThreshold (0.90)
// used for project memories.
func TestLoadGlobalMemories_DedupsNearDuplicates(t *testing.T) {
	db, dbPath := openFileTestDB(t)

	if _, err := db.Exec(`INSERT INTO projects (id, path, name) VALUES ('_global', '_global', 'global')`); err != nil {
		t.Fatalf("insert _global project: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO memories (id, project_id, category, content, source, importance) VALUES ('gaaa0001', '_global', 'convention', 'ORIGINAL restricted repos are read-only', 'manual', 0.9)`,
	); err != nil {
		t.Fatalf("insert a: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO memories (id, project_id, category, content, source, importance) VALUES ('gbbb0001', '_global', 'convention', 'RESTATED restricted repos are read-only too', 'manual', 0.89)`,
	); err != nil {
		t.Fatalf("insert b: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO memory_links (source_id, target_id, relation, strength, source) VALUES ('gaaa0001', 'gbbb0001', 'related', 0.8857, 'auto')`,
	); err != nil {
		t.Fatalf("insert link: %v", err)
	}

	globals, _, _ := loadGlobalMemories(dbPath)
	var sawOriginal, sawRestated bool
	for _, m := range globals {
		if strings.Contains(m.Content, "ORIGINAL") {
			sawOriginal = true
		}
		if strings.Contains(m.Content, "RESTATED") {
			sawRestated = true
		}
	}
	if !sawOriginal {
		t.Errorf("higher-ranked global must survive; got %+v", globals)
	}
	if sawRestated {
		t.Errorf("lower-ranked near-duplicate global must be demoted out; got %+v", globals)
	}
}

// TestLoadGlobalMemories_ExcludesResolved verifies a global memory marked
// resolved_at (via ghost resolve) is excluded from both the injected set and
// totalCount, matching the project-memory queries' resolved_at IS NULL filter.
func TestLoadGlobalMemories_ExcludesResolved(t *testing.T) {
	db, dbPath := openFileTestDB(t)

	if _, err := db.Exec(`INSERT INTO projects (id, path, name) VALUES ('_global', '_global', 'global')`); err != nil {
		t.Fatalf("insert _global project: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO memories (id, project_id, category, content, source) VALUES ('gres0001', '_global', 'fact', 'old cost estimate, no longer relevant', 'manual')`,
	); err != nil {
		t.Fatalf("insert resolved global: %v", err)
	}
	if _, err := db.Exec(
		`UPDATE memories SET resolved_at = datetime('now') WHERE id = 'gres0001'`,
	); err != nil {
		t.Fatalf("mark resolved: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO memories (id, project_id, category, content, source) VALUES ('glive0001', '_global', 'preference', 'never push to main', 'manual')`,
	); err != nil {
		t.Fatalf("insert live global: %v", err)
	}

	globals, total, totalKnown := loadGlobalMemories(dbPath)
	if len(globals) != 1 || globals[0].ID != "glive0001" {
		t.Fatalf("resolved global must be excluded from fetch, got %+v", globals)
	}
	if !totalKnown || total != 1 {
		t.Errorf("resolved global must be excluded from total count: got known=%v total=%d, want known=true total=1", totalKnown, total)
	}
}

// TestHandleSessionStartHook_GlobalsCapAndNotShownLine: more than 8 global
// memories must be capped, and the cap must surface a not-shown count rather
// than silently dropping the rest.
func TestHandleSessionStartHook_GlobalsCapAndNotShownLine(t *testing.T) {
	xdgHome := t.TempDir()
	ghostDir := filepath.Join(xdgHome, "ghost")
	if err := os.MkdirAll(ghostDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	dbPath := filepath.Join(ghostDir, "ghost.db")

	db, err := memory.OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO projects (id, path, name) VALUES ('_global', '_global', 'global')`); err != nil {
		t.Fatalf("insert _global project: %v", err)
	}
	for i := 0; i < 12; i++ {
		id := fmt.Sprintf("gfil%04d", i)
		importance := 0.9 - float64(i)*0.01
		if _, err := db.Exec(
			`INSERT INTO memories (id, project_id, category, content, source, importance) VALUES (?, '_global', 'preference', ?, 'manual', ?)`,
			id, fmt.Sprintf("GLOBALFILLER%02d distinct preference", i), importance,
		); err != nil {
			t.Fatalf("insert global filler %d: %v", i, err)
		}
	}
	_ = db.Close()

	t.Setenv("XDG_DATA_HOME", xdgHome)

	input, _ := json.Marshal(map[string]string{"cwd": "/tmp/no-project-here"})
	var out strings.Builder
	HandleSessionStartHook(strings.NewReader(string(input)), &out)
	result := out.String()

	if !strings.Contains(result, "not shown") {
		t.Errorf("globals section must surface a not-shown count when capped; got:\n%s", result)
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

// TestHandleSessionStartHook_NoMatchExplainsDelimiters: the no-project-match
// branch must still explain the «...» data-delimiter convention when it has
// global content to show — today it's silently missing there.
func TestHandleSessionStartHook_NoMatchExplainsDelimiters(t *testing.T) {
	xdgHome := t.TempDir()
	ghostDir := filepath.Join(xdgHome, "ghost")
	if err := os.MkdirAll(ghostDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	dbPath := filepath.Join(ghostDir, "ghost.db")

	db, err := memory.OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO projects (id, path, name) VALUES ('_global', '_global', 'global')`); err != nil {
		t.Fatalf("insert _global project: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO memories (id, project_id, category, content, source) VALUES ('testid03', '_global', 'convention', 'sign all commits with DCO', 'manual')`,
	); err != nil {
		t.Fatalf("insert global memory: %v", err)
	}
	_ = db.Close()

	t.Setenv("XDG_DATA_HOME", xdgHome)

	input, _ := json.Marshal(map[string]string{"cwd": "/tmp/no-project-here"})
	var out strings.Builder
	HandleSessionStartHook(strings.NewReader(string(input)), &out)
	result := out.String()

	if !strings.Contains(result, "delimits stored memory data") {
		t.Errorf("no-match branch with globals must explain the «...» convention; got:\n%s", result)
	}
	if strings.Count(result, "delimits stored memory data") > 1 {
		t.Errorf("the «...» explainer must appear at most once; got:\n%s", result)
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

// TestSessionCounterIgnoresResumeClearCompact: resume/clear/compact fire
// SessionStart too, but a user perceives those as continuing the same
// session — only "startup" (or an empty/legacy source) should bump the count.
func TestSessionCounterIgnoresResumeClearCompact(t *testing.T) {
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

	run := func(source string) string {
		m := map[string]string{"cwd": projDir}
		if source != "" {
			m["source"] = source
		}
		input, _ := json.Marshal(m)
		var out strings.Builder
		HandleSessionStartHook(strings.NewReader(string(input)), &out)
		return out.String()
	}

	if got := run("startup"); !strings.Contains(got, "Session #1") {
		t.Errorf("startup should show Session #1; got:\n%s", got)
	}
	// resume and compact are now suppressed/shrunk (see
	// TestHandleSessionStartHook_ResumeSuppressed and
	// TestHandleSessionStartHook_CompactShrunk) so they no longer echo the
	// session count at all; clear still gets full injection. What this test
	// cares about is that none of the three bump the underlying counter —
	// proven below by the follow-up startup still landing on Session #2.
	for _, source := range []string{"resume", "clear", "compact"} {
		run(source)
	}
	if got := run("clear"); !strings.Contains(got, "Session #1") {
		t.Errorf("clear should NOT bump the counter, still Session #1; got:\n%s", got)
	}
	if got := run("startup"); !strings.Contains(got, "Session #2") {
		t.Errorf("second startup should show Session #2; got:\n%s", got)
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

// TestInjectionExcludesResolved: a resolved memory must not appear in the
// session-start injection, while an active one does.
func TestInjectionExcludesResolved(t *testing.T) {
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
	if _, err := db.Exec(
		`INSERT INTO memories (id, project_id, category, content, source) VALUES ('activ001', 'p1', 'gotcha', 'ACTIVEMARKER keep this', 'manual')`,
	); err != nil {
		t.Fatalf("insert active: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO memories (id, project_id, category, content, source, resolved_at) VALUES ('rslv0001', 'p1', 'gotcha', 'RESOLVEDMARKER drop this', 'manual', datetime('now'))`,
	); err != nil {
		t.Fatalf("insert resolved: %v", err)
	}
	_ = db.Close()

	t.Setenv("XDG_DATA_HOME", xdgHome)

	input, _ := json.Marshal(map[string]string{"cwd": projDir})
	var out strings.Builder
	HandleSessionStartHook(strings.NewReader(string(input)), &out)
	result := out.String()

	if !strings.Contains(result, "ACTIVEMARKER") {
		t.Errorf("active memory missing from injection; got:\n%s", result)
	}
	if strings.Contains(result, "RESOLVEDMARKER") {
		t.Errorf("resolved memory must not be injected; got:\n%s", result)
	}
}

// TestSessionInjectionBackfillsAfterDemotion: a near-duplicate pair linked
// above the demotion threshold must not both occupy injection slots — the
// backfill (sessionMemoriesCap*2 = 30 over-fetch) must surface a distinct
// memory instead.
func TestSessionInjectionBackfillsAfterDemotion(t *testing.T) {
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
	// Two near-duplicates (a, b) plus 13 filler memories plus a distinct
	// memory (c). loadSessionContext over-fetches at 2x the cap (30) then
	// truncates to 15; with only 3 memories total the >15 demotion gate could never fire,
	// so this fixture pads the candidate set past 15 (16 total) so the gate
	// fires and truncation actually drops the demoted duplicate, letting the
	// distinct memory (ranked last, just past the naive top-15 cut) backfill
	// into the surviving slot instead. Importance ordering: a > b > fillers > c.
	if _, err := db.Exec(
		`INSERT INTO memories (id, project_id, category, content, source, importance) VALUES ('aaaa0001', 'p1', 'fact', 'ALPHAMARKER original', 'manual', 0.99)`,
	); err != nil {
		t.Fatalf("insert a: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO memories (id, project_id, category, content, source, importance) VALUES ('bbbb0001', 'p1', 'fact', 'ALPHAMARKER restated', 'manual', 0.98)`,
	); err != nil {
		t.Fatalf("insert b: %v", err)
	}
	for i := 0; i < 13; i++ {
		fillerID := fmt.Sprintf("filler%02d", i)
		importance := 0.97 - float64(i)*0.01
		if _, err := db.Exec(
			`INSERT INTO memories (id, project_id, category, content, source, importance) VALUES (?, 'p1', 'fact', ?, 'manual', ?)`,
			fillerID, fmt.Sprintf("FILLER%02d unrelated filler content", i), importance,
		); err != nil {
			t.Fatalf("insert filler %d: %v", i, err)
		}
	}
	if _, err := db.Exec(
		`INSERT INTO memories (id, project_id, category, content, source, importance) VALUES ('cccc0001', 'p1', 'fact', 'DISTINCTMARKER unrelated', 'manual', 0.10)`,
	); err != nil {
		t.Fatalf("insert c: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO memory_links (source_id, target_id, relation, strength, source) VALUES ('aaaa0001', 'bbbb0001', 'related', 0.95, 'auto')`,
	); err != nil {
		t.Fatalf("insert link: %v", err)
	}
	_ = db.Close()

	t.Setenv("XDG_DATA_HOME", xdgHome)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	input, _ := json.Marshal(map[string]string{"cwd": projDir})
	var out strings.Builder
	HandleSessionStartHook(strings.NewReader(string(input)), &out)
	result := out.String()

	if !strings.Contains(result, "ALPHAMARKER original") {
		t.Errorf("higher-ranked duplicate must survive; got:\n%s", result)
	}
	if strings.Contains(result, "ALPHAMARKER restated") {
		t.Errorf("lower-ranked duplicate must be demoted out; got:\n%s", result)
	}
	if !strings.Contains(result, "DISTINCTMARKER") {
		t.Errorf("backfill must surface the distinct memory; got:\n%s", result)
	}
}

// TestSessionInjectionRespectsNewCap: the memory cap is 15, not 25 — a
// project with more than 15 active memories must show exactly 15 plus a
// "not shown" count. This test only exercises the cap/not-shown behavior;
// decay-ranking parity with Store.GetTopMemories is covered separately by
// TestSessionInjectionUsesDecayRanking (all rows here are category 'fact',
// which never decays, so this fixture can't distinguish decayed-score
// ordering from raw-importance ordering).
func TestSessionInjectionRespectsNewCap(t *testing.T) {
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
	for i := 0; i < 20; i++ {
		id := fmt.Sprintf("capfil%02d", i)
		importance := 0.9 - float64(i)*0.01
		if _, err := db.Exec(
			`INSERT INTO memories (id, project_id, category, content, source, importance) VALUES (?, 'p1', 'fact', ?, ?, ?)`,
			id, fmt.Sprintf("CAPFILLER%02d content", i), "manual", importance,
		); err != nil {
			t.Fatalf("insert cap filler %d: %v", i, err)
		}
	}
	_ = db.Close()

	t.Setenv("XDG_DATA_HOME", xdgHome)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	input, _ := json.Marshal(map[string]string{"cwd": projDir})
	var out strings.Builder
	HandleSessionStartHook(strings.NewReader(string(input)), &out)
	result := out.String()

	if !strings.Contains(result, "15 shown of 20 total — 5 not shown") {
		t.Errorf("expected cap of 15 with not-shown count, got:\n%s", result)
	}
}

// TestSessionInjectionUsesDecayRanking: ranking must follow the category-aware
// decayed score (parity with Store.GetTopMemories), not raw importance. The
// fixture packs 15 'fact' filler rows (importance 0.2, fresh created_at —
// facts never decay, so score stays 0.2) plus one 'gotcha' row (importance
// 0.9, created_at ~400 days back — decays to the 0.15 floor, score
// 0.9*0.15=0.135). Under raw-importance ordering the gotcha (0.9) would rank
// first and easily survive the 15-cap; under decayed-score ordering it
// scores below every fact (0.135 < 0.2) and is the one row trimmed by the
// cap. Asserting the gotcha marker is absent (and a fact marker present)
// only passes under decay-ranked ordering.
func TestSessionInjectionUsesDecayRanking(t *testing.T) {
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
	// 15 low-importance 'fact' rows — never decay, score stays 0.2 each.
	for i := 0; i < 15; i++ {
		id := fmt.Sprintf("factfil%02d", i)
		if _, err := db.Exec(
			`INSERT INTO memories (id, project_id, category, content, source, importance) VALUES (?, 'p1', 'fact', ?, ?, ?)`,
			id, fmt.Sprintf("FACTMARKER%02d content", i), "manual", 0.2,
		); err != nil {
			t.Fatalf("insert fact filler %d: %v", i, err)
		}
	}
	// High-importance 'gotcha' with an old created_at — decays to the 0.15
	// floor, decayed score 0.9*0.15=0.135, below every fact's 0.2.
	if _, err := db.Exec(
		`INSERT INTO memories (id, project_id, category, content, source, importance, created_at)
		 VALUES ('gotcha01', 'p1', 'gotcha', 'GOTCHAMARKER old high-importance', 'manual', 0.9, datetime('now', '-400 days'))`,
	); err != nil {
		t.Fatalf("insert gotcha: %v", err)
	}
	_ = db.Close()

	t.Setenv("XDG_DATA_HOME", xdgHome)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	input, _ := json.Marshal(map[string]string{"cwd": projDir})
	var out strings.Builder
	HandleSessionStartHook(strings.NewReader(string(input)), &out)
	result := out.String()

	if strings.Contains(result, "GOTCHAMARKER") {
		t.Errorf("expected decayed gotcha (score 0.135) to be trimmed by the 15-cap in favor of higher-scoring facts (0.2); got:\n%s", result)
	}
	if !strings.Contains(result, "FACTMARKER") {
		t.Errorf("expected fact rows (score 0.2) to survive the cap; got:\n%s", result)
	}
	if !strings.Contains(result, "15 shown of 16 total — 1 not shown") {
		t.Errorf("expected cap of 15 with 1 not-shown, got:\n%s", result)
	}
}

// TestHandleSessionStartHook_SubagentSuppressed: a session carrying a
// non-empty agent_id is a subagent spawn — it already inherits working
// context in-band from its parent's prompt, so the hook must emit nothing
// and must not touch the DB (no phantom ghost.db, no counter bump).
func TestHandleSessionStartHook_SubagentSuppressed(t *testing.T) {
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
	// Recreate as "not there yet" to prove the subagent path never opens it.
	if err := os.Remove(dbPath); err != nil {
		t.Fatalf("remove dbPath: %v", err)
	}

	t.Setenv("XDG_DATA_HOME", xdgHome)

	input, _ := json.Marshal(map[string]string{"cwd": projDir, "agent_id": "sub-123", "agent_type": "Explore"})
	var out strings.Builder
	HandleSessionStartHook(strings.NewReader(string(input)), &out)

	if got := out.String(); got != "" {
		t.Errorf("subagent session must produce empty output, got:\n%s", got)
	}
	if _, err := os.Stat(dbPath); !os.IsNotExist(err) {
		t.Errorf("subagent session must not create %s", dbPath)
	}
}

// TestHandleSessionStartHook_TopLevelUnaffected: the same fixture without
// agent_id must still produce the normal full injection (regression guard).
func TestHandleSessionStartHook_TopLevelUnaffected(t *testing.T) {
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

	input, _ := json.Marshal(map[string]string{"cwd": projDir})
	var out strings.Builder
	HandleSessionStartHook(strings.NewReader(string(input)), &out)

	if got := out.String(); !strings.Contains(got, "## Ghost context: myproj") {
		t.Errorf("top-level session should still get full injection, got:\n%s", got)
	}
}

// TestHandleSessionStartHook_ResumeSuppressed: a resume fire's transcript
// already contains the original injection from the earlier startup fire —
// re-emitting it is pure waste.
func TestHandleSessionStartHook_ResumeSuppressed(t *testing.T) {
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

	input, _ := json.Marshal(map[string]string{"cwd": projDir, "source": "resume"})
	var out strings.Builder
	HandleSessionStartHook(strings.NewReader(string(input)), &out)

	if got := out.String(); got != "" {
		t.Errorf("resume must produce empty output, got:\n%s", got)
	}
}

// TestHandleSessionStartHook_CompactShrunk: a compact fire gets a short
// pointer instead of the full re-injected block, since Claude Code's
// compaction behavior toward the original injected block isn't guaranteed.
func TestHandleSessionStartHook_CompactShrunk(t *testing.T) {
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

	input, _ := json.Marshal(map[string]string{"cwd": projDir, "source": "compact"})
	var out strings.Builder
	HandleSessionStartHook(strings.NewReader(string(input)), &out)

	got := out.String()
	if !strings.Contains(got, "ghost_project_context") {
		t.Errorf("compact output should point at ghost_project_context, got:\n%s", got)
	}
	if strings.Contains(got, "## Ghost context:") {
		t.Errorf("compact must NOT re-emit the full block, got:\n%s", got)
	}
}

// TestHandleSessionStartHook_ClearUnaffected: /clear wipes the transcript,
// so this is the one re-fire case that must still get full injection.
func TestHandleSessionStartHook_ClearUnaffected(t *testing.T) {
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

	input, _ := json.Marshal(map[string]string{"cwd": projDir, "source": "clear"})
	var out strings.Builder
	HandleSessionStartHook(strings.NewReader(string(input)), &out)

	if got := out.String(); !strings.Contains(got, "## Ghost context: myproj") {
		t.Errorf("clear should still get full injection, got:\n%s", got)
	}
}
