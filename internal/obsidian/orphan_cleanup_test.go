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

// TestHasGhostIDRefusesSpecialFiles covers the second half of the special-file
// guard. The walk's own d.Type() check is not enough, because hasGhostID
// reopens the pathname: between the entry being stat'ed as a regular file and
// the open, the name can be rebound to something else. os.Open on a FIFO then
// blocks forever, which is the export hang the guard exists to prevent — the
// check that was supposed to prevent it happened to the wrong object.
func TestHasGhostIDRefusesSpecialFiles(t *testing.T) {
	dir := t.TempDir()

	pipe := filepath.Join(dir, "note.md")
	if err := makeFIFO(pipe); err != nil {
		t.Skipf("named pipe unavailable: %v", err)
	}
	link := filepath.Join(dir, "link.md")
	if err := os.Symlink(pipe, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	real := filepath.Join(dir, "real.md")
	mustWrite(t, real, ghostNote)

	for _, tc := range []struct{ name, path string }{
		{"named pipe", pipe},
		{"symlink to a named pipe", link},
		{"directory", dir},
	} {
		t.Run(tc.name, func(t *testing.T) {
			done := make(chan struct{})
			var id string
			var ok bool
			go func() {
				defer close(done)
				id, ok = hasGhostID(tc.path)
			}()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("hasGhostID blocked for 5s on a special file — the open must not block on a FIFO")
			}
			if ok {
				t.Errorf("hasGhostID(%s) = (%q, true), want (false) — only a regular file is Ghost's to touch", tc.path, id)
			}
		})
	}

	// The control: the same call on a real note still reads the id, so the
	// special-file refusals above are not the function failing outright.
	if id, ok := hasGhostID(real); !ok || id != "abc123" {
		t.Errorf("hasGhostID on a regular Ghost note = (%q, %v), want (abc123, true)", id, ok)
	}
}

// TestRestoreDoesNotOverwriteConcurrentFile closes the restore race. Restoring
// checked that the original path was free and then renamed onto it, but on
// POSIX a rename replaces whatever is there: a file created in that window was
// unlinked and its contents lost, silently, by the code whose entire purpose is
// to avoid destroying files it did not inspect. The no-replace rename has to
// fail instead, and the quarantined object has to stay put for the next pass.
func TestRestoreDoesNotOverwriteConcurrentFile(t *testing.T) {
	root := filepath.Join(t.TempDir(), "vault")
	if err := ensureVault(root); err != nil {
		t.Fatalf("ensureVault: %v", err)
	}
	dir := filepath.Join(root, "oldproject")
	mustMkdirAll(t, dir)
	target := filepath.Join(dir, "Managed Note.md")
	mustWrite(t, target, ghostNote)

	// Model the concurrent writer landing between the quarantine and the
	// restore — the window the old Lstat-then-rename left open.
	pruneBeforeRemoveFn.Store(func(path string) {
		if path != target {
			return
		}
		mustWrite(t, path, userNote)
	})
	t.Cleanup(func() { pruneBeforeRemoveFn.Store(func(string) {}) })

	deleted, err := removeGhostFile(target)
	if err != nil {
		t.Fatalf("removeGhostFile: %v", err)
	}
	if deleted {
		t.Fatal("removeGhostFile deleted a file that was replaced, want it left alone")
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("the concurrent file was destroyed: %v", err)
	}
	if string(got) != userNote {
		t.Errorf("content = %q, want the concurrent writer's note intact", got)
	}
}

// TestRestoreDoesNotOverwriteConcurrentFileOnReplace covers the same race from
// the other side: the original path still exists when the restore runs, and the
// restore must not replace it.
func TestRestoreKeepsExistingFileWhenRestoring(t *testing.T) {
	root := filepath.Join(t.TempDir(), "vault")
	if err := ensureVault(root); err != nil {
		t.Fatalf("ensureVault: %v", err)
	}
	dir := filepath.Join(root, "oldproject")
	mustMkdirAll(t, dir)
	target := filepath.Join(dir, "Managed Note.md")
	// The path is already occupied by a file the user wrote, and the object
	// being restored is a Ghost note that must not take its place.
	mustWrite(t, target, userNote)
	quarantine := filepath.Join(dir, ".ghost-prune-restoretest")
	mustWrite(t, quarantine, ghostNote)

	if err := restoreQuarantined(quarantine, target); err == nil {
		t.Fatal("restore onto an occupied path succeeded, want a refusal")
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("the occupying file was destroyed: %v", err)
	}
	if string(got) != userNote {
		t.Errorf("content = %q, want the occupying file intact", got)
	}
	if _, err := os.Stat(quarantine); err != nil {
		t.Errorf("the quarantined object was consumed by a refused restore: %v", err)
	}
}

