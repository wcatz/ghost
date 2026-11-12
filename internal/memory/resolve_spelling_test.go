package memory

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func spellingFixture(t *testing.T) (real, viaLink string) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	real = filepath.Join(root, "real", "workspace-dir")
	if err := os.MkdirAll(filepath.Join(real, "sub"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Symlink(filepath.Join(root, "real"), filepath.Join(root, "link")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	return real, filepath.Join(root, "link", "workspace-dir")
}

func TestResolveProjectFindsRecordedPathUnderAnotherSpelling(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name     string
		recorded func(real, link string) string
		input    func(real, link string) string
	}{
		{"recorded resolved, caller via symlink", func(r, _ string) string { return r }, func(_, l string) string { return l }},
		{"recorded via symlink, caller resolved", func(_, l string) string { return l }, func(r, _ string) string { return r }},
		{"recorded via symlink, caller subdirectory resolved", func(_, l string) string { return l }, func(r, _ string) string { return filepath.Join(r, "sub") }},
		{"recorded resolved, caller subdirectory via symlink", func(r, _ string) string { return r }, func(_, l string) string { return filepath.Join(l, "sub") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := testStore(t)
			real, link := spellingFixture(t)
			if err := s.EnsureProject(ctx, "proj-id", tc.recorded(real, link), "not-the-basename"); err != nil {
				t.Fatalf("EnsureProject: %v", err)
			}
			id, _, err := s.ResolveProject(ctx, tc.input(real, link))
			if err != nil || id != "proj-id" {
				t.Fatalf("resolved (%q, %v), want proj-id", id, err)
			}
		})
	}
}

func TestResolveProjectDoesNotMatchDifferentDirectoriesAcrossSpellings(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	real, link := spellingFixture(t)
	other := filepath.Join(filepath.Dir(real), "other-directory")
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := s.EnsureProject(ctx, "proj-id", link, "not-the-basename"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	id, _, err := s.ResolveProject(ctx, other)
	if err != nil || id != "" {
		t.Fatalf("resolved (%q, %v), want empty", id, err)
	}
}

func TestResolveProjectDeepestWinsAcrossSpellings(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct{ name, realDir, linkDir string }{
		{"equal-length spellings", "real", "link"},
		{"child text shorter than parent text", "a-very-long-real-directory-name", "l"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := testStore(t)
			root, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatalf("EvalSymlinks: %v", err)
			}
			child := filepath.Join(root, tc.realDir, "parent-dir", "child-proj")
			if err := os.MkdirAll(child, 0o755); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
			if err := os.Symlink(filepath.Join(root, tc.realDir), filepath.Join(root, tc.linkDir)); err != nil {
				t.Skipf("symlink unavailable: %v", err)
			}
			if err := s.EnsureProject(ctx, "parent-id", filepath.Join(root, tc.realDir, "parent-dir"), "parent-name"); err != nil {
				t.Fatalf("EnsureProject parent: %v", err)
			}
			if err := s.EnsureProject(ctx, "child-id", filepath.Join(root, tc.linkDir, "parent-dir", "child-proj"), "child-name"); err != nil {
				t.Fatalf("EnsureProject child: %v", err)
			}
			for _, input := range []string{child, filepath.Join(root, tc.linkDir, "parent-dir", "child-proj")} {
				id, _, err := s.ResolveProject(ctx, input)
				if err != nil || id != "child-id" {
					t.Fatalf("ResolveProject(%q) = (%q, %v), want child-id", input, id, err)
				}
			}
		})
	}
}

