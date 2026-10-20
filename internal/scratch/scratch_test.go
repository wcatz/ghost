package scratch

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wcatz/ghost/internal/procstat"
)

// writeOwnerDir creates a scratch-dir stand-in named name under root with an
// optional .owner marker, so Reap's classification can be tested without Open.
func writeOwnerDir(t *testing.T, root, name, marker string) string {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	if marker != "" {
		if err := os.WriteFile(filepath.Join(dir, ownerFile), []byte(marker), 0o600); err != nil {
			t.Fatalf("write marker: %v", err)
		}
	}
	return dir
}

func TestRoot_UsesEnvOverride(t *testing.T) {
	root := filepath.Join(t.TempDir(), "scratch")
	t.Setenv("GHOST_SCRATCH_DIR", root)

	got, err := Root()
	if err != nil {
		t.Fatalf("Root: %v", err)
	}
	if got != root {
		t.Errorf("Root = %q, want %q", got, root)
	}
	info, err := os.Stat(root)
	if err != nil {
		t.Fatalf("stat root: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Errorf("root mode = %o, want 700", perm)
	}
}

func TestRoot_DefaultsUnderDataDir(t *testing.T) {
	dataHome := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dataHome)
	t.Setenv("GHOST_SCRATCH_DIR", "")

	root, err := Root()
	if err != nil {
		t.Fatalf("Root: %v", err)
	}
	want := filepath.Join(dataHome, "ghost", "scratch")
	if root != want {
		t.Errorf("Root = %q, want %q", root, want)
	}
	if info, err := os.Stat(root); err != nil {
		t.Fatalf("stat root: %v", err)
	} else if perm := info.Mode().Perm(); perm != 0o700 {
		t.Errorf("root mode = %o, want 700", perm)
	}
}

func TestOpen_CreatesPrivateDirWithOwnerMarker(t *testing.T) {
	root := t.TempDir()
	t.Setenv("GHOST_SCRATCH_DIR", root)

	d, err := Open()
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Release()

	info, err := os.Stat(d.Path())
	if err != nil {
		t.Fatalf("stat scratch dir: %v", err)
	}
	if !info.IsDir() {
		t.Fatalf("scratch path %q is not a directory", d.Path())
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Errorf("scratch dir mode = %o, want 700", perm)
	}
	if got := filepath.Dir(d.Path()); got != root {
		t.Errorf("scratch dir parent = %q, want the root %q", got, root)
	}

	marker, err := os.ReadFile(filepath.Join(d.Path(), ownerFile))
	if err != nil {
		t.Fatalf("read owner marker: %v", err)
	}
	if want := "pid=" + strconv.Itoa(os.Getpid()); !strings.Contains(string(marker), want) {
		t.Errorf("marker %q missing %q", marker, want)
	}
	if !strings.Contains(string(marker), "token=") {
		t.Errorf("marker %q missing token", marker)
	}
	// The creation-time token is what lets Reap detect a recycled pid; it must
	// be recorded whenever the platform can supply it, and the explicit
	// sentinel must be recorded when it cannot — a missing line would be
	// mistaken for a true legacy marker and lose the live-owner shield.
	if start, ok := procstat.StartTime(os.Getpid()); ok {
		if want := "start=" + start; !strings.Contains(string(marker), want) {
			t.Errorf("marker %q missing %q", marker, want)
		}
	} else if !strings.Contains(string(marker), "start="+startNone) {
		t.Errorf("marker %q missing sentinel %q", marker, startNone)
	}
}

// TestOwnerMarkerText pins the marker-writing helper's two shapes directly,
// including the ok=false path that cannot be forced through Open on a platform
// where StartTime succeeds: an unavailable token is written as the explicit
// sentinel, never as a missing line.
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
			marker := ownerMarkerText(4242, "aabbccddeeff0011", tt.start, tt.haveStart)
			want := "pid=4242\ntoken=aabbccddeeff0011\nstart=" + tt.wantStart + "\n"
			if marker != want {
				t.Errorf("ownerMarkerText = %q, want %q", marker, want)
			}
		})
	}
}

