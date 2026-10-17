//go:build !windows

package mcpinit

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/claudeimport"
	"github.com/wcatz/ghost/internal/memory"
)

// TestImportMemoriesNamesANonRegularEntryAsSkipped: `ghost mcp init` names a
// symlink in the Claude memory directory as skipped because it is not a
// regular file, never as a write a later run retries, and never prints the
// link target's text.
func TestImportMemoriesNamesANonRegularEntryAsSkipped(t *testing.T) {
	root := t.TempDir()
	dataHome := filepath.Join(root, "data")
	dbPath := storeAt(t, filepath.Join(dataHome, "ghost"))

	projPath := filepath.Join(root, "proj")
	mkdirAll(t, projPath)

	db, err := memory.OpenDB(dbPath)
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	store := memory.NewStore(db, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := store.EnsureProject(context.Background(), "initproj", projPath, "initproj"); err != nil {
		_ = db.Close()
		t.Fatalf("EnsureProject: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close the store: %v", err)
	}

	fakeHome := filepath.Join(root, "home")
	setHome(t, fakeHome)
	t.Setenv("XDG_DATA_HOME", dataHome)

	plantedDir := filepath.Join(fakeHome, ".claude", "projects", claudeimport.EncodeProjectPath(projPath), "memory")
	mkdirAll(t, plantedDir)
	if memDir := claudeimport.ClaudeMemoryDir(projPath); memDir != plantedDir {
		t.Fatalf("ClaudeMemoryDir(%q) = %q, want %q", projPath, memDir, plantedDir)
	}
	const outsideText = "text from a file outside the claude memory directory"
	outside := filepath.Join(root, "outside.txt")
	if err := os.WriteFile(outside, []byte(outsideText), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(plantedDir, "a_link.md")); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if _, err := importMemories(&out, false); err != nil {
		t.Fatalf("importMemories = %v, want success; it printed:\n%s", err, out.String())
	}
	want := "  ! initproj — skipped, not a regular file (never read): " + memory.ProjectArg("file", "a_link.md") + "\n"
	if got := strings.Count(out.String(), want); got != 1 {
		t.Errorf("want %q exactly once, got %d; init printed:\n%s", want, got, out.String())
	}
	if strings.Contains(out.String(), "a later run retries it") {
		t.Errorf("the link is worded as a write a later run retries:\n%s", out.String())
	}
	if strings.Contains(out.String(), outsideText) {
		t.Errorf("init echoes the link target's text:\n%s", out.String())
	}
}
