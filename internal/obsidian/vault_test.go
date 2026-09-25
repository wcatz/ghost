package obsidian

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
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
//
// Two dimensions. Both race many DISTINCT payloads per destination and accept
// any payload written for that destination, so a shared temp name is caught
// wherever foreign bytes land: each writer's rename can publish another
// writer's bytes. The second dimension adds the shared-target case — all
// writers replacing one path, which must end as exactly one of the payloads —
// and is POSIX-only by construction: Go opens files for reading without
// FILE_SHARE_DELETE, so on Windows a rename replacing a destination that any
// goroutine holds for a read is refused outright. That is a property of this
// test's own concurrent readers, not of Ghost, whose publishes go through
// renameWithRetry for exactly that transient.
func TestWriteIfChangedConcurrentTempIsolation(t *testing.T) {
	const writersPerTarget = 30

	t.Run("distinct targets", func(t *testing.T) {
		dir := t.TempDir()
		payloadFor := func(target string, i int) string {
			return target + "-" + strconv.Itoa(i) + " " + strings.Repeat(target, 4096+i)
		}
		var wg sync.WaitGroup
		errs := make(chan error, 3*writersPerTarget)
		for i := 0; i < writersPerTarget; i++ {
			for _, target := range []string{"one", "two", "three"} {
				wg.Add(1)
				go func(target string, i int) {
					defer wg.Done()
					p := filepath.Join(dir, target+".md")
					if _, err := writeIfChanged(p, payloadFor(target, i)); err != nil {
						errs <- err
					}
				}(target, i)
			}
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Fatalf("concurrent write failed: %v", err)
		}
		for _, target := range []string{"one", "two", "three"} {
			got, err := os.ReadFile(filepath.Join(dir, target+".md"))
			if err != nil {
				t.Fatalf("read %s.md: %v", target, err)
			}
			for i := 0; i < writersPerTarget; i++ {
				if string(got) == payloadFor(target, i) {
					goto consistent
				}
			}
			t.Errorf("%s.md holds no payload written for it (interleaved write), len=%d", target, len(got))
		consistent:
		}
		assertNoTempLeftovers(t, dir)
	})

	t.Run("shared target", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("Windows refuses a replace while another goroutine holds the destination open for reading")
		}
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
		if string(got) != payloads[0] && string(got) != payloads[1] && string(got) != payloads[2] {
			t.Fatalf("final content is not exactly one payload (interleaved write), len=%d", len(got))
		}
		assertNoTempLeftovers(t, dir)
	})
}

func assertNoTempLeftovers(t *testing.T, dir string) {
	t.Helper()
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

// TestPruneKeepsNoteWithUnclosedFrontmatter: a user note can open with a
// horizontal rule and mention ghost_id later without ever closing the
// frontmatter — a pasted template, a code example. Treating that as a
// Ghost-managed file deletes the user's note outright, which is the defect
// issue #550 exists to prevent. A candidate id counts only once the
// closing --- is actually seen.
func TestPruneKeepsNoteWithUnclosedFrontmatter(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, markerName), `{"schema_version":1}`)
	sub := filepath.Join(root, "proj", "Memories")
	mustMkdirAll(t, sub)
	unclosed := "---\n# My template\n\nExample frontmatter:\n\nghost_id: user-only\n"
	mustWrite(t, filepath.Join(sub, "template.md"), unclosed)
	mustWrite(t, filepath.Join(sub, "stale-dead0000.md"), "---\nghost_id: dead0000\n---\nbody\n")

	if err := prune(root, []string{"proj"}, map[string]string{}, nil); err != nil {
		t.Fatalf("prune: %v", err)
	}
	if _, err := os.Stat(filepath.Join(sub, "template.md")); err != nil {
		t.Errorf("note with an unclosed frontmatter was deleted: %v", err)
	}
	if _, err := os.Stat(filepath.Join(sub, "stale-dead0000.md")); !os.IsNotExist(err) {
		t.Error("a genuinely managed note (closed frontmatter) must still be pruned")
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
