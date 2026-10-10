package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// makeScratchLayout creates every directory a run writes inside ONE root it owns,
// and returns them by name. --keep hands back that tree; the default cleanup
// removes exactly what the run made, because nothing outside it is ever written.
func makeScratchLayout(root string) (map[string]string, error) {
	dirs := make(map[string]string, 4)
	for _, name := range []string{"data", "config", "home", "work"} {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(p, 0o700); err != nil {
			return nil, fmt.Errorf("create %s: %w", p, err)
		}
		dirs[name] = p
	}
	return dirs, nil
}

// scratchEnv builds the environment every ghost process in a run inherits.
//
// This is eval/cycle's isolation, unchanged in substance: the data dir, the
// config dir and HOME all point inside the run's own scratch tree, an inherited
// override of any of them is DROPPED rather than shadowed (os/exec passes the
// environment through in order and duplicate keys resolve inconsistently across
// readers), and ANTHROPIC_API_KEY is stripped so a direct Anthropic credential
// cannot bypass the harness path. OPENCODE_API_KEY is deliberately kept: it is
// the credential the sandboxed opencode child authenticates with, which is how
// eval/cycle's LLM stages work, and Ghost passes it only to that backend.
//
// model is the run's own -model, and it goes in the environment rather than on
// a flag because the arc's classification phases are separate processes that read
// GHOST_OPENCODE_MODEL (internal/ai's allowlist carries it). An inherited pin is
// dropped, so the arc is graded by the model the report names and a stale export
// in the developer's shell cannot quietly become a sixth participant.
func scratchEnv(scratch, model string) []string {
	drop := map[string]bool{
		"XDG_DATA_HOME": true, "XDG_CONFIG_HOME": true, "HOME": true,
		"ANTHROPIC_API_KEY": true, "USERPROFILE": true,
		"GHOST_OPENCODE_MODEL": true,
		// Re-added below with the developer's real data dir listed.
		"GHOST_DEV_FORBID_DATA_DIR": true,
	}
	env := make([]string, 0, len(os.Environ())+4)
	for _, kv := range os.Environ() {
		key, _, _ := strings.Cut(kv, "=")
		if drop[strings.ToUpper(key)] {
			continue
		}
		env = append(env, kv)
	}
	env = append(env,
		"XDG_DATA_HOME="+filepath.Join(scratch, "data"),
		"XDG_CONFIG_HOME="+filepath.Join(scratch, "config"),
		"HOME="+filepath.Join(scratch, "home"),
		// A development build refuses to resolve any data dir listed here, so a
		// resolve that escaped the scratch tree and landed on the developer's own
		// store fails instead of writing to it. The list is taken from the
		// ORIGINAL environment, before HOME and XDG_DATA_HOME were replaced.
		"GHOST_DEV_FORBID_DATA_DIR="+forbiddenDataDirs(os.Getenv),
	)
	if strings.TrimSpace(model) != "" {
		env = append(env, "GHOST_OPENCODE_MODEL="+model)
	}
	return env
}

// forbiddenDataDirs is the GHOST_DEV_FORBID_DATA_DIR value a run's children get:
// whatever the caller already forbids, plus the data directory the CALLER's own
// environment resolves (XDG_DATA_HOME, else $HOME/.local/share, then "ghost"),
// deduplicated and joined with the OS path-list separator.
func forbiddenDataDirs(getenv func(string) string) string {
	var dirs []string
	seen := map[string]bool{}
	add := func(d string) {
		if d != "" && !seen[d] {
			seen[d] = true
			dirs = append(dirs, d)
		}
	}
	for _, d := range filepath.SplitList(getenv("GHOST_DEV_FORBID_DATA_DIR")) {
		add(d)
	}
	if xdg := getenv("XDG_DATA_HOME"); filepath.IsAbs(xdg) {
		add(filepath.Join(xdg, "ghost"))
	} else if home := getenv("HOME"); home != "" {
		add(filepath.Join(home, ".local", "share", "ghost"))
	}
	return strings.Join(dirs, string(os.PathListSeparator))
}

// seedOpencodeAuth copies an opencode credential into the run's own data dir, so
// the sandboxed opencode child finds it under the XDG_DATA_HOME it was given
// (internal/ai's own copy step looks there first) instead of under the
// developer's home. An empty path is a no-op: a local run that already exports
// OPENCODE_API_KEY needs nothing copied. This is eval/cycle's mechanism, flag
// name included.
func seedOpencodeAuth(scratch, authFile string) error {
	if authFile == "" {
		return nil
	}
	b, err := os.ReadFile(authFile)
	if err != nil {
		return fmt.Errorf("read opencode auth file: %w", err)
	}
	dstDir := filepath.Join(scratch, "data", "opencode")
	if err := os.MkdirAll(dstDir, 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dstDir, "auth.json"), b, 0o600); err != nil {
		return fmt.Errorf("seed opencode auth: %w", err)
	}
	return nil
}

// buildGhost compiles the ghost binary the run drives, from the checkout under
// test rather than from whatever happens to be on PATH. A run that graded an
// installed binary would be measuring a different build than the branch it
// belongs to.
func buildGhost(ctx context.Context, repoDir, out string) error {
	cmd := exec.CommandContext(ctx, "go", "build", "-o", out, "./cmd/ghost")
	cmd.Dir = repoDir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("go build ./cmd/ghost: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// runGhost runs one ghost command in the run's environment, keeping stdout and
// stderr apart: the arc stages' warnings (a guarded-drop audit, a classifier that
// gave no verdict) are worth keeping in the report even on success.
func runGhost(ctx context.Context, env []string, bin string, args ...string) (string, string, error) {
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = env
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return out.String(), errb.String(), fmt.Errorf("ghost %s: %w: %s",
			strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
	}
	return out.String(), errb.String(), nil
}
