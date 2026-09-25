package memory

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// TestGlobalNeverRecordsARepository is the blocker: a _global row that carries
// a repo_remote turns a checkout lookup into a write into the bucket that is
// injected into every project. Three doors have to be shut, and each is closed
// by a different caller, so all three are asserted here.
func TestGlobalNeverRecordsARepository(t *testing.T) {
	const remote = "github.com/me/infra"
	ctx := context.Background()

	// 1. The write. EnsureProjectWithRepo must refuse _global outright rather
	//    than recording a remote for it.
	s := testStore(t)
	if err := s.EnsureProject(ctx, "_global", "_global", "global"); err != nil {
		t.Fatalf("ensure _global: %v", err)
	}
	if err := s.EnsureProjectWithRepo(ctx, "_global", "_global", "global", remote); err == nil {
		t.Error("EnsureProjectWithRepo recorded a repository on _global, want a refusal")
	} else if !strings.Contains(err.Error(), "_global") {
		t.Errorf("error = %v, want it to name _global", err)
	}
	assertNoRemoteOnGlobal(t, s, remote)

	// 2. The read. A checkout whose detected remote matches a _global row must
	//    never resolve to it: the identity would be handed to the save path.
	//    The _global remote is seeded from a build that allowed it, and it has
	//    to be seeded BEFORE the real project claims the same identity, because
	//    the unique index would otherwise refuse the second writer.
	const orphanRemote = "github.com/other/leftover"
	if _, err := s.db.Exec(`UPDATE projects SET repo_remote = ? WHERE id = '_global'`, orphanRemote); err != nil {
		t.Fatalf("seed a pre-existing _global remote: %v", err)
	}
	// A regular project legitimately owns an identity and must still resolve
	// through it — the fix excludes _global, not the remote lookup.
	if err := s.EnsureProjectWithRepo(ctx, "real", "/tmp/real", "real", remote); err != nil {
		t.Fatalf("EnsureProjectWithRepo real: %v", err)
	}
	if id, _, err := s.ResolveProject(ctx, remote); err != nil || id != "real" {
		t.Errorf("ResolveProject(remote) = (%q, %v), want real — excluding _global must not break the remote lookup", id, err)
	}
	// The leftover identity on _global resolves to nothing at all.
	if id, _, err := s.ResolveProject(ctx, orphanRemote); err == nil && id != "" {
		t.Errorf("ResolveProject = (%q, nil), want no match — _global must never answer a repository lookup", id)
	}

}

func assertNoRemoteOnGlobal(t *testing.T, s *Store, remote string) {
	t.Helper()
	var got string
	if err := s.db.QueryRow(`SELECT COALESCE(repo_remote, '') FROM projects WHERE id = '_global'`).Scan(&got); err != nil {
		t.Fatalf("read _global repository: %v", err)
	}
	if got != "" {
		t.Errorf("_global records repository %q, want none", got)
	}
	if got == remote {
		t.Errorf("_global still records the seeded repository %q", remote)
	}
}

// TestMigrateV15ClearsGlobalRepository covers the migration side directly: a
// v14 database whose _global row still carries a remote is a valid state
// written by an older build, and upgrading must strip it rather than preserve an
// identity that can never be honoured.
func TestMigrateV15ClearsGlobalRepository(t *testing.T) {
	path := filepath.Join(t.TempDir(), "global-remote.sqlite")
	db, err := OpenDB(path)
	if err != nil {
		t.Fatalf("OpenDB seed: %v", err)
	}
	const remote = "github.com/me/infra"
	if _, err := db.Exec(`INSERT INTO projects (id, path, name, repo_remote) VALUES ('_global', '_global', 'global', ?)`, remote); err != nil {
		t.Fatalf("insert _global: %v", err)
	}
	// Re-stamp so the v15 step actually runs; the seed was written by a build
	// that had already reached this version.
	if _, err := db.Exec(`PRAGMA user_version = 14`); err != nil {
		t.Fatalf("stamp v14: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close seed: %v", err)
	}

	migrated, err := OpenDB(path)
	if err != nil {
		t.Fatalf("OpenDB migrated: %v", err)
	}
	defer migrated.Close() //nolint:errcheck

	assertNoRemoteOnGlobal(t, NewStore(migrated, nil), remote)
}
