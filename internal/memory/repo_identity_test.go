package memory

import (
	"context"
	"errors"
	"os"
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

// TestResolveExplicitProjectRepoTxRefusesEvidenceAboutAnotherCheckout covers the
// arm of the nested-checkout guard that no ordinary save reaches: a project
// re-pointed between the pre-lock evidence read and the transaction that reads
// it again.
//
// The evidence says the project speaks for the repository detected at the
// checkout it used to record. If the row is then re-pointed at a different
// checkout that still contains the saving directory — `ghost project bind` on a
// subdirectory of it, say — the transaction still routes the save to that
// project, and binding the remote would put the old checkout's repository on a
// project that no longer records that checkout. The id is the same row either
// way, which is why comparing the project alone is not enough.
//
// Both halves are driven directly rather than raced for: the evidence through
// s.savingRepository, the re-point through the public bind, then the transaction
// with the stale answer in hand. A test that waited for the two to collide would
// pass without ever reaching the branch.
func TestResolveExplicitProjectRepoTxRefusesEvidenceAboutAnotherCheckout(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	const own = "https://github.com/wcatz/infra.git"
	// ResolveOrCreateRepoProject normalizes the caller's remote once, before the
	// evidence read and before the transaction, so both sides of every
	// comparison here see the normalized spelling rather than the detected URL.
	canon := NormalizeRepoRemote(own)
	// The root is canonicalised once, because BindProjectPath records the
	// physical directory and this test compares what it wrote. t.TempDir() is
	// spelled /var/folders/... on macOS, which is a symlink to
	// /private/var/folders/..., so the raw spelling would not match on every
	// host — and the failure would read as a bind that recorded the wrong
	// directory, which is the opposite of what this test is about.
	root, err := canonicalPath(t.TempDir())
	if err != nil {
		t.Fatalf("resolve temp root: %v", err)
	}
	checkout := filepath.Join(root, "git", "infra")
	saving := filepath.Join(checkout, "src", "api")
	repointed := filepath.Join(checkout, "src")
	for _, dir := range []string{checkout, saving, repointed} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("create %s: %v", dir, err)
		}
	}
	if err := s.EnsureProject(ctx, checkout, checkout, "infra"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	SetDetectRemote(func(string) string { return own })
	t.Cleanup(func() { SetDetectRemote(nil) })

	// What a save from the subdirectory collects while the project still
	// records the checkout root.
	evidence := s.savingRepository(ctx, saving, canon)
	if evidence.projectID != checkout || evidence.recordedPath != checkout {
		t.Fatalf("evidence = %+v, want it to describe %q at %q", evidence, checkout, checkout)
	}
	if !evidence.speaksForProject {
		t.Fatalf("evidence says the subdirectory does not speak for the project: %+v", evidence)
	}

	// The same row, re-pointed at a shorter ancestor of the saving directory, so
	// the transaction's prefix query still answers with this project.
	binding, err := s.BindProjectPath(ctx, checkout, repointed, "")
	if err != nil {
		t.Fatalf("BindProjectPath: %v", err)
	}
	if binding.Path != repointed {
		t.Fatalf("bind recorded path %q, want %q", binding.Path, repointed)
	}

	s.mu.Lock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		s.mu.Unlock()
		t.Fatalf("begin tx: %v", err)
	}
	id, found, resolveErr := s.resolveExplicitProjectRepoTx(ctx, tx, saving, canon, evidence)
	// Committed, not rolled back: the assertion is about whether a remote was
	// written, and a rollback would discard the write under test and pass the
	// test whatever the guard did.
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	s.mu.Unlock()
	if resolveErr != nil {
		t.Fatalf("resolveExplicitProjectRepoTx error = %v, want nil", resolveErr)
	}
	if !found || id != checkout {
		t.Errorf("resolveExplicitProjectRepoTx = (%q, %v), want (%q, true): the re-pointed project still encloses the save", id, found, checkout)
	}

	// The point: the save is routed, and the project is left unclaimed rather
	// than given a remote detected at the checkout it no longer records.
	var got string
	if err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(repo_remote, '') FROM projects WHERE id = ?`, checkout).Scan(&got); err != nil {
		t.Fatalf("read project repository: %v", err)
	}
	if got != "" {
		t.Errorf("evidence about %q bound %q to a project now recording %q", checkout, got, repointed)
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
	// The refusal is returned rather than discarded so the cases below can
	// assert it; the routing this test is about is unchanged by it.
	resolve := func(s *Store, ctx context.Context, projectRef string) (string, *BindingRefusal, error) {
		return s.ResolveOrCreateRepoProject(ctx, projectRef, "ghost", newCheckout, newCheckout, "checkout", remote)
	}

	t.Run("binds the unique unclaimed project", func(t *testing.T) {
		s := testStore(t)
		ctx := context.Background()
		if err := s.EnsureProject(ctx, "ghost", "", "ghost"); err != nil {
			t.Fatalf("EnsureProject: %v", err)
		}

		id, refused, err := resolve(s, ctx, "")
		if err != nil {
			t.Fatalf("ResolveOrCreateRepoProject: %v", err)
		}
		if refused != nil {
			t.Errorf("a project that was allowed to bind reported a refusal: %+v", refused)
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

		id, refused, err = resolve(s, ctx, "")
		if err != nil || id != "ghost" || refused != nil {
			t.Errorf("same binding is not idempotent: got (%q, %+v, %v), want (ghost, nil, nil)", id, refused, err)
		}
	})

	t.Run("resolves an existing remote independently of name", func(t *testing.T) {
		s := testStore(t)
		ctx := context.Background()
		if err := s.EnsureProjectWithRepo(ctx, "custom-id", "", "project-display-name", remote); err != nil {
			t.Fatalf("EnsureProjectWithRepo: %v", err)
		}
		id, refused, err := resolve(s, ctx, "")
		if refused != nil {
			t.Errorf("resolving by recorded remote reported a refusal: %+v", refused)
		}
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

		id, _, err := resolve(s, ctx, pathID)
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

		id, refused, err := resolve(s, ctx, pathID)
		if refused != nil {
			t.Errorf("merging into the remote owner reported a refusal: %+v", refused)
		}
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

	// The routing is unchanged since #613, and the refusal it now returns is
	// asserted on purpose: this subtest used to pin a save that opened a
	// project and said nothing, which is exactly the contract the issue called
	// a deliberate change rather than a bug. Both outcomes are correct; the
	// silent one is what had to go.
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

				id, refused, err := resolve(s, ctx, "")
				if refused == nil {
					t.Errorf("%s name opened the fallback project with nothing reported; the caller cannot tell it apart from a new project", tc.name)
				}
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

	id, _, err := s.ResolveOrCreateRepoProject(
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
	canonical, _, err := s.ResolveOrCreateRepoProject(
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

	canonical, _, err := s.ResolveOrCreateRepoProject(
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

	canonical, _, err := s.ResolveOrCreateRepoProject(
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

// TestATokenInARemoteQueryIsNotStoredOnImportOrProjectCreation covers both
// write paths the finding named. The store's own creation route normalizes a
// remote before storing it, so a token in a clone URL's query string never
// reaches the row; the importer refuses the token-shaped value instead.
func TestATokenInARemoteQueryIsNotStoredOnImportOrProjectCreation(t *testing.T) {
	ctx := context.Background()
	token := syntheticToken()
	remote := "https://github.com/wcatz/ghost?token=" + token
	projectRemote := func(t *testing.T, s *Store, id string) string {
		t.Helper()
		var got string
		if err := s.db.QueryRowContext(ctx,
			`SELECT COALESCE(repo_remote, '') FROM projects WHERE id = ?`, id).Scan(&got); err != nil {
			t.Fatalf("read repo_remote for %s: %v", id, err)
		}
		return got
	}

	// The creation path an ordinary save takes.
	created := portableTestStore(t)
	if err := created.EnsureProjectWithRepo(ctx, "p1", "/src/p1", "one", remote); err != nil {
		t.Fatalf("EnsureProjectWithRepo: %v", err)
	}
	if got := projectRemote(t, created, "p1"); strings.Contains(got, token) || strings.ContainsAny(got, "?#") {
		t.Errorf("the creation path stored a token-bearing remote: %q", got)
	}

	// The import path, whose artifact is the value that travels between machines.
	// It judges the remote as the artifact carries it, before normalising, so a
	// token-shaped remote is refused outright rather than stripped and stored.
	dst := portableTestStore(t)
	if _, err := dst.ImportProject(ctx, PortableProject{ID: "p2", Path: "/src/p2", Name: "two", RepoRemote: remote}, true); !errors.Is(err, ErrSecretContent) {
		t.Fatalf("ImportProject err = %v, want ErrSecretContent", err)
	}
	var n int
	if err := dst.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM projects WHERE id = 'p2'`).Scan(&n); err != nil || n != 0 {
		t.Errorf("a refused import left %d row(s), err %v", n, err)
	}
}

