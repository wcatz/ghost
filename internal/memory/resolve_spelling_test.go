package memory

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// spellingFixture builds <root>/real/proj and a symlink <root>/link -> real,
// with root itself resolved so the only difference between the two spellings
// is the symlink. The project name is deliberately not the basename of the
// directory, so only the path steps can find it.
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
			if err != nil {
				t.Fatalf("ResolveProject: %v", err)
			}
			if id != "proj-id" {
				t.Fatalf("resolved %q, want proj-id: the recorded path names the same directory", id)
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
	if err != nil {
		t.Fatalf("ResolveProject: %v", err)
	}
	if id != "" {
		t.Fatalf("a different directory resolved %q", id)
	}
}

// The caller-side spelling is handled by the narrowing step itself, so the
// common case (recorded resolved, caller through a symlink) costs no scan of
// the projects table.
func TestPathCandidatesBindsTheResolvedSpellingOfTheCaller(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	real, link := spellingFixture(t)
	if err := s.EnsureProject(ctx, "proj-id", real, "not-the-basename"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	got, err := s.pathCandidates(ctx, link, false)
	if err != nil {
		t.Fatalf("pathCandidates: %v", err)
	}
	if len(got) != 1 || got[0].id != "proj-id" {
		t.Fatalf("pathCandidates(%q) = %v, want the project recorded at the resolved spelling", link, got)
	}
}
