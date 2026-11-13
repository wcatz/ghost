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
// built by repo.GitCommand, which drops every inherited git variable that can
// relocate the repository or replace its configuration (GIT_CEILING_DIRECTORIES
// among them) so a parent cannot rename the repository a caller's directory
// answers for. A test can no longer confine the product's walk-up through the
// child's inherited variables, so the premise is asked of git under the SAME
// conditions the product runs it in, through the same helper: if that reports a
// top level, the directory is in a repository and the test skips rather than
// asserting commits it would not get.
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

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}
