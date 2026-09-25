package memory

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestResolveProjectRejectsAmbiguousName(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	if err := s.EnsureProject(ctx, "first", "", "shared"); err != nil {
		t.Fatalf("EnsureProject first: %v", err)
	}
	if err := s.EnsureProject(ctx, "second", "", "shared"); err != nil {
		t.Fatalf("EnsureProject second: %v", err)
	}

	_, _, err := s.ResolveProject(ctx, "shared")
	if !errors.Is(err, ErrAmbiguousProject) {
		t.Fatalf("ResolveProject(shared) error = %v, want ErrAmbiguousProject", err)
	}
}

func TestResolveProjectReadOnlyPreV11Database(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open legacy database: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE projects (
		id TEXT PRIMARY KEY,
		path TEXT NOT NULL UNIQUE,
		name TEXT NOT NULL
	)`); err != nil {
		_ = db.Close()
		t.Fatalf("create legacy projects table: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO projects (id, path, name) VALUES ('legacy-id', '/tmp/legacy-project', 'legacy')`); err != nil {
		_ = db.Close()
		t.Fatalf("insert legacy project: %v", err)
	}
	if _, err := db.Exec(`PRAGMA user_version = 10`); err != nil {
		_ = db.Close()
		t.Fatalf("stamp legacy schema: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close legacy database: %v", err)
	}

	ro, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatalf("open legacy database read-only: %v", err)
	}
	t.Cleanup(func() { _ = ro.Close() })

	id, name, err := NewStore(ro, nil).ResolveProject(context.Background(), "legacy")
	if err != nil {
		t.Fatalf("resolve legacy project: %v", err)
	}
	if id != "legacy-id" || name != "legacy" {
		t.Fatalf("ResolveProject(legacy) = (%q, %q), want (legacy-id, legacy)", id, name)
	}
}

func TestResolveProjectRejectsRelativeStoredPathFromPathPrefix(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO projects (id, path, name) VALUES ('relative-id', 'relative/long/infra', 'infra')
	`); err != nil {
		t.Fatalf("insert relative project: %v", err)
	}

	id, name, err := s.ResolveProject(ctx, "relative/long/infra/sub")
	if err != nil {
		t.Fatalf("ResolveProject: %v", err)
	}
	if id != "" || name != "" {
		t.Fatalf("relative stored path claimed %q (%q), want no match", id, name)
	}
}

func TestResolveProjectRejectsDotDotPathPrefix(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	root := t.TempDir()
	stored := filepath.Join(root, "infra")
	outside := filepath.Join(root, "other", "infra")
	for _, dir := range []string{stored, outside} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	if err := s.EnsureProject(ctx, "infra-id", stored, "infra"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}

	input := stored + string(filepath.Separator) + ".." + string(filepath.Separator) + "other" + string(filepath.Separator) + "infra"
	id, name, err := s.ResolveProject(ctx, input)
	if err != nil {
		t.Fatalf("ResolveProject: %v", err)
	}
	if id != "" || name != "" {
		t.Fatalf("non-canonical path claimed %q (%q), want no match", id, name)
	}
}

func TestResolveProjectRejectsSymlinkEscapeFromPathPrefix(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	root := t.TempDir()
	stored := filepath.Join(root, "infra")
	outside := filepath.Join(root, "outside")
	link := filepath.Join(stored, "link")
	if err := os.MkdirAll(filepath.Join(outside, "sub"), 0o755); err != nil {
		t.Fatalf("mkdir outside child: %v", err)
	}
	if err := os.MkdirAll(stored, 0o755); err != nil {
		t.Fatalf("mkdir stored: %v", err)
	}
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if err := s.EnsureProject(ctx, "infra-id", stored, "infra"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}

	input := filepath.Join(link, "sub")
	id, name, err := s.ResolveProject(ctx, input)
	if err != nil {
		t.Fatalf("ResolveProject: %v", err)
	}
	if id != "" || name != "" {
		t.Fatalf("symlink escape claimed %q (%q), want no match", id, name)
	}
}

func TestNormalizeRepoRemoteRejectsFilesystemPaths(t *testing.T) {
	for _, input := range []string{"../../tmp/checkout", "./checkout", "../checkout", `C:checkout`} {
		if got := NormalizeRepoRemote(input); got != "" {
			t.Errorf("NormalizeRepoRemote(%q) = %q, want no remote for a filesystem path", input, got)
		}
	}
}

func TestResolveProjectDetectsRemoteForRelativePath(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	const remote = "github.com/me/infra"
	projectPath := filepath.Join(t.TempDir(), "infra")
	if err := os.MkdirAll(projectPath, 0o755); err != nil {
		t.Fatalf("mkdir project path: %v", err)
	}
	if err := s.EnsureProjectWithRepo(ctx, "infra-id", projectPath, "infra", remote); err != nil {
		t.Fatalf("EnsureProjectWithRepo: %v", err)
	}
	SetDetectRemote(func(string) string { return remote })
	t.Cleanup(func() { SetDetectRemote(nil) })

	id, name, err := s.ResolveProject(ctx, "../../tmp/infra")
	if err != nil {
		t.Fatalf("ResolveProject: %v", err)
	}
	if id != "infra-id" || name != "infra" {
		t.Fatalf("relative path resolved to %q (%q), want infra-id/infra", id, name)
	}
}