// TestOpen_IgnoresInheritedTempDir pins the whole point of the owned root: a
// broken or full system temp dir must not stop Ghost from preparing a
// harness scratch dir, because the inherited dir is not consulted at all.
func TestOpen_IgnoresInheritedTempDir(t *testing.T) {
	root := t.TempDir()
	t.Setenv("GHOST_SCRATCH_DIR", root)
	brokenTemp := filepath.Join(t.TempDir(), "missing", "temp")
	for _, key := range []string{"TMPDIR", "TMP", "TEMP"} {
		t.Setenv(key, brokenTemp)
	}

	d, err := Open()
	if err != nil {
		t.Fatalf("Open with broken inherited TMPDIR: %v", err)
	}
	defer d.Release()

	if got := filepath.Dir(d.Path()); got != root {
		t.Errorf("scratch dir parent = %q, want the root %q", got, root)
	}
	if _, err := os.Stat(brokenTemp); !os.IsNotExist(err) {
		t.Errorf("Open touched the inherited temp path %q (stat err %v)", brokenTemp, err)
	}
}

func TestRelease_RemovesAndIsIdempotent(t *testing.T) {
	root := t.TempDir()
	t.Setenv("GHOST_SCRATCH_DIR", root)

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
	// deadPid is far beyond every platform's pid space (Linux caps pids at
	// 4<<20), so no live process can own it. Tests must never use a real pid
	// such as 1, which is always alive.
	const deadPid = 1 << 30
	livePid := os.Getpid()
	liveStart, haveStart := procstat.StartTime(livePid)
	oldTime := time.Now().Add(-2 * time.Hour)
	// scratchName has exactly the shape Open creates: without a readable
	// marker, the name is the only ownership evidence Reap accepts.
	const scratchName = "123-0123456789abcdef"

	// markerFor builds an owner marker: pid and random token, plus a start
	// line when start is non-empty. An empty start omits the line entirely,
	// which is the true pre-token legacy form — Open itself always writes
	// either a token or the startNone sentinel.
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
		{"live owner with matching token fresh spared", "entry", markerFor(livePid, liveStart), nil, true, false},
		{"live owner with matching token old mtime spared", "entry", markerFor(livePid, liveStart), &oldTime, true, false},
		{"live owner with mismatched token removed", "entry", markerFor(livePid, "definitely-not-the-real-token"), nil, true, true},
		{"sentinel live owner old mtime spared", "entry", markerFor(livePid, startNone), &oldTime, false, false},
		{"sentinel dead owner removed", "entry", markerFor(deadPid, startNone), nil, false, true},
		{"legacy live owner fresh spared", "entry", markerFor(livePid, ""), nil, false, false},
		{"legacy live owner old removed", "entry", markerFor(livePid, ""), &oldTime, false, true},
		{"malformed pid scratch-shaped old removed", scratchName, "pid=notanumber\ntoken=cc\n", &oldTime, false, true},
		{"malformed pid scratch-shaped fresh spared", scratchName, "pid=notanumber\ntoken=dd\n", nil, false, false},
		{"missing marker scratch-shaped old removed", scratchName, "", &oldTime, false, true},
		{"missing marker scratch-shaped fresh spared", scratchName, "", nil, false, false},
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
				if err := os.Chtimes(dir, *tt.mtime, *tt.mtime); err != nil {
					t.Fatalf("chtimes: %v", err)
				}
			}

			removed, err := Reap(time.Hour)
			if err != nil {
				t.Fatalf("Reap: %v", err)
			}
			wantCount := 0
			if tt.wantRemove {
				wantCount = 1
			}
			if removed != wantCount {
				t.Errorf("removed = %d, want %d", removed, wantCount)
			}
			if _, err := os.Stat(dir); os.IsNotExist(err) != tt.wantRemove {
				t.Errorf("entry removed = %v, want %v (stat err %v)", os.IsNotExist(err), tt.wantRemove, err)
			}
		})
	}
}

