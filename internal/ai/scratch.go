package ai

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"time"

	"github.com/wcatz/ghost/internal/scratch"
)

// harnessWaitDelay bounds how long a harness call's Wait may spend on I/O pipes
// a grandchild still holds after the call's context is done, and is also the
// grace a group signal gets before it is force-killed. The deadline's Cancel
// signals the harness's process group, then force-kills whatever is still in
// it once this grace is up, which ends a child that respects the signal and one
// that ignores it alike; WaitDelay is the backstop for the pipes themselves,
// because os/exec reads a helper's inherited stdout until EOF and that does not
// arrive until the helper's copy of the write end is closed. Without it the
// call blocks for the orphan's lifetime however long that is. Two seconds is a
// grace for a harness that is winding down, not a budget for one still working
// — the timer only starts once the context is done or the child has already
// exited.
const harnessWaitDelay = 2 * time.Second

// boundHarnessProcess gives a harness child the whole-call deadline bound: its
// own process group, a Cancel that signals that group and then force-kills
// whatever the grace period left, and the WaitDelay backstop. harnessCommand
// applies it to every child it builds, and the one caller that must build its
// own child — the cleanup runner, which has to reach the user's own opencode
// store rather than an isolated tree — applies it too. Keeping it one function
// is what makes the bound a property of a harness child rather than of having
// gone through the funnel: a second caller can share it, and a second caller
// that forgets it is a missing call a reader can see.
func boundHarnessProcess(cmd *exec.Cmd) {
	setHarnessProcessGroup(cmd)
	cmd.Cancel = func() error { return terminateHarnessProcess(cmd) }
	cmd.WaitDelay = harnessWaitDelay
}

// harnessCommand builds the subprocess command every LLM harness client runs,
// confined to a fresh per-invocation directory under Ghost's owned scratch
// root (internal/scratch): the child's working directory and TMPDIR/TMP/TEMP
// are pinned there, so harness droppings — opencode's ~4.7 MiB
// per-invocation JIT cache above all — die with the invocation instead of
// accumulating in the shared system temp until that filesystem fills. The
// neutral working directory also keeps the child from loading the repository's
// CLAUDE.md/AGENTS.md, project opencode.json, or git context, and the scratch
// directory serves that purpose while staying inside the owned root.
//
// The environment is filtered here rather than at each call site. This is the
// single spawn funnel for normal calls and probes, so a future backend cannot
// accidentally pass os.Environ() straight to exec.CommandContext.
//
// A child is NEVER built without somewhere private to run. If the scratch root
// cannot be created, the fallback is an invocation-owned os.MkdirTemp directory
// under the system temp, given the same working directory and the same
// TMPDIR/TMP/TEMP rewrite, and removed by the returned release. That is the
// whole point of #751: the confinement is the load-bearing property, not the
// particular directory that provides it, and it used to be reported as a bool
// the claude and codex clients discarded — so a broken data dir silently ran
// every harness child unconfined, in Ghost's source tree, against the shared
// system temp that the scratch root exists to keep harnesses out of. The
// fallback directory is named ghost-<harness>- so a leaked one says which
// harness left it.
//
// If even that fails there is nowhere to confine a child to, so the error is
// returned with a nil command and the call fails. A degraded data dir may not
// silently become a weaker policy: this is the one failure the function cannot
// absorb, and it returns an error rather than a command a caller could run.
//
// Every child is also bounded by its caller's context as a whole. The child is
// started in its own process group, Cancel signals that group and force-kills
// whatever the grace period left, and a WaitDelay backstops the pipes a helper
// holds — so a harness that leaves a helper holding its stdout cannot make
// cmd.Run block past the deadline, and nothing it started outlives the call.
// See harnessWaitDelay for why the default os/exec cancellation is not enough.
//
// The returned release is always safe to call, repeatedly and after the
// directory has already been removed — including on the error path, where it
// removes nothing.
func harnessCommand(ctx context.Context, binary string, args, baseEnv []string, kind harnessKind) (*exec.Cmd, func(), error) {
	env := harnessEnv(baseEnv, kind)

	// Pre-spawn scratch budget check: measure → if over budget reap stale
	// entries → if still over budget warn loudly → record fired checks to
	// maintenance_runs. It never fails and never blocks: every failure path
	// inside EnforceBudget warns and returns, so hygiene cannot stop a spawn —
	// it must just never be quiet. It runs before Open so this invocation's
	// own fresh directory is not counted against the budget it is about to
	// consume.
	scratch.EnforceBudget()

	cmd := exec.CommandContext(ctx, binary, args...)
	// The call's context is a real bound on the whole call, not only on the
	// direct child. os/exec's default Cancel kills just that child, so a harness
	// helper that inherited stdout keeps the copy pipe's write end open and Wait
	// blocks until the orphan exits — measured as a 500 ms context returning
	// after 6 s, which is the deadline failing at exactly the thing
	// defaultTimeout (call_bound.go) claims it prevents. boundHarnessProcess
	// closes that, in the same shape the lifecycle phase spawn uses one level
	// up, and it lives in one function so the cleanup runner's children — which
	// are built here-adjacent rather than through this funnel — share it.
	boundHarnessProcess(cmd)
	dir, err := scratch.Open()
	if err == nil {
		cmd.Dir = dir.Path()
		cmd.Env = scratchEnv(env, dir.Path())
		return cmd, dir.Release, nil
	}

	// The scratch root is unusable. Confine anyway, in a private directory of
	// our own: the alternative is a child in Ghost's working directory that
	// inherits the shared system temp, which is the failure this whole function
	// exists to prevent — and a WARN nobody can act on is not the same thing as a
	// child that cannot litter.
	fallback, ferr := os.MkdirTemp("", "ghost-"+string(kind)+"-")
	if ferr != nil {
		return nil, func() {}, fmt.Errorf("harness scratch dir unavailable (%v) and no fallback temp dir: %w", err, ferr)
	}
	slog.Warn("harness scratch dir unavailable; child confined to a private temp dir instead",
		"harness", kind, "fallback_dir", fallback, "error", err)
	cmd.Dir = fallback
	cmd.Env = scratchEnv(env, fallback)
	return cmd, func() { _ = os.RemoveAll(fallback) }, nil
}

// scratchEnv returns env with every TMPDIR/TMP/TEMP entry removed (any case)
// and the three variables pinned to tempDir, so the child writes its
// temporary files where Ghost can remove them. Inherited entries must be
// dropped rather than shadowed: os/exec passes the environment through in
// order and duplicate keys resolve inconsistently across readers and
// platforms.
func scratchEnv(env []string, tempDir string) []string {
	out := make([]string, 0, len(env)+len(tempDirKeys))
	for _, kv := range env {
		if isTempDirKey(kv) {
			continue
		}
		out = append(out, kv)
	}
	for _, key := range tempDirKeys {
		out = append(out, key+"="+tempDir)
	}
	return out
}
