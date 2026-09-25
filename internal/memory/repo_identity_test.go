package memory

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
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

	// Repository detection is wired once in main() for the whole binary
	// (cmd/ghost/main.go), so every shipped entry point resolves absolute
	// inputs through it. A store built without a detector has no repository
	// identity for a path at all — the comment on SetDetectRemote says so
	// explicitly — and would be resolving the second checkout by directory
	// name, which is not what this test is about. Injecting here keeps the
	// store in the state the binary actually runs in.
	SetDetectRemote(func(string) string { return remote })
	t.Cleanup(func() { SetDetectRemote(nil) })

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
	root := t.TempDir()
	pathA := filepath.Join(root, "src", "ghost")
	pathB := filepath.Join(root, "src", "other-ghost")
	for _, path := range []string{pathA, pathB} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", path, err)
		}
	}
	if err := s.EnsureProjectWithRepo(ctx, "a", pathA, "ghost", "git@github.com:wcatz/ghost.git"); err != nil {
		t.Fatalf("ensure a: %v", err)
	}
	if err := s.EnsureProjectWithRepo(ctx, "b", pathB, "other-ghost", "git@github.com:someone/other-ghost.git"); err != nil {
		t.Fatalf("ensure b: %v", err)
	}

	for _, in := range []string{pathA, pathB} {
		id, _, err := s.ResolveProject(ctx, in)
		if err != nil {
			t.Fatalf("resolve %q: %v", in, err)
		}
		want := "a"
		if in == pathB {
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

func TestMigrateV14MergesDuplicateRepositoryIdentities(t *testing.T) {
	path := filepath.Join(t.TempDir(), "duplicate-remote.sqlite")
	db, err := OpenDB(path)
	if err != nil {
		t.Fatalf("OpenDB seed: %v", err)
	}
	if _, err := db.Exec(`DROP INDEX idx_projects_repo_remote`); err != nil {
		_ = db.Close()
		t.Fatalf("drop seed index: %v", err)
	}
	const remote = "github.com/me/infra"
	if _, err := db.Exec(`
		INSERT INTO projects (id, path, name, repo_remote) VALUES
		('remote-a', '/remote/a', 'a', ?),
		('remote-b', '/remote/b', 'b', ?)
	`, remote, remote); err != nil {
		_ = db.Close()
		t.Fatalf("insert duplicate projects: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO memories (id, project_id, category, content, source)
		VALUES ('remote-memory', 'remote-b', 'fact', 'preserve me', 'manual')
	`); err != nil {
		_ = db.Close()
		t.Fatalf("insert duplicate project memory: %v", err)
	}
	if _, err := db.Exec(`PRAGMA user_version = 13`); err != nil {
		_ = db.Close()
		t.Fatalf("stamp pre-v14 schema: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close seed database: %v", err)
	}

	migrated, err := OpenDB(path)
	if err != nil {
		t.Fatalf("OpenDB migrated: %v", err)
	}
	defer migrated.Close() //nolint:errcheck
	var projects, memories int
	if err := migrated.QueryRow(`SELECT count(*) FROM projects WHERE repo_remote = ?`, remote).Scan(&projects); err != nil {
		t.Fatalf("count migrated projects: %v", err)
	}
	if projects != 1 {
		t.Fatalf("migrated remote project count = %d, want 1", projects)
	}
	if err := migrated.QueryRow(`SELECT count(*) FROM memories WHERE id = 'remote-memory' AND project_id = 'remote-a'`).Scan(&memories); err != nil {
		t.Fatalf("read migrated memory: %v", err)
	}
	if memories != 1 {
		t.Fatal("migration did not preserve the duplicate project's memory")
	}
	var indexName string
	if err := migrated.QueryRow(`SELECT name FROM sqlite_master WHERE type = 'index' AND name = 'idx_projects_repo_remote'`).Scan(&indexName); err != nil {
		t.Fatalf("repository identity index missing after migration: %v", err)
	}
}

func TestEnsureProjectWithRepoConcurrentStoresConverge(t *testing.T) {
	path := filepath.Join(t.TempDir(), "concurrent.sqlite")
	seed, err := OpenDB(path)
	if err != nil {
		t.Fatalf("OpenDB seed: %v", err)
	}
	if err := seed.Close(); err != nil {
		t.Fatalf("close seed: %v", err)
	}

	const workers = 4
	stores := make([]*Store, workers)
	dbs := make([]*sql.DB, workers)
	for i := range stores {
		dbs[i], err = OpenDB(path)
		if err != nil {
			t.Fatalf("OpenDB worker %d: %v", i, err)
		}
		stores[i] = NewStore(dbs[i], nil)
	}
	t.Cleanup(func() {
		for _, db := range dbs {
			_ = db.Close()
		}
	})

	paths := make([]string, workers)
	for i := range paths {
		paths[i] = filepath.Join(t.TempDir(), "checkout")
	}
	start := make(chan struct{})
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for i, store := range stores {
		wg.Add(1)
		go func(i int, store *Store) {
			defer wg.Done()
			<-start
			errs <- store.EnsureProjectWithRepo(context.Background(),
				fmt.Sprintf("concurrent-%d", i),
				paths[i],
				fmt.Sprintf("checkout-%d", i),
				"github.com/me/infra")
		}(i, store)
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent EnsureProjectWithRepo: %v", err)
		}
	}

	var count int
	if err := dbs[0].QueryRow(`SELECT count(*) FROM projects WHERE repo_remote = 'github.com/me/infra'`).Scan(&count); err != nil {
		t.Fatalf("count remote projects: %v", err)
	}
	if count != 1 {
		t.Fatalf("concurrent creation left %d projects for one remote, want 1", count)
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

// TestResolveOrCreateRepoProjectRefusesUnrelatedNameBinding keeps a save from
// an unrelated clone out of a project that merely shares its directory name.
//
// A recorded path and an empty remote is the state of every project upgraded
// from a v9 database, so the unique-name fallback had nothing to contradict it:
// a save reporting ~/Downloads/infra with origin github.com/evil/infra bound
// that remote to the real ~/git/infra project, and from then on the unrelated
// directory resolved as the real project while the real checkout was locked
// out by the remote conflict. A unique name is weak evidence; the candidate's
// own path is what makes it checkable.
func TestResolveOrCreateRepoProjectRefusesUnrelatedNameBinding(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	// Both directories exist: pathsAgree resolves what it compares, so a
	// missing unrelated directory would make this pass for the wrong reason.
	root := t.TempDir()
	realPath := filepath.Join(root, "git", "infra")
	unrelatedPath := filepath.Join(root, "Downloads", "infra")
	for _, dir := range []string{realPath, unrelatedPath} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("create %s: %v", dir, err)
		}
	}
	if err := s.EnsureProject(ctx, realPath, realPath, "infra"); err != nil {
		t.Fatalf("EnsureProject real checkout: %v", err)
	}

	const evil = "https://github.com/evil/infra.git"
	canonical, err := s.ResolveOrCreateRepoProject(
		ctx, unrelatedPath, "infra", unrelatedPath, unrelatedPath, unrelatedPath, evil,
	)
	if err != nil {
		t.Fatalf("ResolveOrCreateRepoProject: %v", err)
	}
	if canonical == realPath {
		t.Errorf("save from the unrelated clone resolved to the real project %q — its name was enough to claim it", realPath)
	}

	var realRemote string
	if err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(repo_remote, '') FROM projects WHERE id = ?`, realPath).Scan(&realRemote); err != nil {
		t.Fatalf("read real project repository: %v", err)
	}
	if realRemote != "" {
		t.Errorf("real project was bound to %q by an unrelated clone, want it left unclaimed", realRemote)
	}

	// The real checkout must still be reachable by its own path, and the
	// unrelated directory must not resolve to it any more.
	if id, _, err := s.ResolveProject(ctx, realPath); err != nil {
		t.Fatalf("ResolveProject real path: %v", err)
	} else if id != realPath {
		t.Errorf("ResolveProject(%q) = %q, want the real project %q", realPath, id, realPath)
	}
	if id, _, err := s.ResolveProject(ctx, unrelatedPath); err != nil {
		t.Fatalf("ResolveProject unrelated path: %v", err)
	} else if id == realPath {
		t.Errorf("ResolveProject(%q) = the real project %q, want the unrelated clone kept apart", unrelatedPath, realPath)
	}

	// The refused save is not dropped: it becomes its own project carrying
	// its own remote, which is what keeps the real one addressable.
	var canonicalRemote string
	if err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(repo_remote, '') FROM projects WHERE id = ?`, canonical).Scan(&canonicalRemote); err != nil {
		t.Fatalf("read canonical project repository: %v", err)
	}
	if want := "github.com/evil/infra"; canonicalRemote != want {
		t.Errorf("refused save recorded repository %q on %q, want %q", canonicalRemote, canonical, want)
	}
}

// TestResolveOrCreateRepoProjectBindsNameWhenPathsAgree is the other half of
// the guard above: refusing a name binding is only safe because a name that
// does agree still binds. A guard that read "the project has a recorded path"
// as "refuse" would strand every project upgraded from a v9 database, whose
// path is present and whose remote is not.
func TestResolveOrCreateRepoProjectBindsNameWhenPathsAgree(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	// A project recorded at a real checkout whose recorded path spells a
	// trailing separator, and no remote: a path a directory can be compared
	// against, that no text comparison finds equal to the plain spelling.
	checkout := filepath.Join(t.TempDir(), "git", "infra")
	if err := os.MkdirAll(checkout, 0o755); err != nil {
		t.Fatalf("create %s: %v", checkout, err)
	}
	if err := s.EnsureProject(ctx, "real-id", checkout+string(os.PathSeparator), "infra"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}

	// The session reports the same directory without that separator. The
	// explicit path query compares text and misses it, so the unique name is
	// the only thing that can claim this save — and the recorded path agrees
	// with where the save came from.
	canonical, err := s.ResolveOrCreateRepoProject(
		ctx, checkout, "infra", checkout, checkout, checkout, "https://github.com/wcatz/infra.git",
	)
	if err != nil {
		t.Fatalf("ResolveOrCreateRepoProject: %v", err)
	}
	if canonical != "real-id" {
		t.Errorf("save from the project's own checkout resolved to %q, want %q", canonical, "real-id")
	}
	var got string
	if err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(repo_remote, '') FROM projects WHERE id = 'real-id'`).Scan(&got); err != nil {
		t.Fatalf("read bound repository: %v", err)
	}
	if want := "github.com/wcatz/infra"; got != want {
		t.Errorf("bound repository = %q, want %q", got, want)
	}
}

// TestResolveOrCreateRepoProjectRefusesUnresolvableRecordedPath pins the one
// case the guard cannot read as a disagreement: a recorded path that is
// absolute and well formed but no longer resolves, because the checkout was
// moved, deleted, or recorded on a volume that is not mounted. pathsAgree
// resolves both sides on disk, so it reports no agreement, and the same two
// rules agreeWithSession applies on the read side refuse there too. The
// project's memories stay put and a save from its own path still binds it;
// what is refused is a different directory inheriting them.
func TestResolveOrCreateRepoProjectRefusesUnresolvableRecordedPath(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	// The recorded checkout no longer exists; the saving one does.
	gone := filepath.Join(t.TempDir(), "git", "infra")
	if err := s.EnsureProject(ctx, "real-id", gone, "infra"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	elsewhere := filepath.Join(t.TempDir(), "Downloads", "infra")
	if err := os.MkdirAll(elsewhere, 0o755); err != nil {
		t.Fatalf("create %s: %v", elsewhere, err)
	}

	canonical, err := s.ResolveOrCreateRepoProject(
		ctx, elsewhere, "infra", elsewhere, elsewhere, elsewhere, "https://github.com/evil/infra.git",
	)
	if err != nil {
		t.Fatalf("ResolveOrCreateRepoProject: %v", err)
	}
	if canonical == "real-id" {
		t.Errorf("save from %q claimed a project whose recorded path does not resolve", elsewhere)
	}
	var got string
	if err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(repo_remote, '') FROM projects WHERE id = 'real-id'`).Scan(&got); err != nil {
		t.Fatalf("read bound repository: %v", err)
	}
	if got != "" {
		t.Errorf("project with an unresolvable recorded path was bound to %q", got)
	}
}

// TestResolveOrCreateRepoProjectNestedRepoKeepsEnclosingProjectUnbound stops a
// nested checkout from lending its remote to the project that encloses it.
//
// The write-side path-prefix step answers a save from ~/git/infra/vendor/lib
// with the project recorded at ~/git/infra, which is right: the save belongs to
// the project whose checkout the session is standing in. It was also writing
// that checkout's identity onto the parent, and a submodule or vendored
// repository is a different repository by definition — so a save from inside one
// bound other/lib to ~/git/infra and locked the real checkout out of its own
// project at every path except exactly its root. The parent is a v9-era row
// with no remote, so the first nested save is enough.
//
// What the enclosing project is asked to absorb is therefore a decision, and
// only the project's own root may make it. The save still lands in the parent:
// routing a session is not the same claim as identifying the repository.
func TestResolveOrCreateRepoProjectNestedRepoKeepsEnclosingProjectUnbound(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	// Both directories exist: the comparison is made on resolved paths, so a
	// nested path that cannot be resolved would make this pass for the wrong
	// reason.
	root := t.TempDir()
	parent := filepath.Join(root, "git", "infra")
	nested := filepath.Join(parent, "vendor", "lib")
	for _, dir := range []string{parent, nested} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("create %s: %v", dir, err)
		}
	}
	if err := s.EnsureProject(ctx, parent, parent, "infra"); err != nil {
		t.Fatalf("EnsureProject parent: %v", err)
	}
	// The detector answers as git would, per directory: the nested checkout has
	// its own origin and the enclosing tree has its own. Without this the guard
	// would be proved by detecting nothing at all rather than by the two
	// repositories disagreeing, which is the claim under test.
	SetDetectRemote(func(dir string) string {
		if strings.HasPrefix(dir, nested) {
			return "https://github.com/other/lib.git"
		}
		return "https://github.com/wcatz/infra.git"
	})
	t.Cleanup(func() { SetDetectRemote(nil) })

	canonical, err := s.ResolveOrCreateRepoProject(
		ctx, nested, "lib", nested, nested, "lib", "https://github.com/other/lib.git",
	)
	if err != nil {
		t.Fatalf("ResolveOrCreateRepoProject from the nested checkout: %v", err)
	}
	if canonical != parent {
		t.Errorf("nested save resolved to %q, want the enclosing project %q", canonical, parent)
	}
	var parentRemote string
	if err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(repo_remote, '') FROM projects WHERE id = ?`, parent).Scan(&parentRemote); err != nil {
		t.Fatalf("read parent project repository: %v", err)
	}
	if parentRemote != "" {
		t.Errorf("a nested checkout bound its remote %q to the enclosing project, want it left unclaimed", parentRemote)
	}

	// A save from the project's own root is the claim the parent is allowed to
	// make, and it must still make it — a guard that read "matched by path
	// prefix" as "refuse" would strand every project with no recorded remote.
	canonical, err = s.ResolveOrCreateRepoProject(
		ctx, parent, "infra", parent, parent, "infra", "https://github.com/wcatz/infra.git",
	)
	if err != nil {
		t.Fatalf("ResolveOrCreateRepoProject from the project root: %v", err)
	}
	if canonical != parent {
		t.Errorf("save from the project root resolved to %q, want %q", canonical, parent)
	}
	if err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(repo_remote, '') FROM projects WHERE id = ?`, parent).Scan(&parentRemote); err != nil {
		t.Fatalf("read bound parent repository: %v", err)
	}
	if want := "github.com/wcatz/infra"; parentRemote != want {
		t.Errorf("project root bound repository %q, want %q", parentRemote, want)
	}
}

// TestResolveOrCreateRepoProjectNestedRepoLeavesEnclosingProjectResolvable is
// what the refusal above buys, stated as the user-visible outcome rather than as
// a column value: after a save from a nested checkout, a session anywhere in
// the enclosing checkout still resolves to it.
//
// The bug did not merely record a wrong remote. Once the nested checkout's
// remote sat on the parent, every save and every read from the real checkout
// reported "belongs to a different repository" except at exactly the root, so
// the project was reachable from one directory out of all of them.
func TestResolveOrCreateRepoProjectNestedRepoLeavesEnclosingProjectResolvable(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	root := t.TempDir()
	parent := filepath.Join(root, "git", "infra")
	nested := filepath.Join(parent, "vendor", "lib")
	subdir := filepath.Join(parent, "docs")
	for _, dir := range []string{parent, nested, subdir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("create %s: %v", dir, err)
		}
	}
	if err := s.EnsureProject(ctx, parent, parent, "infra"); err != nil {
		t.Fatalf("EnsureProject parent: %v", err)
	}

	// Wired before the saves, not only before the reads below: the guard asks
	// the same question this detector answers, and a test that wired it late
	// would be proving the refusal with a detector that is switched off.
	SetDetectRemote(func(dir string) string {
		if strings.HasPrefix(dir, nested) {
			return "https://github.com/other/lib.git"
		}
		return "https://github.com/wcatz/infra.git"
	})
	t.Cleanup(func() { SetDetectRemote(nil) })

	if _, err := s.ResolveOrCreateRepoProject(
		ctx, nested, "lib", nested, nested, "lib", "https://github.com/other/lib.git",
	); err != nil {
		t.Fatalf("ResolveOrCreateRepoProject from the nested checkout: %v", err)
	}
	if _, err := s.ResolveOrCreateRepoProject(
		ctx, parent, "infra", parent, parent, "infra", "https://github.com/wcatz/infra.git",
	); err != nil {
		t.Fatalf("ResolveOrCreateRepoProject from the project root: %v", err)
	}

	if id, _, err := s.ResolveProject(ctx, subdir); err != nil {
		t.Fatalf("ResolveProject from a subdirectory: %v", err)
	} else if id != parent {
		t.Errorf("ResolveProject(%q) = %q, want the enclosing project %q", subdir, id, parent)
	}

	// And the nested checkout is not folded into it: its repository is not the
	// parent's, so a session standing in one is refused rather than answered
	// with the enclosing project's memories.
	if id, _, err := s.ResolveProject(ctx, nested); err != nil {
		t.Fatalf("ResolveProject from the nested checkout: %v", err)
	} else if id != "" {
		t.Errorf("ResolveProject(%q) = %q, want a refusal: the nested checkout is another repository", nested, id)
	}
}

// TestResolveOrCreateRepoProjectNestedRepoDoesNotMergeEnclosingProject is the
// other write the path-prefix step could do with a nested checkout's remote, and
// the more destructive one: when some other project already records that
// repository, the step merges the matched project into it. A save from a nested
// checkout therefore used to delete the enclosing project's row and move its
// memories into the nested repository's project — the same identity theft as the
// bind, with nothing left behind to notice it by.
func TestResolveOrCreateRepoProjectNestedRepoDoesNotMergeEnclosingProject(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	root := t.TempDir()
	parent := filepath.Join(root, "git", "infra")
	nested := filepath.Join(parent, "vendor", "lib")
	for _, dir := range []string{parent, nested} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("create %s: %v", dir, err)
		}
	}
	if err := s.EnsureProject(ctx, parent, parent, "infra"); err != nil {
		t.Fatalf("EnsureProject parent: %v", err)
	}
	const content = "memory that belongs to the enclosing project"
	if _, _, _, err := s.Upsert(ctx, parent, "fact", content, "manual", 0.5, nil); err != nil {
		t.Fatalf("Upsert parent memory: %v", err)
	}
	// The nested repository's project already exists, from another checkout of
	// it — the state in which the prefix step merges instead of binding.
	const nestedOwner = "github.com/other/lib"
	if err := s.EnsureProjectWithRepo(ctx, nestedOwner, "", "lib", "https://github.com/other/lib.git"); err != nil {
		t.Fatalf("EnsureProjectWithRepo nested owner: %v", err)
	}
	SetDetectRemote(func(dir string) string {
		if strings.HasPrefix(dir, nested) {
			return "https://github.com/other/lib.git"
		}
		return "https://github.com/wcatz/infra.git"
	})
	t.Cleanup(func() { SetDetectRemote(nil) })

	canonical, err := s.ResolveOrCreateRepoProject(
		ctx, nested, "lib", nested, nested, "lib", "https://github.com/other/lib.git",
	)
	if err != nil {
		t.Fatalf("ResolveOrCreateRepoProject from the nested checkout: %v", err)
	}
	if canonical != parent {
		t.Errorf("nested save resolved to %q, want the enclosing project %q", canonical, parent)
	}
	var rows int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM projects WHERE id = ?`, parent).Scan(&rows); err != nil {
		t.Fatalf("count parent project: %v", err)
	}
	if rows != 1 {
		t.Errorf("enclosing project rows = %d, want 1: a nested save merged it away", rows)
	}
	var moved int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM memories WHERE project_id = ? AND content = ?`, nestedOwner, content).Scan(&moved); err != nil {
		t.Fatalf("count moved memories: %v", err)
	}
	if moved != 0 {
		t.Errorf("%d memories of the enclosing project moved into the nested checkout's project", moved)
	}
}

// TestResolveOrCreateRepoProjectBindsRemoteFromSubdirectoryOfSameRepository is
// the other half of the nested-checkout rule, and the half a guard written as
// "only the project's own root may bind" gets wrong.
//
// A session is usually started in a subdirectory rather than at the checkout
// root, and `git config --get remote.origin.url` walks up, so the remote found
// there genuinely is the enclosing checkout's own. A v11-era row — a recorded
// path and no remote, which is what migrateV11 leaves every pre-v11 project as —
// is identified by that save and by no other, because the remote is only ever
// written from a save. Refuse it and the project is never identified at all.
//
// A second checkout of the same repository is deliberately not in this test: its
// path is not a prefix match for the first, so the prefix step never sees it, and
// the unique-name fallback refuses it instead — the trade #610 recorded, where a
// second checkout keeps its own project rather than inheriting the first one's
// memories.
func TestResolveOrCreateRepoProjectBindsRemoteFromSubdirectoryOfSameRepository(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	const own = "https://github.com/wcatz/infra.git"
	root := t.TempDir()
	parent := filepath.Join(root, "git", "infra")
	subdir := filepath.Join(parent, "src", "api")
	for _, dir := range []string{parent, subdir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("create %s: %v", dir, err)
		}
	}
	if err := s.EnsureProject(ctx, parent, parent, "infra"); err != nil {
		t.Fatalf("EnsureProject parent: %v", err)
	}
	// One remote for every directory, which is what git reports for a tree that
	// is a single repository however deep the save came from.
	SetDetectRemote(func(string) string { return own })
	t.Cleanup(func() { SetDetectRemote(nil) })

	canonical, err := s.ResolveOrCreateRepoProject(
		ctx, subdir, "infra", subdir, subdir, subdir, own,
	)
	if err != nil {
		t.Fatalf("ResolveOrCreateRepoProject: %v", err)
	}
	if canonical != parent {
		t.Errorf("save from %q resolved to %q, want the project whose checkout encloses it %q", subdir, canonical, parent)
	}
	var got string
	if err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(repo_remote, '') FROM projects WHERE id = ?`, parent).Scan(&got); err != nil {
		t.Fatalf("read bound repository: %v", err)
	}
	if want := "github.com/wcatz/infra"; got != want {
		t.Errorf("project records repository %q after a save from its own repository, want %q", got, want)
	}
}

