//go:build !windows

package mcpinit

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The failure marker lives in the data dir, which every local process can
// write, so an entry wearing a marker's name is as untrusted as its bytes: a
// FIFO there used to block the read in open until a writer appeared, and a
// symlink was followed to whatever it named. A marker is always a regular file
// this package wrote, so anything else is no marker — on the alert and on the
// clear path alike.

// markerDeadline bounds every call here that could block on a FIFO, so a
// regression fails the test instead of wedging the package run.
const markerDeadline = 5 * time.Second

// mkfifoAt creates a FIFO at path — the marker path itself, which the
// transcript helper's mkfifo (a temp dir of its own) cannot name — or skips
// when the platform's filesystem refuses one.
func mkfifoAt(t *testing.T, path string) {
	t.Helper()
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Skipf("mkfifo unavailable: %v", err)
	}
}

// withMarkerDeadline runs fn and fails the test if it has not returned within
// markerDeadline. On a timeout each FIFO in unblock is opened for writing and
// closed, which hands a blocked reader EOF. O_NONBLOCK makes that open fail
// rather than wait when nobody is reading, and the loop repeats it, because a
// read that blocked on one FIFO can block on the next one after it. The
// goroutine is joined before the failure, so nothing is left running.
func withMarkerDeadline(t *testing.T, unblock []string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
		return
	case <-time.After(markerDeadline):
	}
	for {
		for _, p := range unblock {
			if w, err := os.OpenFile(p, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
				_ = w.Close()
			}
		}
		select {
		case <-done:
			t.Fatalf("the marker read blocked for more than %s on an entry that is not a regular file", markerDeadline)
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// markerSessionStart runs the session-start hook for a session in cwd under
// the deadline and returns its first stdout line and full stdout.
func markerSessionStart(t *testing.T, unblock []string, cwd string) (string, string) {
	t.Helper()
	var first, out string
	withMarkerDeadline(t, unblock, func() {
		first, out = alertFirstLine(t, cwd)
	})
	return first, out
}

// The open itself, exercised directly, as it is when a marker path is swapped
// between a caller's stat and its open: each case must come back refused, at
// once, and without a byte of the marker.
func TestReadMarkerAtPathRefusesWhatIsNotARegularFile(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "planted.fifo")
	mkfifoAt(t, fifo)
	marker := filepath.Join(dir, "real.json")
	at := time.Now().UTC().Format(time.RFC3339)
	if err := os.WriteFile(marker, mustJSON(testMarker{Project: "p1", At: at}), 0o600); err != nil {
		t.Fatal(err)
	}
	toFIFO := filepath.Join(dir, "link-to-fifo.json")
	toMarker := filepath.Join(dir, "link-to-marker.json")
	for link, target := range map[string]string{toFIFO: fifo, toMarker: marker} {
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("FIFO", func(t *testing.T) {
		var got *lifecycleFailureMarker
		withMarkerDeadline(t, []string{fifo}, func() { got = readMarkerAtPath(fifo) })
		if got != nil {
			t.Errorf("readMarkerAtPath(FIFO) = %+v, want nil", got)
		}
	})
	for name, link := range map[string]string{"symlink to a FIFO": toFIFO, "symlink to a marker": toMarker} {
		t.Run(name, func(t *testing.T) {
			var got *lifecycleFailureMarker
			withMarkerDeadline(t, []string{fifo}, func() { got = readMarkerAtPath(link) })
			if got != nil {
				t.Errorf("readMarkerAtPath(%s) = %+v, want nil", name, got)
			}
		})
	}
	t.Run("regular marker", func(t *testing.T) {
		got := readMarkerAtPath(marker)
		if got == nil || got.Project != "p1" {
			t.Errorf("readMarkerAtPath(regular) = %+v, want the p1 marker", got)
		}
	})
}

// The alert surface, and the hang the issue describes: a FIFO at the
// project's marker path made the session-start hook block in open until a
// writer appeared, so the host waited or killed it at its own timeout on every
// session start.
func TestSessionStart_AFIFOAtTheMarkerPathAlertsNothing(t *testing.T) {
	isolatedHome(t)
	xdgHome := t.TempDir()
	t.Setenv("XDG_DATA_HOME", xdgHome)
	projDir := seedSessionProject(t, xdgHome, "p1", "myproj")

	fifo := markerPath(xdgHome, "p1")
	mkfifoAt(t, fifo)

	_, out := markerSessionStart(t, []string{fifo}, projDir)
	if strings.Contains(out, "Ghost maintenance alert") {
		t.Errorf("a FIFO at the marker path must read as no marker, got:\n%s", out)
	}
}

// The symlink half: a link at the marker path was followed to whatever it
// named — a FIFO (a hang) or a real marker in another directory (a read of a
// file the product never wrote into the data dir).
func TestSessionStart_ASymlinkAtTheMarkerPathAlertsNothing(t *testing.T) {
	isolatedHome(t)
	xdgHome := t.TempDir()
	t.Setenv("XDG_DATA_HOME", xdgHome)
	projDir := seedSessionProject(t, xdgHome, "p1", "myproj")

	fifo := filepath.Join(t.TempDir(), "elsewhere.fifo")
	mkfifoAt(t, fifo)
	at := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	real := filepath.Join(t.TempDir(), "real.json")
	if err := os.WriteFile(real, mustJSON(testMarker{
		Project: "p1", PhasesFailed: []string{"reflect"}, Error: "boom", At: at,
	}), 0o600); err != nil {
		t.Fatal(err)
	}

	for name, target := range map[string]string{"to a FIFO": fifo, "to a real marker": real} {
		t.Run(name, func(t *testing.T) {
			link := markerPath(xdgHome, "p1")
			if err := os.Symlink(target, link); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Remove(link) })
			_, out := markerSessionStart(t, []string{fifo}, projDir)
			if strings.Contains(out, "Ghost maintenance alert") {
				t.Errorf("a symlink at the marker path must not be followed (%s), got:\n%s", name, out)
			}
		})
	}
}

