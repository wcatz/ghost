//go:build !windows

package main

import (
	"os"
	"testing"
)

// The published artifact keeps the 0600 width the temporary file was created
// with. POSIX mode bits only; Windows carries access in the directory ACL.
func TestExportPublishedArtifactIsOwnerOnly(t *testing.T) {
	_, path, run := plantAtomicExportRow(t)
	if err := run(); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", fi.Mode().Perm())
	}
}