// TestResolveOrCreateRepoProjectDetectsRemoteOnce pins the cost of deciding
// whether a save may identify a project, because the write side is otherwise
// the one place that never spawns git: the remote at the saving directory was
// detected by the caller that passed it in, so the only question left is the one
// at the project's recorded path.
//
// The third case is the one that matters in production, since it is every save
// after the first: once the project records its repository, the transaction
// returns before the guard is reached, so the answer is provably unused and must
// cost nothing. "Provably" is the part under test — the pre-lock read has the
// row, so it can see that the remote is already recorded and skip the spawn
// rather than computing an answer nobody will read.
func TestResolveOrCreateRepoProjectDetectsRemoteOnce(t *testing.T) {
	const own = "https://github.com/wcatz/infra.git"
	for _, c := range []struct {
		name string
		// bindFirst saves from the project root first, which is what puts the
		// project in the state every later save meets.
		bindFirst bool
		saving    func(parent string) string
		want      int
	}{
		{
			name:   "a save at the project root",
			saving: func(parent string) string { return parent },
			want:   0,
		},
		{
			name:   "a save from a subdirectory",
			saving: func(parent string) string { return filepath.Join(parent, "src") },
			want:   1,
		},
		{
			name:      "a save from a subdirectory of a project that records its remote",
			bindFirst: true,
			saving:    func(parent string) string { return filepath.Join(parent, "src") },
			want:      0,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := testStore(t)
			ctx := context.Background()

			root := t.TempDir()
			parent := filepath.Join(root, "git", "infra")
			saving := c.saving(parent)
			for _, dir := range []string{parent, saving} {
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatalf("create %s: %v", dir, err)
				}
			}
			if err := s.EnsureProject(ctx, parent, parent, "infra"); err != nil {
				t.Fatalf("EnsureProject parent: %v", err)
			}
			if c.bindFirst {
				if _, err := s.ResolveOrCreateRepoProject(
					ctx, parent, "infra", parent, parent, parent, own,
				); err != nil {
					t.Fatalf("bind the project from its root: %v", err)
				}
			}

			detections := 0
			SetDetectRemote(func(string) string { detections++; return own })
			t.Cleanup(func() { SetDetectRemote(nil) })

			if _, err := s.ResolveOrCreateRepoProject(
				ctx, saving, "infra", saving, saving, saving, own,
			); err != nil {
				t.Fatalf("ResolveOrCreateRepoProject: %v", err)
			}
			if detections != c.want {
				t.Errorf("a repository-aware save spawned %d detections, want %d", detections, c.want)
			}
		})
	}
}

