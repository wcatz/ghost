// Package repo reads repository identity.
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

const detectTimeout = 2 * time.Second

// DetectRemote returns dir's origin remote URL, or "" if none.
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

	out, err := GitCommand(ctx, "-C", dir, "config", "--get", "remote.origin.url").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// TopLevel returns the physical top-level directory, or "" if not in a checkout.
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

var gitOverridingVars = []string{
	"GIT_DIR",
	"GIT_WORK_TREE",
	"GIT_COMMON_DIR",
	"GIT_INDEX_FILE",
	"GIT_CEILING_DIRECTORIES",
	"GIT_OBJECT_DIRECTORY",
	"GIT_ALTERNATE_OBJECT_DIRECTORIES",
	"GIT_NAMESPACE",
	"GIT_PREFIX",
	"GIT_DISCOVERY_ACROSS_FILESYSTEM",
	"GIT_CONFIG_PARAMETERS",
	"GIT_CONFIG_COUNT",
	"GIT_CONFIG",
	"GIT_CONFIG_GLOBAL",
	"GIT_CONFIG_SYSTEM",
}

var gitOverridingVarPrefixes = []string{
	"GIT_CONFIG_KEY_",
	"GIT_CONFIG_VALUE_",
}

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

// GitCommand builds a git child that drops overriding git variables.
func GitCommand(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Env = withoutGitOverrides(os.Environ())
	return cmd
}
