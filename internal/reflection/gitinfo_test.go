package reflection

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
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

// excludeEnclosingRepo stops git from walking out of dir towards a repository
// above it, so "this directory is not in a repository" is established here
// rather than assumed of the host.
//
// t.TempDir() lands under TMPDIR, so with TMPDIR pointed at a scratch
// directory inside a checkout — the recommended way to keep test scratch out
// of /tmp, which is exactly what makes these temp dirs sit under a repository
// — every such dir has one as an ancestor and `git -C dir log` walks up into
// it. The walk-up is the product's, and it is intended: a project's recorded
// path is often a subdirectory of a checkout (repo.DetectRemote walks up for
// the same reason), so the PREMISE is what has to be pinned here, not git.
//
// GIT_CEILING_DIRECTORIES is git's own opt-out from the upward walk, and the
// entry has to be dir's PARENT rather than dir: git searches everything below a
// ceiling entry and reports the entry itself as no repository, but it does not
// stop the walk when the ceiling is the starting directory — so pinning dir
// would leave the ancestors reachable and the failure would read as a product
// bug. That is why the premise is then checked against git rather than trusted:
// if a future git changes this, this fails as a broken premise. And when git is
// absent the check is skipped rather than passed — `exec` reports a missing
// binary as an error, so without this the helper would return having established
// nothing and the assertion below would be asserting nothing too.
func excludeEnclosingRepo(t *testing.T, dir string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed, so whether this directory is in a repository is not a question this host can answer")
	}
	t.Setenv("GIT_CEILING_DIRECTORIES", filepath.Dir(dir))
	if out, err := exec.Command("git", "-C", dir, "rev-parse", "--show-toplevel").CombinedOutput(); err == nil {
		t.Fatalf("premise broken: %s is inside the git repository at %s, so a test asserting "+
			"no commits here is asserting nothing", dir, strings.TrimSpace(string(out)))
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
