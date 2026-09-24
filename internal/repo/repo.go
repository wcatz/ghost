// Package repo reads repository identity out of the filesystem.
//
// It exists to keep process-spawning out of internal/memory: the memory store
// must stay a pure storage layer, but the only place that can say which
// repository a path belongs to is a caller that is willing to ask git. Callers
// that own a path ask here and hand the answer to the store.
package repo

import (
	"context"
	"os"
	"os/exec"
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
	out, err := exec.CommandContext(ctx, "git", "-C", dir, "config", "--get", "remote.origin.url").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
