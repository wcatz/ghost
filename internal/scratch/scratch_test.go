package scratch

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wcatz/ghost/internal/procstat"
)

// errForcedRemoval is the failure the removal seams are pinned to return.
var errForcedRemoval = errors.New("removal forced to fail by test")

// idFileBody is the content every follow-up id file in these tests carries.
const idFileBody = "# one id\naaaaaaaa1111111111111111111111\n"

// must fails the test on a setup error.
func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// writeOwnerDir creates a scratch-dir stand-in named name under root with an
// optional .owner marker, so Reap's classification can be tested without Open.
func writeOwnerDir(t *testing.T, root, name, marker string) string {
	t.Helper()
	dir := filepath.Join(root, name)
	must(t, os.Mkdir(dir, 0o700))
	if marker != "" {
		must(t, os.WriteFile(filepath.Join(dir, ownerFile), []byte(marker), 0o600))
	}
	return dir
}

// restrictiveSub adds a 0555 subtree under dir, as module caches leave.
func restrictiveSub(t *testing.T, dir string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("permission bits are not enforced the same way on Windows")
	}
	sub := filepath.Join(dir, "sub")
	must(t, os.Mkdir(sub, 0o700))
	must(t, os.WriteFile(filepath.Join(sub, "f"), []byte("x"), 0o600))
	must(t, os.Chmod(sub, 0o555))
	t.Cleanup(func() { _ = os.Chmod(sub, 0o700) })
}

func skipWindowsSymlink(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires privileges on Windows")
	}
}

func TestRoot(t *testing.T) {
	override := filepath.Join(t.TempDir(), "scratch")
	dataHome := t.TempDir()
	tests := []struct {
		name, override, want string
	}{
		{"env override", override, override},
		{"default under data dir", "", filepath.Join(dataHome, "ghost", "scratch")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("XDG_DATA_HOME", dataHome)
			t.Setenv("GHOST_SCRATCH_DIR", tt.override)
			got, err := Root()
			if err != nil {
				t.Fatalf("Root: %v", err)
			}
			if got != tt.want {
				t.Errorf("Root = %q, want %q", got, tt.want)
			}
			info, err := os.Stat(got)
			if err != nil {
				t.Fatalf("stat root: %v", err)
			}
			if perm := info.Mode().Perm(); perm != 0o700 {
				t.Errorf("root mode = %o, want 700", perm)
			}
		})
	}
}

// TestOpen_CreatesPrivateDirWithOwnerMarker also pins that a broken inherited
// temp dir is never consulted or touched.
func TestOpen_CreatesPrivateDirWithOwnerMarker(t *testing.T) {
	root := t.TempDir()
	t.Setenv("GHOST_SCRATCH_DIR", root)
	brokenTemp := filepath.Join(t.TempDir(), "missing", "temp")
	for _, key := range []string{"TMPDIR", "TMP", "TEMP"} {
		t.Setenv(key, brokenTemp)
	}

	d, err := Open()
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Release()

	info, err := os.Stat(d.Path())
	if err != nil {
		t.Fatalf("stat scratch dir: %v", err)
	}
	if !info.IsDir() || info.Mode().Perm() != 0o700 {
		t.Errorf("scratch dir mode = %v, want a 0700 directory", info.Mode())
	}
	if got := filepath.Dir(d.Path()); got != root {
		t.Errorf("scratch dir parent = %q, want the root %q", got, root)
	}
	if _, err := os.Stat(brokenTemp); !os.IsNotExist(err) {
		t.Errorf("Open touched the inherited temp path %q (stat err %v)", brokenTemp, err)
	}

	raw, err := os.ReadFile(filepath.Join(d.Path(), ownerFile))
	if err != nil {
		t.Fatalf("read owner marker: %v", err)
	}
	marker := string(raw)
	if want := "pid=" + strconv.Itoa(os.Getpid()); !strings.Contains(marker, want) {
		t.Errorf("marker %q missing %q", marker, want)
	}
	if !strings.Contains(marker, "token=") {
		t.Errorf("marker %q missing token", marker)
	}
	// An unavailable creation-time token must be the explicit sentinel, never a
	// missing line, which would be mistaken for a legacy marker.
	want := "start=" + startNone
	if start, ok := procstat.StartTime(os.Getpid()); ok {
		want = "start=" + start
	}
	if !strings.Contains(marker, want) {
		t.Errorf("marker %q missing %q", marker, want)
	}
}

