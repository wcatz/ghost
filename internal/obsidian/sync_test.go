package obsidian

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

// TestSyncRetriesFailedExport: a failed export must not advance the
// data_version baseline — the loop retries on the next tick without
// requiring another DB commit.
func TestSyncRetriesFailedExport(t *testing.T) {
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

	readDB, err := memory.OpenDB(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	readStore := memory.NewStore(readDB, logger)
	defer readStore.Close() //nolint:errcheck

	var buf logBuffer
	vault := filepath.Join(dir, "vault")
	ex := &Exporter{Store: readStore, Logger: slog.New(slog.NewTextHandler(&buf, nil))}

	syncCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- Sync(syncCtx, ex, readDB, vault, "", 50*time.Millisecond) }()

	waitFor(t, func() bool {
		m, _ := filepath.Glob(filepath.Join(vault, "p1", "Memories", "*.md"))
		return len(m) == 1
	})

	// Break the vault: ensureVault's ReadDir fails on an unreadable root.
	if err := os.Chmod(vault, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(vault, 0o755) }) // let TempDir removal succeed on failure paths

	// A commit from the other connection triggers an export that fails.
	if _, err := writeStore.Create(ctx, "p1", memory.Memory{Category: "fact", Content: "second memory", Importance: 0.7, Source: "mcp"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return strings.Contains(buf.String(), "export failed") })

	// Fix the vault. NO further commits: only a retained baseline retries.
	if err := os.Chmod(vault, 0o755); err != nil {
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

// logBuffer is a goroutine-safe io.Writer for capturing slog output from the
// Sync goroutine while the test polls it.
type logBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	// Generous deadline: these tests drive a real 50ms ticker, and under
	// `go test -race ./...` scheduler contention can delay a tick well past a
	// tight bound. A passing condition is met in well under a second; the
	// headroom only guards against false failures under load.
	const deadline = 30 * time.Second
	end := time.Now().Add(deadline)
	for time.Now().Before(end) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("condition not met within %s", deadline)
}
