package mcpinit

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wcatz/ghost/internal/config"
	"github.com/wcatz/ghost/internal/memory"
)

func TestOverdueMaintenanceProjects_ListsIdleWithConsolidatableRows(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_DATA_HOME", filepath.Join(tmp, "data"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tmp, "config"))
	setHome(t, filepath.Join(tmp, "home"))
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
	defer func() { _ = db.Close() }()
	store := memory.NewStore(db, nil)
	ctx := context.Background()
	for _, pid := range []string{"idle-proj", "active-proj", "empty-proj"} {
		if err := store.EnsureProject(ctx, pid, "/tmp/"+pid, pid); err != nil {
			t.Fatalf("EnsureProject %s: %v", pid, err)
		}
	}
	for i := 0; i < 3; i++ {
		if _, err := store.Create(ctx, "idle-proj", memory.Memory{Category: "fact", Content: fmt.Sprintf("idle %d alpha", i), Importance: 0.5, Source: "mcp"}); err != nil {
			t.Fatalf("create idle: %v", err)
		}
		if _, err := store.Create(ctx, "active-proj", memory.Memory{Category: "fact", Content: fmt.Sprintf("active %d beta", i), Importance: 0.5, Source: "mcp"}); err != nil {
			t.Fatalf("create active: %v", err)
		}
	}
	if err := TouchLifecycleStart("active-proj"); err != nil {
		t.Fatalf("TouchLifecycleStart: %v", err)
	}
	overdue, err := OverdueMaintenanceProjects(ctx, store, dataDir, time.Now(), 30*time.Minute, true)
	if err != nil {
		t.Fatalf("OverdueMaintenanceProjects: %v", err)
	}
	foundIdle, foundActive, foundEmpty := false, false, false
	for _, o := range overdue {
		switch o.ID {
		case "idle-proj":
			foundIdle = true
			if o.Count != 3 {
				t.Errorf("idle count=%d want 3", o.Count)
			}
			if o.HasLastRun {
				t.Errorf("idle should have no last run")
			}
		case "active-proj":
			foundActive = true
		case "empty-proj":
			foundEmpty = true
		}
	}
	if !foundIdle {
		t.Errorf("idle-proj not in overdue %v", overdue)
	}
	if foundActive {
		t.Errorf("active-proj should not be overdue (recent run) %v", overdue)
	}
	if foundEmpty {
		t.Errorf("empty-proj should not be overdue (no work) %v", overdue)
	}
	text, ok := FormatOverdueMaintenance(overdue, time.Now())
	if !ok {
		t.Fatalf("FormatOverdueMaintenance returned not ok")
	}
	if !contains(text, "ghost lifecycle --project") {
		t.Errorf("format missing command: %q", text)
	}
	if !contains(text, "idle-proj") {
		t.Errorf("format missing idle project: %q", text)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i <= len(s)-len(sub); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}

func TestOverdueIsNotReportedWhenInsideCooldown(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_DATA_HOME", filepath.Join(tmp, "data"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tmp, "config"))
	setHome(t, filepath.Join(tmp, "home"))
	t.Setenv("GHOST_REFLECTION_AUTO_REFLECT", "true")
	if err := os.MkdirAll(filepath.Join(tmp, "home"), 0700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	dataDir, _ := config.DataDir()
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	dbPath := filepath.Join(dataDir, "ghost.db")
	db, _ := memory.OpenDB(dbPath)
	defer func() { _ = db.Close() }()
	store := memory.NewStore(db, nil)
	ctx := context.Background()
	if err := store.EnsureProject(ctx, "proj", "/tmp/proj", "proj"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	for i := 0; i < 2; i++ {
		_, _ = store.Create(ctx, "proj", memory.Memory{Category: "fact", Content: fmt.Sprintf("mem %d", i), Importance: 0.5, Source: "mcp"})
	}
	if err := TouchLifecycleStart("proj"); err != nil {
		t.Fatalf("TouchLifecycleStart: %v", err)
	}
	overdue, _ := OverdueMaintenanceProjects(ctx, store, dataDir, time.Now(), 30*time.Minute, true)
	for _, o := range overdue {
		if o.ID == "proj" {
			t.Fatalf("proj should not be overdue inside cooldown: %v", overdue)
		}
	}
	// Far future: overdue because outside window
	overdue2, _ := OverdueMaintenanceProjects(ctx, store, dataDir, time.Now().Add(2*time.Hour), 30*time.Minute, true)
	found := false
	for _, o := range overdue2 {
		if o.ID == "proj" {
			found = true
		}
	}
	if !found {
		t.Errorf("proj should be overdue after 2h, got %v", overdue2)
	}
}