// TestResolveOrCreateRepoProjectDetectsBeforeTakingTheStoreLock pins where that
// detection happens, because the cost is not only a process spawn: the resolve
// holds the store-wide mutex and the single write connection for its whole
// duration, so a git that hangs there stalls every other reader and writer on
// this Store to answer one save.
//
// Holding the mutex and watching for the detection is the only way to say which
// side of it the work falls on. If the detection moved back inside the lock, the
// channel below would never close and this fails rather than hangs, because the
// test releases the mutex on the timeout path.
func TestResolveOrCreateRepoProjectDetectsBeforeTakingTheStoreLock(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	const own = "https://github.com/wcatz/infra.git"
	root := t.TempDir()
	parent := filepath.Join(root, "git", "infra")
	subdir := filepath.Join(parent, "src", "api")
	for _, dir := range []string{parent, subdir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("create %s: %v", dir, err)
		}
	}
	if err := s.EnsureProject(ctx, parent, parent, "infra"); err != nil {
		t.Fatalf("EnsureProject parent: %v", err)
	}

	// Buffered, and a non-blocking send: a second detection is a regression this
	// test is here to catch, and closing an already-closed channel would panic
	// the whole package binary instead of failing this test.
	detected := make(chan struct{}, 1)
	SetDetectRemote(func(string) string {
		select {
		case detected <- struct{}{}:
		default:
		}
		return own
	})
	t.Cleanup(func() { SetDetectRemote(nil) })

	s.mu.Lock()
	done := make(chan error, 1)
	go func() {
		_, err := s.ResolveOrCreateRepoProject(ctx, subdir, "infra", subdir, subdir, subdir, own)
		done <- err
	}()

	select {
	case <-detected:
	case <-time.After(5 * time.Second):
		s.mu.Unlock()
		t.Fatal("the repository detection ran behind the store mutex")
	}
	s.mu.Unlock()

	if err := <-done; err != nil {
		t.Fatalf("ResolveOrCreateRepoProject: %v", err)
	}
}

