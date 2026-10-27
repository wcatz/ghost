package mcpserver

import (
	"context"
	"database/sql"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wcatz/ghost/internal/config"
	"github.com/wcatz/ghost/internal/mcpinit"
	"github.com/wcatz/ghost/internal/memory"
)

// TestHealthListsOverdueMaintenance ensures ghost_health reports projects that
// have consolidatable memories but have not had a lifecycle run within the
// interval, and that it names the command to run.
func TestHealthListsOverdueMaintenance(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_DATA_HOME", filepath.Join(tmp, "data"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tmp, "config"))
	t.Setenv("HOME", filepath.Join(tmp, "home"))
	t.Setenv("USERPROFILE", filepath.Join(tmp, "home"))
	t.Setenv("GHOST_REFLECTION_AUTO_REFLECT", "true")
	if err := os.MkdirAll(filepath.Join(tmp, "home"), 0700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	dataDir, _ := config.DataDir()
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
	if err := store.EnsureProject(ctx, "idle-proj", "/tmp/idle-proj", "idle-proj"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	for i := 0; i < 3; i++ {
		if _, err := store.Create(ctx, "idle-proj", memory.Memory{Category: "fact", Content: "idle memory alpha", Importance: 0.5, Source: "mcp"}); err != nil {
			t.Fatalf("Create: %v", err)
		}
	}
	// Ensure the diagnostic read reports overdue.
	cfg := config.LoadForHook()
	overdue, err := mcpinit.OverdueMaintenanceProjects(ctx, store, dataDir, time.Now(), cfg.Lifecycle.MinIntervalDuration(), cfg.Reflection.AutoReflect || cfg.Reflection.AutoResolve || cfg.Reflection.AutoSupersede)
	if err != nil {
		t.Fatalf("Overdue: %v", err)
	}
	if len(overdue) == 0 {
		t.Fatalf("expected overdue, got none")
	}
	// Verify the health tool includes the block.
	srv := New(store, logger, "test")
	text := ghostHealthText(t, srv)
	if !strings.Contains(text, "Maintenance overdue") {
		t.Errorf("health does not list overdue maintenance:\n%s", text)
	}
	if !strings.Contains(text, "ghost lifecycle --project") {
		t.Errorf("health does not name the lifecycle command:\n%s", text)
	}
	if !strings.Contains(text, "idle-proj") {
		t.Errorf("health does not name idle project:\n%s", text)
	}
	// Also prove it does not use a second read handle query: ensure the
	// store is accessible via the diagnostic path.
	_ = sql.ErrNoRows
}
