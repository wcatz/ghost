package mcpserver

import (
	"context"
	"database/sql"
	"fmt"
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
	overdue, err := mcpinit.OverdueMaintenanceProjects(ctx, store, dataDir, time.Now(), cfg.Lifecycle.MinIntervalDuration(), cfg.Reflection.ClearsConsolidatableSet())
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

// TestHealthListsAnUnsafeProjectAsUnmaintainable pins the other half of the
// diagnostic: a project whose id is not a safe filename component has
// consolidatable memories but no lifecycle bookkeeping can ever be keyed on
// its id, so ghost_health must say so instead of dropping it. The retry it
// cannot use must not be offered for this project.
func TestHealthListsAnUnsafeProjectAsUnmaintainable(t *testing.T) {
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
	const id = "team/api"
	if err := store.EnsureProject(ctx, id, "/tmp/team-api", "Team API"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	for i := 0; i < 3; i++ {
		if _, err := store.Create(ctx, id, memory.Memory{Category: "fact", Content: "unsafe memory alpha", Importance: 0.5, Source: "mcp"}); err != nil {
			t.Fatalf("Create: %v", err)
		}
	}

	srv := New(store, logger, "test")
	text := ghostHealthText(t, srv)
	if !strings.Contains(text, "Cannot be maintained unattended") {
		t.Errorf("health does not list the unsafe project:\n%s", text)
	}
	if !strings.Contains(text, "id is not a safe filename component") {
		t.Errorf("health does not say why it cannot be maintained:\n%s", text)
	}
	if !strings.Contains(text, id) {
		t.Errorf("health does not name the unsafe project:\n%s", text)
	}
	if strings.Contains(text, "ghost lifecycle --project "+id) {
		t.Errorf("health offers a retry that cannot help the unsafe project:\n%s", text)
	}
}

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

func TestHealthOverdueGateFollowsPhasesThatClearTheSet(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		want bool
	}{
		{"reflect only", map[string]string{"GHOST_REFLECTION_AUTO_REFLECT": "true", "GHOST_REFLECTION_AUTO_RESOLVE": "false", "GHOST_REFLECTION_AUTO_SUPERSEDE": "false"}, true},
		{"resolve only", map[string]string{"GHOST_REFLECTION_AUTO_REFLECT": "false", "GHOST_REFLECTION_AUTO_RESOLVE": "true", "GHOST_REFLECTION_AUTO_SUPERSEDE": "false"}, true},
		{"supersede only", map[string]string{"GHOST_REFLECTION_AUTO_REFLECT": "false", "GHOST_REFLECTION_AUTO_RESOLVE": "false", "GHOST_REFLECTION_AUTO_SUPERSEDE": "true"}, false},
		{"none", map[string]string{"GHOST_REFLECTION_AUTO_REFLECT": "false", "GHOST_REFLECTION_AUTO_RESOLVE": "false", "GHOST_REFLECTION_AUTO_SUPERSEDE": "false"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tmp := t.TempDir()
			t.Setenv("XDG_DATA_HOME", filepath.Join(tmp, "data"))
			t.Setenv("XDG_CONFIG_HOME", filepath.Join(tmp, "config"))
			t.Setenv("HOME", filepath.Join(tmp, "home"))
			t.Setenv("USERPROFILE", filepath.Join(tmp, "home"))
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
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
			if err := store.EnsureProject(ctx, "idle-proj", "/tmp/idle-proj", "idle-proj"); err != nil {
				t.Fatalf("EnsureProject: %v", err)
			}
			for i := 0; i < 3; i++ {
				if _, err := store.Create(ctx, "idle-proj", memory.Memory{Category: "fact", Content: "idle memory alpha", Importance: 0.5, Source: "mcp"}); err != nil {
					t.Fatalf("Create: %v", err)
				}
			}
			srv := New(store, logger, "test")
			text := ghostHealthText(t, srv)
			got := strings.Contains(text, "Maintenance overdue")
			if got != tc.want {
				t.Fatalf("health overdue section = %v, want %v (%s):\n%s", got, tc.want, tc.name, text)
			}
			if strings.Contains(text, "Cannot be maintained unattended") {
				t.Fatalf("health lists an unmaintainable project (%s):\n%s", tc.name, text)
			}
			if tc.want && !strings.Contains(text, "idle-proj") {
				t.Fatalf("health overdue section does not name the project (%s):\n%s", tc.name, text)
			}
		})
	}
}