// TestOwnerMarkerText pins both marker shapes, including the ok=false path that
// cannot be forced through Open where StartTime succeeds.
func TestOwnerMarkerText(t *testing.T) {
	tests := []struct {
		name      string
		start     string
		haveStart bool
		wantStart string
	}{
		{"token available", "boot-12345", true, "boot-12345"},
		{"token unavailable", "", false, startNone},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ownerMarkerText(4242, "aabbccddeeff0011", tt.start, tt.haveStart)
			want := "pid=4242\ntoken=aabbccddeeff0011\nstart=" + tt.wantStart + "\n"
			if got != want {
				t.Errorf("ownerMarkerText = %q, want %q", got, want)
			}
		})
	}
}

func TestRelease_RemovesAndIsIdempotent(t *testing.T) {
	t.Setenv("GHOST_SCRATCH_DIR", t.TempDir())
	d, err := Open()
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	path := d.Path()
	d.Release()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("scratch dir survived Release (stat err %v)", err)
	}
	d.Release() // must be safe when the directory is already gone
}

func TestReap_ClassifiesEntries(t *testing.T) {
	// deadPid is far beyond every platform's pid space, so no live process can
	// own it. Never use a real pid such as 1, which is always alive.
	const deadPid = 1 << 30
	livePid := os.Getpid()
	liveStart, haveStart := procstat.StartTime(livePid)
	oldTime := time.Now().Add(-2 * time.Hour)
	// scratchName has exactly the shape Open creates: without a readable
	// marker, the name is the only ownership evidence Reap accepts.
	const scratchName = "123-0123456789abcdef"

	// An empty start omits the line, the true pre-token legacy form.
	markerFor := func(pid int, start string) string {
		marker := "pid=" + strconv.Itoa(pid) + "\ntoken=bb\n"
		if start != "" {
			marker += "start=" + start + "\n"
		}
		return marker
	}

	tests := []struct {
		name       string
		dirName    string
		marker     string
		mtime      *time.Time
		needToken  bool
		wantRemove bool
	}{
		{"dead owner removed", "entry", markerFor(deadPid, "stale-token"), nil, false, true},
		{"live owner with matching token old mtime spared", "entry", markerFor(livePid, liveStart), &oldTime, true, false},
		{"live owner with mismatched token removed", "entry", markerFor(livePid, "definitely-not-the-real-token"), nil, true, true},
		{"sentinel live owner old mtime spared", "entry", markerFor(livePid, startNone), &oldTime, false, false},
		{"legacy live owner fresh spared", "entry", markerFor(livePid, ""), nil, false, false},
		{"legacy live owner old removed", "entry", markerFor(livePid, ""), &oldTime, false, true},
		{"malformed pid scratch-shaped old removed", scratchName, "pid=notanumber\ntoken=cc\n", &oldTime, false, true},
		{"malformed pid scratch-shaped fresh spared", scratchName, "pid=notanumber\ntoken=dd\n", nil, false, false},
		{"missing marker scratch-shaped old removed", scratchName, "", &oldTime, false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.needToken && !haveStart {
				t.Skip("process start tokens are unavailable on this platform")
			}
			root := t.TempDir()
			t.Setenv("GHOST_SCRATCH_DIR", root)
			dir := writeOwnerDir(t, root, tt.dirName, tt.marker)
			if tt.mtime != nil {
				must(t, os.Chtimes(dir, *tt.mtime, *tt.mtime))
			}

			removed, _, err := Reap(time.Hour)
			if err != nil {
				t.Fatalf("Reap: %v", err)
			}
			if (removed == 1) != tt.wantRemove || removed > 1 {
				t.Errorf("removed = %d, want removal = %v", removed, tt.wantRemove)
			}
			if _, err := os.Stat(dir); os.IsNotExist(err) != tt.wantRemove {
				t.Errorf("entry removed = %v, want %v", os.IsNotExist(err), tt.wantRemove)
			}
		})
	}
}

