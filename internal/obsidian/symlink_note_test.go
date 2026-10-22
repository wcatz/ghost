package obsidian

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// TestExportReplacesSymlinkedNoteWithRegularFile: a note path that is a symlink
// is treated as not identical and the export replaces the link with a regular
// file, as it did before the FIFO fix.
func TestExportReplacesSymlinkedNoteWithRegularFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink test needs a POSIX filesystem")
	}
	store := seedStore(t)
	ctx := context.Background()
	if _, err := store.Create(ctx, "ghost", memory.Memory{Category: "fact", Content: "Symlinked note fact", Importance: 0.8, Source: "mcp"}); err != nil {
		t.Fatal(err)
	}

	vault := filepath.Join(t.TempDir(), "vault")
	ex := &Exporter{Store: store, Logger: slog.Default()}
	if err := ex.Export(ctx, vault, ""); err != nil {
		t.Fatalf("first export: %v", err)
	}
	notes, _ := filepath.Glob(filepath.Join(vault, "ghost", "Memories", "*.md"))
	if len(notes) != 1 {
		t.Fatalf("want 1 note, got %d", len(notes))
	}
	note := notes[0]

	// Replace the note with a symlink to an unrelated file.
	target := filepath.Join(t.TempDir(), "target.md")
	if err := os.WriteFile(target, []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(note); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, note); err != nil {
		t.Fatal(err)
	}

	if err := ex.Export(ctx, vault, ""); err != nil {
		t.Fatalf("export over symlink: %v", err)
	}
	fi, err := os.Lstat(note)
	if err != nil {
		t.Fatalf("note missing after export: %v", err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		t.Fatal("export left a symlink in place of the note")
	}
	if !fi.Mode().IsRegular() {
		t.Fatalf("exported note is not a regular file: %v", fi.Mode())
	}
	data, err := os.ReadFile(note)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "Symlinked note fact") {
		t.Errorf("exported note missing expected content: %s", data)
	}
}

// TestPruneRemovesSymlinkedOrphanNote: a symlinked Ghost note in an orphan
// folder is still pruned (hasGhostID follows symlinks).
func TestPruneRemovesSymlinkedOrphanNote(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink test needs a POSIX filesystem")
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, markerName), []byte(`{"schema_version":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(root, "proj", "Memories")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}

	// Create a regular Ghost note first.
	note := filepath.Join(sub, "real.md")
	mustWrite(t, note, "---\nghost_id: abc123\ntype: memory\n---\ncontent\n")

	// Replace it with a symlink to another Ghost note.
	target := filepath.Join(t.TempDir(), "target.md")
	mustWrite(t, target, "---\nghost_id: abc123\ntype: memory\n---\ncontent\n")
	if err := os.Remove(note); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, note); err != nil {
		t.Fatal(err)
	}

	// Prune should follow the symlink and remove it (since it's not in keep set).
	if err := prune(root, []string{"proj"}, keepSet{}, nil); err != nil {
		t.Fatalf("prune: %v", err)
	}
	if _, err := os.Lstat(note); !os.IsNotExist(err) {
		t.Errorf("symlinked orphan note should be pruned: %v", err)
	}
}

// TestExportLeavesSymlinkedNoteWithIdenticalContentAlone: a symlinked note
// whose target already holds identical content is left as a symlink after
// export (the identical-content check reads through the link).
func TestExportLeavesSymlinkedNoteWithIdenticalContentAlone(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink test needs a POSIX filesystem")
	}
	store := seedStore(t)
	ctx := context.Background()
	if _, err := store.Create(ctx, "ghost", memory.Memory{Category: "fact", Content: "Identical content fact", Importance: 0.8, Source: "mcp"}); err != nil {
		t.Fatal(err)
	}

	vault := filepath.Join(t.TempDir(), "vault")
	ex := &Exporter{Store: store, Logger: slog.Default()}
	if err := ex.Export(ctx, vault, ""); err != nil {
		t.Fatalf("first export: %v", err)
	}
	notes, _ := filepath.Glob(filepath.Join(vault, "ghost", "Memories", "*.md"))
	if len(notes) != 1 {
		t.Fatalf("want 1 note, got %d", len(notes))
	}
	note := notes[0]

	// Read the note content to create an identical target.
	data, err := os.ReadFile(note)
	if err != nil {
		t.Fatal(err)
	}

	// Replace the note with a symlink to a file with IDENTICAL content.
	target := filepath.Join(t.TempDir(), "target.md")
	if err := os.WriteFile(target, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(note); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, note); err != nil {
		t.Fatal(err)
	}

	// Export again — the symlink should be left alone because the target
	// already has identical content.
	if err := ex.Export(ctx, vault, ""); err != nil {
		t.Fatalf("export over identical symlink: %v", err)
	}
	fi, err := os.Lstat(note)
	if err != nil {
		t.Fatalf("note missing after export: %v", err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("export replaced symlink whose target had identical content")
	}
}

// TestWriteIfChangedDanglingSymlink: a dangling symlink at a note path is
// replaced with a regular file on write (does not error).
func TestWriteIfChangedDanglingSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink test needs a POSIX filesystem")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "note.md")
	// Create a dangling symlink (target doesn't exist).
	if err := os.Symlink("/nonexistent/target.md", path); err != nil {
		t.Fatal(err)
	}
	wrote, err := writeIfChanged(path, "new content")
	if err != nil {
		t.Fatalf("writeIfChanged(dangling symlink) = %v, want nil", err)
	}
	if !wrote {
		t.Fatal("expected write to happen for dangling symlink")
	}
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		t.Fatal("dangling symlink should be replaced with regular file")
	}
	if !fi.Mode().IsRegular() {
		t.Fatalf("should be regular file: %v", fi.Mode())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "new content" {
		t.Errorf("content = %q, want %q", data, "new content")
	}
}

// TestWriteIfChangedUnreadableFile: an existing note with mode 000 is
// rewritten on write (does not error). Skipped when running as root or on Windows.
func TestWriteIfChangedUnreadableFile(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("permission test needs POSIX and non-root")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "note.md")
	if err := os.WriteFile(path, []byte("old content"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o644) })
	wrote, err := writeIfChanged(path, "new content")
	if err != nil {
		t.Fatalf("writeIfChanged(mode 000) = %v, want nil", err)
	}
	if !wrote {
		t.Fatal("expected write to happen for unreadable file")
	}
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		t.Fatal("should not be a symlink")
	}
	if !fi.Mode().IsRegular() {
		t.Fatalf("should be regular file: %v", fi.Mode())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "new content" {
		t.Errorf("content = %q, want %q", data, "new content")
	}
}
