//go:build e2e && !windows

package e2e

// A FIFO at the per-project lifecycle claim path. The claim is a file in the
// data directory, which every local process of the user's own can write, and
// the hook read it with a bare os.ReadFile: opening a FIFO for reading blocks
// until a writer appears, so `ghost hook stop` hung on it until the host's own
// timeout killed it. Only a regular file can be a claim, so a planted FIFO is
// no claim at all — the hook answers at once, and goes on to spawn the chain
// it was about to spawn.

import (
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// TestStopHookWithAFIFOClaimReturns drives the Stop hook with a FIFO planted
// where the project's lifecycle claim belongs. The hook must return, and the
// chain it spawns must write the start stamp — a FIFO is no claim, so the
// spawn the guard exists for still happens.
func TestStopHookWithAFIFOClaimReturns(t *testing.T) {
	s := newSandbox(t)
	call(t, s.mcpSession(t), "ghost_memory_save", map[string]any{
		"project_id": e2eProject,
		"content":    "a memory so the project exists for the stop hook",
	})
	s.mustRun("project", "bind", e2eProject, s.work)
	// The claim is read behind the "a phase is configured" guard, so the hook
	// has to be configured to consolidate for the read to happen at all.
	s.reconfigure(configOpts{autoResolve: true, minInterval: "30m"})

	claim := filepath.Join(s.dataDir(), "lifecycle-"+e2eProject+".pid")
	if err := syscall.Mkfifo(claim, 0o600); err != nil {
		t.Skipf("mkfifo unavailable: %v", err)
	}

	transcript, format := transcriptFor(t, "claude-code", true)
	s.mustRunStdin(stopPayload("claude-code", s.work, transcript, format),
		"hook", "stop", "--source", "claude-code")

	// The stamp is written by the spawned chain, once it holds the claim, so
	// its arrival is the evidence that the hook treated the FIFO as no claim
	// and spawned the chain rather than waiting on a claim that was never
	// going to answer.
	waitForFile(t, filepath.Join(s.dataDir(), "lifecycle-"+e2eProject+".last"),
		60*time.Second, "the lifecycle start stamp")
}