// TestReap_LeavesForeignEntriesAlone proves Reap deletes only what Ghost
// created: $GHOST_SCRATCH_DIR may point at a shared directory.
func TestReap_LeavesForeignEntriesAlone(t *testing.T) {
	root := t.TempDir()
	t.Setenv("GHOST_SCRATCH_DIR", root)
	oldTime := time.Now().Add(-2 * time.Hour)

	foreignDir := filepath.Join(root, "not-ghost")
	must(t, os.Mkdir(foreignDir, 0o700))
	foreignFile := filepath.Join(root, "some-notes.txt")
	must(t, os.WriteFile(foreignFile, []byte("not ghost's"), 0o600))
	foreignMalformed := writeOwnerDir(t, root, "foreign-with-marker", "pid=notanumber\n")
	foreign := []string{foreignDir, foreignFile, foreignMalformed}
	for _, p := range foreign {
		must(t, os.Chtimes(p, oldTime, oldTime))
	}

	removed, _, err := Reap(time.Hour)
	if err != nil {
		t.Fatalf("Reap: %v", err)
	}
	if removed != 0 {
		t.Errorf("removed = %d, want 0 — foreign entries must survive", removed)
	}
	for _, p := range foreign {
		if _, err := os.Lstat(p); err != nil {
			t.Errorf("foreign entry %s was removed: %v", p, err)
		}
	}
}

// TestReap_LeavesMarkerlessEntriesWithImpossiblePidsAlone: a pid segment of "0"
// or one with a leading zero is not a pid any Ghost run can have produced, so a
// markerless entry wearing one is left alone however old it is. The controls
// naming a possible pid are removed in the same reap, so a survivor survives on
// the pid segment alone.
func TestReap_LeavesMarkerlessEntriesWithImpossiblePidsAlone(t *testing.T) {
	root := t.TempDir()
	t.Setenv("GHOST_SCRATCH_DIR", root)
	oldTime := time.Now().Add(-2 * time.Hour)
	pastWindow := time.Now().Add(-idFileStaleAge - time.Hour)

	entries := []struct {
		entry   string
		file    bool
		mtime   time.Time
		removed bool
	}{
		{"0-0123456789abcdef", false, oldTime, false},
		{"007-0123456789abcdef", false, oldTime, false},
		{"supersede-reassess-p-20260101T000000Z-0-0123456789abcdef.ids", true, pastWindow, false},
		{"123-0123456789abcdef", false, oldTime, true},
		{"supersede-reassess-p-20260101T000000Z-123-0123456789abcdef.ids", true, pastWindow, true},
	}
	for _, e := range entries {
		path := filepath.Join(root, e.entry)
		if e.file {
			must(t, os.WriteFile(path, []byte(idFileBody), 0o600))
		} else {
			must(t, os.Mkdir(path, 0o700))
		}
		must(t, os.Chtimes(path, e.mtime, e.mtime))
	}

	removed, _, err := Reap(time.Hour)
	if err != nil {
		t.Fatalf("Reap: %v", err)
	}
	if removed != 2 {
		t.Errorf("removed = %d, want 2", removed)
	}
	for _, e := range entries {
		_, err := os.Lstat(filepath.Join(root, e.entry))
		if os.IsNotExist(err) != e.removed {
			t.Errorf("%s: removed = %v, want %v (lstat err %v)", e.entry, os.IsNotExist(err), e.removed, err)
		}
	}
}

// TestUniqueSuffix: callers needing a name nothing else will take use the same
// token shape Open does; two calls must differ.
func TestUniqueSuffix(t *testing.T) {
	first, err1 := UniqueSuffix()
	second, err2 := UniqueSuffix()
	if err1 != nil || err2 != nil {
		t.Fatalf("UniqueSuffix: %v, %v", err1, err2)
	}
	if first == second {
		t.Errorf("two calls returned the same suffix %q", first)
	}
	if !isScratchDirName(first) {
		t.Errorf("suffix %q is not the package's own token shape", first)
	}
}

