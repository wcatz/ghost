package obsidian

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
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

// TestEnsureVaultTightensLegacyPermissions: 0700/0600 only reaches files
// Ghost writes after the fact — writeIfChanged skips unchanged content and
// MkdirAll never retightens an existing directory — so a vault created
// before the permission fix stays 0755/0644 forever. ensureVault is the
// one place that sees the whole vault on startup; when the marker says the
// vault is Ghost's, it must tighten what Ghost owns and leave user files'
// modes alone.
func TestEnsureVaultTightensLegacyPermissions(t *testing.T) {
	root := filepath.Join(t.TempDir(), "vault")
	proj := filepath.Join(root, "proj")
	memories := filepath.Join(proj, "Memories")
	mustMkdirAll(t, memories)
	marker := filepath.Join(root, markerName)
	mustWrite(t, marker, `{"schema_version":1}`+"\n")
	note := filepath.Join(memories, "n.md")
	mustWrite(t, note, ghostNote)
	own := filepath.Join(proj, "own.md") // no frontmatter — the user's file
	mustWrite(t, own, userNote)

	// Chmod the legacy modes outright: relying on umask would let the
	// environment hand us 0700/0600 for free and the assertions would pass
	// without the fix.
	for _, d := range []string{root, proj, memories} {
		if err := os.Chmod(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{marker, note, own} {
		if err := os.Chmod(f, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	if err := ensureVault(root); err != nil {
		t.Fatalf("ensureVault: %v", err)
	}

	for _, tc := range []struct {
		path string
		want os.FileMode
	}{
		{root, 0o700},
		{proj, 0o700},
		{memories, 0o700},
		{marker, 0o600},
		{note, 0o600},
	} {
		st, err := os.Stat(tc.path)
		if err != nil {
			t.Fatalf("stat %s: %v", tc.path, err)
		}
		if st.Mode().Perm() != tc.want {
			t.Errorf("%s mode = %04o, want %04o", tc.path, st.Mode().Perm(), tc.want)
		}
	}
	// Tightening is scoped to what the mirror owns; the user's own file keeps
	// the mode it had.
	if st, err := os.Stat(own); err != nil {
		t.Fatalf("stat user file: %v", err)
	} else if st.Mode().Perm() != 0o644 {
		t.Errorf("user file mode = %04o, want it left at 0644", st.Mode().Perm())
	}
}

// TestHasGhostIDLongFrontmatterLines: Scanner's default 64 KiB token cap
// silently classified a Ghost note with one long frontmatter value as the
// user's file — prune would then leave the stale note behind and the
// permission pass would skip it. Lines up to maxFrontmatterLine must scan;
// a line past the cap must classify as "not Ghost's file", the safe
// direction (stale beats deleted), rather than an unknown id ever making a
// file deletable.
func TestHasGhostIDLongFrontmatterLines(t *testing.T) {
	dir := t.TempDir()

	under := filepath.Join(dir, "long-value.md")
	mustWrite(t, under, "---\nurls: "+strings.Repeat("u", 128<<10)+"\nghost_id: abc123\n---\nbody\n")
	if id, found := hasGhostID(under); !found || id != "abc123" {
		t.Errorf("hasGhostID with a long frontmatter line = (%q, %v), want (abc123, true)", id, found)
	}

	past := filepath.Join(dir, "over-cap.md")
	mustWrite(t, past, "---\nurls: "+strings.Repeat("u", maxFrontmatterLine+4096)+"\nghost_id: abc123\n---\nbody\n")
	if id, found := hasGhostID(past); found {
		t.Errorf("hasGhostID past the line cap = (%q, true), want the note left alone as non-Ghost", id)
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
