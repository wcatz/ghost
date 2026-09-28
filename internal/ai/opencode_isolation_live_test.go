package ai

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestLiveOpenCodeDebugPathsHonourScratchRoot is the half of the #588 evidence
// the fake-binary tests structurally cannot provide.
//
// The unit tests assert that Ghost SETS XDG_DATA_HOME to a per-invocation
// directory. That is only half the claim. The other half — that OpenCode
// actually resolves its session store from that variable, and does not fall back
// to a compiled-in or home-relative path — is a property of the OpenCode binary,
// and a fake binary cannot demonstrate it: a fake that honours the variable
// proves only that the fake honours it.
//
// So this runs the real `opencode debug paths` under the exact environment
// subprocessEnv builds (not a hand-rolled one) and reads back the paths OpenCode
// resolved for itself. Verified against opencode v2.0.15, which reports:
//
//	data  <data home>/opencode      db   <data home>/opencode/opencode.db
//	log   <data home>/opencode/log  repos <data home>/opencode/repos
//
// The store `opencode session list` reads is the `db` file, so a resolved `db`
// inside the invocation directory is what keeps a titled "[ghost]" run out of
// the user's session list.
//
// It makes no LLM call and spends nothing: `debug paths` only prints the paths
// it resolved. It is still gated behind GHOST_LIVE_TESTS=1, like every other
// test here that spawns a real binary, so a plain `go test ./...` never
// requires an OpenCode install.
func TestLiveOpenCodeDebugPathsHonourScratchRoot(t *testing.T) {
	if !LiveTestsEnabled() {
		t.Skip("live CLI test spawns the real opencode binary; set GHOST_LIVE_TESTS=1 to run")
	}
	bin := liveOpenCodeBinary(t)
	if _, err := exec.LookPath(bin); err != nil {
		t.Skipf("opencode not resolvable at %q (cli.opencode_binary or PATH): %v", bin, err)
	}

	root := t.TempDir()
	t.Setenv("GHOST_SCRATCH_DIR", root)
	// A decoy home carrying the default data path, so a child that ignored the
	// override and resolved the store from HOME would resolve into a directory
	// this test can see and name. Nothing real is at risk.
	home := t.TempDir()
	userData := filepath.Join(home, ".local", "share")
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_DATA_HOME", userData)

	c := &OpenCodeClient{binary: bin}
	// Bound the real-binary spawn, as every other opencode spawn in the tree
	// does (openCodeCleanupRunner.run, verifyOpencodeRegistration). A first run
	// against a fresh data dir can do migrations or a version check; without a
	// bound a stall would burn the package's whole go test timeout and panic
	// with every other test's goroutines in the dump, instead of failing here.
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
		if ctx.Err() == context.DeadlineExceeded {
			t.Fatalf("opencode debug paths: timed out after %s", sessionCommandTimeout)
		}
		t.Fatalf("opencode debug paths: %v", err)
	}

	paths := parseOpenCodeDebugPaths(stdout.String())
	if len(paths) == 0 {
		t.Fatalf("no paths parsed from debug paths output:\n%s", stdout.String())
	}
	// The store and the session-scoped siblings. `tmp` is deliberately NOT in
	// this set, and the reason is worth stating precisely, because "opencode
	// ignores TMPDIR" is the wrong reading and would undercut the temp-file
	// confinement this package depends on (see subprocessEnv's comment on the
	// per-invocation JIT object).
	//
	// `opencode debug paths` reports `tmp` as OpenCode's own global namespace
	// (/tmp/opencode on v2.0.15) even when TMPDIR points elsewhere, so that
	// field is NOT where the child's temporary files go. The per-invocation JIT
	// object is a different file and it does follow TMPDIR: a .bun-<uid>-*.so
	// of ~5.6 MiB was found inside Ghost's own scratch directory
	// (<root>/<pid>-<token>/.bun-*.so) on this machine, and #465 recorded the
	// same check when it added the pinning ("a reflect run with a private
	// TMPDIR left exactly one such file in it and none elsewhere"). So nothing
	// is uncovered by leaving `tmp` out — it is a separate namespace, and
	// asserting it would pin down a path with no bearing on the session store
	// this test exists to check.
	owned := []string{"home", "data", "cache", "config", "state", "bin", "log", "repos", "db"}
	var missing []string
	for _, key := range owned {
		value, ok := paths[key]
		if !ok || value == "" {
			missing = append(missing, key)
			continue
		}
		if !strings.HasPrefix(value, cmd.Dir+string(os.PathSeparator)) {
			t.Errorf("%s = %q, want it inside the invocation dir %q", key, value, cmd.Dir)
		}
		if strings.HasPrefix(value, home) {
			t.Errorf("%s = %q resolved into the user's home %q", key, value, home)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Errorf("debug paths reported no value for %s; got keys %v", strings.Join(missing, ", "), keysOf(paths))
	}
	// Nothing resolved may be the store the user's own `opencode session list`
	// reads, which is the one this whole mechanism exists to avoid.
	if db := paths["db"]; db != "" && strings.HasPrefix(db, userData) {
		t.Errorf("db = %q resolved into the user's data dir %q", db, userData)
	}
}

// parseOpenCodeDebugPaths reads `opencode debug paths` output: one "key<spaces>
// value" pair per line, in a fixed-width column. A value may itself contain
// spaces, so everything after the first field is kept; a line with a single
// field carries no value and is dropped, because a key with no path asserts
// nothing.
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

func keysOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
