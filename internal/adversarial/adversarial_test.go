package adversarial

import (
	"os"
	"path/filepath"
	"testing"
)

// TestCorpusIsDistinct guards the corpus itself: a Payload.Name that repeats
// would make two fixtures indistinguishable in a failure message, and an empty
// Text would make AssertVerbatim pass on any stored value.
func TestCorpusIsDistinct(t *testing.T) {
	seen := map[string]bool{}
	for _, p := range All() {
		if p.Name == "" {
			t.Errorf("payload with no name: %q", p.Text)
		}
		if p.Text == "" {
			t.Errorf("payload %q has no text to plant", p.Name)
		}
		if seen[p.Name] {
			t.Errorf("payload name %q appears twice", p.Name)
		}
		seen[p.Name] = true
	}
	if len(All()) < 20 {
		t.Errorf("corpus has %d payloads, too few to be worth sharing across suites", len(All()))
	}
}

func TestAssertVerbatim(t *testing.T) {
	t.Run("passes", func(t *testing.T) {
		AssertVerbatim(t, "surface", "prefix "+"payload\x00with\xffbytes"+" suffix", "payload\x00with\xffbytes")
	})
	t.Run("reports_a_dropped_payload", func(t *testing.T) {
		fake := &testing.T{}
		AssertVerbatim(fake, "surface", "the payload was dropped", "planted")
		if !fake.Failed() {
			t.Error("a dropped payload must fail the assertion")
		}
	})
}

// TestAssertLocalName drives the containment rule over the shapes that break
// it, and over the shapes that must keep working — a rule that rejected every
// name would satisfy the traversal cases while making the exporter useless.
func TestAssertLocalName(t *testing.T) {
	for _, name := range []string{
		"../../../../etc/cron.d/ghost",
		"/etc/passwd",
		`..\..\Windows\System32`,
		"note\x00.png",
		"..",
		".",
		"",
		"a/b",
		string(make([]byte, 200)),
	} {
		fake := &testing.T{}
		AssertLocalName(fake, "name", name)
		if !fake.Failed() {
			t.Errorf("AssertLocalName accepted %q", name)
		}
	}
	for _, name := range []string{
		"938891EAF111890B",
		"note-938891ea.md",
		"_global",
		"my project name",
		"naïve-project",
		"note.md.",
	} {
		fake := &testing.T{}
		AssertLocalName(fake, "name", name)
		if fake.Failed() {
			t.Errorf("AssertLocalName rejected a legitimate name %q", name)
		}
	}
}

func TestSnapshotAndAssertions(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "sub", "a.md"), []byte("one"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := Snapshot(t, root)
	after := Snapshot(t, root)
	before.AssertUnchanged(t, "idle", after)
	after.AssertTreeInside(t, "idle", root)

	t.Run("created", func(t *testing.T) {
		if err := os.WriteFile(filepath.Join(root, "new.md"), []byte("two"), 0o600); err != nil {
			t.Fatal(err)
		}
		fake := &testing.T{}
		before.AssertUnchanged(fake, "writer", Snapshot(t, root))
		if !fake.Failed() {
			t.Error("a created file must be reported")
		}
	})

	t.Run("rewrote", func(t *testing.T) {
		if err := os.WriteFile(filepath.Join(root, "sub", "a.md"), []byte("changed"), 0o600); err != nil {
			t.Fatal(err)
		}
		fake := &testing.T{}
		before.AssertUnchanged(fake, "writer", Snapshot(t, root))
		if !fake.Failed() {
			t.Error("a rewritten file must be reported")
		}
	})

	t.Run("removed", func(t *testing.T) {
		if err := os.Remove(filepath.Join(root, "sub", "a.md")); err != nil {
			t.Fatal(err)
		}
		fake := &testing.T{}
		before.AssertUnchanged(fake, "pruner", Snapshot(t, root))
		if !fake.Failed() {
			t.Error("a removed file must be reported")
		}
	})

	t.Run("escaping component", func(t *testing.T) {
		fake := &testing.T{}
		Tree{"sub/../../escape.md": ""}.AssertTreeInside(fake, "crawler", root)
		if !fake.Failed() {
			t.Error("an entry whose components climb out must be reported")
		}
	})
}

func TestSnapshotOfMissingRootIsEmpty(t *testing.T) {
	if got := Snapshot(t, filepath.Join(t.TempDir(), "never-created")); len(got) != 0 {
		t.Errorf("snapshot of a missing root = %v, want empty", got)
	}
}

// TestSnapshotRecordsSymlinkWithoutFollowingIt keeps the snapshot a statement
// about the tree under root: a link's target belongs to whoever created it, and
// reading through one would assert containment over a directory Ghost never
// touched.
func TestSnapshotRecordsSymlinkWithoutFollowingIt(t *testing.T) {
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "target.md"), []byte("not ours"), 0o600); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := os.Symlink(filepath.Join(outside, "target.md"), filepath.Join(root, "link.md")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	got := Snapshot(t, root)
	if v, ok := got["link.md"]; !ok || v == "not ours" {
		t.Errorf("Snapshot followed the symlink: %q (present=%v)", v, ok)
	}
}
