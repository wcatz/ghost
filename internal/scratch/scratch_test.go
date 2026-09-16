package scratch

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
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
	liveMarker := "pid=" + strconv.Itoa(os.Getpid()) + "\ntoken=bb\n"
	oldTime := time.Now().Add(-2 * time.Hour)
	// scratchName has exactly the shape Open creates: without a readable
	// marker, the name is the only ownership evidence Reap accepts.
	const scratchName = "123-0123456789abcdef"

	tests := []struct {
		name       string
		dirName    string
		marker     string
		mtime      *time.Time
		wantRemove bool
	}{
		{"dead owner removed", "entry", "pid=" + strconv.Itoa(deadPid) + "\ntoken=aa\n", nil, true},
		{"live owner fresh spared", "entry", liveMarker, nil, false},
		{"live owner old mtime spared", "entry", liveMarker, &oldTime, false},
		{"malformed pid scratch-shaped old removed", scratchName, "pid=notanumber\ntoken=cc\n", &oldTime, true},
		{"malformed pid scratch-shaped fresh spared", scratchName, "pid=notanumber\ntoken=dd\n", nil, false},
		{"missing marker scratch-shaped old removed", scratchName, "", &oldTime, true},
		{"missing marker scratch-shaped fresh spared", scratchName, "", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
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
