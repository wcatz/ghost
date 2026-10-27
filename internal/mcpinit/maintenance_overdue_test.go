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

func TestOverdueMaintenanceProjects_DisabledCooldown(t *testing.T) {
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
	if err := store.EnsureProject(ctx, "proj", "/tmp/proj", "proj"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	for i := 0; i < 2; i++ {
		if _, err := store.Create(ctx, "proj", memory.Memory{Category: "fact", Content: fmt.Sprintf("mem %d", i), Importance: 0.5, Source: "mcp"}); err != nil {
			t.Fatalf("create: %v", err)
		}
	}
	if err := TouchLifecycleStart("proj"); err != nil {
		t.Fatalf("TouchLifecycleStart: %v", err)
	}
	now := time.Now()
	overdue, err := OverdueMaintenanceProjects(ctx, store, dataDir, now, 0, true)
	if err != nil {
		t.Fatalf("OverdueMaintenanceProjects: %v", err)
	}
	found := false
	for _, o := range overdue {
		if o.ID == "proj" {
			found = true
			if o.MinInterval != 0 {
				t.Errorf("minInterval=%v want 0", o.MinInterval)
			}
		}
	}
	if !found {
		t.Fatalf("proj should be overdue with cooldown disabled, got %v", overdue)
	}
	text, ok := FormatOverdueMaintenance(overdue, now)
	if !ok {
		t.Fatalf("FormatOverdueMaintenance returned not ok")
	}
	if contains(text, "within 0s") {
		t.Errorf("should not render within 0s: %q", text)
	}
	if !contains(text, "lifecycle cooldown disabled") {
		t.Errorf("missing disabled cooldown text: %q", text)
	}
	if !contains(text, "has not had lifecycle maintenance") {
		t.Errorf("missing singular agreement: %q", text)
	}
	if contains(text, "have not had lifecycle maintenance") {
		t.Errorf("singular case must not use plural: %q", text)
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
	if !contains(text, "has not had lifecycle maintenance with the lifecycle cooldown disabled") {
		t.Fatalf("missing singular disabled wording: %q", text)
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
	if !contains(text, "have not had lifecycle maintenance with the lifecycle cooldown disabled") {
		t.Fatalf("missing plural disabled wording: %q", text)
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
