//go:build !windows

package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// TestExportArtifactIsNotGroupOrWorldReadable: the artifact holds every memory
// in the file, in plain text, so it must land at the width the database has. The
// data directory is 0700, but an --out destination is often a shared folder or a
// synced directory, where the file's own mode is the only thing protecting it.
func TestExportArtifactIsNotGroupOrWorldReadable(t *testing.T) {
	store := transferTestStore(t)
	ctx := context.Background()
	if err := store.EnsureProject(ctx, "p1", "/src/p1", "p1"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	if _, err := store.Create(ctx, "p1", memory.Memory{Category: "fact", Content: "a private fact", Source: "mcp"}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	dest := filepath.Join(t.TempDir(), "export.jsonl")
	if err := runExportCore(ctx, store, &strings.Builder{}, dest, ""); err != nil {
		t.Fatalf("runExportCore: %v", err)
	}
	info, err := os.Lstat(dest)
	if err != nil {
		t.Fatalf("no artifact at %s: %v", dest, err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("artifact mode = %#o, want 0600", got)
	}
	// The width has to be requested at creation: a file created wider and
	// narrowed afterwards has been group-readable for a moment, which is the
	// window #553 was about.
	if !info.Mode().IsRegular() {
		t.Errorf("artifact is not a regular file: %v", info.Mode())
	}
}
