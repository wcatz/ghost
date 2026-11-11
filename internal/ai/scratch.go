package ai

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"time"

	"github.com/wcatz/ghost/internal/scratch"
)

const harnessWaitDelay = 2 * time.Second

func boundHarnessProcess(cmd *exec.Cmd) {
	setHarnessProcessGroup(cmd)
	cmd.Cancel = func() error { return terminateHarnessProcess(cmd) }
	cmd.WaitDelay = harnessWaitDelay
}

func runHarnessCommand(cmd *exec.Cmd) error {
	err := cmd.Run()
	sweepHarnessProcessGroup(cmd)
	return err
}

func runHarnessCommandOutput(cmd *exec.Cmd) ([]byte, error) {
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	err := runHarnessCommand(cmd)
	return stdout.Bytes(), err
}

func harnessCommand(ctx context.Context, binary string, args, baseEnv []string, kind harnessKind) (*exec.Cmd, func(), error) {
	env := harnessEnv(baseEnv, kind)

	scratch.EnforceBudget()

	cmd := exec.CommandContext(ctx, binary, args...)

	boundHarnessProcess(cmd)
	dir, err := scratch.Open()
	if err == nil {
		cmd.Dir = dir.Path()
		cmd.Env = scratchEnv(env, dir.Path())
		return cmd, dir.Release, nil
	}

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
