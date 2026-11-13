package mcpinit

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wcatz/ghost/internal/config"
	"github.com/wcatz/ghost/internal/memory"
)

func TestOverdueMaintenanceListsOnlyUnsafeProject(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_DATA_HOME", filepath.Join(tmp, "data"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tmp, "config"))
	setHome(t, filepath.Join(tmp, "home"))
	t.Setenv("GHOST_REFLECTION_AUTO_REFLECT", "true")
	if err := os.MkdirAll(filepath.Join(tmp, "home"), 0700); err != nil {
		t.Fatalf("MkdirAll home: %v", err)
	}
	dataDir, err := config.DataDir()
	if err != nil {
		t.Fatalf("DataDir: %v", err)
	}
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		t.Fatalf("MkdirAll data dir: %v", err)
	}
	db, err := memory.OpenDB(filepath.Join(dataDir, "ghost.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	store := memory.NewStore(db, nil)
	ctx := context.Background()
	if err := store.EnsureProject(ctx, "team/api", "/tmp/team-api", "Team API"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	for i := 0; i < 2; i++ {
		if _, err := store.Create(ctx, "team/api", memory.Memory{
			Category:   "fact",
			Content:    fmt.Sprintf("unsafe only %d", i),
			Importance: 0.5,
			Source:     "mcp",
		}); err != nil {
			t.Fatalf("Create: %v", err)
		}
	}

	projects, err := OverdueMaintenanceProjects(ctx, store, dataDir, time.Now(), 30*time.Minute, true)
	if err != nil {
		t.Fatalf("OverdueMaintenanceProjects: %v", err)
	}
	text, ok := FormatOverdueMaintenance(projects, time.Now())
	if !ok {
		t.Fatalf("FormatOverdueMaintenance returned not ok for an unsafe-only store")
	}
	if strings.Contains(text, "Maintenance overdue") {
		t.Errorf("an unsafe-only store printed an overdue section:\n%s", text)
	}
	if !strings.Contains(text, "Cannot be maintained unattended") {
		t.Errorf("an unsafe-only store printed nothing about it:\n%s", text)
	}

	var status bytes.Buffer
	reportOverdueMaintenanceStatus(&status, store, dataDir)
	out := status.String()
	if !strings.Contains(out, "! cannot be maintained unattended:") {
		t.Errorf("the status path dropped an unsafe-only project:\n%s", out)
	}
	if strings.Contains(out, "! maintenance overdue:") {
		t.Errorf("the status path printed an empty overdue section:\n%s", out)
	}
}
