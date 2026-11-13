package obsidian

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

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
