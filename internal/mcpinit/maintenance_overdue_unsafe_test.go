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

// seedUnsafeOverdueStore creates a store holding one project whose id is a
// safe filename component and one whose id is path-shaped, each with the same
// number of consolidatable memories.
func seedUnsafeOverdueStore(t *testing.T) (*memory.Store, string) {
	t.Helper()
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
	for _, p := range []struct{ id, path, name string }{
		{"team/api", "/tmp/team-api", "Team API"},
		{"okproj", "/tmp/okproj", "okproj"},
	} {
		if err := store.EnsureProject(ctx, p.id, p.path, p.name); err != nil {
			t.Fatalf("EnsureProject %q: %v", p.id, err)
		}
	}
	for i := 0; i < 3; i++ {
		for _, id := range []string{"team/api", "okproj"} {
			if _, err := store.Create(ctx, id, memory.Memory{
				Category:   "fact",
				Content:    fmt.Sprintf("%s consolidatable %d", id, i),
				Importance: 0.5,
				Source:     "mcp",
			}); err != nil {
				t.Fatalf("Create %q: %v", id, err)
			}
		}
	}
	return store, dataDir
}

// TestOverdueMaintenanceListsUnsafeProjectID pins the fix for an id that is not
// a safe filename component: such a project has consolidatable memories but no
// lifecycle bookkeeping can ever be keyed on its id, so it used to be dropped
// from the diagnostic with no output at all. It is now listed separately from
// the overdue projects, because the retry that clears overdue work cannot help
// it, while the safe project keeps its retry line.
func TestOverdueMaintenanceListsUnsafeProjectID(t *testing.T) {
	store, dataDir := seedUnsafeOverdueStore(t)
	ctx := context.Background()
	now := time.Now()

	projects, err := OverdueMaintenanceProjects(ctx, store, dataDir, now, 30*time.Minute, true)
	if err != nil {
		t.Fatalf("OverdueMaintenanceProjects: %v", err)
	}
	foundUnsafe, foundSafe := false, false
	for _, p := range projects {
		switch p.ID {
		case "team/api":
			foundUnsafe = true
			if !p.UnsafeID {
				t.Errorf("team/api listed without UnsafeID")
			}
			if p.Count != 3 {
				t.Errorf("team/api count = %d, want 3", p.Count)
			}
		case "okproj":
			foundSafe = true
			if p.UnsafeID {
				t.Errorf("okproj marked unsafe")
			}
		}
	}
	if !foundUnsafe {
		t.Fatalf("an unsafe project id with consolidatable memories was dropped: %v", projects)
	}
	if !foundSafe {
		t.Fatalf("the safe project disappeared from the overdue list: %v", projects)
	}

	text, ok := FormatOverdueMaintenance(projects, now)
	if !ok {
		t.Fatalf("FormatOverdueMaintenance returned not ok")
	}
	if !strings.Contains(text, "Maintenance overdue") || !strings.Contains(text, "ghost lifecycle --project") {
		t.Errorf("the safe project lost its overdue retry:\n%s", text)
	}
	if !strings.Contains(text, "Cannot be maintained unattended") {
		t.Errorf("the unsafe project is not reported as unmaintainable:\n%s", text)
	}
	if !strings.Contains(text, "id is not a safe filename component") {
		t.Errorf("the unmaintainable line does not say why:\n%s", text)
	}
	if !strings.Contains(text, "team/api") {
		t.Errorf("the unmaintainable line does not name the project:\n%s", text)
	}
	if strings.Contains(text, "ghost lifecycle --project team/api") {
		t.Errorf("the unsafe project is offered a retry that cannot help it:\n%s", text)
	}

	var status bytes.Buffer
	ReportOverdueMaintenance(&status, projects, now)
	out := status.String()
	if !strings.Contains(out, "! maintenance overdue:") {
		t.Errorf("status lost the overdue line:\n%s", out)
	}
	if !strings.Contains(out, "! cannot be maintained unattended:") {
		t.Errorf("status does not report the unmaintainable project:\n%s", out)
	}
	if !strings.Contains(out, "id is not a safe filename component") {
		t.Errorf("status does not say why the project cannot be maintained:\n%s", out)
	}
	if !strings.Contains(out, "team/api") {
		t.Errorf("status does not name the unsafe project:\n%s", out)
	}
}

// TestOverdueMaintenanceListsOnlyUnsafeProject covers the unsafe-only shape:
// the status path must still print a line, and Format must not invent an empty
// overdue section.
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
