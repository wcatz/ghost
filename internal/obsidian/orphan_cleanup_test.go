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

func TestOrphanCleanupDoesNotDeleteConcurrentReplacement(t *testing.T) {
	root := filepath.Join(t.TempDir(), "vault")
	if err := ensureVault(root); err != nil {
		t.Fatalf("ensureVault: %v", err)
	}
	orphan := filepath.Join(root, "oldproject")
	mustMkdirAll(t, orphan)
	target := filepath.Join(orphan, "Managed Note.md")
	mustWrite(t, target, ghostNote)

	pruneBeforeRemoveFn.Store(func(path string) {
		if path != target {
			return
		}
		replacement := path + ".replacement"
		if err := os.WriteFile(replacement, []byte(userNote), 0o600); err != nil {
			t.Fatalf("write replacement: %v", err)
		}
		if err := os.Rename(replacement, path); err != nil {
			t.Fatalf("replace target: %v", err)
		}
	})
	t.Cleanup(func() { pruneBeforeRemoveFn.Store(func(string) {}) })

	if err := prune(root, nil, map[string]string{}, []string{"liveproject"}); err != nil {
		t.Fatalf("prune: %v", err)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("replacement was deleted: %v", err)
	}
	if string(got) != userNote {
		t.Fatalf("replacement content = %q, want user note preserved", got)
	}
}

