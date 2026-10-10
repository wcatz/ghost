package repo

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// gitIn creates a repository at dir with the given origin remote, or skips if
// git is unavailable — detection is worthless to test without it, and the
// suite must not depend on a particular host being installed.
//
// The fixture is built through withoutGitOverrides for the same reason the
// product builds its children through it: a `go test` run from inside a git
// hook, or from a shell that exports one, would otherwise hand git init a
// GIT_DIR — or a GIT_CONFIG_PARAMETERS naming another remote — of its own and
// the fixture would describe a different repository than the one this test is
// about.
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
		cmd.Env = append(withoutGitOverrides(os.Environ()),
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

// excludeEnclosingRepo establishes that dir is not inside a repository, so a
// test asserting no top level and no remote here is asserting something rather
// than nothing.
//
// This used to set GIT_CEILING_DIRECTORIES, which is git's own opt-out from the
// upward walk, and that worked because the variable reached the detector's
// child. It no longer does, and the change is the product's: GitCommand drops
// every git location variable from its child, so an inherited
// GIT_CEILING_DIRECTORIES can no longer pin the place discovery stops — which is
// the whole point of it being in that list. A test cannot confine the product's
// walk-up through the child's inherited variables any more, so the premise is
// asked of git under the SAME conditions the product runs it in, through the
// same helper: if that reports a top level, the directory is in a repository and
// the assertion below would be asserting an answer the directory does not have,
// so the test skips rather than passing vacuously.
func excludeEnclosingRepo(t *testing.T, dir string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed, so whether this directory is in a repository is not a question this host can answer")
	}
	if out, err := GitCommand(context.Background(), "-C", dir, "rev-parse", "--show-toplevel").CombinedOutput(); err == nil {
		t.Skipf("%s is inside the git repository at %s (TMPDIR under a checkout), so a test asserting "+
			"no repository here would assert nothing", dir, strings.TrimSpace(string(out)))
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

// TestTopLevelNonRepoAndEmpty: the empty and missing cases answer "" without
// asking anything, and a directory discovery cannot place answers "".
func TestTopLevelNonRepoAndEmpty(t *testing.T) {
	if got := TopLevel(""); got != "" {
		t.Errorf("TopLevel(\"\") = %q, want empty", got)
	}
	if got := TopLevel(filepath.Join(t.TempDir(), "missing")); got != "" {
		t.Errorf("TopLevel(missing) = %q, want empty", got)
	}
	nonRepo := t.TempDir()
	excludeEnclosingRepo(t, nonRepo)
	if got := TopLevel(nonRepo); got != "" {
		t.Errorf("TopLevel(non-repository) = %q, want empty", got)
	}

	// The same directory with a parent that carries GIT_DIR pointing at a real
	// repository still answers "": an inherited location variable must not
	// ANSWER for a directory that discovery places in nothing, any more than it
	// may re-answer for one it places in a repository.
	other := filepath.Join(t.TempDir(), "other")
	gitIn(t, other, "git@example.com:other/repo.git")
	t.Setenv("GIT_DIR", filepath.Join(other, ".git"))
	if got := TopLevel(nonRepo); got != "" {
		t.Errorf("TopLevel(non-repository, GIT_DIR set) = %q, want empty", got)
	}
	if got := DetectRemote(nonRepo); got != "" {
		t.Errorf("DetectRemote(non-repository, GIT_DIR set) = %q, want empty", got)
	}
}

// TestTopLevelSubdirectory: discovery walks up, so a project's recorded path
// being a subdirectory of its checkout — the ordinary case, since a session is
// started inside the tree and not at its root — reports the checkout's top
// level rather than the directory git was asked about.
func TestTopLevelSubdirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "repo")
	gitIn(t, dir, "")
	sub := filepath.Join(dir, "a", "b")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if got, want := TopLevel(sub), evalDir(t, dir); got != want {
		t.Errorf("TopLevel(subdir) = %q, want %q", got, want)
	}
}

// TestTopLevelBareRepository: a bare repository has no working tree, so there is
// no top level to report and the answer is "no repository known" for the same
// reason a non-repository is.
func TestTopLevelBareRepository(t *testing.T) {
	requireGit(t)
	dir := filepath.Join(t.TempDir(), "bare.git")
	if out, err := exec.Command("git", "init", "-q", "--bare", dir).CombinedOutput(); err != nil {
		t.Fatalf("git init --bare: %v\n%s", err, out)
	}
	if got := TopLevel(dir); got != "" {
		t.Errorf("TopLevel(bare) = %q, want empty", got)
	}
}