func TestIsScratchDirName(t *testing.T) {
	tests := []struct {
		name string
		want bool
	}{
		{"123-0123456789abcdef", true},
		{"1230123456789abcdef", false},   // missing dash
		{"123-0123456789abcde", false},   // short hex
		{"123-0123456789abcdef0", false}, // long hex
		{"123-0123456789ABCDEF", false},  // uppercase hex
		{"12-3-0123456789abcdef", false}, // extra dash
		{" 123-0123456789abcdef", false}, // leading space
		{"", false},
		{"abc-0123456789abcdef", false}, // non-numeric pid
		{"0-0123456789abcdef", false},   // pid 0 is not a pid
		{"007-0123456789abcdef", false}, // a pid never carries a leading zero
	}
	for _, tt := range tests {
		if got := isScratchDirName(tt.name); got != tt.want {
			t.Errorf("isScratchDirName(%q) = %v, want %v", tt.name, got, tt.want)
		}
	}
}

// TestReap_LeavesSymlinksAlone: a link inside the root — live or dangling —
// must survive, and its target outside the root must never be touched.
func TestReap_LeavesSymlinksAlone(t *testing.T) {
	skipWindowsSymlink(t)
	root := t.TempDir()
	t.Setenv("GHOST_SCRATCH_DIR", root)

	target := t.TempDir()
	link := filepath.Join(root, "link")
	must(t, os.Symlink(target, link))
	dangling := filepath.Join(root, "dangling")
	must(t, os.Symlink(filepath.Join(root, "does-not-exist"), dangling))

	removed, _, err := Reap(0)
	if err != nil {
		t.Fatalf("Reap: %v", err)
	}
	if removed != 0 {
		t.Errorf("removed = %d, want 0", removed)
	}
	for _, p := range []string{link, dangling} {
		if _, err := os.Lstat(p); err != nil {
			t.Errorf("symlink %s removed: %v", p, err)
		}
	}
	if info, err := os.Stat(target); err != nil || !info.IsDir() {
		t.Errorf("symlink target damaged (stat err %v)", err)
	}
}

// TestReap_ReclaimsOnlyStaleReassessIDFiles: the follow-up id file is the one
// markerless regular file Reap may collect, on its own window (idFileStaleAge)
// rather than maxAge. A directory or link wearing the name is never removed and
// a foreign name is never touched. Every case runs Reap(0), so a survivor
// survives on the id file's own window and nothing else.
func TestReap_ReclaimsOnlyStaleReassessIDFiles(t *testing.T) {
	const (
		idFile   = "supersede-reassess-myproj-20260101T000000Z-123-0123456789abcdef.ids"
		upperHex = "supersede-reassess-myproj-20260101T000000Z-123-0123456789ABCDEF.ids"
		nearMiss = "supersede-reassess-myproj-20260101T000000Z-123-0123456789abcdef.ids.txt"
	)
	stale := time.Now().Add(-90 * 24 * time.Hour)
	oneHourInside := time.Now().Add(-idFileStaleAge + time.Hour)
	oneHourPast := time.Now().Add(-idFileStaleAge - time.Hour)

	tests := []struct {
		name       string
		entry      string
		kind       string // "file", "dir" or "symlink"
		mtime      time.Time
		wantRemove bool
	}{
		{"an id file past its window is reclaimed", idFile, "file", stale, true},
		{"an id file one hour inside its window is kept", idFile, "file", oneHourInside, false},
		{"an id file one hour past its window is reclaimed", idFile, "file", oneHourPast, true},
		{"an id file with uppercase hex is not ours", upperHex, "file", stale, false},
		{"an id file name with a longer suffix is not ours", nearMiss, "file", stale, false},
		{"a directory wearing an id file's name is kept", idFile, "dir", stale, false},
		{"a symlink wearing an id file's name is kept", idFile, "symlink", stale, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			t.Setenv("GHOST_SCRATCH_DIR", root)
			entry := filepath.Join(root, tt.entry)
			switch tt.kind {
			case "dir":
				must(t, os.Mkdir(entry, 0o700))
			case "symlink":
				skipWindowsSymlink(t)
				// The target lives outside the root: a surviving link must leave
				// its target alone.
				target := filepath.Join(t.TempDir(), "outside.txt")
				must(t, os.WriteFile(target, []byte("keep me\n"), 0o600))
				must(t, os.Symlink(target, entry))
				defer func() {
					if _, err := os.Stat(target); err != nil {
						t.Errorf("the link's target outside the root was damaged: %v", err)
					}
				}()
			default:
				must(t, os.WriteFile(entry, []byte(idFileBody), 0o600))
			}
			must(t, os.Chtimes(entry, tt.mtime, tt.mtime))

			removed, _, err := Reap(0)
			if err != nil {
				t.Fatalf("Reap: %v", err)
			}
			if (removed == 1) != tt.wantRemove || removed > 1 {
				t.Errorf("removed = %d, want removal = %v", removed, tt.wantRemove)
			}
			if _, statErr := os.Lstat(entry); os.IsNotExist(statErr) != tt.wantRemove {
				t.Errorf("entry removed = %v, want %v (lstat err %v)", os.IsNotExist(statErr), tt.wantRemove, statErr)
			}
		})
	}
}

