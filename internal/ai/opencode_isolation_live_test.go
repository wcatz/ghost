package ai

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestLiveOpenCodeDebugPathsHonourScratchRoot is the half of the #588 evidence
func TestLiveOpenCodeDebugPathsHonourScratchRoot(t *testing.T) {
	if !LiveTestsEnabled() {
		t.Skip("live CLI test; set GHOST_LIVE_TESTS=1 to run")
	}
	bin := liveOpenCodeBinary(t)
	if _, err := exec.LookPath(bin); err != nil {
		t.Skipf("opencode not resolvable: %v", err)
	}
	root := t.TempDir()
	t.Setenv("GHOST_SCRATCH_DIR", root)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, ".local", "share"))
	c := &OpenCodeClient{binary: bin}
	ctx, cancel := context.WithTimeout(context.Background(), sessionCommandTimeout)
	defer cancel()
	cmd, cleanup, err := c.subprocessEnv(ctx, []string{"debug", "paths"}, openCodeAskConfig)
	if err != nil {
		t.Fatalf("subprocessEnv: %v", err)
	}
	defer cleanup()
	var stdout strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("opencode debug paths: %v", err)
	}
	paths := parseOpenCodeDebugPaths(stdout.String())
	if len(paths) == 0 {
		t.Fatalf("no paths parsed")
	}
	for _, key := range []string{"home", "data", "cache", "config", "state", "bin", "log", "repos", "db"} {
		value := paths[key]
		if value == "" || !strings.HasPrefix(value, cmd.Dir+string(os.PathSeparator)) {
			t.Errorf("%s=%q outside %q", key, value, cmd.Dir)
		}
	}
}

func parseOpenCodeDebugPaths(out string) map[string]string {
	paths := make(map[string]string)
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		paths[fields[0]] = strings.Join(fields[1:], " ")
	}
	return paths
}