func TestResolveProjectTiesTwoSpellingsOfOneDirectory(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	real := filepath.Join(root, "real", "workspace-dir")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Symlink(filepath.Join(root, "real"), filepath.Join(root, "a-much-longer-link-name")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	long := filepath.Join(root, "a-much-longer-link-name", "workspace-dir")
	if err := s.EnsureProject(ctx, "short-id", real, "short-name"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	if err := s.EnsureProject(ctx, "long-id", long, "long-name"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	for _, input := range []string{real, long} {
		if id, _, err := s.ResolveProject(ctx, input); !errors.Is(err, ErrAmbiguousProject) {
			t.Fatalf("ResolveProject(%q) = (%q, %v), want ErrAmbiguousProject", input, id, err)
		}
	}
}

func TestResolveProjectMemoIsRefreshedByAProjectWrite(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	dirA := filepath.Join(root, "dir-a-workspace")
	dirB := filepath.Join(root, "dir-b-workspace")
	other := filepath.Join(root, "unrelated-workspace")
	for _, d := range []string{dirA, dirB, other} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}
	link := filepath.Join(root, "moving-link")
	if err := os.Symlink(dirA, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if err := s.EnsureProject(ctx, "proj-id", link, "not-the-basename"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	if id, _, err := s.ResolveProject(ctx, dirA); err != nil || id != "proj-id" {
		t.Fatalf("before retarget: (%q, %v)", id, err)
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(dirB, link); err != nil {
		t.Fatal(err)
	}
	if err := s.EnsureProject(ctx, "other-id", other, "other-name"); err != nil {
		t.Fatalf("EnsureProject other: %v", err)
	}
	if _, err := s.DeleteProject(ctx, "other-id", true); err != nil {
		t.Fatalf("DeleteProject other: %v", err)
	}
	if id, _, err := s.ResolveProject(ctx, dirA); err != nil || id != "" {
		t.Fatalf("after retarget A: (%q, %v)", id, err)
	}
	if id, _, err := s.ResolveProject(ctx, dirB); err != nil || id != "proj-id" {
		t.Fatalf("after retarget B: (%q, %v)", id, err)
	}
}

func TestResolveProjectNeverRoutesOnAStaleMemoEntry(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	dirA := filepath.Join(root, "dir-a-workspace")
	dirB := filepath.Join(root, "dir-b-workspace")
	for _, d := range []string{dirA, dirB} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(root, "moving-link")
	if err := os.Symlink(dirA, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if err := s.EnsureProject(ctx, "proj-id", link, "not-the-basename"); err != nil {
		t.Fatal(err)
	}
	if id, _, _ := s.ResolveProject(ctx, dirA); id != "proj-id" {
		t.Fatalf("before retarget: %q", id)
	}
	_ = os.Remove(link)
	if err := os.Symlink(dirB, link); err != nil {
		t.Fatal(err)
	}
	if id, _, err := s.ResolveProject(ctx, dirA); err != nil || id != "" {
		t.Fatalf("stale memo routed: (%q, %v)", id, err)
	}
}

func TestResolveProjectMemoExpiresAndRetriesUnresolvable(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	now := time.Now()
	s.canon.now = func() time.Time { return now }
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	later := filepath.Join(root, "later-workspace")
	if err := s.EnsureProject(ctx, "proj-id", later, "not-the-basename"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	if _, ok := s.canon.lookup(later); ok {
		t.Fatal("unresolved path should not be cached")
	}
	if err := os.MkdirAll(later, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.canon.lookup(later); ok {
		t.Fatal("retried before canonMemoRetry")
	}
	now = now.Add(canonMemoRetry + time.Second)
	if id, _, err := s.ResolveProject(ctx, later); err != nil || id != "proj-id" {
		t.Fatalf("after retry: (%q, %v)", id, err)
	}
	if err := os.RemoveAll(later); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.canon.lookup(later); !ok {
		t.Fatal("resolved entry expired too early")
	}
	now = now.Add(canonMemoTTL + time.Second)
	if _, ok := s.canon.lookup(later); ok {
		t.Fatal("resolved entry outlived canonMemoTTL")
	}
}

func TestResolveProjectStaleMemoCanHideADeeperProjectUntilItExpires(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	now := time.Now()
	s.canon.now = func() time.Time { return now }
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	parent := filepath.Join(root, "parent-workspace")
	elsewhere := filepath.Join(root, "elsewhere-dir")
	child := filepath.Join(parent, "child-dir")
	for _, d := range []string{child, elsewhere} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(parent, "link")
	if err := os.Symlink(elsewhere, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if err := s.EnsureProject(ctx, "parent-id", parent, "parent-name"); err != nil {
		t.Fatal(err)
	}
	if err := s.EnsureProject(ctx, "child-id", link, "child-name"); err != nil {
		t.Fatal(err)
	}
	if id, _, _ := s.ResolveProject(ctx, child); id != "parent-id" {
		t.Fatalf("warm-up: %q", id)
	}
	_ = os.Remove(link)
	if err := os.Symlink(child, link); err != nil {
		t.Fatal(err)
	}
	if id, _, _ := s.ResolveProject(ctx, child); id != "parent-id" {
		t.Fatalf("within TTL: %q", id)
	}
	now = now.Add(canonMemoTTL + time.Second)
	if id, _, _ := s.ResolveProject(ctx, child); id != "child-id" {
		t.Fatalf("after TTL: %q", id)
	}
}

func TestResolveProjectNewDeeperProjectIsFoundWithWarmMemo(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	parent := filepath.Join(root, "parent-workspace")
	child := filepath.Join(parent, "child-dir")
	if err := os.MkdirAll(child, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := s.EnsureProject(ctx, "parent-id", parent, "parent-name"); err != nil {
		t.Fatal(err)
	}
	if id, _, _ := s.ResolveProject(ctx, child); id != "parent-id" {
		t.Fatalf("warm-up: %q", id)
	}
	if err := s.EnsureProject(ctx, "child-id", child, "child-name"); err != nil {
		t.Fatal(err)
	}
	if id, _, _ := s.ResolveProject(ctx, child); id != "child-id" {
		t.Fatalf("after adding child: %q", id)
	}
}