// syntheticToken is a GitHub-token-shaped value used only as a marker: the
// tests assert it does not reach a stored column, never that it is detected.
func syntheticToken() string {
	return "ghp_" + strings.Repeat("a1B2c3D4e5F6", 3) + "AbCd"
}

// TestNormalizeRepoRemoteDropsUserinfoOnEveryBranch: userinfo is not part of
// repository identity and is where a clone URL most often carries a token, so
// no spelling may keep it — the scheme-less ones included, which are the ones
// a human types and which used to fall through to the scp or bare branch with
// the userinfo intact. The rows after the blank comment must not change.
func TestNormalizeRepoRemoteDropsUserinfoOnEveryBranch(t *testing.T) {
	tok := syntheticToken()
	for _, tc := range []struct{ in, want string }{
		// Scheme-less userinfo: the finding's two spellings and their variants.
		{"git@github.com/owner/repo", "github.com/owner/repo"},
		{"git@github.com/owner/repo.git", "github.com/owner/repo"},
		{"x-token-auth:" + tok + "@bitbucket.org/w/r.git", "bitbucket.org/w/r"},
		{"user:" + tok + "@github.com/owner/repo", "github.com/owner/repo"},
		{"user:p@ss@github.com/owner/repo", "github.com/owner/repo"},
		{tok + "@github.com/owner/repo", "github.com/owner/repo"},
		// With a scheme, a password containing "@" is still all userinfo.
		{"https://user:p@ss@github.com/owner/repo", "github.com/owner/repo"},
		// Ordinary spellings, unchanged.
		{"https://github.com/owner/repo", "github.com/owner/repo"},
		{"https://github.com/owner/repo.git", "github.com/owner/repo"},
		{"https://user:" + tok + "@github.com/owner/repo.git", "github.com/owner/repo"},
		{"ssh://git@github.com/owner/repo.git", "github.com/owner/repo"},
		{"ssh://git@github.com:22/owner/repo.git", "github.com/owner/repo"},
		{"git@github.com:owner/repo.git", "github.com/owner/repo"},
		{"GitHub.com/Owner/Repo", "github.com/Owner/Repo"},
		{"github.com/owner/repo", "github.com/owner/repo"},
		// An "@" past the host belongs to the path and is kept.
		{"https://github.com/owner/repo@v1", "github.com/owner/repo@v1"},
	} {
		if got := NormalizeRepoRemote(tc.in); got != tc.want {
			t.Errorf("NormalizeRepoRemote(%q) = %q, want %q", tc.in, got, tc.want)
		}
		if got := NormalizeRepoRemote(tc.in); strings.Contains(got, tok) {
			t.Errorf("NormalizeRepoRemote(%q) kept the token: %q", tc.in, got)
		}
	}
}
