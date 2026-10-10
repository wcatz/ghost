package reflection

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/repo"
)

func TestDetectLanguage(t *testing.T) {
	cases := []struct {
		file string
		want string
	}{
		{"go.mod", "Go"},
		{"Cargo.toml", "Rust"},
		{"package.json", "JavaScript/TypeScript"},
		{"pyproject.toml", "Python"},
		{"Gemfile", "Ruby"},
		{"composer.json", "PHP"},
		// Glob-branch markers (*.csproj/*.sln) exercise the filepath.Glob path.
		{"foo.csproj", "C#"},
		{"App.sln", "C#"},
	}
	for _, tc := range cases {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, tc.file), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := detectLanguage(dir); got != tc.want {
			t.Errorf("detectLanguage with %s = %q, want %q", tc.file, got, tc.want)
		}
	}
	if got := detectLanguage(t.TempDir()); got != "" {
		t.Errorf("detectLanguage(empty dir) = %q, want empty", got)
	}
	if got := detectLanguage(""); got != "" {
		t.Errorf(`detectLanguage("") = %q, want empty`, got)
	}
}

func TestCollectGitContextNonRepo(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	excludeEnclosingRepo(t, dir)

	commits, language := CollectGitContext(dir)
	if language != "Go" {
		t.Errorf("language = %q, want Go", language)
	}
	if len(commits) != 0 {
		t.Errorf("commits = %v, want none for a non-repo directory", commits)
	}
	if commits, _ := CollectGitContext(""); commits != nil {
		t.Errorf("empty dir should yield nil commits, got %v", commits)
	}
}

// excludeEnclosingRepo establishes that dir is not inside a repository, so a
// test asserting no commits here is asserting something.
//
// This used to set GIT_CEILING_DIRECTORIES, git's own opt-out from the upward
// walk, and that worked while the ceiling reached CollectGitContext's child. It
// no longer does, and the change is the product's: every git child Ghost runs is
// built by repo.GitCommand, which drops the inherited git location variables
// (GIT_CEILING_DIRECTORIES among them) so a parent cannot rename the repository
// a caller's directory answers for. A test can no longer confine the product's
// walk-up through the child's inherited variables, so the premise is asked of
// git under the SAME conditions the product runs it in, through the same helper:
// if that reports a top level, the directory is in a repository and the test
// skips rather than asserting commits it would not get.
func excludeEnclosingRepo(t *testing.T, dir string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed, so whether this directory is in a repository is not a question this host can answer")
	}
	if out, err := repo.GitCommand(context.Background(), "-C", dir, "rev-parse", "--show-toplevel").CombinedOutput(); err == nil {
		t.Skipf("%s is inside the git repository at %s (TMPDIR under a checkout), so a test asserting "+
			"no commits here would assert nothing", dir, strings.TrimSpace(string(out)))
	}
}

// TestCollectGitContextCommits pins the producer that was missing entirely:
// LastCommits and ProjectLanguage used to be dead fields, so the prompt's
// Recent Git Activity section never rendered and the fabrication guard had no
// legitimate SHA source beyond ExistingMemories.
func TestCollectGitContextCommits(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	runGit(t, dir, "init", "-q")
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", ".")
	runGit(t, dir, "-c", "user.email=test@example.com", "-c", "user.name=Test",
		"commit", "-q", "-m", "add module file")

	commits, language := CollectGitContext(dir)
	if language != "Go" {
		t.Errorf("language = %q, want Go", language)
	}
	if len(commits) != 1 {
		t.Fatalf("commits = %v, want exactly 1 entry", commits)
	}
	if !strings.Contains(commits[0], "add module file") {
		t.Errorf("commit line %q is missing the subject", commits[0])
	}
	// The producer must emit a SHA-shaped field: shaLikeRe's word-anchored
	// 7-40 hex characters. It must NOT be required to satisfy shaLikeToken,
	// which additionally demands both a digit and a letter so that ordinary
	// numbers (764824073, 20260116) and words (defaced) are never mistaken for
	// SHAs and dropped. git abbreviations made of a single character class —
	// all digits (e.g. 6457253, ~3.7% of hashes) or all letters — are
	// therefore invisible to the guard by design; asserting otherwise makes
	// this test fail on those runs for a false negative, not a producer bug.
	sha := strings.Fields(commits[0])[0]
	if shaLikeRe.FindString(sha) != sha {
		t.Errorf("commit SHA %q is not a 7-40 character hex token", sha)
	}
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// TestCollectGitContextIgnoresInheritedGitLocationVariables: the commit log is
// grounding for the consolidation prompt, so it must describe the directory the
// caller named — never the repository a parent's git location variables name.
// A `ghost reflect` started from inside a git hook, or from a shell that
// exports one, would otherwise be shown another checkout's history.
func TestCollectGitContextIgnoresInheritedGitLocationVariables(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	target := filepath.Join(root, "target")
	other := filepath.Join(root, "other")
	for _, dir := range []string{target, other} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		runGit(t, dir, "init", "-q")
	}
	runGit(t, target, "-c", "user.email=test@example.com", "-c", "user.name=Test",
		"commit", "-q", "--allow-empty", "-m", "the target's own subject")
	runGit(t, other, "-c", "user.email=test@example.com", "-c", "user.name=Test",
		"commit", "-q", "--allow-empty", "-m", "the other repository's subject")

	t.Setenv("GIT_DIR", filepath.Join(other, ".git"))
	t.Setenv("GIT_WORK_TREE", other)
	t.Setenv("GIT_COMMON_DIR", filepath.Join(other, ".git"))
	t.Setenv("GIT_INDEX_FILE", filepath.Join(other, ".git", "index"))
	t.Setenv("GIT_CEILING_DIRECTORIES", root)
	t.Setenv("GIT_OBJECT_DIRECTORY", filepath.Join(other, ".git", "objects"))
	t.Setenv("GIT_ALTERNATE_OBJECT_DIRECTORIES", filepath.Join(other, ".git", "objects"))
	t.Setenv("GIT_NAMESPACE", "ns")
	t.Setenv("GIT_PREFIX", "sub/")

	commits, _ := CollectGitContext(target)
	if len(commits) != 1 {
		t.Fatalf("commits = %v, want the one commit the target holds", commits)
	}
	if !strings.Contains(commits[0], "the target's own subject") {
		t.Errorf("commits = %q, want the target's own subject", commits[0])
	}
	if strings.Contains(commits[0], "other repository") {
		t.Errorf("commits = %q, want nothing from the repository the inherited variables name", commits[0])
	}
}
