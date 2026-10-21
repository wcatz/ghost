package mcpinit

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The self-clean removes an expired marker the read reaches, and the read that
// resolves to no project reaches EVERY marker in the data directory: it walks
// them all and returns the one that ranks first. An expired marker the ranking
// buries behind a current one was therefore never returned and never removed —
// the dead file stayed on disk for as long as the current one did. The
// per-project read does not have that hole: it walks its candidates in order
// and removes the expired one it reaches.

// TestSessionStart_ExpiredMarkerBehindACurrentOneIsSwept pins the sweep in the
// no-project read: a marker past the 30-day horizon sits behind a current one,
// the current one is the failure the alert names, and the expired one is
// removed rather than left on disk. The per-project read already removed it
// (the expired marker is that project's own file, and it is read first), so
// this is the one path where it stayed.
func TestSessionStart_ExpiredMarkerBehindACurrentOneIsSwept(t *testing.T) {
	isolatedHome(t)
	xdgHome := t.TempDir()
	t.Setenv("XDG_DATA_HOME", xdgHome)

	expiredPath := writeMarkerFile(t, xdgHome, testMarker{
		Project: "stale", PhasesFailed: []string{"reflect"}, Error: "boom",
		At: time.Now().UTC().Add(-40 * 24 * time.Hour).Format(time.RFC3339), Version: 1,
	})
	writeMarkerFile(t, xdgHome, testMarker{
		Project: "current", PhasesFailed: []string{"resolve"}, Error: "boom",
		At: time.Now().UTC().Format(time.RFC3339), Version: 1,
	})

	firstLine, out := alertFirstLine(t, filepath.Join(t.TempDir(), "no-such-project"))

	if !strings.HasPrefix(firstLine, "**Ghost maintenance alert:**") {
		t.Fatalf("the current failure must still alert, got:\n%s", out)
	}
	if !strings.Contains(firstLine, "for project current") {
		t.Errorf("the current marker must be the one reported, got: %s", firstLine)
	}
	if _, err := os.Stat(expiredPath); !os.IsNotExist(err) {
		t.Errorf("the expired marker behind the current one must be swept, stat err = %v", err)
	}
}

// TestSessionStart_TheNoProjectSweepKeepsEveryMarkerInsideItsBounds pins the
// other side of the same sweep: only an EXPIRED marker is removed. A marker in
// the kept-but-silent band (past the 14-day alert horizon, inside the 30-day
// one) is kept on disk by design, and a marker inside the tolerated clock skew
// is a real, recent failure — neither is expired, so the sweep must leave both
// alone while it removes the expired one beside them.
func TestSessionStart_TheNoProjectSweepKeepsEveryMarkerInsideItsBounds(t *testing.T) {
	isolatedHome(t)
	xdgHome := t.TempDir()
	t.Setenv("XDG_DATA_HOME", xdgHome)

	silentPath := writeMarkerFile(t, xdgHome, testMarker{
		Project: "quiet", PhasesFailed: []string{"reflect"}, Error: "old",
		At: time.Now().UTC().Add(-20 * 24 * time.Hour).Format(time.RFC3339), Version: 1,
	})
	skewedPath := writeMarkerFile(t, xdgHome, testMarker{
		Project: "skewed", PhasesFailed: []string{"resolve"}, Error: "ahead",
		At: time.Now().UTC().Add(2 * time.Hour).Format(time.RFC3339), Version: 1,
	})
	expiredPath := writeMarkerFile(t, xdgHome, testMarker{
		Project: "stale", PhasesFailed: []string{"reflect"}, Error: "boom",
		At: time.Now().UTC().Add(-40 * 24 * time.Hour).Format(time.RFC3339), Version: 1,
	})
	writeMarkerFile(t, xdgHome, testMarker{
		Project: "current", PhasesFailed: []string{"resolve"}, Error: "boom",
		At: time.Now().UTC().Format(time.RFC3339), Version: 1,
	})

	firstLine, out := alertFirstLine(t, filepath.Join(t.TempDir(), "no-such-project"))

	if !strings.HasPrefix(firstLine, "**Ghost maintenance alert:**") {
		t.Fatalf("the current failure must still alert, got:\n%s", out)
	}
	if _, err := os.Stat(expiredPath); !os.IsNotExist(err) {
		t.Errorf("the expired marker must be swept, stat err = %v", err)
	}
	if _, err := os.Stat(silentPath); err != nil {
		t.Errorf("a kept-but-silent marker is not expired and must stay on disk: %v", err)
	}
	if _, err := os.Stat(skewedPath); err != nil {
		t.Errorf("a marker inside the tolerated skew is not expired and must stay on disk: %v", err)
	}
}

