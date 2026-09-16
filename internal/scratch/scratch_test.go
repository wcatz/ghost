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

	tests := []struct {
		name       string
		marker     string
		mtime      *time.Time
		wantRemove bool
	}{
		{"dead owner removed", "pid=" + strconv.Itoa(deadPid) + "\ntoken=aa\n", nil, true},
		{"live owner spared", liveMarker, nil, false},
		{"old live owner still spared", liveMarker, &oldTime, false},
		{"malformed pid old removed", "pid=notanumber\ntoken=cc\n", &oldTime, true},
		{"malformed pid fresh spared", "pid=notanumber\ntoken=dd\n", nil, false},
		{"missing marker old removed", "", &oldTime, true},
		{"missing marker fresh spared", "", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			t.Setenv("GHOST_SCRATCH_DIR", root)
			dir := writeOwnerDir(t, root, "entry", tt.marker)
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
