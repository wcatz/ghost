package obsidian

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEnsureVault(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "vault")
	if err := ensureVault(dir); err != nil { // fresh dir: created + marker
		t.Fatalf("fresh: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, markerName)); err != nil {
		t.Fatalf("marker missing: %v", err)
	}
	if err := ensureVault(dir); err != nil { // idempotent
		t.Fatalf("second run: %v", err)
	}
	// Existing non-empty dir WITHOUT marker → refuse.
	dirty := t.TempDir()
	os.WriteFile(filepath.Join(dirty, "keep.md"), []byte("user file"), 0o644)
	if err := ensureVault(dirty); err == nil {
		t.Fatal("expected refusal for unmarked non-empty dir")
	}
}

func TestWriteIfChanged(t *testing.T) {
	p := filepath.Join(t.TempDir(), "a.md")
	w1, err := writeIfChanged(p, "hello")
	if err != nil || !w1 {
		t.Fatalf("first write: wrote=%v err=%v", w1, err)
	}
	w2, _ := writeIfChanged(p, "hello")
	if w2 {
		t.Fatal("unchanged content must not rewrite")
	}
	w3, _ := writeIfChanged(p, "hello2")
	if !w3 {
		t.Fatal("changed content must rewrite")
	}
}

func TestPrune(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, markerName), []byte(`{"schema_version":1}`), 0o644)
	sub := filepath.Join(root, "proj", "Memories")
	os.MkdirAll(sub, 0o755)
	ghostNote := "---\nghost_id: dead0000\ntype: memory\n---\nbody\n"
	os.WriteFile(filepath.Join(sub, "stale-dead0000.md"), []byte(ghostNote), 0o644)
	os.WriteFile(filepath.Join(sub, "user-note.md"), []byte("no frontmatter"), 0o644)

	if err := prune(root, []string{"proj"}, map[string]bool{}); err != nil {
		t.Fatalf("prune: %v", err)
	}
	if _, err := os.Stat(filepath.Join(sub, "stale-dead0000.md")); !os.IsNotExist(err) {
		t.Fatal("stale ghost note should be deleted")
	}
	if _, err := os.Stat(filepath.Join(sub, "user-note.md")); err != nil {
		t.Fatal("user note must survive")
	}
	// No marker → prune must refuse to delete anything.
	os.Remove(filepath.Join(root, markerName))
	os.WriteFile(filepath.Join(sub, "stale2-beef0000.md"), []byte(ghostNote), 0o644)
	if err := prune(root, []string{"proj"}, map[string]bool{}); err == nil {
		t.Fatal("prune without marker must error")
	}
}
