package obsidian

import (
	"os"
	"path/filepath"
	"testing"
)

// mustWrite / mustMkdirAll fail the test on setup errors instead of
// silently proceeding against a half-built fixture.
func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mustMkdirAll(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
}

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
	mustWrite(t, filepath.Join(dirty, "keep.md"), "user file")
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
	mustWrite(t, filepath.Join(root, markerName), `{"schema_version":1}`)
	sub := filepath.Join(root, "proj", "Memories")
	mustMkdirAll(t, sub)
	ghostNote := "---\nghost_id: dead0000\ntype: memory\n---\nbody\n"
	mustWrite(t, filepath.Join(sub, "stale-dead0000.md"), ghostNote)
	mustWrite(t, filepath.Join(sub, "user-note.md"), "no frontmatter")

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
	if err := os.Remove(filepath.Join(root, markerName)); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(sub, "stale2-beef0000.md"), ghostNote)
	if err := prune(root, []string{"proj"}, map[string]bool{}); err == nil {
		t.Fatal("prune without marker must error")
	}
}

func TestPruneGuards(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, markerName), `{"schema_version":1}`)
	sub := filepath.Join(root, "proj", "Memories")
	mustMkdirAll(t, sub)

	// (a) keep-set retention: ghost_id in keep must survive.
	keptNote := "---\nghost_id: cafe0000\ntype: memory\n---\nbody\n"
	mustWrite(t, filepath.Join(sub, "kept-cafe0000.md"), keptNote)
	// (b) subtree scoping: ghost note outside the pruned subtrees must survive.
	other := filepath.Join(root, "otherproj", "Memories")
	mustMkdirAll(t, other)
	staleNote := "---\nghost_id: dead0000\ntype: memory\n---\nbody\n"
	mustWrite(t, filepath.Join(other, "stale-dead0000.md"), staleNote)
	// (c) body-only ghost_id: no frontmatter, ghost_id appears after a --- in the body.
	bodyOnly := "just a user note\n\n---\nghost_id: feed0000\n"
	mustWrite(t, filepath.Join(sub, "body-only.md"), bodyOnly)

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
	mustMkdirAll(t, root)
	mustWrite(t, filepath.Join(root, markerName), `{"schema_version":1}`)
	// Sibling dir outside the vault holding a ghost-looking note.
	escape := filepath.Join(parent, "escape")
	mustMkdirAll(t, escape)
	victim := filepath.Join(escape, "victim-dead0000.md")
	mustWrite(t, victim, "---\nghost_id: dead0000\ntype: memory\n---\nbody\n")

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
