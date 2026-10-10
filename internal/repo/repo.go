// Package repo reads repository identity out of the filesystem.
//
// It exists to keep process-spawning out of internal/memory: the memory store
// must stay a pure storage layer, but the only place that can say which
// repository a path belongs to is a caller that is willing to ask git. Callers
// that own a path ask here and hand the answer to the store.
//
// Every git child this package spawns is built by GitCommand, which is also the
// one helper internal/reflection uses for its own commit log: a caller anywhere
// in Ghost asks git about a directory it named, and neither an inherited git
// location variable nor an inherited git config override may be able to answer
// about a different one.
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

// gitOverridingVars are the inherited variables a git child honours that can
// make it answer about a repository other than the one the caller named, or
// about a configuration that is not the checkout's own. A server started from
// inside a git hook, or from a shell that exports one, must resolve the same
// repository its working directory implies, so GitCommand drops every one of
// them. Nothing else is dropped — see withoutGitOverrides.
var gitOverridingVars = []string{
	// Location: each of these answers "which repository is this?", and an
	// inherited value overrides the directory the caller named with -C.
	"GIT_DIR",
	"GIT_WORK_TREE",
	"GIT_COMMON_DIR",
	"GIT_INDEX_FILE",
	"GIT_CEILING_DIRECTORIES",
	"GIT_OBJECT_DIRECTORY",
	"GIT_ALTERNATE_OBJECT_DIRECTORIES",
	"GIT_NAMESPACE",
	"GIT_PREFIX",
	// GIT_DISCOVERY_ACROSS_FILESYSTEM is in the same family as the ceiling: it
	// widens where discovery may walk to, so it changes which repository a
	// directory resolves to just as the ceiling narrows it.
	"GIT_DISCOVERY_ACROSS_FILESYSTEM",

	// Config: each of these decides WHICH configuration the child reads, and
	// they can answer the one key DetectRemote reads.
	//
	// GIT_CONFIG_PARAMETERS is what `git -c key=value` exports to the child it
	// spawns — a hook included — and GIT_CONFIG_COUNT with GIT_CONFIG_KEY_n and
	// GIT_CONFIG_VALUE_n is the same job in an indexed spelling. That layer
	// outranks the checkout's own .git/config, so either one can name another
	// remote.origin.url.
	"GIT_CONFIG_PARAMETERS",
	"GIT_CONFIG_COUNT",
	// GIT_CONFIG, GIT_CONFIG_GLOBAL and GIT_CONFIG_SYSTEM replace the files the
	// child reads. GIT_CONFIG replaces the whole read, so it outranks even a
	// local value; the global and system ones cannot outrank a local one, but
	// they still supply a key the checkout does not carry — and a repository
	// with no origin of its own is exactly the ordinary case for a project
	// bound before its remote was recorded.
	//
	// What dropping these three costs is stated rather than hidden: they are
	// also the supported way to RELOCATE that config — a container or a CI
	// sandbox pointing the global at a file of its own. Dropped, the child falls
	// back to $HOME/.gitconfig and /etc/gitconfig, which is where safe.directory
	// normally lives anyway, so a checkout owned by another uid still resolves.
	// One whose safe.directory was parked only in the relocated file stops
	// resolving and both detectors answer "". The alternative is a parent that
	// can name the global file, so the pointers go.
	"GIT_CONFIG",
	"GIT_CONFIG_GLOBAL",
	"GIT_CONFIG_SYSTEM",
}

// gitOverridingVarPrefixes are the indexed form of the command-line config
// layer: GIT_CONFIG_COUNT says how many pairs follow, and each pair is
// GIT_CONFIG_KEY_<n> beside GIT_CONFIG_VALUE_<n>. The count goes in the list
// above and the pairs are matched by prefix, because the index is unbounded.
//
// The count is the load-bearing half. Dropping only the pairs while leaving the
// count makes the child fail outright — git reports a missing config key and
// exits non-zero — so DetectRemote would answer "no repository known" rather
// than answering about the directory it was handed. Both halves go, or neither.
var gitOverridingVarPrefixes = []string{
	"GIT_CONFIG_KEY_",
	"GIT_CONFIG_VALUE_",
}

// isGitOverriding reports whether name is a variable a git child would honour
// into answering about a repository or a configuration it was not asked about.
func isGitOverriding(name string) bool {
	if slices.Contains(gitOverridingVars, name) {
		return true
	}
	for _, prefix := range gitOverridingVarPrefixes {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

// withoutGitOverrides returns vars minus every inherited git variable that can
// relocate the repository or replace the file its configuration is read from. It
// keeps everything else, including the git variables that are not about location
// or config: GIT_AUTHOR_NAME and friends name a commit, and PATH and HOME decide
// which git runs at all, so dropping them would break the child rather than
// answer a different question.
func withoutGitOverrides(vars []string) []string {
	out := make([]string, 0, len(vars))
	for _, kv := range vars {
		name, _, _ := strings.Cut(kv, "=")
		if isGitOverriding(name) {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// GitCommand builds a git child process that does not inherit the git variables
// that can relocate the repository or replace the file its configuration is read
// from. Every git child Ghost runs goes through here — TopLevel, DetectRemote and
// internal/reflection's commit log — so the -C directory a caller passes is the
// only input that decides which repository answers, and a parent that carries
// GIT_DIR cannot rename the one it gets, nor can GIT_CONFIG_PARAMETERS name a
// different remote.origin.url for it.
//
// The scrub is deliberately narrow. A location variable is dropped because it
// answers a question the caller already answered with -C, and a config variable
// is dropped because it names a config FILE for a question the checkout's own
// config already answers; a value inherited from a hook overrides that answer in
// both cases. What the scrub does NOT reach is stated with the same precision: the checkout's own .git/config,
// and the user's own global and system config. `git config --get` reads local
// first, so the checkout's own value still wins, and when the checkout carries
// none the user's own $HOME/.gitconfig answers — the answer git gives at the
// user's own shell, which is not something a parent process can point anywhere.
//
// Everything else the git child needs is passed through untouched, so the child
// behaves exactly as the caller asked and no more.
func GitCommand(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Env = withoutGitOverrides(os.Environ())
	return cmd
}
