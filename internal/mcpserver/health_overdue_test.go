package mcpserver

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/config"
	"github.com/wcatz/ghost/internal/memory"
)

// TestHealthCapsOverdueAndUnmaintainableSectionsIndependently pins that a long
// overdue list cannot cut the cannot-be-maintained-unattended section: both
// diagnostics must report the unsafe project whatever the number of overdue
// ones in front of it.
func TestHealthCapsOverdueAndUnmaintainableSectionsIndependently(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_DATA_HOME", filepath.Join(tmp, "data"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tmp, "config"))
	t.Setenv("HOME", filepath.Join(tmp, "home"))
	t.Setenv("USERPROFILE", filepath.Join(tmp, "home"))
	t.Setenv("GHOST_REFLECTION_AUTO_REFLECT", "true")
	if err := os.MkdirAll(filepath.Join(tmp, "home"), 0700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	dataDir, err := config.DataDir()
	if err != nil {
		t.Fatalf("DataDir: %v", err)
	}
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	dbPath := filepath.Join(dataDir, "ghost.db")
	db, err := memory.OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	readDB, err := memory.OpenReadDB(dbPath)
	if err != nil {
		t.Fatalf("OpenReadDB: %v", err)
	}
	t.Cleanup(func() { _ = readDB.Close() })
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	store := memory.NewStoreWithRead(db, readDB, logger)
	ctx := context.Background()

	// One more overdue project than the per-section cap, so without an
	// independent cap the unsafe section falls behind the cut.
	const overdueProjects = 21
	for i := 0; i < overdueProjects; i++ {
		id := fmt.Sprintf("overdue-%02d", i)
		if err := store.EnsureProject(ctx, id, "/tmp/"+id, id); err != nil {
			t.Fatalf("EnsureProject %s: %v", id, err)
		}
		if _, err := store.Create(ctx, id, memory.Memory{Category: "fact", Content: "overdue memory " + id, Importance: 0.5, Source: "mcp"}); err != nil {
			t.Fatalf("Create %s: %v", id, err)
		}
	}
	const unsafeID = "team/api"
	if err := store.EnsureProject(ctx, unsafeID, "/tmp/team-api", "Team API"); err != nil {
		t.Fatalf("EnsureProject %s: %v", unsafeID, err)
	}
	if _, err := store.Create(ctx, unsafeID, memory.Memory{Category: "fact", Content: "unsafe memory", Importance: 0.5, Source: "mcp"}); err != nil {
		t.Fatalf("Create %s: %v", unsafeID, err)
	}

	srv := New(store, logger, "test")
	text := ghostHealthText(t, srv)
	if !strings.Contains(text, "Maintenance overdue") {
		t.Errorf("health lost the overdue section:\n%s", text)
	}
	if !strings.Contains(text, "... and 1 more") {
		t.Errorf("the overdue section was not capped on its own:\n%s", text)
	}
	if !strings.Contains(text, "Cannot be maintained unattended") {
		t.Errorf("the unsafe section fell behind the overdue cap:\n%s", text)
	}
	if !strings.Contains(text, unsafeID) {
		t.Errorf("health does not name the unsafe project:\n%s", text)
	}
}