// TestResolveProjectDerivesRemoteOnceAndOnlyWhereItIsRead keeps remote
// detection — a `git config` spawn per call in the shipped binary, capped at
// two seconds each — off the resolutions that do not read it.
//
// The path-prefix filter could decide from a candidate that asserts no
// repository of its own, and the same input was then derived a second time for
// the repository step below, so one unresolved path cost two spawns and the
// Stop hook paid that per turn in an unmatched directory.
func TestResolveProjectDerivesRemoteOnceAndOnlyWhereItIsRead(t *testing.T) {
	t.Run("a path-prefix hit detects nothing", func(t *testing.T) {
		s := testStore(t)
		ctx := context.Background()
		// Recorded path and session directory differ, so the resolution is
		// carried by the path-prefix step rather than the exact-id lookup.
		root := t.TempDir()
		inner := filepath.Join(root, "sub")
		if err := os.MkdirAll(inner, 0o755); err != nil {
			t.Fatalf("create %s: %v", inner, err)
		}
		if err := s.EnsureProject(ctx, "ghost-proj", root, "ghost"); err != nil {
			t.Fatalf("EnsureProject: %v", err)
		}

		detections := 0
		SetDetectRemote(func(string) string { detections++; return "" })
		t.Cleanup(func() { SetDetectRemote(nil) })

		if id, _, err := s.ResolveProject(ctx, inner); err != nil {
			t.Fatalf("ResolveProject: %v", err)
		} else if id != "ghost-proj" {
			t.Fatalf("ResolveProject(%q) = %q, want the enclosing project", inner, id)
		}
		if detections != 0 {
			t.Errorf("a path-prefix hit spawned %d remote detections, want 0", detections)
		}
	})

	t.Run("a recorded remote is still checked once", func(t *testing.T) {
		s := testStore(t)
		ctx := context.Background()
		root := t.TempDir()
		inner := filepath.Join(root, "sub")
		if err := os.MkdirAll(inner, 0o755); err != nil {
			t.Fatalf("create %s: %v", inner, err)
		}
		if err := s.EnsureProjectWithRepo(ctx, "ghost-proj", root, "ghost", "https://github.com/wcatz/ghost.git"); err != nil {
			t.Fatalf("EnsureProjectWithRepo: %v", err)
		}

		detections := 0
		SetDetectRemote(func(string) string {
			detections++
			return "https://github.com/evil/ghost.git"
		})
		t.Cleanup(func() { SetDetectRemote(nil) })

		// A session standing in a project that claims another repository is
		// refused, which is the rule the session's remote exists to enforce.
		if id, _, err := s.ResolveProject(ctx, inner); err != nil {
			t.Fatalf("ResolveProject: %v", err)
		} else if id != "" {
			t.Errorf("ResolveProject(%q) = %q, want a refusal", inner, id)
		}
		if detections > 1 {
			t.Errorf("a rejected path-prefix hit spawned %d remote detections, want at most 1", detections)
		}
	})

	t.Run("an unmatched path detects at most once", func(t *testing.T) {
		s := testStore(t)
		ctx := context.Background()
		missing := filepath.Join(t.TempDir(), "no-such-checkout")

		detections := 0
		SetDetectRemote(func(string) string { detections++; return "" })
		t.Cleanup(func() { SetDetectRemote(nil) })

		if id, _, err := s.ResolveProject(ctx, missing); err != nil {
			t.Fatalf("ResolveProject: %v", err)
		} else if id != "" {
			t.Fatalf("ResolveProject(%q) = %q, want a miss", missing, id)
		}
		if detections > 1 {
			t.Errorf("an unmatched path spawned %d remote detections, want at most 1", detections)
		}
	})
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
