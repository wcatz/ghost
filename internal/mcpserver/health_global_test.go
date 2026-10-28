package mcpserver

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// TestHealthReportsGlobal seeds a sandbox store with _global near-duplicate
// reflection rows and asserts ghost_health renders the _global section with the
// right counts and the consolidate-global pointer. It also asserts the pointer
// is absent when _global has nothing to consolidate.
func TestHealthReportsGlobal(t *testing.T) {
	t.Run("reports consolidatable _global rows and clusters", func(t *testing.T) {
		store, _ := testStoreWithPath(t)
		logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
		srv := New(store, logger, "test")

		ctx := context.Background()
		if err := store.EnsureProject(ctx, memory.GlobalProjectID, memory.GlobalProjectID, "global"); err != nil {
			t.Fatalf("EnsureProject _global: %v", err)
		}
		if _, err := store.Create(ctx, memory.GlobalProjectID, memory.Memory{
			Category:   "preference",
			Content:    "always run go vet before committing any change",
			Importance: 0.9,
			Source:     "reflection",
		}); err != nil {
			t.Fatalf("create global1: %v", err)
		}
		if _, err := store.Create(ctx, memory.GlobalProjectID, memory.Memory{
			Category:   "preference",
			Content:    "always run go vet before committing a change",
			Importance: 0.5,
			Source:     "reflection",
		}); err != nil {
			t.Fatalf("create global2: %v", err)
		}

		text := ghostHealthText(t, srv)
		if !strings.Contains(text, "**_global:**") {
			t.Fatalf("ghost_health missing _global section:\n%s", text)
		}
		if !strings.Contains(text, "2 memories, 2 reflection-written consolidatable, 1 near-duplicate cluster(s) covering 2 rows") {
			t.Errorf("ghost_health _global counts wrong:\n%s", text)
		}
		if !strings.Contains(text, "ghost maintenance consolidate-global --apply") {
			t.Errorf("ghost_health _global missing consolidate-global pointer:\n%s", text)
		}
		if !strings.Contains(text, "⚠ 1 cluster(s)") {
			t.Errorf("ghost_health _global missing warning line:\n%s", text)
		}
	})

	t.Run("absent when _global has nothing to consolidate", func(t *testing.T) {
		store, _ := testStoreWithPath(t)
		logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
		srv := New(store, logger, "test")

		ctx := context.Background()
		if err := store.EnsureProject(ctx, memory.GlobalProjectID, memory.GlobalProjectID, "global"); err != nil {
			t.Fatalf("EnsureProject _global: %v", err)
		}
		// No reflection rows — _global has nothing consolidatable.
		text := ghostHealthText(t, srv)
		if !strings.Contains(text, "**_global:**") {
			t.Fatalf("ghost_health missing _global section when empty:\n%s", text)
		}
		if !strings.Contains(text, "0 reflection-written consolidatable") {
			t.Errorf("ghost_health should report 0 reflection-written consolidatable when empty:\n%s", text)
		}
		if strings.Contains(text, "consolidate-global --apply") {
			t.Errorf("ghost_health should not show consolidate pointer when nothing to consolidate:\n%s", text)
		}
		if strings.Contains(text, "⚠") && strings.Contains(text, "_global") {
			// The warning must not appear for an empty _global.
			if strings.Contains(text, "cluster(s) — run `ghost maintenance consolidate-global") {
				t.Errorf("ghost_health shows warning when nothing to fold:\n%s", text)
			}
		}
	})
}

func TestHealthGlobalConsolidatePointerOnce(t *testing.T) {
	t.Run("_global with pairs shows pointer once in health", func(t *testing.T) {
		store, _ := testStoreWithPath(t)
		logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
		srv := New(store, logger, "test")

		ctx := context.Background()
		if err := store.EnsureProject(ctx, memory.GlobalProjectID, memory.GlobalProjectID, "global"); err != nil {
			t.Fatalf("EnsureProject _global: %v", err)
		}
		if _, err := store.Create(ctx, memory.GlobalProjectID, memory.Memory{
			Category:   "preference",
			Content:    "always run go vet before committing any change",
			Importance: 0.9,
			Source:     "reflection",
		}); err != nil {
			t.Fatalf("create global1: %v", err)
		}
		if _, err := store.Create(ctx, memory.GlobalProjectID, memory.Memory{
			Category:   "preference",
			Content:    "always run go vet before committing a change",
			Importance: 0.5,
			Source:     "reflection",
		}); err != nil {
			t.Fatalf("create global2: %v", err)
		}

		text := ghostHealthText(t, srv)
		count := strings.Count(text, "ghost maintenance consolidate-global --apply")
		if count != 1 {
			t.Fatalf("expected consolidate-global pointer exactly once, got %d:\n%s", count, text)
		}
	})

	t.Run("empty _global shows pointer zero times", func(t *testing.T) {
		store, _ := testStoreWithPath(t)
		logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
		srv := New(store, logger, "test")

		ctx := context.Background()
		if err := store.EnsureProject(ctx, memory.GlobalProjectID, memory.GlobalProjectID, "global"); err != nil {
			t.Fatalf("EnsureProject _global: %v", err)
		}
		text := ghostHealthText(t, srv)
		count := strings.Count(text, "ghost maintenance consolidate-global --apply")
		if count != 0 {
			t.Fatalf("expected consolidate-global pointer zero times for empty _global, got %d:\n%s", count, text)
		}
	})
}

func TestHealthGlobalMCPSourceDoesNotContradictNearDuplicates(t *testing.T) {
	store, _ := testStoreWithPath(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	srv := New(store, logger, "test")

	ctx := context.Background()
	if err := store.EnsureProject(ctx, memory.GlobalProjectID, memory.GlobalProjectID, "global"); err != nil {
		t.Fatalf("EnsureProject _global: %v", err)
	}
	if _, err := store.Create(ctx, memory.GlobalProjectID, memory.Memory{
		Category:   "preference",
		Content:    "always run go vet before committing any change",
		Importance: 0.9,
		Source:     "mcp",
	}); err != nil {
		t.Fatalf("create global1: %v", err)
	}
	if _, err := store.Create(ctx, memory.GlobalProjectID, memory.Memory{
		Category:   "preference",
		Content:    "always run go vet before committing a change",
		Importance: 0.5,
		Source:     "mcp",
	}); err != nil {
		t.Fatalf("create global2: %v", err)
	}

	text := ghostHealthText(t, srv)
	if !strings.Contains(text, "**Near duplicates:**") {
		t.Fatalf("ghost_health missing Near duplicates section:\n%s", text)
	}
	// Near duplicates must list the _global pair
	if !strings.Contains(text, "_global") || !strings.Contains(text, "pair") {
		t.Fatalf("ghost_health missing _global pair in Near duplicates:\n%s", text)
	}
	// The _global consolidation line must be present and labelled reflection-written
	if !strings.Contains(text, "**_global:**") {
		t.Fatalf("ghost_health missing _global section:\n%s", text)
	}
	if !strings.Contains(text, "reflection-written consolidatable") {
		t.Fatalf("ghost_health _global line not labelled reflection-written:\n%s", text)
	}
	if strings.Contains(text, "no near-duplicate clusters") {
		t.Fatalf("ghost_health must not claim no near-duplicate clusters when Near duplicates lists _global pairs:\n%s", text)
	}
}
