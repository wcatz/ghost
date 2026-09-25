package obsidian

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
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

// TestWriteIfChangedConcurrentTempIsolation pins the unique-temp fix: a fixed
// temp name let two concurrent writers share one path, so a reader could see a
// partially written or interleaved note (and a rename could hit a missing
// file). Payloads are large enough that interleaving corrupts the result
// rather than coincidentally matching.
func TestWriteIfChangedConcurrentTempIsolation(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.md")
	payloads := []string{
		strings.Repeat("A", 4096) + "one",
		strings.Repeat("B", 4096) + "two",
		strings.Repeat("C", 4096) + "three",
	}

	var wg sync.WaitGroup
	errs := make(chan error, len(payloads)*50)
	for i := 0; i < 50; i++ {
		for _, pl := range payloads {
			wg.Add(1)
			go func(pl string) {
				defer wg.Done()
				if _, err := writeIfChanged(p, pl); err != nil {
					errs <- err
				}
			}(pl)
		}
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent write failed: %v", err)
	}

	got, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read final: %v", err)
	}
	for _, pl := range payloads {
		if string(got) == pl {
			goto consistent
		}
	}
	t.Fatalf("final content is not exactly one payload (interleaved write), len=%d", len(got))
consistent:
	leftovers, _ := filepath.Glob(filepath.Join(dir, "*.ghost-tmp*"))
	if len(leftovers) != 0 {
		t.Fatalf("temp files left behind: %v", leftovers)
	}
}

func TestPruneLeavesOpenOldGhostTemp(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, markerName), `{"schema_version":1}`)
	sub := filepath.Join(root, "proj", "Memories")
	mustMkdirAll(t, sub)
	path := filepath.Join(sub, "active.md.ghost-tmp-abc123")
	mustWrite(t, path, "active")
	old := time.Now().Add(-2 * ghostTempMinAge)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close() //nolint:errcheck

	if err := prune(root, []string{"proj"}, map[string]string{}, nil); err != nil {
		t.Fatalf("prune: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("open old temp was removed: %v", err)
	}
}

func TestPruneLeavesRecentAndNearMissTemps(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, markerName), `{"schema_version":1}`)
	sub := filepath.Join(root, "proj", "Memories")
	mustMkdirAll(t, sub)
	recent := filepath.Join(sub, "recent.md.ghost-tmp-abc123")
	nearMiss := filepath.Join(sub, "note.ghost-tmp-draft.md")
	mustWrite(t, recent, "active")
	mustWrite(t, nearMiss, "user")
	old := time.Now().Add(-2 * ghostTempMinAge)
	if err := os.Chtimes(nearMiss, old, old); err != nil {
		t.Fatal(err)
	}

	if err := prune(root, []string{"proj"}, map[string]string{}, nil); err != nil {
		t.Fatalf("prune: %v", err)
	}
	if _, err := os.Stat(recent); err != nil {
		t.Errorf("recent active temp was removed: %v", err)
	}
	if _, err := os.Stat(nearMiss); err != nil {
		t.Errorf("near-miss user filename was removed: %v", err)
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

	if err := prune(root, []string{"proj"}, map[string]string{}, nil); err != nil {
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
	if err := prune(root, []string{"proj"}, map[string]string{}, nil); err == nil {
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
	// (d) slug rename: same live ghost_id under a non-canonical basename is stale.
	renamed := "---\nghost_id: cafe0000\ntype: memory\n---\nbody\n"
	mustWrite(t, filepath.Join(sub, "old-slug-cafe0000.md"), renamed)

	if err := prune(root, []string{"proj"}, map[string]string{"cafe0000": "kept-cafe0000.md"}, nil); err != nil {
		t.Fatalf("prune: %v", err)
	}
	if _, err := os.Stat(filepath.Join(sub, "kept-cafe0000.md")); err != nil {
		t.Fatal("note with ghost_id in keep must survive")
	}
	if _, err := os.Stat(filepath.Join(sub, "old-slug-cafe0000.md")); !os.IsNotExist(err) {
		t.Fatal("live ghost_id under a stale (renamed) basename must be pruned")
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

	if err := prune(root, []string{"../escape"}, map[string]string{}, nil); err == nil {
		t.Fatal("prune with escaping subtree must error")
	}
	if _, err := os.Stat(victim); err != nil {
		t.Fatal("file outside the vault root must survive an escaping-subtree prune attempt")
	}
	// Absolute paths must be refused too.
	if err := prune(root, []string{escape}, map[string]string{}, nil); err == nil {
		t.Fatal("prune with absolute subtree must error")
	}
	if _, err := os.Stat(victim); err != nil {
		t.Fatal("file outside the vault root must survive an absolute-subtree prune attempt")
	}
}
