package memory

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

// TestEnsureProjectWithRepoMergesSameRepositoryAcrossPaths is the whole point
// of repository identity: two checkouts of one repository are one project, not
// two.
//
// Path-based identity cannot express this. ~/src/ghost and ~/work/ghost are
// different strings with different memories, so an agent that changes working
// directory silently starts a second project and loses everything the first
// one knew. The remote is what the two paths have in common, and it is the
// only thing that says they are the same project.
func TestEnsureProjectWithRepoMergesSameRepositoryAcrossPaths(t *testing.T) {
	db, err := OpenDB(filepath.Join(t.TempDir(), "repo.sqlite"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer func() { _ = db.Close() }()

	ctx := context.Background()
	s := NewStore(db, nil)

	const remote = "git@github.com:wcatz/ghost.git"

	// First checkout creates the project.
	if err := s.EnsureProjectWithRepo(ctx, "proj-src", "/home/u/src/ghost", "ghost", remote); err != nil {
		t.Fatalf("ensure first checkout: %v", err)
	}
	// Give it something to lose, so the merge has content to preserve.
	if _, _, _, err := s.Upsert(ctx, "proj-src", "fact", "the API listens on 8443", "manual", 0.7, nil); err != nil {
		t.Fatalf("seed memory: %v", err)
	}

	// A second checkout of the same repository, different path.
	if err := s.EnsureProjectWithRepo(ctx, "proj-work", "/home/u/work/ghost", "ghost-work", remote); err != nil {
		t.Fatalf("ensure second checkout: %v", err)
	}

	id, name, err := s.ResolveProject(ctx, "/home/u/work/ghost")
	if err != nil {
		t.Fatalf("resolve second path: %v", err)
	}
	if id != "proj-src" {
		t.Errorf("second checkout resolved to project %q (%q), want the existing %q — two paths in one repository must not become two projects", id, name, "proj-src")
	}

	// The first project's memories must have survived the merge rather than
	// being stranded on a project nothing resolves to.
	got, err := s.GetAll(ctx, "proj-src", 10)
	if err != nil {
		t.Fatalf("GetAll: %v", err)
	}
	if len(got) == 0 {
		t.Error("memories were lost: the merged-into project holds none of them")
	}
}

// TestEnsureProjectWithRepoKeepsDifferentRepositoriesApart guards the inverse:
// the same basename in two repositories is two projects, and collapsing them
// would cross-contaminate unrelated work.
func TestEnsureProjectWithRepoKeepsDifferentRepositoriesApart(t *testing.T) {
	db, err := OpenDB(filepath.Join(t.TempDir(), "repo-distinct.sqlite"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer func() { _ = db.Close() }()

	ctx := context.Background()
	s := NewStore(db, nil)

	if err := s.EnsureProjectWithRepo(ctx, "a", "/home/u/src/ghost", "ghost", "git@github.com:wcatz/ghost.git"); err != nil {
		t.Fatalf("ensure a: %v", err)
	}
	if err := s.EnsureProjectWithRepo(ctx, "b", "/home/u/src/other-ghost", "other-ghost", "git@github.com:someone/other-ghost.git"); err != nil {
		t.Fatalf("ensure b: %v", err)
	}

	for _, in := range []string{"/home/u/src/ghost", "/home/u/src/other-ghost"} {
		id, _, err := s.ResolveProject(ctx, in)
		if err != nil {
			t.Fatalf("resolve %q: %v", in, err)
		}
		want := "a"
		if in == "/home/u/src/other-ghost" {
			want = "b"
		}
		if id != want {
			t.Errorf("resolve %q = %q, want %q — distinct repositories must not be collapsed", in, id, want)
		}
	}
}

// TestResolveProjectByRemote lets a caller name the project the way a human
// or an agent naturally would — by repository rather than by a path that may
// not exist on this machine.
func TestResolveProjectByRemote(t *testing.T) {
	db, err := OpenDB(filepath.Join(t.TempDir(), "repo-resolve.sqlite"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer func() { _ = db.Close() }()

	ctx := context.Background()
	s := NewStore(db, nil)
	if err := s.EnsureProjectWithRepo(ctx, "proj", "/home/u/src/ghost", "ghost-app", "git@github.com:wcatz/ghost.git"); err != nil {
		t.Fatalf("ensure: %v", err)
	}

	// Every spelling a caller might use for the same repository.
	for _, in := range []string{
		"git@github.com:wcatz/ghost.git",
		"https://github.com/wcatz/ghost.git",
		"https://github.com/wcatz/ghost",
		"github.com/wcatz/ghost",
		"https://GitHub.com/wcatz/ghost.git", // host casing must not matter
	} {
		id, _, err := s.ResolveProject(ctx, in)
		if err != nil {
			t.Fatalf("resolve %q: %v", in, err)
		}
		if id != "proj" {
			t.Errorf("ResolveProject(%q) = %q, want %q — the same repository must be reachable by any spelling of its remote", in, id, "proj")
		}
	}
}

// TestMigrateV11AddsRepoRemote upgrades a schema-v10 database — the projects
// DDL that shipped through v10 — and asserts the column arrives.
//
// The pre-existing row matters as much as the column: an additive migration
// must leave existing projects untouched with repo_remote reading NULL. A
// default would assert that a project has a repository it was never observed
// to have, and a fabricated remote is worse than none because it merges
// unrelated projects.
func TestMigrateV11AddsRepoRemote(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "ghost.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open v10 db: %v", err)
	}
	defer func() { _ = db.Close() }()

	v10 := []string{
		`CREATE TABLE projects (
    id          TEXT PRIMARY KEY,
    path        TEXT NOT NULL UNIQUE,
    name        TEXT NOT NULL,
    created_at  TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at  TEXT NOT NULL DEFAULT (datetime('now'))
)`,
		`CREATE TABLE ghost_state (
    project_id          TEXT PRIMARY KEY,
    interaction_count   INTEGER NOT NULL DEFAULT 0,
    learned_context     TEXT DEFAULT '',
    last_reflection_at  TEXT,
    reflection_summary  TEXT DEFAULT '',
    reflect_input_sig   TEXT NOT NULL DEFAULT '',
    updated_at          TEXT NOT NULL DEFAULT (datetime('now'))
)`,
		`INSERT INTO projects (id, path, name) VALUES ('p1', '/tmp/very-long-v10-path', 'p1')`,
		`PRAGMA user_version = 10`,
	}
	for _, s := range v10 {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("seed v10 db: %v", err)
		}
	}

	if err := migrate(db, 10); err != nil {
		t.Fatalf("migrate v10->v11: %v", err)
	}
	if v := schemaVersionOf(t, db); v != schemaVersion {
		t.Errorf("user_version = %d, want %d", v, schemaVersion)
	}

	var hasColumn int
	if err := db.QueryRow(
		`SELECT count(*) FROM pragma_table_info('projects') WHERE name = 'repo_remote'`,
	).Scan(&hasColumn); err != nil {
		t.Fatalf("inspect projects columns: %v", err)
	}
	if hasColumn != 1 {
		t.Error("projects.repo_remote missing after migrateV11")
	}

	var remote sql.NullString
	var path, name string
	if err := db.QueryRow(`SELECT path, name, repo_remote FROM projects WHERE id = 'p1'`).
		Scan(&path, &name, &remote); err != nil {
		t.Fatalf("read migrated project: %v", err)
	}
	if path != "/tmp/very-long-v10-path" || name != "p1" {
		t.Errorf("project row changed: path=%q name=%q", path, name)
	}
	if remote.Valid {
		t.Errorf("repo_remote = %v, want NULL — a fabricated remote merges unrelated projects", remote.String)
	}
}

// TestMigrateFreshDBHasRepoRemote: a brand-new database takes the initSQL path
// and never runs migrate(), so it needs the column from the start.
func TestMigrateFreshDBHasRepoRemote(t *testing.T) {
	db, err := OpenDB(filepath.Join(t.TempDir(), "fresh.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer func() { _ = db.Close() }()

	var hasColumn int
	if err := db.QueryRow(
		`SELECT count(*) FROM pragma_table_info('projects') WHERE name = 'repo_remote'`,
	).Scan(&hasColumn); err != nil {
		t.Fatalf("inspect projects columns: %v", err)
	}
	if hasColumn != 1 {
		t.Error("projects.repo_remote missing on a fresh database (initSQL)")
	}
}