// TestTopLevelGitMissing: git absent from PATH is an ordinary situation for a
// project path, so both detectors answer "" rather than failing the save that
// asked.
func TestTopLevelGitMissing(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "repo")
	gitIn(t, dir, "git@example.com:x/y.git")
	t.Setenv("PATH", t.TempDir())
	if _, err := exec.LookPath("git"); err == nil {
		t.Fatal("premise broken: git is still resolvable, so the assertions below are not about a missing binary")
	}
	if got := TopLevel(dir); got != "" {
		t.Errorf("TopLevel without git = %q, want empty", got)
	}
	if got := DetectRemote(dir); got != "" {
		t.Errorf("DetectRemote without git = %q, want empty", got)
	}
}

// TestInheritedGitLocationVariablesAreIgnored: a parent that exports git
// location variables pointing at a different repository must not change the
// answer for the directory the caller names. This is the state a server started
// from inside a git hook, or from a shell that exports one, runs in — and
// without the scrub TopLevel answers for `other` and DetectRemote reads other's
// origin, so the project a session opens is bound to a checkout nobody is
// working in.
func TestInheritedGitLocationVariablesAreIgnored(t *testing.T) {
	requireGit(t)
	root := t.TempDir()
	target := filepath.Join(root, "target")
	other := filepath.Join(root, "other")
	gitIn(t, target, "git@example.com:target/repo.git")
	gitIn(t, other, "git@example.com:other/repo.git")

	t.Setenv("GIT_DIR", filepath.Join(other, ".git"))
	t.Setenv("GIT_WORK_TREE", other)
	t.Setenv("GIT_COMMON_DIR", filepath.Join(other, ".git"))
	t.Setenv("GIT_INDEX_FILE", filepath.Join(other, ".git", "index"))
	t.Setenv("GIT_CEILING_DIRECTORIES", root)
	t.Setenv("GIT_OBJECT_DIRECTORY", filepath.Join(other, ".git", "objects"))
	t.Setenv("GIT_ALTERNATE_OBJECT_DIRECTORIES", filepath.Join(other, ".git", "objects"))
	t.Setenv("GIT_NAMESPACE", "ns")
	t.Setenv("GIT_PREFIX", "sub/")

	if got, want := TopLevel(target), evalDir(t, target); got != want {
		t.Errorf("TopLevel = %q, want %q", got, want)
	}
	if got := DetectRemote(target); got != "git@example.com:target/repo.git" {
		t.Errorf("DetectRemote = %q, want the target's remote", got)
	}
}

