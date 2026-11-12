package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

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

func scratchEnv(scratch, model string) []string {
	drop := map[string]bool{
		"XDG_DATA_HOME": true, "XDG_CONFIG_HOME": true, "HOME": true,
		"ANTHROPIC_API_KEY": true, "USERPROFILE": true,
		"GHOST_OPENCODE_MODEL":      true,
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
		"GHOST_DEV_FORBID_DATA_DIR="+forbiddenDataDirs(os.Getenv),
	)
	if strings.TrimSpace(model) != "" {
		env = append(env, "GHOST_OPENCODE_MODEL="+model)
	}
	return env
}

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

func buildFakeHarness(ctx context.Context, repoDir, out string) error {
	cmd := exec.CommandContext(ctx, "go", "build", "-o", out, "./eval/storyline/fakeharness")
	cmd.Dir = repoDir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("go build ./eval/storyline/fakeharness: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

var fakeHarnessNames = []string{"opencode", "claude", "codex", "goose"}

func oraclePairs(s Storyline) []map[string]string {
	var out []map[string]string
	for _, p := range reversals(s) {
		out = append(out, map[string]string{"older_mark": p.older.Mark, "newer_mark": p.newer.Mark, "replaced": p.older.Mark})
	}
	for _, l := range s.Lures {
		older, newer := mustRecord(s, l.Older), mustRecord(s, l.Newer)
		out = append(out, map[string]string{"older_mark": older.Mark, "newer_mark": newer.Mark, "replaced": older.Mark})
	}
	return out
}

func installFakeHarness(scratch, fakeBin string, s Storyline) (string, error) {
	dir := filepath.Join(scratch, "fakebin")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	b, err := os.ReadFile(fakeBin)
	if err != nil {
		return "", fmt.Errorf("read fake harness: %w", err)
	}
	for _, name := range fakeHarnessNames {
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o700); err != nil {
			return "", fmt.Errorf("install fake %s: %w", name, err)
		}
	}
	oracle, err := json.MarshalIndent(oraclePairs(s), "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(dir, "oracle.json"), oracle, 0o600); err != nil {
		return "", fmt.Errorf("write oracle: %w", err)
	}
	return dir, nil
}

func prependPath(env []string, dir string) []string {
	out := make([]string, 0, len(env)+1)
	path := ""
	for _, kv := range env {
		if strings.HasPrefix(kv, "PATH=") {
			path = strings.TrimPrefix(kv, "PATH=")
			continue
		}
		out = append(out, kv)
	}
	if path == "" {
		return append(out, "PATH="+dir)
	}
	return append(out, "PATH="+dir+string(os.PathListSeparator)+path)
}
