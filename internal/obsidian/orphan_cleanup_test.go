package obsidian

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wcatz/ghost/internal/memory"
)

const ghostNote = "---\nghost_id: abc123\ntitle: Managed by Ghost\n---\nghost body\n"
const userNote = "# My own notes\n\nI wrote this myself and Ghost has no claim on it.\n"

// TestOrphanCleanupKeepsUserFiles is the data-loss defect (issue #550).
//
// The orphan sweep ran os.RemoveAll over a whole top-level vault directory
// whenever it held at least one ghost_id note. A user's own note or
// attachment sitting beside that Ghost file went with it, with no undo —
// and the design spec is explicit that "User-created files are never
// touched" (2026-07-10 obsidian-vault-mirror-design.md).
//
// Triggers are ordinary: a project rename or merge changes the folder set,
// and any user folder that happens to hold a copied Ghost note is
// indistinguishable from an orphaned project folder by name alone.
func TestOrphanCleanupKeepsUserFiles(t *testing.T) {
	root := filepath.Join(t.TempDir(), "vault")
	if err := ensureVault(root); err != nil {
		t.Fatalf("ensureVault: %v", err)
	}

	orphan := filepath.Join(root, "oldproject")
	mustMkdirAll(t, orphan)
	mustWrite(t, filepath.Join(orphan, "Managed Note.md"), ghostNote)
	mustWrite(t, filepath.Join(orphan, "My Notes.md"), userNote)
	mustWrite(t, filepath.Join(orphan, "diagram.png"), "PNGDATA")

	// oldproject is not in the known set, so it is an orphan.
	if err := prune(root, nil, map[string]string{}, []string{"liveproject"}); err != nil {
		t.Fatalf("prune: %v", err)
	}

	if _, err := os.Stat(filepath.Join(orphan, "Managed Note.md")); !os.IsNotExist(err) {
		t.Errorf("Ghost-managed note survived the orphan sweep (err=%v) — stale Ghost content is exactly what this sweep is for", err)
	}
	for _, keep := range []string{"My Notes.md", "diagram.png"} {
		if _, err := os.Stat(filepath.Join(orphan, keep)); err != nil {
			t.Errorf("user's %q was deleted by the orphan sweep: %v", keep, err)
		}
	}
	if _, err := os.Stat(orphan); err != nil {
		t.Errorf("orphan folder itself was removed while it still holds user files: %v", err)
	}
}

// TestOrphanCleanupRemovesFullyGhostFolder keeps the sweep useful. A folder
// holding nothing but Ghost's own notes is dead weight after the project
// goes, and leaving every one of them behind would trade one silent loss for
// unbounded accumulation.
func TestOrphanCleanupRemovesFullyGhostFolder(t *testing.T) {
	root := filepath.Join(t.TempDir(), "vault")
	if err := ensureVault(root); err != nil {
		t.Fatalf("ensureVault: %v", err)
	}

	orphan := filepath.Join(root, "oldproject")
	nested := filepath.Join(orphan, "Memories")
	mustMkdirAll(t, nested)
	mustWrite(t, filepath.Join(nested, "Managed Note.md"), ghostNote)

	if err := prune(root, nil, map[string]string{}, []string{"liveproject"}); err != nil {
		t.Fatalf("prune: %v", err)
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Errorf("an orphan folder containing only Ghost files was left behind (err=%v)", err)
	}
}

// TestOrphanCleanupIgnoresUserOnlyFolder: a folder with no Ghost content is
// not this sweep's business at all, whatever it is called.
func TestOrphanCleanupIgnoresUserOnlyFolder(t *testing.T) {
	root := filepath.Join(t.TempDir(), "vault")
	if err := ensureVault(root); err != nil {
		t.Fatalf("ensureVault: %v", err)
	}

	folder := filepath.Join(root, "notes")
	mustMkdirAll(t, folder)
	mustWrite(t, filepath.Join(folder, "anything.md"), "# just notes\n")

	if err := prune(root, nil, map[string]string{}, []string{"liveproject"}); err != nil {
		t.Fatalf("prune: %v", err)
	}
	if _, err := os.Stat(filepath.Join(folder, "anything.md")); err != nil {
		t.Errorf("a folder with no Ghost content was touched: %v", err)
	}
}

// TestVaultIsNotWorldReadable: the vault holds whatever the mirror holds and
// was written 0755/0644 — readable by every account on the machine.
func TestVaultIsNotWorldReadable(t *testing.T) {
	root := filepath.Join(t.TempDir(), "vault")
	if err := ensureVault(root); err != nil {
		t.Fatalf("ensureVault: %v", err)
	}
	if st, err := os.Stat(root); err != nil {
		t.Fatalf("stat vault: %v", err)
	} else if st.Mode().Perm() != 0o700 {
		t.Errorf("vault dir mode = %04o, want 0700", st.Mode().Perm())
	}
	if st, err := os.Stat(filepath.Join(root, markerName)); err != nil {
		t.Fatalf("stat marker: %v", err)
	} else if st.Mode().Perm() != 0o600 {
		t.Errorf("marker mode = %04o, want 0600", st.Mode().Perm())
	}

	// A written note exercises both the nested directory and the file itself.
	note := filepath.Join(root, "proj", "Notes", "n.md")
	if _, err := writeIfChanged(note, "hello\n"); err != nil {
		t.Fatalf("writeIfChanged: %v", err)
	}
	// Chmod sets the mode outright, bypassing umask — so this assertion
	// fails deterministically whatever the environment's umask happens to be.
	if st, err := os.Stat(note); err != nil {
		t.Fatalf("stat note: %v", err)
	} else if st.Mode().Perm() != 0o600 {
		t.Errorf("note mode = %04o, want 0600", st.Mode().Perm())
	}
	if st, err := os.Stat(filepath.Dir(note)); err != nil {
		t.Fatalf("stat note dir: %v", err)
	} else if st.Mode().Perm() != 0o700 {
		t.Errorf("note dir mode = %04o, want 0700", st.Mode().Perm())
	}
}

// TestSyncRetriesFailedInitialExport: Sync reads data_version BEFORE the
// first export, so when that export fails the baseline already equals the
// database's current value. Every later tick then sees v == last and skips —
// while the code logs "initial export failed, will retry next tick". The
// retry it promises never happens, and the export stays broken until some
// unrelated commit moves the counter.
//
// TestSyncRetriesFailedExport covers the tick path, where the baseline is
// legitimately retained; this is the startup path, where nothing was ever
// written to retry from.
func TestSyncRetriesFailedInitialExport(t *testing.T) {
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

	// Break the vault BEFORE Sync starts, so the initial export is the one
	// that fails. Same ReadDir seam the tick-path test uses — a chmod would
	// be unreliable across CI privilege levels.
	readDirFn.Store(func(string) ([]os.DirEntry, error) {
		return nil, errors.New("injected: unreadable vault")
	})
	t.Cleanup(func() { readDirFn.Store(os.ReadDir) })

	syncCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- Sync(syncCtx, ex, readDB, vault, "", 50*time.Millisecond) }()

	waitForDebug(t, "initial export failure log", func() bool {
		return strings.Contains(buf.String(), "initial export failed")
	}, func() string { return buf.String() })

	// Repair the vault. NO further commits: if the loop retries, it is
	// because a failed export does not advance the baseline, not because
	// something happened to the database.
	readDirFn.Store(os.ReadDir)

	waitFor(t, func() bool {
		m, _ := filepath.Glob(filepath.Join(vault, "p1", "Memories", "*.md"))
		return len(m) == 1
	})

	cancel()
	if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("sync returned: %v", err)
	}
}