// TestInheritedGitConfigOverridesAreIgnored: git hands its own configuration
// layer to every child it spawns, and three separate variable families can supply
// a value for the one key DetectRemote reads — remote.origin.url.
// GIT_CONFIG_PARAMETERS is what `git -c key=value` exports, and a hook is a child
// of exactly that command; GIT_CONFIG_COUNT with GIT_CONFIG_KEY_n /
// GIT_CONFIG_VALUE_n is the same job in an indexed spelling; GIT_CONFIG,
// GIT_CONFIG_GLOBAL and GIT_CONFIG_SYSTEM replace the config files that are read.
// The command-line layer outranks the checkout's own .git/config, and the file
// variables supply a key the local config does not carry, so a ghost process
// started from inside a hook — or under a wrapper that sets any of them — derives
// the project from a remote that is not the checkout's own.
func TestInheritedGitConfigOverridesAreIgnored(t *testing.T) {
	requireGit(t)
	root := t.TempDir()
	target := filepath.Join(root, "target")
	gitIn(t, target, "git@example.com:target/repo.git")
	top := evalDir(t, target)
	targetRemote := "git@example.com:target/repo.git"
	otherRemote := "git@example.com:other/repo.git"

	// The file variables need something real to point at: the answer they supply
	// has to come from a file git can actually read.
	evil := filepath.Join(t.TempDir(), "inherited.config")
	evilBody := []byte("[remote \"origin\"]\n\turl = " + otherRemote + "\n")
	if err := os.WriteFile(evil, evilBody, 0o600); err != nil {
		t.Fatal(err)
	}

	// answersBoth: the directory named with -C is the only thing that decides
	// which repository answers, so both detectors must still describe it.
	answersBoth := func(t *testing.T) {
		t.Helper()
		if got := DetectRemote(target); got != targetRemote {
			t.Errorf("DetectRemote = %q, want the checkout's own remote", got)
		}
		if got := TopLevel(target); got != top {
			t.Errorf("TopLevel = %q, want %q", got, top)
		}
	}

	// setConfigOverrides puts every inherited config-override variable into the
	// environment, each of them answering remote.origin.url from `other` rather
	// than from the checkout. COUNT counts the pairs that follow it, and git
	// reads every pair it was given, so a partial set would make the child fail
	// rather than answer — the aggregate case is the one a hook's environment
	// actually presents.
	setConfigOverrides := func(t *testing.T) {
		t.Helper()
		t.Setenv("GIT_CONFIG_PARAMETERS", "'remote.origin.url'='"+otherRemote+"'")
		t.Setenv("GIT_CONFIG_COUNT", "2")
		t.Setenv("GIT_CONFIG_KEY_0", "remote.origin.url")
		t.Setenv("GIT_CONFIG_VALUE_0", otherRemote)
		t.Setenv("GIT_CONFIG_KEY_1", "remote.origin.url")
		t.Setenv("GIT_CONFIG_VALUE_1", otherRemote)
		t.Setenv("GIT_CONFIG", evil)
		t.Setenv("GIT_CONFIG_GLOBAL", evil)
		t.Setenv("GIT_CONFIG_SYSTEM", evil)
	}

	// A command-line layer on its own: the spelling git itself exports.
	t.Run("parameters", func(t *testing.T) {
		t.Setenv("GIT_CONFIG_PARAMETERS", "'remote.origin.url'='"+otherRemote+"'")
		answersBoth(t)
	})

	// The indexed twin, which does the same job without the quoting.
	t.Run("count key and value", func(t *testing.T) {
		t.Setenv("GIT_CONFIG_COUNT", "1")
		t.Setenv("GIT_CONFIG_KEY_0", "remote.origin.url")
		t.Setenv("GIT_CONFIG_VALUE_0", otherRemote)
		answersBoth(t)
	})

	// GIT_CONFIG replaces the file read outright, so it outranks even a local
	// remote.origin.url.
	t.Run("config file", func(t *testing.T) {
		t.Setenv("GIT_CONFIG", evil)
		answersBoth(t)
	})

	// GIT_CONFIG_GLOBAL and GIT_CONFIG_SYSTEM cannot outrank a local value — a
	// repository that carries its own origin wins over a global one — so the case
	// that matters is a checkout whose own config does not carry one, which is
	// also the ordinary case for a project bound before its origin was recorded.
	noOrigin := filepath.Join(root, "no-origin")
	gitIn(t, noOrigin, "")
	t.Run("global and system supply a remote the checkout does not carry", func(t *testing.T) {
		t.Setenv("GIT_CONFIG_GLOBAL", evil)
		t.Setenv("GIT_CONFIG_SYSTEM", evil)
		if got := DetectRemote(noOrigin); got != "" {
			t.Errorf("DetectRemote = %q, want empty: an inherited config file may not answer about a repository that carries no origin", got)
		}
	})

	// A hook's environment carries several of them at once, and the pairs are
	// consumed as a set: the aggregate is its own case, because dropping one
	// family while leaving the rest would still let the remaining one name
	// another repository's remote.
	t.Run("every family at once", func(t *testing.T) {
		setConfigOverrides(t)
		answersBoth(t)
	})
}

// TestWithoutGitOverridesKeepsOtherVariables: the scrub is narrow, and a scrub
// that took the wrong variable would break the child rather than answer a
// different question. A git variable that is not about location or config (a
// commit's author, a template directory), and one that is not about a repository
// at all (the ssh command, SSL verification), have to survive beside the ordinary
// ones.
func TestWithoutGitOverridesKeepsOtherVariables(t *testing.T) {
	got := withoutGitOverrides([]string{
		"PATH=/bin", "GIT_DIR=/x", "GIT_AUTHOR_NAME=t",
		"GIT_PREFIX=p", "HOME=/h", "GIT_TEMPLATE_DIR=/t", "GIT_NAMESPACE=n",
		"GIT_CONFIG_PARAMETERS=x", "GIT_CONFIG_COUNT=2", "GIT_CONFIG=/c",
		"GIT_CONFIG_GLOBAL=/g", "GIT_CONFIG_SYSTEM=/s",
		"GIT_CONFIG_KEY_0=k", "GIT_CONFIG_VALUE_0=v", "GIT_CONFIG_KEY_17=k",
		"GIT_CONFIG_VALUE_17=v", "GIT_DISCOVERY_ACROSS_FILESYSTEM=1",
		"GIT_SSH_COMMAND=ssh", "GIT_SSL_NO_VERIFY=1",
	})
	want := []string{
		"PATH=/bin", "GIT_AUTHOR_NAME=t", "HOME=/h", "GIT_TEMPLATE_DIR=/t",
		"GIT_SSH_COMMAND=ssh", "GIT_SSL_NO_VERIFY=1",
	}
	if !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
}

func evalDir(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
