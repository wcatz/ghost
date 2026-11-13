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

func TestOverdueMaintenanceProjects_WordingSingularAndPlural(t *testing.T) {
	now := time.Now()

	// disabled cooldown, singular
	single := []OverdueProject{{ID: "a", Name: "a", Count: 1, HasLastRun: false, MinInterval: 0}}
	text, ok := FormatOverdueMaintenance(single, now)
	if !ok {
		t.Fatalf("FormatOverdueMaintenance returned not ok")
	}
	if !contains(text, "**Maintenance overdue:** 1 project") {
		t.Fatalf("missing singular header: %q", text)
	}
	if !contains(text, "is due for lifecycle maintenance (lifecycle cooldown disabled)") {
		t.Fatalf("missing singular disabled wording: %q", text)
	}
	if contains(text, "has not had") || contains(text, "have not had") {
		t.Fatalf("disabled-cooldown header must not claim unmaintained: %q", text)
	}

	// disabled cooldown, singular, HasLastRun:true
	singleRun := []OverdueProject{{ID: "a", Name: "a", Count: 1, HasLastRun: true, MinInterval: 0, LastRun: now}}
	text, ok = FormatOverdueMaintenance(singleRun, now)
	if !ok {
		t.Fatalf("FormatOverdueMaintenance returned not ok")
	}
	if contains(text, "has not had") || contains(text, "have not had") {
		t.Fatalf("disabled-cooldown header must not claim unmaintained: %q", text)
	}

	// disabled cooldown, plural
	plural := []OverdueProject{
		{ID: "a", Name: "a", Count: 1, HasLastRun: false, MinInterval: 0},
		{ID: "b", Name: "b", Count: 2, HasLastRun: false, MinInterval: 0},
	}
	text, ok = FormatOverdueMaintenance(plural, now)
	if !ok {
		t.Fatalf("FormatOverdueMaintenance returned not ok")
	}
	if !contains(text, "**Maintenance overdue:** 2 projects") {
		t.Fatalf("missing plural header: %q", text)
	}
	if !contains(text, "are due for lifecycle maintenance (lifecycle cooldown disabled)") {
		t.Fatalf("missing plural disabled wording: %q", text)
	}
	if contains(text, "has not had") || contains(text, "have not had") {
		t.Fatalf("disabled-cooldown header must not claim unmaintained: %q", text)
	}

	// interval, singular
	single2 := []OverdueProject{{ID: "a", Name: "a", Count: 1, HasLastRun: false, MinInterval: 30 * time.Minute}}
	text, ok = FormatOverdueMaintenance(single2, now)
	if !ok {
		t.Fatalf("FormatOverdueMaintenance returned not ok")
	}
	if !contains(text, "has not had lifecycle maintenance within 30m0s") {
		t.Fatalf("missing singular interval wording: %q", text)
	}

	// interval, plural
	plural2 := []OverdueProject{
		{ID: "a", Name: "a", Count: 1, HasLastRun: false, MinInterval: 30 * time.Minute},
		{ID: "b", Name: "b", Count: 2, HasLastRun: false, MinInterval: 30 * time.Minute},
	}
	text, ok = FormatOverdueMaintenance(plural2, now)
	if !ok {
		t.Fatalf("FormatOverdueMaintenance returned not ok")
	}
	if !contains(text, "have not had lifecycle maintenance within 30m0s") {
		t.Fatalf("missing plural interval wording: %q", text)
	}
}