func TestReap_UnusableRootReturnsError(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "not-a-dir")
	must(t, os.WriteFile(blocker, []byte("x"), 0o600))
	t.Setenv("GHOST_SCRATCH_DIR", filepath.Join(blocker, "scratch"))

	if removed, _, err := Reap(time.Hour); err == nil || removed != 0 {
		t.Errorf("Reap = (removed %d, %v), want an error and nothing removed", removed, err)
	}
}

func TestOwnerMarker_RefusesSymlink(t *testing.T) {
	skipWindowsSymlink(t)
	dir := filepath.Join(t.TempDir(), "entry")
	must(t, os.Mkdir(dir, 0o700))
	outside := filepath.Join(t.TempDir(), "outside")
	must(t, os.WriteFile(outside, []byte("pid=1\nstart=none\n"), 0o600))
	must(t, os.Symlink(outside, filepath.Join(dir, ownerFile)))
	if pid, start, ok := ownerMarker(dir); ok {
		t.Fatalf("ownerMarker followed a symlinked marker: pid=%d start=%q", pid, start)
	}
}

func TestOwnerMarker_RefusesOversized(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "entry")
	must(t, os.Mkdir(dir, 0o700))
	marker := filepath.Join(dir, ownerFile)
	valid := "pid=" + strconv.Itoa(os.Getpid()) + "\nstart=none\n"
	must(t, os.WriteFile(marker, []byte(valid), 0o600))
	if pid, start, ok := ownerMarker(dir); !ok {
		t.Fatalf("valid unpadded marker refused: pid=%d start=%q", pid, start)
	}

	big := valid + strings.Repeat("a", ownerMarkerMaxBytes+1-len(valid))
	must(t, os.WriteFile(marker, []byte(big), 0o600))
	if pid, start, ok := ownerMarker(dir); ok {
		t.Fatalf("ownerMarker accepted an oversized marker: pid=%d start=%q", pid, start)
	}
}

// TestRelease_RemovesTreeWithRestrictiveSubdir: a subtree left without write
// permission must still be removed by Release. Reproduction for issue #1960.
func TestRelease_RemovesTreeWithRestrictiveSubdir(t *testing.T) {
	t.Setenv("GHOST_SCRATCH_DIR", t.TempDir())
	d, err := Open()
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	restrictiveSub(t, d.Path())

	d.Release()
	if _, err := os.Stat(d.Path()); !os.IsNotExist(err) {
		t.Fatalf("Release left dir behind (stat err %v)", err)
	}
}

// TestReap_RemovesDeadOwnerEntryWithRestrictiveSubdir: an aged dead-owner entry
// with a 0555 subtree is removed, counted as removed and not as failed.
// Reproduction for issue #1960.
func TestReap_RemovesDeadOwnerEntryWithRestrictiveSubdir(t *testing.T) {
	root := t.TempDir()
	t.Setenv("GHOST_SCRATCH_DIR", root)

	dir := writeOwnerDir(t, root, "999-0123456789abcdef", "pid="+strconv.Itoa(1<<30)+"\ntoken=aa\nstart=gone\n")
	restrictiveSub(t, dir)
	old := time.Now().Add(-25 * time.Hour)
	must(t, os.Chtimes(dir, old, old))

	removed, failed, err := Reap(time.Hour)
	if err != nil {
		t.Fatalf("Reap: %v", err)
	}
	if removed != 1 || failed != 0 {
		t.Errorf("Reap = (removed=%d, failed=%d), want (1, 0)", removed, failed)
	}
	if _, statErr := os.Stat(dir); !os.IsNotExist(statErr) {
		t.Fatalf("entry survived Reap (stat err %v)", statErr)
	}
}

