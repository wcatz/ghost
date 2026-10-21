package main

// `ghost upgrade` run from a plugin-managed binary used to print its refusal
// on stdout and exit 0. Every other path in the command that does not upgrade
// because something stopped it exits 1 — a bad argument, an unknown binary
// path, a downgrade, a prerelease, an attestation that is missing, unreachable
// or unverifiable — and this one installed nothing, verified nothing and
// replaced nothing, so a caller branching on the exit status saw success.
//
// The test drives the built binary, because the exit status and the stream are
// the contract: an in-process call cannot read either, and the refusal is the
// one thing this command does that a user reaches from a terminal rather than
// from a script.
//
// The environment is built rather than filtered, and the plugin root is a
// directory under the test's own sandbox: the signal the refusal reads is
// CLAUDE_PLUGIN_ROOT, and a real one would point at a real plugin cache.

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestUpgradeRefusesAPluginManagedBinaryWithAFailedExit: the plugin-managed
// refusal exits 1, writes to stderr, and writes nothing to stdout.
func TestUpgradeRefusesAPluginManagedBinaryWithAFailedExit(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the ghost binary")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "ghost")
	build := exec.Command("go", "build", "-o", bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}

	pluginRoot := filepath.Join(dir, "plugin")
	if err := os.MkdirAll(pluginRoot, 0o755); err != nil {
		t.Fatalf("mkdir the plugin root: %v", err)
	}

	cmd := exec.Command(bin, "upgrade")
	cmd.Env = []string{
		"HOME=" + dir,
		"XDG_CONFIG_HOME=" + dir,
		"XDG_DATA_HOME=" + dir,
		"PATH=" + os.Getenv("PATH"),
		"CLAUDE_PLUGIN_ROOT=" + pluginRoot,
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()

	ee, ok := err.(*exec.ExitError)
	if !ok || ee.ExitCode() != 1 {
		t.Fatalf("exit = %v, want exit status 1: a refusal is a run that started and failed, whatever stopped it", err)
	}
	const want = "This ghost binary is managed by the Claude Code plugin — update it with `/plugin update` in Claude Code, not `ghost upgrade`."
	if !strings.Contains(stderr.String(), want) {
		t.Errorf("stderr does not carry the refusal:\n%s", stderr.String())
	}
	if stdout.String() != "" {
		t.Errorf("stdout = %q, want nothing: the refusal is a diagnostic, not a result", stdout.String())
	}
}
