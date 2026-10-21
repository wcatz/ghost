package mcpinit

import (
	"encoding/json"
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

// TestSessionStart_TheProjectReadSweepsAnExpiredMarkerBehindACurrentOne pins
// the sweep on the per-project read. That read returns the FIRST candidate it
// can use and never reads the ones behind it, so a project whose own digest
// marker is current left an expired marker keyed on its name on disk for as
// long as that marker did — the dead file came back as news when the marker
// in front of it aged out. The read sweeps its candidates first, so the
// expired one is removed and the current one is still the failure shown.
func TestSessionStart_TheProjectReadSweepsAnExpiredMarkerBehindACurrentOne(t *testing.T) {
	isolatedHome(t)
	xdgHome := t.TempDir()
	projDir := seedSessionProject(t, xdgHome, "p1", "myproj")
	t.Setenv("XDG_DATA_HOME", xdgHome)

	// The project's own digest marker is current, so it is the candidate the
	// read returns; the name-keyed marker behind it is expired.
	writeMarkerFile(t, xdgHome, testMarker{
		Project: "p1", PhasesFailed: []string{"reflect"}, Error: "current",
		At: time.Now().UTC().Format(time.RFC3339), Version: 1,
	})
	expiredPath := writeMarkerFile(t, xdgHome, testMarker{
		Project: "myproj", PhasesFailed: []string{"resolve"}, Error: "boom",
		At: time.Now().UTC().Add(-40 * 24 * time.Hour).Format(time.RFC3339), Version: 1,
	})

	firstLine, out := alertFirstLine(t, projDir)

	if !strings.HasPrefix(firstLine, "**Ghost maintenance alert:**") {
		t.Fatalf("the current failure must still alert, got:\n%s", out)
	}
	if !strings.Contains(firstLine, "for project p1") {
		t.Errorf("the current digest marker must be the one reported, got: %s", firstLine)
	}
	if _, err := os.Stat(expiredPath); !os.IsNotExist(err) {
		t.Errorf("the expired marker behind the current one must be swept, stat err = %v", err)
	}
}

// TestSessionStart_ARefusedExpiredMarkerOfAnotherProjectDoesNotHideTheCurrentOne:
// a refused removal must never hide a current failure. The shared legacy file
// holds another project's expired failure and the directory refuses to remove
// it, so the sweep cannot clear it — and the session still has a current
// failure of its own to show.
func TestSessionStart_ARefusedExpiredMarkerOfAnotherProjectDoesNotHideTheCurrentOne(t *testing.T) {
	isolatedHome(t)
	xdgHome := t.TempDir()
	seedProject(t, xdgHome, "p1", "/tmp/my-project", "myproj")
	t.Setenv("XDG_DATA_HOME", xdgHome)

	writeMarkerFile(t, xdgHome, testMarker{
		Project: "p1", PhasesFailed: []string{"reflect"}, Error: "current",
		At: time.Now().UTC().Format(time.RFC3339), Version: 1,
	})
	legacyPath := writeLegacyMarkerFile(t, xdgHome, testMarker{
		Project: "other", PhasesFailed: []string{"resolve"}, Error: "boom",
		At: time.Now().UTC().Add(-40 * 24 * time.Hour).Format(time.RFC3339), Version: 1,
	})
	refuseRemovalOf(t, legacyPath)

	alert := lifecycleFailureAlert("p1", "myproj")

	if !strings.Contains(alert, "for project p1") {
		t.Errorf("the current failure must still be shown, got: %q", alert)
	}
}

// TestSessionStart_ARefusedExpiredMarkerOfThisProjectDoesNotHideTheCurrentOne:
// the same rule when the marker that cannot be removed is this project's own
// and the directory is still writable — the removal failed, but the directory
// is not the blocker, so the blocker line would be an empty one and the
// current failure would go with it.
func TestSessionStart_ARefusedExpiredMarkerOfThisProjectDoesNotHideTheCurrentOne(t *testing.T) {
	isolatedHome(t)
	xdgHome := t.TempDir()
	seedProject(t, xdgHome, "p1", "/tmp/my-project", "myproj")
	t.Setenv("XDG_DATA_HOME", xdgHome)

	writeMarkerFile(t, xdgHome, testMarker{
		Project: "p1", PhasesFailed: []string{"reflect"}, Error: "current",
		At: time.Now().UTC().Format(time.RFC3339), Version: 1,
	})
	legacyPath := writeLegacyMarkerFile(t, xdgHome, testMarker{
		Project: "p1", PhasesFailed: []string{"resolve"}, Error: "boom",
		At: time.Now().UTC().Add(-40 * 24 * time.Hour).Format(time.RFC3339), Version: 1,
	})
	refuseRemovalOf(t, legacyPath)

	alert := lifecycleFailureAlert("p1", "myproj")

	if !strings.Contains(alert, "for project p1") {
		t.Errorf("the current failure must still be shown, got: %q", alert)
	}
	if strings.Contains(alert, "refuses new files") {
		t.Errorf("a writable directory is not the blocker, got: %q", alert)
	}
}

// TestSessionStart_ARefusedExpiredMarkerBeforeTheCurrentOneDoesNotHideIt: the
// same rule when the marker that cannot be removed comes FIRST in the lookup
// — the project's own digest marker — so the current failure sits behind it.
// The lookup carries on past the marker that cannot be removed, because it is
// expired and it stays.
func TestSessionStart_ARefusedExpiredMarkerBeforeTheCurrentOneDoesNotHideIt(t *testing.T) {
	isolatedHome(t)
	xdgHome := t.TempDir()
	seedProject(t, xdgHome, "p1", "/tmp/my-project", "myproj")
	t.Setenv("XDG_DATA_HOME", xdgHome)

	expiredPath := writeMarkerFile(t, xdgHome, testMarker{
		Project: "p1", PhasesFailed: []string{"reflect"}, Error: "boom",
		At: time.Now().UTC().Add(-40 * 24 * time.Hour).Format(time.RFC3339), Version: 1,
	})
	writeMarkerFile(t, xdgHome, testMarker{
		Project: "myproj", PhasesFailed: []string{"resolve"}, Error: "current",
		At: time.Now().UTC().Format(time.RFC3339), Version: 1,
	})
	refuseRemovalOf(t, expiredPath)

	alert := lifecycleFailureAlert("p1", "myproj")

	if !strings.Contains(alert, "for project myproj") {
		t.Errorf("the current failure behind the marker that could not be removed must still be shown, got: %q", alert)
	}
}

// TestSessionStart_ARefusedExpiredMarkerBehindAnotherDoesNotHideTheCurrentOne:
// two expired markers the directory refuses to remove, and a current failure
// behind both of them. The sweep tries every expired candidate, so both are
// known to be un-removable and the lookup passes over both — a refusal says
// nothing about the file behind it — and the current failure is what the
// session is told.
func TestSessionStart_ARefusedExpiredMarkerBehindAnotherDoesNotHideTheCurrentOne(t *testing.T) {
	isolatedHome(t)
	xdgHome := t.TempDir()
	seedProject(t, xdgHome, "p1", "/tmp/my-project", "myproj")
	t.Setenv("XDG_DATA_HOME", xdgHome)

	firstPath := writeMarkerFile(t, xdgHome, testMarker{
		Project: "p1", PhasesFailed: []string{"reflect"}, Error: "boom",
		At: time.Now().UTC().Add(-40 * 24 * time.Hour).Format(time.RFC3339), Version: 1,
	})
	secondPath := writeMarkerFile(t, xdgHome, testMarker{
		Project: "myproj", PhasesFailed: []string{"resolve"}, Error: "boom",
		At: time.Now().UTC().Add(-40 * 24 * time.Hour).Format(time.RFC3339), Version: 1,
	})
	writeLegacyMarkerFile(t, xdgHome, testMarker{
		Project: "p1", PhasesFailed: []string{"reflect"}, Error: "current",
		At: time.Now().UTC().Format(time.RFC3339), Version: 1,
	})
	refuseRemovalOf(t, firstPath, secondPath)

	alert := lifecycleFailureAlert("p1", "myproj")

	if !strings.Contains(alert, "for project p1") {
		t.Errorf("the current failure behind two markers that could not be removed must still be shown, got: %q", alert)
	}
}

// writeLegacyMarkerFile records a failure in the shared legacy file — the one
// an older build wrote — under the project it records.
func writeLegacyMarkerFile(t *testing.T, dataHome string, m testMarker) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dataHome, "ghost"), 0o700); err != nil {
		t.Fatalf("mkdir ghost dir: %v", err)
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal marker: %v", err)
	}
	path := filepath.Join(dataHome, "ghost", legacyMarkerFile)
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatalf("write marker: %v", err)
	}
	return path
}

// refuseRemovalOf makes the removal of the given paths fail the way a data
// directory that refuses new entries does, without making the directory
// unwritable.
func refuseRemovalOf(t *testing.T, paths ...string) {
	t.Helper()
	restore := removeMarkerFile
	removeMarkerFile = func(p string) error {
		for _, path := range paths {
			if p == path {
				return fs.ErrPermission
			}
		}
		return os.Remove(p)
	}
	t.Cleanup(func() { removeMarkerFile = restore })
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