// TestReap_LeavesForeignEntriesAlone proves Reap deletes only what Ghost
// created: an old foreign directory, file, or malformed-marker directory under
// the root survives, because $GHOST_SCRATCH_DIR may point at a shared
// directory. The same assertions run against the explicit override root and
// against the default <dataDir>/scratch, with both resolving to the same path.
func TestReap_LeavesForeignEntriesAlone(t *testing.T) {
	oldTime := time.Now().Add(-2 * time.Hour)

	for _, tc := range []struct {
		name string
		set  func(t *testing.T, root string)
	}{
		{
			name: "env override root",
			set: func(t *testing.T, root string) {
				t.Setenv("GHOST_SCRATCH_DIR", root)
			},
		},
		{
			name: "default data dir root",
			set: func(t *testing.T, root string) {
				t.Setenv("GHOST_SCRATCH_DIR", "")
				t.Setenv("XDG_DATA_HOME", filepath.Dir(filepath.Dir(root)))
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dataHome := t.TempDir()
			root := filepath.Join(dataHome, "ghost", "scratch")
			if err := os.MkdirAll(root, 0o700); err != nil {
				t.Fatal(err)
			}
			tc.set(t, root)
			if got, err := Root(); err != nil {
				t.Fatalf("Root: %v", err)
			} else if got != root {
				t.Fatalf("Root = %q, want %q", got, root)
			}

			foreignDir := filepath.Join(root, "not-ghost")
			if err := os.Mkdir(foreignDir, 0o700); err != nil {
				t.Fatal(err)
			}
			foreignFile := filepath.Join(root, "some-notes.txt")
			if err := os.WriteFile(foreignFile, []byte("not ghost's"), 0o600); err != nil {
				t.Fatal(err)
			}
			foreignMalformed := writeOwnerDir(t, root, "foreign-with-marker", "pid=notanumber\n")
			foreign := []string{foreignDir, foreignFile, foreignMalformed}
			for _, p := range foreign {
				if err := os.Chtimes(p, oldTime, oldTime); err != nil {
					t.Fatalf("chtimes %s: %v", p, err)
				}
			}

			removed, err := Reap(time.Hour)
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
		})
	}
}

// TestReap_LeavesMarkerlessEntriesWithImpossiblePidsAlone covers the names
// Reap cannot place in Open's shape. A pid segment of "0", or one carrying a
// leading zero, is not a pid any Ghost run can have produced, so an entry
// wearing one is not Ghost's — and a markerless entry is only ever removed on
// the strength of its name, so it is left alone however old it is. The two
// eligible kinds are covered: a per-invocation directory, whose age bound is
// the caller's maxAge, and the follow-up id file, whose age bound is its own
// window. The entries that DO name a pid Open could have produced are removed
// in the same reap, so a survivor is surviving on the pid segment and on
// nothing else.
func TestReap_LeavesMarkerlessEntriesWithImpossiblePidsAlone(t *testing.T) {
	root := t.TempDir()
	t.Setenv("GHOST_SCRATCH_DIR", root)

	// Older than maxAge for the directories, older than the id file's own
	// window for the file: each entry is past every age bound that could
	// otherwise remove it.
	oldTime := time.Now().Add(-2 * time.Hour)
	pastWindow := time.Now().Add(-idFileStaleAge - time.Hour)

	impossible := []struct {
		name  string
		entry string
		kind  string // "dir" or "file"
		mtime time.Time
	}{
		{"a directory with pid 0", "0-0123456789abcdef", "dir", oldTime},
		{"a directory with a leading-zero pid", "007-0123456789abcdef", "dir", oldTime},
		{"an id file with pid 0", "supersede-reassess-p-20260101T000000Z-0-0123456789abcdef.ids", "file", pastWindow},
	}
	for _, tt := range impossible {
		path := filepath.Join(root, tt.entry)
		if tt.kind == "dir" {
			if err := os.Mkdir(path, 0o700); err != nil {
				t.Fatalf("mkdir %s: %v", path, err)
			}
		} else if err := os.WriteFile(path, []byte("# one id\naaaaaaaa1111111111111111111111\n"), 0o600); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
		if err := os.Chtimes(path, tt.mtime, tt.mtime); err != nil {
			t.Fatalf("chtimes %s: %v", path, err)
		}
	}

	// The two controls: the same two kinds with a pid Open could have
	// produced, both of which this reap must collect.
	ownDir := filepath.Join(root, "123-0123456789abcdef")
	if err := os.Mkdir(ownDir, 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", ownDir, err)
	}
	if err := os.Chtimes(ownDir, oldTime, oldTime); err != nil {
		t.Fatalf("chtimes %s: %v", ownDir, err)
	}
	ownFile := filepath.Join(root, "supersede-reassess-p-20260101T000000Z-123-0123456789abcdef.ids")
	if err := os.WriteFile(ownFile, []byte("# one id\naaaaaaaa1111111111111111111111\n"), 0o600); err != nil {
		t.Fatalf("write %s: %v", ownFile, err)
	}
	if err := os.Chtimes(ownFile, pastWindow, pastWindow); err != nil {
		t.Fatalf("chtimes %s: %v", ownFile, err)
	}

	removed, err := Reap(time.Hour)
	if err != nil {
		t.Fatalf("Reap: %v", err)
	}
	if removed != 2 {
		t.Errorf("removed = %d, want 2 (the two entries naming a pid Ghost could have produced)", removed)
	}
	for _, tt := range impossible {
		if _, err := os.Lstat(filepath.Join(root, tt.entry)); err != nil {
			t.Errorf("%s was removed: %v", tt.name, err)
		}
	}
	for _, p := range []string{ownDir, ownFile} {
		if _, err := os.Lstat(p); !os.IsNotExist(err) {
			t.Errorf("%s survived a reap that should have collected it (stat err %v)", p, err)
		}
	}
}