func TestManagedPruneDoesNotDeleteConcurrentReplacement(t *testing.T) {
	root := filepath.Join(t.TempDir(), "vault")
	if err := ensureVault(root); err != nil {
		t.Fatalf("ensureVault: %v", err)
	}
	dir := filepath.Join(root, "live", "Memories")
	mustMkdirAll(t, dir)
	target := filepath.Join(dir, "Managed Note.md")
	mustWrite(t, target, ghostNote)

	pruneBeforeRemoveFn.Store(func(path string) {
		if path != target {
			return
		}
		replacement := path + ".replacement"
		if err := os.WriteFile(replacement, []byte(userNote), 0o600); err != nil {
			t.Fatalf("write replacement: %v", err)
		}
		if err := os.Rename(replacement, path); err != nil {
			t.Fatalf("replace target: %v", err)
		}
	})
	t.Cleanup(func() { pruneBeforeRemoveFn.Store(func(string) {}) })

	if err := prune(root, []string{"live"}, map[string]string{}, []string{"live"}); err != nil {
		t.Fatalf("prune: %v", err)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("replacement was deleted: %v", err)
	}
	if string(got) != userNote {
		t.Fatalf("replacement content = %q, want user note preserved", got)
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

// TestOrphanCleanupKeepsUserEmptySubdir: emptiness alone is not permission
// to delete. An empty folder the user created beside a Ghost note is
// indistinguishable from a stale Ghost folder by emptiness, so the sweep
// only offers directories that actually held a deleted Ghost file — and the
// folders above them only while they close up.
func TestOrphanCleanupKeepsUserEmptySubdir(t *testing.T) {
	root := filepath.Join(t.TempDir(), "vault")
	if err := ensureVault(root); err != nil {
		t.Fatalf("ensureVault: %v", err)
	}

	orphan := filepath.Join(root, "oldproject")
	mustMkdirAll(t, orphan)
	mustWrite(t, filepath.Join(orphan, "Managed Note.md"), ghostNote)
	scratch := filepath.Join(orphan, "Scratch") // user-created and empty
	mustMkdirAll(t, scratch)

	if err := prune(root, nil, map[string]string{}, []string{"liveproject"}); err != nil {
		t.Fatalf("prune: %v", err)
	}
	if _, err := os.Stat(filepath.Join(orphan, "Managed Note.md")); !os.IsNotExist(err) {
		t.Errorf("Ghost-managed note survived the orphan sweep (err=%v)", err)
	}
	if _, err := os.Stat(scratch); err != nil {
		t.Errorf("user-created empty dir was deleted by the orphan sweep: %v", err)
	}
	if _, err := os.Stat(orphan); err != nil {
		t.Errorf("orphan folder was removed while it still holds the user's empty dir: %v", err)
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

// TestOrphanCleanupKeepsUserSymlink: the walk used to open any .md and
// remove whatever carried a ghost_id — including a user's own symlink
// ("shortcut.md" pointing at a Ghost note) whose link itself was then
// unlinked. Only regular files are Ghost's to classify; a symlink is the
// user's own indirection and must survive. Both prune entry points are
// covered: the managed-subtree walk and the orphan sweep.
func TestOrphanCleanupKeepsUserSymlink(t *testing.T) {
	for _, mode := range []string{"orphan", "subtree"} {
		t.Run(mode, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "vault")
			if err := ensureVault(root); err != nil {
				t.Fatalf("ensureVault: %v", err)
			}
			var dir string
			if mode == "orphan" {
				dir = filepath.Join(root, "oldproject")
			} else {
				dir = filepath.Join(root, "live", "Memories")
			}
			mustMkdirAll(t, dir)
			mustWrite(t, filepath.Join(dir, "Managed Note.md"), ghostNote)

			target := filepath.Join(t.TempDir(), "target.md")
			mustWrite(t, target, ghostNote)
			link := filepath.Join(dir, "shortcut.md")
			if err := os.Symlink(target, link); err != nil {
				t.Skipf("symlinks unavailable: %v", err)
			}

			subtrees, known := []string{"live"}, []string{"live"}
			if mode == "orphan" {
				subtrees, known = nil, []string{"live"}
			}
			if err := prune(root, subtrees, map[string]string{}, known); err != nil {
				t.Fatalf("prune: %v", err)
			}
			if _, err := os.Lstat(link); err != nil {
				t.Errorf("user symlink was deleted by the prune: %v", err)
			}
			if _, err := os.Stat(target); err != nil {
				t.Errorf("the symlink's target was removed: %v", err)
			}
			if _, err := os.Stat(filepath.Join(dir, "Managed Note.md")); !os.IsNotExist(err) {
				t.Errorf("the real Ghost note should still be pruned (err=%v)", err)
			}
		})
	}
}

// TestPruneSkipsSpecialFiles: a named pipe named *.md reaches os.Open
// through the walk, and opening a FIFO blocks forever — a one-shot export
// or the sync loop hangs on it. Only regular files may be opened.
func TestPruneSkipsSpecialFiles(t *testing.T) {
	for _, mode := range []string{"orphan", "subtree"} {
		t.Run(mode, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "vault")
			if err := ensureVault(root); err != nil {
				t.Fatalf("ensureVault: %v", err)
			}
			var dir string
			if mode == "orphan" {
				dir = filepath.Join(root, "oldproject")
			} else {
				dir = filepath.Join(root, "live", "Memories")
			}
			mustMkdirAll(t, dir)
			mustWrite(t, filepath.Join(dir, "Managed Note.md"), ghostNote)
			pipe := filepath.Join(dir, "pipe.md")
			if err := makeFIFO(pipe); err != nil {
				t.Skipf("named pipe unavailable: %v", err)
			}

			subtrees, known := []string{"live"}, []string{"live"}
			if mode == "orphan" {
				subtrees, known = nil, []string{"live"}
			}
			done := make(chan error, 1)
			go func() { done <- prune(root, subtrees, map[string]string{}, known) }()
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("prune: %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("prune blocked for 5s on a FIFO — os.Open on a named pipe never returns")
			}
			if _, err := os.Lstat(pipe); err != nil {
				t.Errorf("the FIFO was removed: %v", err)
			}
			if _, err := os.Stat(filepath.Join(dir, "Managed Note.md")); !os.IsNotExist(err) {
				t.Errorf("the real Ghost note should still be pruned (err=%v)", err)
			}
		})
	}
}

// TestPruneOrphanFolderReportsRemoveError: the directory cleanup used to
// swallow every os.Remove error, so a permission or sharing failure left
// an empty stale directory while prune reported success, and the next
// export retried the same removal forever. Only "not empty" (the guard
// doing its job) and "already gone" may pass silently.
func TestPruneOrphanFolderReportsRemoveError(t *testing.T) {
	root := filepath.Join(t.TempDir(), "vault")
	if err := ensureVault(root); err != nil {
		t.Fatalf("ensureVault: %v", err)
	}
	orphan := filepath.Join(root, "oldproject")
	mustMkdirAll(t, orphan)
	mustWrite(t, filepath.Join(orphan, "Managed Note.md"), ghostNote)

	wantErr := errors.New("injected: directory removal failed")
	removeDirFn.Store(func(string) error { return wantErr })
	t.Cleanup(func() { removeDirFn.Store(os.Remove) })

	if err := prune(root, nil, map[string]string{}, []string{"liveproject"}); !errors.Is(err, wantErr) {
		t.Errorf("prune error = %v, want it to wrap %v", err, wantErr)
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
