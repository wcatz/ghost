package fileguard

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
)

var runOpenProbeTool = func(ctx context.Context, tool, path string) error {
	return exec.CommandContext(ctx, tool, path).Run()
}

// NativeOpenProbe exposes the platform-native probe for focused tests.
func NativeOpenProbe(ctx context.Context, path string) (bool, bool, error) {
	return nativeOpenProbe(ctx, path)
}

func detectOpenFile(ctx context.Context, path string) (bool, error) {
	tool, err := exec.LookPath("lsof")
	if err != nil {
		tool, err = exec.LookPath("fuser")
	}
	var toolErr error
	if err == nil {
		inUse, known, probeErr := probeOpenFileWithTool(ctx, tool, path)
		if known {
			return inUse, probeErr
		}
		toolErr = probeErr
	}
	if inUse, known, nativeErr := nativeOpenProbe(ctx, path); known {
		return inUse, nativeErr
	}
	if err != nil {
		return false, errors.New("neither a native open-file probe nor lsof/fuser is available")
	}
	return false, toolErr
}

func probeOpenFileWithTool(ctx context.Context, tool, path string) (inUse, known bool, err error) {
	runErr := runOpenProbeTool(ctx, tool, path)
	if runErr == nil {
		return true, true, nil
	}
	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) && exitErr.ExitCode() == 1 {
		return false, true, nil
	}
	return false, false, fmt.Errorf("%s: %w", filepath.Base(tool), runErr)
}