// TestWriteIfChangedRefusesSpecialDestination covers the publish path's version
// of the special-file hazard. writeIfChanged opened the destination with
// os.ReadFile before checking what it was: a FIFO at a canonical note path
// blocks that open until a writer appears, so the export hangs on a file it was
// going to refuse anyway, and a symlink is read through while the rename below
// replaces the link entry — the target gets compared, the link gets unlinked.
func TestWriteIfChangedRefusesSpecialDestination(t *testing.T) {
	dir := t.TempDir()

	pipe := filepath.Join(dir, "Memory.md")
	if err := makeFIFO(pipe); err != nil {
		t.Skipf("named pipe unavailable: %v", err)
	}
	target := filepath.Join(dir, "secret.md")
	mustWrite(t, target, "target content\n")
	link := filepath.Join(dir, "Linked.md")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	pipeDone := make(chan error, 1)
	go func() {
		_, err := writeIfChanged(pipe, "---\nghost_id: abc\n---\nbody\n")
		pipeDone <- err
	}()
	select {
	case err := <-pipeDone:
		if err == nil {
			t.Error("writeIfChanged published over a named pipe, want a refusal")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("writeIfChanged blocked for 5s on a FIFO — the destination must be typed before it is read")
	}

	if _, err := writeIfChanged(link, "---\nghost_id: abc\n---\nreplaced\n"); err == nil {
		t.Error("writeIfChanged published over a symlink, want a refusal")
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read symlink target: %v", err)
	}
	if string(got) != "target content\n" {
		t.Errorf("symlink target = %q, want it untouched", got)
	}
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("the symlink itself was replaced (err=%v), want it left in place", err)
	}

	// The control: an ordinary destination still publishes.
	plain := filepath.Join(dir, "Plain.md")
	if _, err := writeIfChanged(plain, "---\nghost_id: abc\n---\nbody\n"); err != nil {
		t.Fatalf("writeIfChanged on a regular path: %v", err)
	}
}

// TestRemoveGhostFileRestoresAfterFailedRemoval covers a note stranded under a
// name prune can never see. By the time the final os.Remove runs, the note has
// already been moved aside, so a failed deletion returned without putting it
// back — leaving it under a .ghost-prune-* name, which every later pass
// ignores. The mirror then reports success while permanently missing a note it
// still owns.
func TestRemoveGhostFileRestoresAfterFailedRemoval(t *testing.T) {
	root := filepath.Join(t.TempDir(), "vault")
	if err := ensureVault(root); err != nil {
		t.Fatalf("ensureVault: %v", err)
	}
	dir := filepath.Join(root, "oldproject")
	mustMkdirAll(t, dir)
	target := filepath.Join(dir, "Managed Note.md")
	mustWrite(t, target, ghostNote)

	// Fail the deletion of the quarantined object, and only that: the walk
	// creates and removes temp names of its own, so the hook keys on the
	// .ghost-prune- prefix.
	removeFileFn.Store(func(name string) error {
		if strings.HasPrefix(filepath.Base(name), ".ghost-prune-") {
			return errors.New("injected: cannot delete quarantined note")
		}
		return os.Remove(name)
	})
	t.Cleanup(func() { removeFileFn.Store(os.Remove) })

	deleted, err := removeGhostFile(target)
	if err == nil {
		t.Fatal("removeGhostFile reported success, want the deletion failure")
	}
	if deleted {
		t.Error("removeGhostFile claimed a deletion it could not complete")
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("the note was not restored to its original path: %v", err)
	}
	if string(got) != ghostNote {
		t.Errorf("restored content = %q, want the original note", got)
	}
	// Nothing left behind under the name prune ignores.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".ghost-prune-") {
			t.Errorf("a quarantined copy was stranded: %s", e.Name())
		}
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

