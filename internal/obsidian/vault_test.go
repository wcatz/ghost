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

func TestPruneGuards(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, markerName), []byte(`{"schema_version":1}`), 0o644)
	sub := filepath.Join(root, "proj", "Memories")
	os.MkdirAll(sub, 0o755)

	// (a) keep-set retention: ghost_id in keep must survive.
	keptNote := "---\nghost_id: cafe0000\ntype: memory\n---\nbody\n"
	os.WriteFile(filepath.Join(sub, "kept-cafe0000.md"), []byte(keptNote), 0o644)
	// (b) subtree scoping: ghost note outside the pruned subtrees must survive.
	other := filepath.Join(root, "otherproj", "Memories")
	os.MkdirAll(other, 0o755)
	staleNote := "---\nghost_id: dead0000\ntype: memory\n---\nbody\n"
	os.WriteFile(filepath.Join(other, "stale-dead0000.md"), []byte(staleNote), 0o644)
	// (c) body-only ghost_id: no frontmatter, ghost_id appears after a --- in the body.
	bodyOnly := "just a user note\n\n---\nghost_id: feed0000\n"
	os.WriteFile(filepath.Join(sub, "body-only.md"), []byte(bodyOnly), 0o644)

	if err := prune(root, []string{"proj"}, map[string]bool{"cafe0000": true}); err != nil {
		t.Fatalf("prune: %v", err)
	}
	if _, err := os.Stat(filepath.Join(sub, "kept-cafe0000.md")); err != nil {
		t.Fatal("note with ghost_id in keep must survive")
	}
	if _, err := os.Stat(filepath.Join(other, "stale-dead0000.md")); err != nil {
		t.Fatal("ghost note outside the pruned subtrees must survive")
	}
	if _, err := os.Stat(filepath.Join(sub, "body-only.md")); err != nil {
		t.Fatal("file with ghost_id only in the body must survive")
	}
}

func TestPruneRefusesEscapingSubtree(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "vault")
	os.MkdirAll(root, 0o755)
	os.WriteFile(filepath.Join(root, markerName), []byte(`{"schema_version":1}`), 0o644)
	// Sibling dir outside the vault holding a ghost-looking note.
	escape := filepath.Join(parent, "escape")
	os.MkdirAll(escape, 0o755)
	victim := filepath.Join(escape, "victim-dead0000.md")
	os.WriteFile(victim, []byte("---\nghost_id: dead0000\ntype: memory\n---\nbody\n"), 0o644)

	if err := prune(root, []string{"../escape"}, map[string]bool{}); err == nil {
		t.Fatal("prune with escaping subtree must error")
	}
	if _, err := os.Stat(victim); err != nil {
		t.Fatal("file outside the vault root must survive an escaping-subtree prune attempt")
	}
	// Absolute paths must be refused too.
	if err := prune(root, []string{escape}, map[string]bool{}); err == nil {
		t.Fatal("prune with absolute subtree must error")
	}
	if _, err := os.Stat(victim); err != nil {
		t.Fatal("file outside the vault root must survive an absolute-subtree prune attempt")
	}
}