// TestUniqueSuffix: the token Open names its directories with is the one shape
// in this package, so a caller that needs a name nothing else will take uses the
// same one. Two calls must differ, and the result must be a shape Reap's
// markerless-entry rule recognises — not because the caller wants reaping, but
// because a second naming scheme is a second thing to get wrong.
func TestUniqueSuffix(t *testing.T) {
	first, err := UniqueSuffix()
	if err != nil {
		t.Fatalf("UniqueSuffix: %v", err)
	}
	second, err := UniqueSuffix()
	if err != nil {
		t.Fatalf("UniqueSuffix: %v", err)
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
		{"123-0123456789abcdeg", false},  // non-hex letter
		{"12-3-0123456789abcdef", false}, // extra dash
		{" 123-0123456789abcdef", false}, // leading space
		{"123-0123456789abcdef ", false}, // trailing space
		{"", false},                      // empty
		{"abc-0123456789abcdef", false},  // non-numeric pid
		{"0-0123456789abcdef", false},    // pid 0 is not a pid
		{"007-0123456789abcdef", false},  // a pid never carries a leading zero
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isScratchDirName(tt.name); got != tt.want {
				t.Errorf("isScratchDirName(%q) = %v, want %v", tt.name, got, tt.want)
			}
		})
	}
}

// TestReap_LeavesSymlinksAlone covers the symlink guard: a link inside the
// root — live or dangling — must survive, and its target outside the root must
// never be touched.
func TestReap_LeavesSymlinksAlone(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires privileges on Windows")
	}
	root := t.TempDir()
	t.Setenv("GHOST_SCRATCH_DIR", root)

	target := t.TempDir()
	link := filepath.Join(root, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	dangling := filepath.Join(root, "dangling")
	if err := os.Symlink(filepath.Join(root, "does-not-exist"), dangling); err != nil {
		t.Fatalf("dangling symlink: %v", err)
	}

	removed, err := Reap(0)
	if err != nil {
		t.Fatalf("Reap: %v", err)
	}
	if removed != 0 {
		t.Errorf("removed = %d, want 0", removed)
	}
	if _, err := os.Lstat(link); err != nil {
		t.Errorf("symlink removed: %v", err)
	}
	if info, err := os.Stat(target); err != nil || !info.IsDir() {
		t.Errorf("symlink target damaged (stat err %v)", err)
	}
	if _, err := os.Lstat(dangling); err != nil {
		t.Errorf("dangling symlink removed: %v", err)
	}
}

