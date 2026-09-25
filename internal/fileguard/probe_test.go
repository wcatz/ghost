package fileguard

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestExternalOpenProbeCancellationIsUnknown(t *testing.T) {
	oldRunner := runOpenProbeTool
	runOpenProbeTool = func(ctx context.Context, _, _ string) error {
		<-ctx.Done()
		return ctx.Err()
	}
	t.Cleanup(func() { runOpenProbeTool = oldRunner })

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	inUse, known, err := probeOpenFileWithTool(ctx, "probe", "path")
	if known || inUse || err == nil {
		t.Fatalf("cancelled probe = inUse=%v known=%v err=%v, want unknown error", inUse, known, err)
	}
}

func TestNativeOpenProbeHonorsContextDeadline(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "windows" {
		t.Skip("native probe is implemented for Linux and Windows")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, known, err := NativeOpenProbe(ctx, filepath.Join(t.TempDir(), "held"))
	if known || err == nil {
		t.Fatalf("cancelled native probe known=%v err=%v, want unknown error", known, err)
	}
}

func TestNativeOpenProbeFindsHeldFile(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "windows" {
		t.Skip("native probe is implemented for Linux and Windows")
	}
	path := filepath.Join(t.TempDir(), "held")
	if err := os.WriteFile(path, []byte("held"), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close() //nolint:errcheck
	ctx, cancel := context.WithTimeout(context.Background(), ProbeTimeout)
	defer cancel()
	inUse, known, err := NativeOpenProbe(ctx, path)
	if err != nil || !known || !inUse {
		t.Fatalf("native probe = inUse=%v known=%v err=%v, want held", inUse, known, err)
	}
}

func TestNativeOpenProbeFindsHeldFileThroughSymlinkedDirectory(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("symlink regression is Linux-specific")
	}
	realDir := t.TempDir()
	linkDir := filepath.Join(t.TempDir(), "linked")
	if err := os.Symlink(realDir, linkDir); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	path := filepath.Join(linkDir, "held")
	if err := os.WriteFile(path, []byte("held"), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close() //nolint:errcheck
	ctx, cancel := context.WithTimeout(context.Background(), ProbeTimeout)
	defer cancel()
	inUse, known, err := NativeOpenProbe(ctx, path)
	if err != nil || !known || !inUse {
		t.Fatalf("native probe through symlink = inUse=%v known=%v err=%v, want held", inUse, known, err)
	}
}
