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

// TestExportArtifactIsNotGroupOrWorldReadable: the artifact holds every memory in
// plain text and an --out destination is often a shared or synced folder, so it
// is created at 0600 (requested at creation; narrowing afterwards leaves the
// window #553 was about).
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
	if err := runExportCore(ctx, store, &strings.Builder{}, &strings.Builder{}, dest, ""); err != nil {
		t.Fatalf("runExportCore: %v", err)
	}
	info, err := os.Lstat(dest)
	if err != nil {
		t.Fatalf("no artifact at %s: %v", dest, err)
	}
	if got := info.Mode().Perm(); got != 0o600 || !info.Mode().IsRegular() {
		t.Errorf("artifact mode = %v, want a regular file at 0600", info.Mode())
	}
}