// The no-project read globs every marker in the data dir, so a FIFO planted
// under the glob — `ghost context`'s path, and the session-start read for a
// session that resolves to no project — hung the read before the fix.
func TestSessionStart_AFIFOAtTheLegacyMarkerPathAlertsNothing(t *testing.T) {
	isolatedHome(t)
	xdgHome := t.TempDir()
	t.Setenv("XDG_DATA_HOME", xdgHome)
	if err := os.MkdirAll(filepath.Join(xdgHome, "ghost"), 0o700); err != nil {
		t.Fatal(err)
	}

	fifo := filepath.Join(xdgHome, "ghost", "lifecycle-last-failure-planted.json")
	mkfifoAt(t, fifo)

	_, out := markerSessionStart(t, []string{fifo}, filepath.Join(t.TempDir(), "no-such-project"))
	if strings.Contains(out, "Ghost maintenance alert") {
		t.Errorf("a FIFO under the marker glob must read as no marker, got:\n%s", out)
	}
}

// The clear path reads the same way: a clean run's ClearLifecycleFailure goes
// through readMarkerAtPath, and a FIFO at the legacy path blocked it before the
// fix. The planted entry is left in place — the product never wrote it, so it
// is not this package's marker to remove.
func TestClearLifecycleFailureReturnsAtOnceWithAFIFOAtTheLegacyMarkerPath(t *testing.T) {
	isolatedHome(t)
	xdgHome := t.TempDir()
	t.Setenv("XDG_DATA_HOME", xdgHome)
	seedProject(t, xdgHome, "p1", "/tmp/my-project", "My Project")

	fifo := filepath.Join(xdgHome, "ghost", legacyMarkerFile)
	mkfifoAt(t, fifo)

	var err error
	withMarkerDeadline(t, []string{fifo}, func() { err = ClearLifecycleFailure("p1") })
	if err != nil {
		t.Errorf("ClearLifecycleFailure = %v, want nil (a FIFO is not a marker)", err)
	}
	if _, err := os.Lstat(fifo); err != nil {
		t.Errorf("a FIFO at the legacy path is not this package's marker to remove: %v", err)
	}
}

// The control: a regular marker beside a planted FIFO is still read and still
// alerts, so the refusal above is about the path's type and not about the
// marker's absence.
func TestSessionStart_RegularMarkerStillAlertsBesideAFIFO(t *testing.T) {
	isolatedHome(t)
	xdgHome := t.TempDir()
	t.Setenv("XDG_DATA_HOME", xdgHome)
	projDir := seedSessionProject(t, xdgHome, "p1", "myproj")

	at := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	writeMarkerFile(t, xdgHome, testMarker{
		Project: "p1", PhasesFailed: []string{"reflect"}, Error: "boom", At: at, Version: 1,
	})
	mkfifoAt(t, filepath.Join(xdgHome, "ghost", "lifecycle-last-failure-planted.json"))

	first, _ := markerSessionStart(t, nil, projDir)
	if !strings.HasPrefix(first, "**Ghost maintenance alert:**") {
		t.Errorf("a regular marker must still alert, got: %s", first)
	}
}