// TestReap_ReclaimsOnlyStaleReassessIDFiles: the follow-up id file
// (supersede-reassess-<project>-<utc>-<suffix>.ids) is the one markerless
// regular file Reap may collect, and the age bound it uses is its own window
// rather than maxAge — the file is an operator's pending repair, so 13 days
// old must survive a Reap(0) that would collect anything of the caller's own
// age, and idFileStaleAge is the boundary both ways. Everything else under the
// root keeps its old treatment: a directory wearing the name is never removed
// (it is not a file Ghost wrote), a link is never followed, and a foreign name
// is never touched.
func TestReap_ReclaimsOnlyStaleReassessIDFiles(t *testing.T) {
	const (
		idFile   = "supersede-reassess-myproj-20260101T000000Z-123-0123456789abcdef.ids"
		upperHex = "supersede-reassess-myproj-20260101T000000Z-123-0123456789ABCDEF.ids"
		nearMiss = "supersede-reassess-myproj-20260101T000000Z-123-0123456789abcdef.ids.txt"
		foreign  = "someone-elses-list.ids"
	)
	// Ages read as: past the window, inside the window (14 days), and just
	// written. Every case is reaped with Reap(0), so a survivor is surviving on
	// the id file's own window and on nothing else.
	stale := time.Now().Add(-90 * 24 * time.Hour)
	insideWindow := time.Now().Add(-13 * 24 * time.Hour)
	fresh := time.Now().Add(-time.Hour)
	// The window read off the constant rather than restated: a bound only one
	// of the two can change is a bound that drifts silently.
	oneHourInside := time.Now().Add(-idFileStaleAge + time.Hour)
	oneHourPast := time.Now().Add(-idFileStaleAge - time.Hour)

	tests := []struct {
		name       string
		entry      string
		kind       string // "file", "dir" or "symlink"
		mtime      *time.Time
		wantRemove bool
	}{
		{"an id file past its window is reclaimed", idFile, "file", &stale, true},
		{"an id file one hour inside its window is kept", idFile, "file", &oneHourInside, false},
		{"an id file one hour past its window is reclaimed", idFile, "file", &oneHourPast, true},
		{"an id file inside its window is kept", idFile, "file", &insideWindow, false},
		{"a freshly written id file is kept", idFile, "file", &fresh, false},
		{"an id file with uppercase hex is not ours", upperHex, "file", &stale, false},
		{"an id file name with a longer suffix is not ours", nearMiss, "file", &stale, false},
		{"a foreign id list is not ours", foreign, "file", &stale, false},
		{"a directory wearing an id file's name is kept", idFile, "dir", &stale, false},
		{"a symlink wearing an id file's name is kept", idFile, "symlink", &stale, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.kind == "symlink" && runtime.GOOS == "windows" {
				t.Skip("symlink creation requires privileges on Windows")
			}
			root := t.TempDir()
			t.Setenv("GHOST_SCRATCH_DIR", root)
			entry := filepath.Join(root, tt.entry)
			switch tt.kind {
			case "dir":
				if err := os.Mkdir(entry, 0o700); err != nil {
					t.Fatalf("mkdir: %v", err)
				}
			case "symlink":
				// The target lives outside the root: Reap scans the root's own
				// entries, so a link that survives must leave its target alone.
				target := filepath.Join(t.TempDir(), "outside.txt")
				if err := os.WriteFile(target, []byte("keep me\n"), 0o600); err != nil {
					t.Fatalf("write the link target: %v", err)
				}
				if err := os.Symlink(target, entry); err != nil {
					t.Fatalf("symlink: %v", err)
				}
				defer func() {
					if _, err := os.Stat(target); err != nil {
						t.Errorf("the link's target outside the root was damaged: %v", err)
					}
				}()
			default:
				if err := os.WriteFile(entry, []byte("# one id\naaaaaaaa1111111111111111111111\n"), 0o600); err != nil {
					t.Fatalf("write: %v", err)
				}
			}
			if err := os.Chtimes(entry, *tt.mtime, *tt.mtime); err != nil {
				t.Fatalf("chtimes: %v", err)
			}

			removed, err := Reap(0)
			if err != nil {
				t.Fatalf("Reap: %v", err)
			}
			wantCount := 0
			if tt.wantRemove {
				wantCount = 1
			}
			if removed != wantCount {
				t.Errorf("removed = %d, want %d", removed, wantCount)
			}
			_, statErr := os.Lstat(entry)
			if os.IsNotExist(statErr) != tt.wantRemove {
				t.Errorf("entry present = %v, want %v (lstat err %v)", !os.IsNotExist(statErr), !tt.wantRemove, statErr)
			}
		})
	}
}

func TestReap_UnusableRootReturnsError(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GHOST_SCRATCH_DIR", filepath.Join(blocker, "scratch"))

	removed, err := Reap(time.Hour)
	if err == nil {
		t.Fatal("expected an error for an unusable scratch root")
	}
	if removed != 0 {
		t.Errorf("removed = %d, want 0", removed)
	}
}
