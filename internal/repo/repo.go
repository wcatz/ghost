// Package repo reads repository identity out of the filesystem.
//
// It exists to keep process-spawning out of internal/memory: the memory store
// must stay a pure storage layer, but the only place that can say which
// repository a path belongs to is a caller that is willing to ask git. Callers
// that own a path ask here and hand the answer to the store.
//
// Every git child this package spawns is built by GitCommand, which is also the
// one helper internal/reflection uses for its own commit log: a caller anywhere
// in Ghost asks git about a directory it named, and an inherited git location
// variable must not be able to answer about a different one.
package repo

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// detectTimeout bounds the git invocation. Detection runs on save paths, so a
// hung git — a network-mounted worktree, an NFS-backed checkout, a corrupt
// repository — must never stall a memory write. A timeout is not a failure:
// "no repository known" is a legitimate answer.
const detectTimeout = 2 * time.Second

// DetectRemote returns dir's origin remote URL, or "" when there is none.
//
// Every failure path returns "" rather than an error: a directory that is not
// a repository, has no origin remote, cannot be read, or has no git binary
// installed are all ordinary situations for a project path, and none of them
// should stop a memory from being saved. The caller treats "" as "no
// repository", which simply disables repository identity for that project.
func DetectRemote(dir string) string {
	if dir == "" {
		return ""
	}
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return ""
	}

	ctx, cancel := context.WithTimeout(context.Background(), detectTimeout)
	defer cancel()

	// -C walks up to the enclosing repository, so a path pointing inside a
	// checkout still reports that checkout's remote.
	out, err := GitCommand(ctx, "-C", dir, "config", "--get", "remote.origin.url").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// TopLevel returns the physical top-level directory of the git checkout that
// contains dir, or "" when dir is not inside one (or git is unavailable, or the
// answer takes too long). A bare repository has no working tree and answers "".
func TopLevel(dir string) string {
	if dir == "" {
		return ""
	}
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return ""
	}

	ctx, cancel := context.WithTimeout(context.Background(), detectTimeout)
	defer cancel()

	out, err := GitCommand(ctx, "-C", dir, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return ""
	}
	top := strings.TrimSpace(string(out))
	if top == "" {
		return ""
	}
	if resolved, err := filepath.EvalSymlinks(top); err == nil {
		top = resolved
	}
	return top
}

// gitLocationVars are the variables that tell git WHERE a repository is rather
// than how to behave. Inherited, they override the directory a caller names with
// -C: a server started from inside a git hook, or from a shell that exports one,
// would resolve a different top level or remote than its working directory
// implies. Every git child Ghost runs is built by GitCommand, which drops them,
// so the directory passed with -C is the only thing that decides which
// repository answers.
var gitLocationVars = []string{
	"GIT_DIR",
	"GIT_WORK_TREE",
	"GIT_COMMON_DIR",
	"GIT_INDEX_FILE",
	"GIT_CEILING_DIRECTORIES",
	"GIT_OBJECT_DIRECTORY",
	"GIT_ALTERNATE_OBJECT_DIRECTORIES",
	"GIT_NAMESPACE",
	"GIT_PREFIX",
}

// withoutGitLocation returns vars minus every inherited git location variable.
// It keeps everything else, including the git variables that are not about
// location: GIT_AUTHOR_NAME and friends name a commit, and PATH and HOME decide
// which git runs at all, so dropping them would break the child rather than
// answer a different question.
func withoutGitLocation(vars []string) []string {
	out := make([]string, 0, len(vars))
	for _, kv := range vars {
		name, _, _ := strings.Cut(kv, "=")
		if slices.Contains(gitLocationVars, name) {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// GitCommand builds a git child process that does not inherit the git location
// variables. Every git child Ghost runs goes through here — TopLevel,
// DetectRemote and internal/reflection's commit log — so the -C directory a
// caller passes is the only input that decides which repository answers, and a
// parent that carries GIT_DIR cannot rename the one it gets.
//
// The scrub is deliberately narrow. A location variable is dropped because it
// answers a question the caller already answered with -C, and a value inherited
// from a hook overrides that answer; everything else the git child needs is
// passed through untouched, so the child behaves exactly as the caller asked and
// no more.
func GitCommand(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Env = withoutGitLocation(os.Environ())
	return cmd
}