// TestSessionStart_AMarkerSweptByAnotherSessionStartIsNotABlocker: two
// session starts can walk the same expired marker, and the one that loses the
// race finds the file already removed rather than a directory that refuses.
// A file that is already gone is the sweep done, not a refusal, so the current
// failure is still shown: the blocker line belongs to a removal the directory
// really refused, and naming the directory over a marker that is already gone
// would hide the current failure behind it.
func TestSessionStart_AMarkerSweptByAnotherSessionStartIsNotABlocker(t *testing.T) {
	isolatedHome(t)
	xdgHome := t.TempDir()
	t.Setenv("XDG_DATA_HOME", xdgHome)

	expiredPath := writeMarkerFile(t, xdgHome, testMarker{
		Project: "stale", PhasesFailed: []string{"reflect"}, Error: "boom",
		At: time.Now().UTC().Add(-40 * 24 * time.Hour).Format(time.RFC3339), Version: 1,
	})
	writeMarkerFile(t, xdgHome, testMarker{
		Project: "current", PhasesFailed: []string{"resolve"}, Error: "boom",
		At: time.Now().UTC().Format(time.RFC3339), Version: 1,
	})

	// Another session start swept the same expired marker between this read
	// and this removal, so the removal finds the file gone.
	sweepAwayOnce(t, expiredPath)

	firstLine, out := alertFirstLine(t, filepath.Join(t.TempDir(), "no-such-project"))

	if !strings.HasPrefix(firstLine, "**Ghost maintenance alert:**") {
		t.Fatalf("the current failure must still alert, got:\n%s", out)
	}
	if !strings.Contains(firstLine, "for project current") {
		t.Errorf("the current marker must be the one reported, got: %s", firstLine)
	}
	if strings.Contains(firstLine, "refuses new files") {
		t.Errorf("a marker another session start swept is not a refusal, got: %s", firstLine)
	}
}

// TestSessionStart_AMarkerSweptByAnotherSessionStartIsNotABlockerInTheProjectRead:
// the same race on the per-project read, where the expired marker is the
// project's own file and the current one is keyed on its name. The expired
// file is gone by the time it is removed, so the read carries on to the next
// candidate and the current failure is shown — not the directory named as the
// blocker over a file that is already gone.
func TestSessionStart_AMarkerSweptByAnotherSessionStartIsNotABlockerInTheProjectRead(t *testing.T) {
	isolatedHome(t)
	xdgHome := t.TempDir()
	seedProject(t, xdgHome, "p1", "/tmp/my-project", "My Project")
	t.Setenv("XDG_DATA_HOME", xdgHome)

	expiredPath := writeMarkerFile(t, xdgHome, testMarker{
		Project: "p1", PhasesFailed: []string{"reflect"}, Error: "boom",
		At: time.Now().UTC().Add(-40 * 24 * time.Hour).Format(time.RFC3339), Version: 1,
	})
	currentPath := writeMarkerFile(t, xdgHome, testMarker{
		Project: "My Project", PhasesFailed: []string{"resolve"}, Error: "boom",
		At: time.Now().UTC().Format(time.RFC3339), Version: 1,
	})

	// Another session start swept the same expired marker between this read
	// and this removal, so the removal finds the file gone.
	sweepAwayOnce(t, expiredPath)

	alert := lifecycleFailureAlert("p1", "My Project")

	if !strings.Contains(alert, "for project My Project") {
		t.Errorf("the current marker must be the one reported, got: %q", alert)
	}
	if strings.Contains(alert, "refuses new files") {
		t.Errorf("a marker another session start swept is not a refusal, got: %q", alert)
	}
	if _, err := os.Stat(currentPath); err != nil {
		t.Errorf("the current marker must survive the sweep, stat err = %v", err)
	}
}

// sweepAwayOnce makes the removal of path report the file as already gone,
// the way a concurrent session start's sweep leaves it: the file goes away
// between the read and the removal, so the removal finds nothing to remove.
func sweepAwayOnce(t *testing.T, path string) {
	t.Helper()
	restore := removeMarkerFile
	removeMarkerFile = func(p string) error {
		if p == path {
			_ = os.Remove(path)
			return fs.ErrNotExist
		}
		return os.Remove(p)
	}
	t.Cleanup(func() { removeMarkerFile = restore })
}

// TestSessionStart_ExpiredMarkerOnAnUnwritableDataDirNamesTheDirectory pins
// the sweep's other half: a marker the data directory REFUSES to remove is as
// un-clearable as a current one, so the alert names the directory as the
// blocker rather than going silent and repeating the failed removal at every
// session start. The per-project read already holds that rule; the sweep in
// the no-project read reports it the same way. A refusal is not the same as a
// marker another session start already removed, and only the refusal is
// reported.
func TestSessionStart_ExpiredMarkerOnAnUnwritableDataDirNamesTheDirectory(t *testing.T) {
	skipUnlessModesRestrict(t)
	isolatedHome(t)
	xdgHome := t.TempDir()
	t.Setenv("XDG_DATA_HOME", xdgHome)

	path := writeMarkerFile(t, xdgHome, testMarker{
		Project: "stale", PhasesFailed: []string{"reflect"}, Error: "boom",
		At: time.Now().UTC().Add(-40 * 24 * time.Hour).Format(time.RFC3339), Version: 1,
	})
	refuseDataDirWrites(t, filepath.Join(xdgHome, "ghost"))

	alert := lifecycleFailureAlert("", "")

	if !strings.Contains(alert, "refuses new files") {
		t.Errorf("the directory was not named as the blocker: %q", alert)
	}
	if strings.Contains(alert, "Memory consolidation is paused") {
		t.Errorf("an un-clearable marker still claims consolidation is paused: %q", alert)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("the marker must be left for a writable directory, stat err = %v", err)
	}
}
