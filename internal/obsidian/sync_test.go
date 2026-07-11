package obsidian

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wcatz/ghost/internal/memory"
)

func TestSyncDetectsChange(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "ghost.db")

	writeDB, err := memory.OpenDB(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	writeStore := memory.NewStore(writeDB, logger)
	defer writeStore.Close() //nolint:errcheck
	ctx := context.Background()
	if err := writeStore.EnsureProject(ctx, "p1", "/tmp/p1", "p1"); err != nil {
		t.Fatal(err)
	}
	if _, err := writeStore.Create(ctx, "p1", memory.Memory{Category: "fact", Content: "first memory", Importance: 0.7, Source: "mcp"}); err != nil {
		t.Fatal(err)
	}

	readDB, err := memory.OpenDB(dbPath) // second connection: sees writeDB's commits as data_version bumps
	if err != nil {
		t.Fatal(err)
	}
	readStore := memory.NewStore(readDB, logger)
	defer readStore.Close() //nolint:errcheck

	vault := filepath.Join(dir, "vault")
	ex := &Exporter{Store: readStore, Logger: logger}

	syncCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- Sync(syncCtx, ex, readDB, vault, "", 50*time.Millisecond) }()

	// Initial export happens immediately.
	waitFor(t, func() bool {
		m, _ := filepath.Glob(filepath.Join(vault, "p1", "Memories", "*.md"))
		return len(m) == 1
	})
	// A write from the other connection is picked up within a few ticks.
	if _, err := writeStore.Create(ctx, "p1", memory.Memory{Category: "fact", Content: "second memory", Importance: 0.7, Source: "mcp"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		m, _ := filepath.Glob(filepath.Join(vault, "p1", "Memories", "*.md"))
		return len(m) == 2
	})
	cancel()
	if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("sync returned: %v", err)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("condition not met within 5s")
}
