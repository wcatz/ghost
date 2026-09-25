package ai

import (
	"context"
	"log/slog"
	"os/exec"

	"github.com/wcatz/ghost/internal/scratch"
)

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
// ok is false when the scratch root cannot be created. That is a WARN, not an
// error: the child then keeps the allowlisted environment and the inherited
// working directory, so a broken data dir degrades loudly rather than taking
// down reflection. The returned release is always safe to call, repeatedly and
// after the directory has already been removed; it is a no-op when ok is false.
func harnessCommand(ctx context.Context, binary string, args, baseEnv []string, kind harnessKind) (*exec.Cmd, func(), bool) {
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
	dir, err := scratch.Open()
	if err != nil {
		slog.Warn("harness scratch dir unavailable; child keeps the allowlisted environment and inherited working directory",
			"harness", kind, "error", err)
		cmd.Env = env
		return cmd, func() {}, false
	}
	cmd.Dir = dir.Path()
	cmd.Env = scratchEnv(env, dir)
	return cmd, dir.Release, true
}

// scratchEnv returns env with every TMPDIR/TMP/TEMP entry removed (any case)
// and the three variables pinned to d's path, so the child writes its
// temporary files where Ghost can remove them. Inherited entries must be
// dropped rather than shadowed: os/exec passes the environment through in
// order and duplicate keys resolve inconsistently across readers and
// platforms.
func scratchEnv(env []string, d *scratch.Dir) []string {
	out := make([]string, 0, len(env)+len(tempDirKeys))
	for _, kv := range env {
		if isTempDirKey(kv) {
			continue
		}
		out = append(out, kv)
	}
	for _, key := range tempDirKeys {
		out = append(out, key+"="+d.Path())
	}
	return out
}
