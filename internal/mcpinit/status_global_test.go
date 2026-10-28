package mcpinit

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// TestStatusReportsGlobal seeds a sandbox store with _global near-duplicate
// rows and asserts `ghost mcp status` (via reportGlobalConsolidation) renders
// the _global section with the right counts and the consolidate-global pointer.
// It also asserts the pointer is absent when _global has nothing to consolidate.
func TestStatusReportsGlobal(t *testing.T) {
	t.Run("reports consolidatable _global rows and clusters", func(t *testing.T) {
		statusEnv(t)
		root := t.TempDir()
		dataHome := filepath.Join(root, "data")
		dataDir := filepath.Join(dataHome, "ghost")
		dbPath := storeAt(t, dataDir)
		t.Setenv("XDG_DATA_HOME", dataHome)

		db, err := memory.OpenDB(dbPath)
		if err != nil {
			t.Fatalf("open db: %v", err)
		}
		store := memory.NewStore(db, slog.New(slog.NewTextHandler(io.Discard, nil)))
		ctx := context.Background()
		if err := store.EnsureProject(ctx, memory.GlobalProjectID, memory.GlobalProjectID, "global"); err != nil {
			_ = db.Close()
			t.Fatalf("EnsureProject _global: %v", err)
		}
		if _, err := store.Create(ctx, memory.GlobalProjectID, memory.Memory{
			Category:   "preference",
			Content:    "always run go vet before committing any change",
			Importance: 0.9,
			Source:     "reflection",
		}); err != nil {
			_ = db.Close()
			t.Fatalf("create global1: %v", err)
		}
		if _, err := store.Create(ctx, memory.GlobalProjectID, memory.Memory{
			Category:   "preference",
			Content:    "always run go vet before committing a change",
			Importance: 0.5,
			Source:     "reflection",
		}); err != nil {
			_ = db.Close()
			t.Fatalf("create global2: %v", err)
		}
		if err := db.Close(); err != nil {
			t.Fatalf("close db: %v", err)
		}

		db, err = memory.OpenReadDB(dbPath)
		if err != nil {
			t.Fatalf("open read db: %v", err)
		}
		store = memory.NewStore(db, slog.New(slog.NewTextHandler(io.Discard, nil)))
		defer store.Close() //nolint:errcheck

		var out bytes.Buffer
		reportGlobalConsolidation(&out, store)
		text := out.String()
		if !strings.Contains(text, "_global:") {
			t.Fatalf("missing _global section:\n%s", text)
		}
		if !strings.Contains(text, "2 memories, 2 reflection-written consolidatable, 1 near-duplicate cluster(s) covering 2 rows") {
			t.Errorf("wrong _global counts:\n%s", text)
		}
		if !strings.Contains(text, "ghost maintenance consolidate-global --apply") {
			t.Errorf("missing consolidate-global pointer:\n%s", text)
		}
		if !strings.Contains(text, "! _global:") {
			t.Errorf("expected ! line when clusters exist:\n%s", text)
		}
	})

	t.Run("absent when _global has nothing to consolidate", func(t *testing.T) {
		statusEnv(t)
		root := t.TempDir()
		dataHome := filepath.Join(root, "data")
		dataDir := filepath.Join(dataHome, "ghost")
		dbPath := storeAt(t, dataDir)
		t.Setenv("XDG_DATA_HOME", dataHome)

		db, err := memory.OpenDB(dbPath)
		if err != nil {
			t.Fatalf("open db: %v", err)
		}
		store := memory.NewStore(db, slog.New(slog.NewTextHandler(io.Discard, nil)))
		ctx := context.Background()
		if err := store.EnsureProject(ctx, memory.GlobalProjectID, memory.GlobalProjectID, "global"); err != nil {
			_ = db.Close()
			t.Fatalf("EnsureProject _global: %v", err)
		}
		// No reflection rows — nothing consolidatable.
		if err := db.Close(); err != nil {
			t.Fatalf("close db: %v", err)
		}

		db, err = memory.OpenReadDB(dbPath)
		if err != nil {
			t.Fatalf("open read db: %v", err)
		}
		store = memory.NewStore(db, slog.New(slog.NewTextHandler(io.Discard, nil)))
		defer store.Close() //nolint:errcheck

		var out bytes.Buffer
		reportGlobalConsolidation(&out, store)
		text := out.String()
		if !strings.Contains(text, "_global:") {
			t.Fatalf("missing _global section when empty:\n%s", text)
		}
		if !strings.Contains(text, "0 reflection-written consolidatable") {
			t.Errorf("should report 0 reflection-written consolidatable when empty:\n%s", text)
		}
		if strings.Contains(text, "consolidate-global --apply") {
			t.Errorf("should not show consolidate pointer when nothing to consolidate:\n%s", text)
		}
		if strings.Contains(text, "! _global:") {
			t.Errorf("should be - line, not !, when nothing to fold:\n%s", text)
		}
	})
}

func TestStatusGlobalMCPSourceDoesNotContradictNearDuplicates(t *testing.T) {
	statusEnv(t)
	root := t.TempDir()
	dataHome := filepath.Join(root, "data")
	dataDir := filepath.Join(dataHome, "ghost")
	dbPath := storeAt(t, dataDir)
	t.Setenv("XDG_DATA_HOME", dataHome)
	db, err := memory.OpenDB(dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	store := memory.NewStore(db, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx := context.Background()
	if err := store.EnsureProject(ctx, memory.GlobalProjectID, memory.GlobalProjectID, "global"); err != nil {
		_ = db.Close()
		t.Fatalf("EnsureProject _global: %v", err)
	}
	if _, err := store.Create(ctx, memory.GlobalProjectID, memory.Memory{
		Category:   "preference",
		Content:    "always run go vet before committing any change",
		Importance: 0.9,
		Source:     "mcp",
	}); err != nil {
		_ = db.Close()
		t.Fatalf("create global1: %v", err)
	}
	if _, err := store.Create(ctx, memory.GlobalProjectID, memory.Memory{
		Category:   "preference",
		Content:    "always run go vet before committing a change",
		Importance: 0.5,
		Source:     "mcp",
	}); err != nil {
		_ = db.Close()
		t.Fatalf("create global2: %v", err)
	}
	defer db.Close()
	var buf strings.Builder
	reportGlobalConsolidation(&buf, store)
	text := buf.String()
	if strings.Contains(text, "0 reflection-written consolidatable") && strings.Contains(text, "no near-duplicate clusters") {
		t.Fatalf("_global line contradicts Near duplicates logic:\n%s", text)
	}
}