// TestPruneKeepsCrashTempYoungerThanGrace is the concurrency half of the crash
// reclaim. writeIfChanged publishes through a temp file in the same directory
// and renames it into place, so between CreateTemp and that rename the file is a
// LIVE file owned by a concurrent sync. Deleting it destroys a note that writer
// is about to publish, and the name alone cannot tell the two apart — only age
// can.
func TestPruneKeepsCrashTempYoungerThanGrace(t *testing.T) {
	root := filepath.Join(t.TempDir(), "vault")
	if err := ensureVault(root); err != nil {
		t.Fatalf("ensureVault: %v", err)
	}
	dir := filepath.Join(root, "live", "Memories")
	mustMkdirAll(t, dir)
	mustWrite(t, filepath.Join(dir, "Managed Note.md"), ghostNote)

	// A temp file created just now, exactly as a concurrent publish would look.
	live := filepath.Join(dir, "Other Note.md.ghost-tmp-1234")
	mustWrite(t, live, "half-written note")

	// An old one, from a publish that died hours ago.
	abandoned := filepath.Join(dir, "Dead Note.md.ghost-tmp-5678")
	mustWrite(t, abandoned, ghostNote)
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(abandoned, old, old); err != nil {
		t.Fatalf("age the abandoned temp: %v", err)
	}

	if err := prune(root, []string{"live"}, map[string]string{}, []string{"live"}); err != nil {
		t.Fatalf("prune: %v", err)
	}
	if _, err := os.Stat(live); err != nil {
		t.Errorf("a concurrent writer's live temp file was deleted: %v", err)
	}
	if _, err := os.Stat(abandoned); !os.IsNotExist(err) {
		t.Errorf("an abandoned temp file older than the grace period was not reclaimed (err=%v)", err)
	}
}

// TestExportSkipsNonRegularNotePath covers the write side. A symlink or a named
// pipe sitting at a note path belongs to the user or to another tool, and Ghost
// will not replace it — but one occupied path must not end the export. The
// mirror is there to sync the other notes.
func TestExportSkipsNonRegularNotePath(t *testing.T) {
	store := seedStore(t)
	ctx := context.Background()
	if _, err := store.Create(ctx, "ghost", memory.Memory{Category: "fact", Content: "first fact", Source: "mcp"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(ctx, "ghost", memory.Memory{Category: "fact", Content: "second fact", Source: "mcp"}); err != nil {
		t.Fatal(err)
	}

	vault := filepath.Join(t.TempDir(), "vault")
	ex := &Exporter{Store: store, Logger: slog.Default()}
	if err := ex.Export(ctx, vault, ""); err != nil {
		t.Fatalf("first Export: %v", err)
	}

	// Something that is not a regular file where a note goes.
	memories := filepath.Join(vault, "ghost", "Memories")
	entries, err := os.ReadDir(memories)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("no notes were written to occupy the path")
	}
	occupied := filepath.Join(memories, entries[0].Name())
	if err := os.Remove(occupied); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if err := makeFIFO(occupied); err != nil {
		t.Skipf("named pipe unavailable: %v", err)
	}

	// A second export must still succeed, and must still write the other note.
	if err := ex.Export(ctx, vault, ""); err != nil {
		t.Fatalf("Export aborted on a non-regular note path: %v", err)
	}
	if fi, err := os.Lstat(occupied); err != nil {
		t.Errorf("the special file was removed: %v", err)
	} else if fi.Mode()&os.ModeNamedPipe == 0 {
		t.Errorf("occupied path is no longer a FIFO (mode %v)", fi.Mode())
	}
}

// TestPruneReclaimsStrandedQuarantinedNote covers the crash window in
// removeGhostFile. It renames a note to .ghost-prune-* and only then deletes it;
// a process that dies between the two leaves the note under a name nothing else
// searches, so the mirror silently loses a note it still owns. The reclaim sweep
// now covers the quarantine on the same terms as a crashed publish — including
// the grace period, so a note mid-delete is never taken out from under itself.
func TestPruneReclaimsStrandedQuarantinedNote(t *testing.T) {
	root := filepath.Join(t.TempDir(), "vault")
	if err := ensureVault(root); err != nil {
		t.Fatalf("ensureVault: %v", err)
	}
	dir := filepath.Join(root, "live", "Memories")
	mustMkdirAll(t, dir)
	mustWrite(t, filepath.Join(dir, "Managed Note.md"), ghostNote)

	// A note abandoned mid-prune, long ago.
	stranded := filepath.Join(dir, ".ghost-prune-4821")
	mustWrite(t, stranded, ghostNote)
	old := time.Now().Add(-3 * time.Hour)
	if err := os.Chtimes(stranded, old, old); err != nil {
		t.Fatalf("age the stranded note: %v", err)
	}
	// One still inside the grace period: a prune in flight.
	inFlight := filepath.Join(dir, ".ghost-prune-9137")
	mustWrite(t, inFlight, ghostNote)

	if err := prune(root, []string{"live"}, map[string]string{}, []string{"live"}); err != nil {
		t.Fatalf("prune: %v", err)
	}
	if _, err := os.Stat(stranded); !os.IsNotExist(err) {
		t.Errorf("a note stranded under the prune quarantine was not reclaimed (err=%v)", err)
	}
	if _, err := os.Stat(inFlight); err != nil {
		t.Errorf("a note inside the grace period was reclaimed while a prune could still be using it: %v", err)
	}
}
