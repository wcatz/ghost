package adversarial

import (
	"maps"
	"os"
	"path/filepath"
	"runtime"
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
		Tree{"sub/../../escape.md": {kind: entryFile, content: "x"}}.AssertTreeInside(fake, "crawler", root)
		if !fake.Failed() {
			t.Error("an entry whose components climb out must be reported")
		}
	})
}

// TestSnapshotPinsItsPlaceholders covers the three kinds Snapshot records with
// no file content — an unreadable file, a directory, and a non-regular entry,
// which records its type — because all three are blind spots a fixture author
// has to know about, and because an unreadable file's whole reason for recording
// a kind and nothing else is that what it failed at must NOT vary with how the
// root was spelled.
func TestSnapshotPinsItsPlaceholders(t *testing.T) {
	t.Run("unreadable_file", func(t *testing.T) {
		skipWithoutPOSIXModes(t)
		// Two roots with the same relative structure and the same contents. The
		// reason the unreadable entry records no cause is this: a value carrying
		// the path it failed at would differ between two trees that are the same,
		// and AssertUnchanged would report a rewrite of a file nobody touched.
		build := func() string {
			root := t.TempDir()
			locked := filepath.Join(root, "locked.md")
			if err := os.WriteFile(locked, []byte("secret"), 0o000); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(locked, 0o600) })
			if err := os.WriteFile(filepath.Join(root, "readable.md"), []byte("visible"), 0o600); err != nil {
				t.Fatal(err)
			}
			return root
		}
		first := Snapshot(t, build())
		second := Snapshot(t, build())

		if got := first["locked.md"]; got.kind != entryUnreadable {
			t.Errorf("unreadable file recorded as %v, want kind entryUnreadable", got)
		}
		// Compared over the whole trees, so the property covers every entry and
		// not only the locked file's kind: the added readable.md is what makes
		// this more than a one-key check.
		if !maps.Equal(first, second) {
			t.Errorf("two identical trees snapshotted differently:\n  first:  %v\n  second: %v", first, second)
		}
	})

	t.Run("content_can_never_be_mistaken_for_a_kind", func(t *testing.T) {
		// The kind is a separate field, so a file whose body is exactly the text
		// a placeholder used to be spelled with is still a file. A name that
		// swaps between a directory and such a file is a change this assertion
		// exists to report.
		for _, body := range []string{"", "<dir>", "<unreadable>", "L---------", "dir"} {
			fileRoot := t.TempDir()
			if err := os.WriteFile(filepath.Join(fileRoot, "swapped"), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			dirRoot := t.TempDir()
			if err := os.Mkdir(filepath.Join(dirRoot, "swapped"), 0o700); err != nil {
				t.Fatal(err)
			}
			fileTree := Snapshot(t, fileRoot)
			dirTree := Snapshot(t, dirRoot)
			if fileTree["swapped"] == dirTree["swapped"] {
				t.Errorf("a file whose body is %q and a directory both record as %v, so a swap is invisible",
					body, fileTree["swapped"])
			}
			if fileTree["swapped"].kind != entryFile {
				t.Errorf("a file whose body is %q recorded with kind %v, want entryFile", body, fileTree["swapped"].kind)
			}
		}
	})

	t.Run("non_regular_entry", func(t *testing.T) {
		// The third kind, with the type as a literal rather than derived from
		// fs.FileMode.String(): the property Snapshot documents is that a symlink
		// records as its type and never as its target's content, and a derived
		// expectation would move with any change to how the type is rendered
		// instead of failing.
		outside := t.TempDir()
		if err := os.WriteFile(filepath.Join(outside, "target.md"), []byte("not ours"), 0o600); err != nil {
			t.Fatal(err)
		}
		root := t.TempDir()
		if err := os.Symlink(filepath.Join(outside, "target.md"), filepath.Join(root, "link.md")); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		got := Snapshot(t, root)
		// "L" then nine dashes: fs.FileMode.String() spells the type and then the
		// permission bits, and a DirEntry's Type() carries only the type, so the
		// permission half is always unknown.
		want := entry{kind: entryOther, content: "L---------"}
		if got["link.md"] != want {
			t.Errorf("a symlink records as %v, want %v", got["link.md"], want)
		}
	})

	t.Run("unwalkable_subtree", func(t *testing.T) {
		skipWithoutPOSIXModes(t)
		root := t.TempDir()
		locked := filepath.Join(root, "locked")
		if err := os.MkdirAll(locked, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(locked, "inside.md"), []byte("invisible"), 0o600); err != nil {
			t.Fatal(err)
		}
		// Sealed after the file is in place: a directory the fixture cannot write
		// into is a directory the fixture could not have planted anything in.
		if err := os.Chmod(locked, 0o000); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })

		got := Snapshot(t, root)
		// WalkDir hands the directory's own entry over before it tries to list
		// it, so the directory is in the snapshot and its contents are not. A
		// change inside an unreadable subtree is invisible to AssertUnchanged;
		// pinned here so a fixture that relies on it fails here first.
		if _, ok := got["locked"]; !ok {
			t.Error("the unreadable directory itself was not recorded; a fixture cannot tell an empty subtree from an unreadable one")
		}
		if _, ok := got["locked/inside.md"]; ok {
			t.Errorf("Snapshot descended into a directory it could not read: %v", got)
		}
		if _, ok := got["."]; ok {
			t.Error("root itself was recorded as an entry")
		}
	})
}

// skipWithoutPOSIXModes skips a fixture that needs a mode bit to actually deny
// access.
//
// Two hosts have to be excluded, and only one of them is obvious. Root reads a
// mode-0000 file whatever its bits say. Windows is the other: os.Geteuid returns
// -1 there, so an euid check alone does not fire, and os.Chmod there only sets
// FILE_ATTRIBUTE_READONLY, which blocks neither a read nor a directory listing —
// so both reads and listings succeed and the fixture fails. The repo's other
// mode-bit tests gate this with a //go:build !windows tag on the file; these
// subtests share a function with two that run everywhere, so the check is here.
func skipWithoutPOSIXModes(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX mode bits do not deny a read or a listing on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("a mode-0000 path is still readable by root, so there is nothing to pin")
	}
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
	v, ok := got["link.md"]
	if !ok || v.kind == entryFile || v.content == "not ours" {
		t.Errorf("Snapshot followed the symlink: %v (present=%v)", v, ok)
	}
}
