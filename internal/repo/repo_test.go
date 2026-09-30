package repo

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gitIn creates a repository at dir with the given origin remote, or skips if
// git is unavailable — detection is worthless to test without it, and the
// suite must not depend on a particular host being installed.
func gitIn(t *testing.T, dir, remote string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q")
	if remote != "" {
		run("remote", "add", "origin", remote)
	}
}

// excludeEnclosingRepo stops git from walking out of dir towards a repository
// above it, so "this directory is not a repository" is established here rather
// than assumed of the host. t.TempDir() sits under TMPDIR, so a TMPDIR pointed
// inside a checkout gives every temp dir a repository ancestor.
//
// The ceiling is dir's PARENT, not dir: git searches everything below a ceiling
// entry and reports the entry itself as no repository, but it does not stop the
// walk when the ceiling is the starting directory, so pinning dir would leave
// the ancestors reachable and the failure would read as a product bug. Hence the
// premise is then checked against git rather than trusted.
func excludeEnclosingRepo(t *testing.T, dir string) {
	t.Helper()
	t.Setenv("GIT_CEILING_DIRECTORIES", filepath.Dir(dir))
	if out, err := exec.Command("git", "-C", dir, "rev-parse", "--show-toplevel").CombinedOutput(); err == nil {
		t.Fatalf("premise broken: %s is inside the git repository at %s, so a test asserting "+
			"no remote here is asserting nothing", dir, strings.TrimSpace(string(out)))
	}
}

// TestDetectRemoteReturnsOrigin: the only thing detection exists to answer.
func TestDetectRemoteReturnsOrigin(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "repo")
	gitIn(t, dir, "git@github.com:wcatz/ghost.git")

	if got := DetectRemote(dir); got != "git@github.com:wcatz/ghost.git" {
		t.Errorf("DetectRemote = %q, want the configured origin remote", got)
	}

	// A path *inside* the checkout still reports that repository's remote:
	// a session is usually started from a subdirectory, not the root.
	sub := filepath.Join(dir, "internal", "memory")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := DetectRemote(sub); got != "git@github.com:wcatz/ghost.git" {
		t.Errorf("DetectRemote from a subdirectory = %q, want the repository's remote", got)
	}
}

// TestDetectRemoteReturnsEmpty covers every situation that must degrade to
// "no repository known" rather than to an error: none of them may stall a save.
func TestDetectRemoteReturnsEmpty(t *testing.T) {
	if got := DetectRemote(""); got != "" {
		t.Errorf("DetectRemote(\"\") = %q, want empty", got)
	}
	if got := DetectRemote(filepath.Join(t.TempDir(), "does-not-exist")); got != "" {
		t.Errorf("DetectRemote(missing dir) = %q, want empty", got)
	}
	// The non-repository case is the one that needs its premise stated rather
	// than assumed: DetectRemote's `git -C` walks up to the enclosing
	// repository on purpose (TestDetectRemoteReturnsOrigin pins that), so with
	// TMPDIR pointed inside a checkout — the recommended way to keep test
	// scratch off a small /tmp — this temp dir has a repository above it and
	// the assertion below would be asserting nothing.
	nonRepo := t.TempDir()
	excludeEnclosingRepo(t, nonRepo)
	if got := DetectRemote(nonRepo); got != "" {
		t.Errorf("DetectRemote(non-repository) = %q, want empty", got)
	}

	// A repository with no origin configured.
	noOrigin := filepath.Join(t.TempDir(), "no-origin")
	gitIn(t, noOrigin, "")
	if got := DetectRemote(noOrigin); got != "" {
		t.Errorf("DetectRemote(no origin) = %q, want empty", got)
	}
}
