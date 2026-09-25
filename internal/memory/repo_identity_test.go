package memory

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
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

// TestResolveOrCreateRepoProject covers the transactional write-side identity
// bridge used when a repository-aware save arrives before the named project
// has recorded a remote.
func TestResolveOrCreateRepoProject(t *testing.T) {
	const (
		remote      = "https://github.com/wcatz/ghost.git"
		other       = "https://github.com/someone/ghost.git"
		canon       = "github.com/wcatz/ghost"
		otherID     = "github.com/someone/ghost"
		newCheckout = "/new/checkout"
	)
	resolve := func(s *Store, ctx context.Context, projectRef string) (string, error) {
		return s.ResolveOrCreateRepoProject(ctx, projectRef, "ghost", newCheckout, newCheckout, "checkout", remote)
	}

	t.Run("binds the unique unclaimed project", func(t *testing.T) {
		s := testStore(t)
		ctx := context.Background()
		if err := s.EnsureProject(ctx, "ghost", "", "ghost"); err != nil {
			t.Fatalf("EnsureProject: %v", err)
		}

		id, err := resolve(s, ctx, "")
		if err != nil {
			t.Fatalf("ResolveOrCreateRepoProject: %v", err)
		}
		if id != "ghost" {
			t.Fatalf("canonical project = %q, want ghost", id)
		}
		var got string
		if err := s.db.QueryRowContext(ctx, `SELECT repo_remote FROM projects WHERE id = 'ghost'`).Scan(&got); err != nil {
			t.Fatalf("read bound repository: %v", err)
		}
		if got != canon {
			t.Errorf("persisted repository = %q, want %q", got, canon)
		}

		id, err = resolve(s, ctx, "")
		if err != nil || id != "ghost" {
			t.Errorf("same binding is not idempotent: got (%q, %v), want (ghost, nil)", id, err)
		}
	})

	t.Run("resolves an existing remote independently of name", func(t *testing.T) {
		s := testStore(t)
		ctx := context.Background()
		if err := s.EnsureProjectWithRepo(ctx, "custom-id", "", "project-display-name", remote); err != nil {
			t.Fatalf("EnsureProjectWithRepo: %v", err)
		}
		id, err := resolve(s, ctx, "")
		if err != nil || id != "custom-id" {
			t.Errorf("existing repository resolved to (%q, %v), want (custom-id, nil)", id, err)
		}
	})

	t.Run("explicit path conflict beats remote owner and unique name", func(t *testing.T) {
		s := testStore(t)
		ctx := context.Background()
		const pathID = "/work/ghost"
		if err := s.EnsureProjectWithRepo(ctx, pathID, pathID, "checkout", other); err != nil {
			t.Fatalf("EnsureProjectWithRepo path: %v", err)
		}
		if err := s.EnsureProject(ctx, "ghost", "", "ghost"); err != nil {
			t.Fatalf("EnsureProject name: %v", err)
		}
		if err := s.EnsureProjectWithRepo(ctx, "remote-owner", "", "owner", remote); err != nil {
			t.Fatalf("EnsureProjectWithRepo owner: %v", err)
		}

		id, err := resolve(s, ctx, pathID)
		if err == nil || !strings.Contains(err.Error(), "different repository") {
			t.Fatalf("ResolveOrCreateRepoProject = (%q, %v), want a different-repository error", id, err)
		}
		var pathRemote, nameRemote string
		if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(repo_remote, '') FROM projects WHERE id = ?`, pathID).Scan(&pathRemote); err != nil {
			t.Fatalf("read path repository: %v", err)
		}
		if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(repo_remote, '') FROM projects WHERE id = 'ghost'`).Scan(&nameRemote); err != nil {
			t.Fatalf("read named repository: %v", err)
		}
		if pathRemote != otherID || nameRemote != "" {
			t.Errorf("repositories after conflict: path=%q name=%q, want original %q and unclaimed", pathRemote, nameRemote, otherID)
		}
	})

	t.Run("merges an unclaimed explicit project into the remote owner", func(t *testing.T) {
		s := testStore(t)
		ctx := context.Background()
		const pathID = "/work/checkout"
		if err := s.EnsureProject(ctx, pathID, pathID, "checkout"); err != nil {
			t.Fatalf("EnsureProject path: %v", err)
		}
		if _, _, _, err := s.Upsert(ctx, pathID, "fact", "memory from the explicit checkout", "manual", 0.5, nil); err != nil {
			t.Fatalf("Upsert path memory: %v", err)
		}
		if err := s.EnsureProjectWithRepo(ctx, "remote-owner", "", "owner", remote); err != nil {
			t.Fatalf("EnsureProjectWithRepo owner: %v", err)
		}

		id, err := resolve(s, ctx, pathID)
		if err != nil || id != "remote-owner" {
			t.Fatalf("resolve explicit project = (%q, %v), want (remote-owner, nil)", id, err)
		}
		var oldRows, movedMemories int
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM projects WHERE id = ?`, pathID).Scan(&oldRows); err != nil {
			t.Fatalf("count old project: %v", err)
		}
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM memories WHERE project_id = 'remote-owner'`).Scan(&movedMemories); err != nil {
			t.Fatalf("count moved memories: %v", err)
		}
		if oldRows != 0 || movedMemories != 1 {
			t.Errorf("explicit project merge left old rows=%d moved memories=%d, want 0/1", oldRows, movedMemories)
		}
	})

	t.Run("ambiguous or conflicting names create the fallback project", func(t *testing.T) {
		for _, tc := range []struct {
			name  string
			seed  []string
			owner string
		}{
			{name: "ambiguous", seed: []string{"first", "second"}},
			{name: "conflicting", owner: otherID},
		} {
			t.Run(tc.name, func(t *testing.T) {
				s := testStore(t)
				ctx := context.Background()
				for _, id := range tc.seed {
					if err := s.EnsureProject(ctx, id, "", "ghost"); err != nil {
						t.Fatalf("EnsureProject %s: %v", id, err)
					}
				}
				if tc.owner != "" {
					if err := s.EnsureProjectWithRepo(ctx, "ghost", "", "ghost", tc.owner); err != nil {
						t.Fatalf("EnsureProjectWithRepo: %v", err)
					}
				}

				id, err := resolve(s, ctx, "")
				if err != nil || id != newCheckout {
					t.Fatalf("fallback resolve = (%q, %v), want (%q, nil)", id, err, newCheckout)
				}
				var count int
				if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM projects WHERE repo_remote = ?`, canon).Scan(&count); err != nil {
					t.Fatalf("count bound repositories: %v", err)
				}
				if count != 1 {
					t.Errorf("fallback recorded %d repository identities, want 1", count)
				}
			})
		}
	})
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
		// A real schema-v10 database always has memories; seeding only
		// projects made this fixture unrealistically minimal, and the later
		// migrateV12 step then failed on a missing table.
		`CREATE TABLE memories (
    id            TEXT PRIMARY KEY DEFAULT (hex(randomblob(16))),
    project_id    TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    category      TEXT NOT NULL DEFAULT 'fact',
    content       TEXT NOT NULL,
    importance    REAL NOT NULL DEFAULT 0.5,
    access_count  INTEGER NOT NULL DEFAULT 0,
    last_accessed TEXT,
    source        TEXT NOT NULL DEFAULT 'reflection',
    tags          TEXT DEFAULT '[]',
    pinned        INTEGER NOT NULL DEFAULT 0,
    created_at    TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at    TEXT NOT NULL DEFAULT (datetime('now')),
    resolved_at   TEXT,
    resolve_kept_hash TEXT NOT NULL DEFAULT '',
    valid_from    TEXT,
    valid_until   TEXT,
    verified_at   TEXT,
    agent         TEXT,
    session_id    TEXT,
    source_ref    TEXT,
    confidence    REAL
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
		// migrate() now continues to v13, which ALTERs memory_snapshots; a real
		// database of this vintage always has the table (it predates v6 — see the
		// pre-v6 schema in this file), just not the identity columns v13 adds.
		`CREATE TABLE memory_snapshots (
    id            TEXT PRIMARY KEY DEFAULT (hex(randomblob(16))),
    snapshot_id   TEXT NOT NULL,
    project_id    TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    category      TEXT NOT NULL,
    content       TEXT NOT NULL,
    importance    REAL NOT NULL,
    source        TEXT NOT NULL,
    tags          TEXT DEFAULT '[]',
    created_at    TEXT NOT NULL DEFAULT (datetime('now'))
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

// TestEnsureProjectDoesNotClearRecordedRemote guards the identity a path-based
// save established against a later save that has no path to inspect.
//
// This was a real defect: the ON CONFLICT clause used COALESCE(excluded,
// existing), and in SQLite an empty string is not NULL — so COALESCE(”, X)
// returns ”. Every ordinary named save therefore erased the remote, and the
// next save from a different checkout no longer matched, quietly creating the
// duplicate project this feature exists to prevent.
func TestEnsureProjectDoesNotClearRecordedRemote(t *testing.T) {
	db, err := OpenDB(filepath.Join(t.TempDir(), "keep-remote.sqlite"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer func() { _ = db.Close() }()

	ctx := context.Background()
	s := NewStore(db, nil)

	const remote = "git@github.com:wcatz/ghost.git"
	if err := s.EnsureProjectWithRepo(ctx, "proj", "/home/u/src/ghost", "ghost", remote); err != nil {
		t.Fatalf("ensure with remote: %v", err)
	}

	// The common case afterwards: a caller that names the project and has no
	// filesystem path to inspect.
	if err := s.EnsureProject(ctx, "proj", "", "ghost"); err != nil {
		t.Fatalf("ensure without remote: %v", err)
	}

	var got sql.NullString
	if err := db.QueryRow(`SELECT repo_remote FROM projects WHERE id = 'proj'`).Scan(&got); err != nil {
		t.Fatalf("read repo_remote: %v", err)
	}
	if !got.Valid || got.String == "" {
		t.Errorf("repo_remote = %v after a named save, want %q preserved — an empty update must not erase identity", got.String, remote)
	}
}

// TestEnsureProjectWithRepoDoesNotOverwriteDifferentRemote keeps project
// identity immutable once recorded. A later path save that detects another
// repository must not relabel the existing project and redirect writes to it.
func TestEnsureProjectWithRepoDoesNotOverwriteDifferentRemote(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	const (
		original = "https://github.com/someone/ghost.git"
		attempt  = "https://github.com/wcatz/ghost.git"
	)
	if err := s.EnsureProjectWithRepo(ctx, "proj", "/work/ghost", "ghost", original); err != nil {
		t.Fatalf("EnsureProjectWithRepo original: %v", err)
	}
	if err := s.EnsureProjectWithRepo(ctx, "proj", "/work/ghost", "ghost", attempt); err == nil {
		t.Fatal("EnsureProjectWithRepo accepted a different remote for an existing project")
	} else if !strings.Contains(err.Error(), "different repository") {
		t.Fatalf("conflict error = %q, want different-repository explanation", err)
	}

	var got string
	if err := s.db.QueryRowContext(ctx, `SELECT repo_remote FROM projects WHERE id = 'proj'`).Scan(&got); err != nil {
		t.Fatalf("read repository: %v", err)
	}
	if want := "github.com/someone/ghost"; got != want {
		t.Errorf("persisted repository = %q, want original %q", got, want)
	}
}

// TestEnsureProjectWithRepoDoesNotMergeDifferentRemoteProject stops the
// repository-duplicate merge before it can erase a project's prior identity.
// The incoming project already belongs to one repository; matching another
// project is a contradiction, not evidence that the first should be merged.
func TestEnsureProjectWithRepoDoesNotMergeDifferentRemoteProject(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	const (
		original   = "https://github.com/someone/project.git"
		attempt    = "https://github.com/wcatz/project.git"
		originalID = "github.com/someone/project"
		attemptID  = "github.com/wcatz/project"
	)
	if err := s.EnsureProjectWithRepo(ctx, "original-id", "/original", "original", original); err != nil {
		t.Fatalf("EnsureProjectWithRepo original: %v", err)
	}
	if err := s.EnsureProjectWithRepo(ctx, "attempt-id", "/attempt", "attempt", attempt); err != nil {
		t.Fatalf("EnsureProjectWithRepo attempt: %v", err)
	}

	if err := s.EnsureProjectWithRepo(ctx, "original-id", "/original", "original", attempt); err == nil {
		t.Fatal("EnsureProjectWithRepo merged a project across conflicting repositories")
	} else if !strings.Contains(err.Error(), "different repository") {
		t.Fatalf("conflict error = %q, want different-repository explanation", err)
	}

	for id, want := range map[string]string{
		"original-id": originalID,
		"attempt-id":  attemptID,
	} {
		var got string
		if err := s.db.QueryRowContext(ctx, `SELECT repo_remote FROM projects WHERE id = ?`, id).Scan(&got); err != nil {
			t.Fatalf("read repository for %s: %v", id, err)
		}
		if got != want {
			t.Errorf("repository for %s = %q, want %q", id, got, want)
		}
	}
}

// TestEnsureProjectWithRepoRejectsPathOwnerWithDifferentRemote keeps path
// self-healing inside repository identity. A new id carrying a known remote
// cannot erase or absorb a path already owned by another repository.
func TestEnsureProjectWithRepoRejectsPathOwnerWithDifferentRemote(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	const (
		path       = "/work/ghost"
		original   = "https://github.com/someone/ghost.git"
		attempt    = "https://github.com/wcatz/ghost.git"
		originalID = "github.com/someone/ghost"
	)
	if err := s.EnsureProjectWithRepo(ctx, "existing", path, "existing", original); err != nil {
		t.Fatalf("EnsureProjectWithRepo existing: %v", err)
	}

	err := s.EnsureProjectWithRepo(ctx, "incoming", path, "ghost", attempt)
	if err == nil || !strings.Contains(err.Error(), "different repository") {
		t.Fatalf("EnsureProjectWithRepo error = %v, want different-repository conflict", err)
	}
	var existingRows, incomingRows int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM projects WHERE id = 'existing'`).Scan(&existingRows); err != nil {
		t.Fatalf("count existing project: %v", err)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM projects WHERE id = 'incoming'`).Scan(&incomingRows); err != nil {
		t.Fatalf("count incoming project: %v", err)
	}
	if existingRows != 1 || incomingRows != 0 {
		t.Errorf("path conflict rows: existing=%d incoming=%d, want 1/0", existingRows, incomingRows)
	}
	var got string
	if err := s.db.QueryRowContext(ctx, `SELECT repo_remote FROM projects WHERE id = 'existing'`).Scan(&got); err != nil {
		t.Fatalf("read existing repository: %v", err)
	}
	if got != originalID {
		t.Errorf("existing repository = %q, want %q", got, originalID)
	}
}

// TestEnsureProjectWithRepoRejectsSameNameOwnerWithDifferentRemote applies the
// same identity rule to the legacy MCP same-name auto-merge.
func TestEnsureProjectWithRepoRejectsSameNameOwnerWithDifferentRemote(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	const (
		original   = "https://github.com/someone/ghost.git"
		attempt    = "https://github.com/wcatz/ghost.git"
		originalID = "github.com/someone/ghost"
	)
	if err := s.EnsureProjectWithRepo(ctx, "incoming", "/work/ghost", "ghost", attempt); err != nil {
		t.Fatalf("EnsureProjectWithRepo incoming: %v", err)
	}
	if err := s.EnsureProjectWithRepo(ctx, "named", "", "ghost", original); err != nil {
		t.Fatalf("EnsureProjectWithRepo named: %v", err)
	}

	err := s.EnsureProjectWithRepo(ctx, "incoming", "/work/ghost", "ghost", attempt)
	if err == nil || !strings.Contains(err.Error(), "different repository") {
		t.Fatalf("EnsureProjectWithRepo error = %v, want different-repository conflict", err)
	}
	for id, want := range map[string]string{"incoming": "github.com/wcatz/ghost", "named": originalID} {
		var got string
		if err := s.db.QueryRowContext(ctx, `SELECT repo_remote FROM projects WHERE id = ?`, id).Scan(&got); err != nil {
			t.Fatalf("read repository for %s: %v", id, err)
		}
		if got != want {
			t.Errorf("repository for %s = %q, want %q", id, got, want)
		}
	}
}

// TestEnsureProjectWithRepoRollsBackRepositoryBindingWhenMergeFails proves the
// target binding and row move are one transaction. A failed child update must
// not leave the target looking like the incoming repository while the source
// and its records remain in place.
func TestEnsureProjectWithRepoRollsBackRepositoryBindingWhenMergeFails(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	const targetPath = "/merge-target"
	if err := s.EnsureProject(ctx, "target", targetPath, "target"); err != nil {
		t.Fatalf("EnsureProject target: %v", err)
	}
	if err := s.EnsureProject(ctx, "source", "", "source"); err != nil {
		t.Fatalf("EnsureProject source: %v", err)
	}
	if _, err := s.Create(ctx, "source", Memory{Category: "fact", Content: "source memory", Source: "manual"}); err != nil {
		t.Fatalf("Create source memory: %v", err)
	}
	if _, err := s.db.ExecContext(ctx, `
		CREATE TRIGGER fail_source_merge
		BEFORE UPDATE OF project_id ON memories
		WHEN OLD.project_id = 'source'
		BEGIN
			SELECT RAISE(ABORT, 'forced merge failure');
		END
	`); err != nil {
		t.Fatalf("create failure trigger: %v", err)
	}

	err := s.EnsureProjectWithRepo(ctx, "source", targetPath, "target", "https://github.com/wcatz/target.git")
	if err == nil || !strings.Contains(err.Error(), "forced merge failure") {
		t.Fatalf("EnsureProjectWithRepo error = %v, want forced merge failure", err)
	}
	for _, id := range []string{"source", "target"} {
		var remote string
		if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(repo_remote, '') FROM projects WHERE id = ?`, id).Scan(&remote); err != nil {
			t.Fatalf("read repository for %s: %v", id, err)
		}
		if remote != "" {
			t.Errorf("failed merge left %s bound to %q", id, remote)
		}
	}
	if count, err := s.CountMemories(ctx, "source"); err != nil || count != 1 {
		t.Errorf("source memories after failed merge = %d, err=%v; want 1/nil", count, err)
	}
}

// TestResolveOrCreateRepoProjectRefusesGlobalPathOwner protects the shared
// transaction boundary even though MCP currently passes path == id and cannot
// reach this branch.
func TestResolveOrCreateRepoProjectRefusesGlobalPathOwner(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if err := s.EnsureProject(ctx, "_global", "", "_global"); err != nil {
		t.Fatalf("EnsureProject global: %v", err)
	}
	globalPath, err := s.GetProjectPath(ctx, "_global")
	if err != nil {
		t.Fatalf("GetProjectPath global: %v", err)
	}

	id, err := s.ResolveOrCreateRepoProject(
		ctx, "new-id", "ghost", "new-id", globalPath, "new-project", "https://github.com/wcatz/ghost.git",
	)
	if err == nil || !strings.Contains(err.Error(), "_global") {
		t.Fatalf("ResolveOrCreateRepoProject = (%q, %v), want _global refusal", id, err)
	}
	var remote string
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(repo_remote, '') FROM projects WHERE id = '_global'`).Scan(&remote); err != nil {
		t.Fatalf("read global repository: %v", err)
	}
	if remote != "" {
		t.Errorf("global project was bound to %q", remote)
	}
	var rows int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM projects WHERE id = 'new-id'`).Scan(&rows); err != nil {
		t.Fatalf("count new project: %v", err)
	}
	if rows != 0 {
		t.Errorf("refused global-path transaction created %d new project rows", rows)
	}
}

// TestMergeProjectPreservesSupersedeChecked keeps the project-scoped NEITHER
// cache attached to the canonical project when repository identity merges two
// rows. Deleting the old project without this reassignment cascades the cache
// away and forces needless classifier work.
func TestMergeProjectPreservesSupersedeChecked(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	for _, id := range []string{"old", "new"} {
		if err := s.EnsureProject(ctx, id, "", id); err != nil {
			t.Fatalf("EnsureProject %s: %v", id, err)
		}
	}
	newer, err := s.Create(ctx, "old", Memory{Category: "fact", Content: "newer memory", Source: "manual"})
	if err != nil {
		t.Fatalf("Create newer: %v", err)
	}
	older, err := s.Create(ctx, "old", Memory{Category: "fact", Content: "older memory", Source: "manual"})
	if err != nil {
		t.Fatalf("Create older: %v", err)
	}
	if err := s.MarkSupersedeNeither(ctx, "old", map[[2]string]SupersedeCheck{
		{newer, older}: {NewerHash: "newer-hash", OlderHash: "older-hash"},
	}); err != nil {
		t.Fatalf("MarkSupersedeNeither: %v", err)
	}

	if err := s.MergeProject(ctx, "old", "new"); err != nil {
		t.Fatalf("MergeProject: %v", err)
	}
	checks, err := s.SupersedeChecked(ctx, "new")
	if err != nil {
		t.Fatalf("SupersedeChecked: %v", err)
	}
	if len(checks) != 1 {
		t.Errorf("canonical project retained %d supersede checks, want 1", len(checks))
	}
}

// TestEnsureProjectWithRepoRejectsGlobalRemote keeps _global outside
// repository identity entirely. A checkout named global must never relabel or
// route writes into the bucket injected into every project.
func TestEnsureProjectWithRepoRejectsGlobalRemote(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if err := s.EnsureProject(ctx, "_global", "", "_global"); err != nil {
		t.Fatalf("EnsureProject global: %v", err)
	}
	if err := s.EnsureProjectWithRepo(ctx, "_global", "", "_global", "https://github.com/wcatz/ghost.git"); err == nil {
		t.Fatal("EnsureProjectWithRepo assigned a repository to _global")
	}
	var remote string
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(repo_remote, '') FROM projects WHERE id = '_global'`).Scan(&remote); err != nil {
		t.Fatalf("read global repository: %v", err)
	}
	if remote != "" {
		t.Errorf("global repository = %q, want empty", remote)
	}
}

// TestResolveOrCreateRepoProjectRejectsExistingIDConflict validates the
// fallback id even when projectRef is empty. An existing repository-A row
// cannot be returned or merged into for a repository-B save.
func TestResolveOrCreateRepoProjectRejectsExistingIDConflict(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	const other = "https://github.com/someone/ghost.git"
	if err := s.EnsureProjectWithRepo(ctx, "existing", "", "existing", other); err != nil {
		t.Fatalf("EnsureProjectWithRepo existing: %v", err)
	}
	if _, err := s.Create(ctx, "existing", Memory{Category: "fact", Content: "existing memory", Source: "manual"}); err != nil {
		t.Fatalf("Create existing memory: %v", err)
	}

	id, err := s.ResolveOrCreateRepoProject(
		ctx, "", "ghost", "existing", "existing", "existing", "https://github.com/wcatz/ghost.git",
	)
	if err == nil || !strings.Contains(err.Error(), "different repository") {
		t.Fatalf("ResolveOrCreateRepoProject = (%q, %v), want different-repository conflict", id, err)
	}
	if count, err := s.CountMemories(ctx, "existing"); err != nil || count != 1 {
		t.Errorf("existing memories after conflict = %d, err=%v; want 1/nil", count, err)
	}
}

// TestEnsureProjectWithRepoRollsBackNewProjectWhenSameNameMergeFails makes the
// incoming insert and same-name merge one transaction. A failed move must not
// leave behind a new project row with no children.
func TestEnsureProjectWithRepoRollsBackNewProjectWhenSameNameMergeFails(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if err := s.EnsureProject(ctx, "named", "", "ghost"); err != nil {
		t.Fatalf("EnsureProject named: %v", err)
	}
	if _, err := s.Create(ctx, "named", Memory{Category: "fact", Content: "named memory", Source: "manual"}); err != nil {
		t.Fatalf("Create named memory: %v", err)
	}
	if _, err := s.db.ExecContext(ctx, `
		CREATE TRIGGER fail_named_merge
		BEFORE UPDATE OF project_id ON memories
		WHEN OLD.project_id = 'named'
		BEGIN
			SELECT RAISE(ABORT, 'forced named merge failure');
		END
	`); err != nil {
		t.Fatalf("create failure trigger: %v", err)
	}

	err := s.EnsureProjectWithRepo(ctx, "incoming", "/work/ghost", "ghost", "https://github.com/wcatz/ghost.git")
	if err == nil || !strings.Contains(err.Error(), "forced named merge failure") {
		t.Fatalf("EnsureProjectWithRepo error = %v, want forced named merge failure", err)
	}
	var rows int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM projects WHERE id = 'incoming'`).Scan(&rows); err != nil {
		t.Fatalf("count incoming project: %v", err)
	}
	if rows != 0 {
		t.Errorf("failed same-name merge left %d incoming project rows", rows)
	}
}

// TestEnsureProjectWithRepoDoesNotChooseAmbiguousNameOwner keeps the legacy
// same-name auto-merge fail-closed. Two candidates provide no defensible merge
// target, just an arbitrary LIMIT 1 choice.
func TestEnsureProjectWithRepoDoesNotChooseAmbiguousNameOwner(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	for _, id := range []string{"first", "second"} {
		if err := s.EnsureProject(ctx, id, "", "ghost"); err != nil {
			t.Fatalf("EnsureProject %s: %v", id, err)
		}
	}
	if err := s.EnsureProjectWithRepo(ctx, "incoming", "/work/ghost", "ghost", "https://github.com/wcatz/ghost.git"); err != nil {
		t.Fatalf("EnsureProjectWithRepo incoming: %v", err)
	}
	for _, id := range []string{"first", "second"} {
		var rows int
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM projects WHERE id = ?`, id).Scan(&rows); err != nil {
			t.Fatalf("count %s: %v", id, err)
		}
		if rows != 1 {
			t.Errorf("ambiguous same-name merge changed %s: rows=%d, want 1", id, rows)
		}
	}
}

// TestNormalizeRepoRemoteTrailingSlash: git permits trailing slashes in remote
// URLs, and two spellings of one repository that normalize differently are two
// projects again.
func TestNormalizeRepoRemoteTrailingSlash(t *testing.T) {
	want := "github.com/wcatz/ghost"
	for _, in := range []string{
		"https://github.com/wcatz/ghost.git",
		"https://github.com/wcatz/ghost.git/",
		"https://github.com/wcatz/ghost/",
		"https://github.com/wcatz/ghost",
		"git@github.com:wcatz/ghost.git/",
		"github.com/wcatz/ghost/",
	} {
		if got := NormalizeRepoRemote(in); got != want {
			t.Errorf("NormalizeRepoRemote(%q) = %q, want %q", in, got, want)
		}
	}
}
