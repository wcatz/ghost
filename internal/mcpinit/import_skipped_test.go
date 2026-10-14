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

// TestImportMemoriesNamesARefusedFileOnce is the init-output half of the #1235
// reporting test: `ghost mcp init` names a file the guard refused exactly once,
// by name, and never echoes its text. The store is a temp data dir and home is
// a temp dir, so nothing outside the test is touched.
func TestImportMemoriesNamesARefusedFileOnce(t *testing.T) {
	root := t.TempDir()
	dataHome := filepath.Join(root, "data")
	dataDir := filepath.Join(dataHome, "ghost")
	dbPath := storeAt(t, dataDir)

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
	t.Setenv("HOME", fakeHome)
	t.Setenv("XDG_DATA_HOME", dataHome)
	memDir := filepath.Join(fakeHome, ".claude", "projects", claudeimport.EncodeProjectPath(projPath), "memory")
	mkdirAll(t, memDir)
	const refusedName = "refused-note.md"
	const refusedText = "The service account value is ghp_0123456789abcdefghijklmnopqrstuvwxyzAB for CI"
	if err := os.WriteFile(filepath.Join(memDir, refusedName), []byte("Imported from a hostile file.\n\n"+refusedText), 0o600); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if _, err := importMemories(&out, false); err != nil {
		t.Fatalf("importMemories = %v, want success; it printed:\n%s", err, out.String())
	}
	if got := strings.Count(out.String(), refusedName); got != 1 {
		t.Errorf("the refused file is named %d times, want exactly once; init printed:\n%s", got, out.String())
	}
	if strings.Contains(out.String(), refusedText) {
		t.Errorf("init echoes the refused file's text:\n%s", out.String())
	}
}