// TestReap_ReportsUnremovable: an entry or stale id file whose removal fails is
// counted as failed, not silently dropped, with no error and the entry on disk.
func TestReap_ReportsUnremovable(t *testing.T) {
	tests := []struct {
		name   string
		seam   *func(string) error
		make   func(t *testing.T, root string) string
		maxAge time.Duration
	}{
		{"dir", &removeTree, func(t *testing.T, root string) string {
			return writeOwnerDir(t, root, "999-0123456789abcdef", "pid="+strconv.Itoa(1<<30)+"\ntoken=aa\nstart=gone\n")
		}, time.Hour},
		{"id file", &removeFile, func(t *testing.T, root string) string {
			path := filepath.Join(root, "supersede-reassess-proj-20260101T000000Z-123-0123456789abcdef.ids")
			must(t, os.WriteFile(path, []byte(idFileBody), 0o600))
			old := time.Now().Add(-90 * 24 * time.Hour)
			must(t, os.Chtimes(path, old, old))
			return path
		}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			t.Setenv("GHOST_SCRATCH_DIR", root)
			path := tt.make(t, root)
			prev := *tt.seam
			*tt.seam = func(string) error { return errForcedRemoval }
			t.Cleanup(func() { *tt.seam = prev })

			removed, failed, err := Reap(tt.maxAge)
			if err != nil {
				t.Fatalf("Reap: %v", err)
			}
			if removed != 0 || failed != 1 {
				t.Errorf("Reap = (removed=%d, failed=%d), want (0, 1)", removed, failed)
			}
			if _, statErr := os.Lstat(path); statErr != nil {
				t.Errorf("entry vanished although removal failed: %v", statErr)
			}
		})
	}
}

// TestSameRealDir_RefusesSymlinkAndRename: a symlink swapped in, or a different
// directory renamed into place, is not the directory first lstat'ed.
func TestSameRealDir_RefusesSymlinkAndRename(t *testing.T) {
	skipWindowsSymlink(t)
	root := t.TempDir()
	dir := filepath.Join(root, "dir")
	must(t, os.Mkdir(dir, 0o700))
	want, err := os.Lstat(dir)
	must(t, err)
	if err := sameRealDir(dir, want); err != nil {
		t.Fatalf("sameRealDir on the unchanged directory: %v", err)
	}

	target := filepath.Join(root, "target")
	must(t, os.Mkdir(target, 0o700))
	must(t, os.Rename(dir, filepath.Join(root, "moved")))
	must(t, os.Symlink(target, dir))
	if err := sameRealDir(dir, want); err == nil {
		t.Fatal("sameRealDir accepted a symlink swapped in for the directory")
	}
	must(t, os.Remove(dir))

	other := filepath.Join(root, "other")
	must(t, os.Mkdir(other, 0o700))
	must(t, os.Rename(other, dir))
	if err := sameRealDir(dir, want); err == nil {
		t.Fatal("sameRealDir accepted a different directory renamed into place")
	}
}

// TestMakeRemovable_NeverFollowsASymlink: the target's mode is untouched and its
// contents are not recursed into.
func TestMakeRemovable_NeverFollowsASymlink(t *testing.T) {
	skipWindowsSymlink(t)
	target := t.TempDir()
	must(t, os.WriteFile(filepath.Join(target, "keep"), []byte("keep me\n"), 0o600))
	must(t, os.Chmod(target, 0o555))
	t.Cleanup(func() { _ = os.Chmod(target, 0o700) })
	link := filepath.Join(t.TempDir(), "linked")
	must(t, os.Symlink(target, link))

	makeRemovable(link)

	info, err := os.Stat(target)
	must(t, err)
	if perm := info.Mode().Perm(); perm != 0o555 {
		t.Errorf("target mode = %o after makeRemovable on the link, want 555 (never followed)", perm)
	}
	if _, err := os.Stat(link); err != nil {
		t.Errorf("link was removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(target, "keep")); err != nil {
		t.Errorf("link target's contents were damaged: %v", err)
	}
}
