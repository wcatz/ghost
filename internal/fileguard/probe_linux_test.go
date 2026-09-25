//go:build linux

package fileguard

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestNativeOpenProbeFindsMappedFileWithoutFD(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mapped")
	if err := os.WriteFile(path, make([]byte, 4096), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	mapped, err := syscall.Mmap(int(file.Fd()), 0, 4096, syscall.PROT_READ, syscall.MAP_SHARED)
	if err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	_ = mapped[0]
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	defer syscall.Munmap(mapped) //nolint:errcheck
	ctx, cancel := context.WithTimeout(context.Background(), ProbeTimeout)
	defer cancel()
	inUse, known, err := NativeOpenProbe(ctx, path)
	if err != nil || !known || !inUse {
		t.Fatalf("native mmap probe = inUse=%v known=%v err=%v, want held", inUse, known, err)
	}
}
