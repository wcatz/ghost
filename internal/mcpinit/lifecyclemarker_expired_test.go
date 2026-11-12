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

// TestSessionStart_ARefusedExpiredMarkerDoesNotOutrankTheCurrentOne pins the
// no-project read's choice when an expired marker the directory refuses to
// remove sits beside a current failure. The expired marker is 40 days old, so
// it is past the age horizon and the sweep must try to remove it; the removal
// is refused, so the file stays on disk. The current failure is stamped an
// hour AHEAD — inside the tolerated clock skew, so it is a real, recent
// failure and is never swept — and a stamp dated ahead of now ranks BELOW
// every stamp at or before it, so the expired marker used to outrank it. The
// read then returned the expired one, the alert tried to remove it, the
// removal failed, and the session was told about the directory (or told
// nothing at all) instead of about the failure that is actually current.
//
// The per-project read already passes over every marker the sweep could not
// remove; this pins the same rule on the no-project read, whose ranking is
// over every marker in the data directory.
func TestSessionStart_ARefusedExpiredMarkerDoesNotOutrankTheCurrentOne(t *testing.T) {
	isolatedHome(t)
	xdgHome := t.TempDir()
	t.Setenv("XDG_DATA_HOME", xdgHome)

	expiredPath := writeMarkerFile(t, xdgHome, testMarker{
		Project: "stale", PhasesFailed: []string{"reflect"}, Error: "boom",
		At: time.Now().UTC().Add(-40 * 24 * time.Hour).Format(time.RFC3339), Version: 1,
	})
	writeMarkerFile(t, xdgHome, testMarker{
		Project: "current", PhasesFailed: []string{"resolve"}, Error: "boom",
		At: time.Now().UTC().Add(time.Hour).Format(time.RFC3339), Version: 1,
	})
	refuseRemovalOf(t, expiredPath)

	m := readLifecycleFailure("", "")
	if m == nil {
		t.Fatal("the current failure must still be read")
	}
	if m.Project != "current" {
		t.Errorf("the read returned project %q, want the current failure", m.Project)
	}

	alert := lifecycleFailureAlert("", "")
	if !strings.Contains(alert, "for project current") {
		t.Errorf("the current failure must be the one shown, got: %q", alert)
	}
	if strings.Contains(alert, "stale") {
		t.Errorf("the expired marker must not be shown, got: %q", alert)
	}
	if strings.Contains(alert, "refuses new files") {
		t.Errorf("the directory is writable, so it is not the blocker, got: %q", alert)
	}
}

// TestSessionStart_RefusedExpiredMarkersAloneStillNameTheDirectory pins the
// fallback the no-project read ends on when nothing current is left: two
// markers 40 days old, for two different projects, both of them markers the
// sweep could not remove, and no current failure beside them. The ranking has
// nothing to rank — every marker is dead — so the read falls back to the
// first refused one, and the alert names the directory as the blocker: a
// marker that can be neither cleared nor called stale is still something the
// reader has to be told about, and the directory is what has to be fixed.
//
// The directory refuses new files here, so both removals are refused and the
// blocker line is the line it earns. It passes with and without the skips in
// the ranking — before them the ranking returned an expired marker and the
// alert reported the same thing — so what it pins is that the fallback still
// reports it.
func TestSessionStart_RefusedExpiredMarkersAloneStillNameTheDirectory(t *testing.T) {
	skipUnlessModesRestrict(t)
	isolatedHome(t)
	xdgHome := t.TempDir()
	t.Setenv("XDG_DATA_HOME", xdgHome)

	writeMarkerFile(t, xdgHome, testMarker{
		Project: "stale-one", PhasesFailed: []string{"reflect"}, Error: "boom",
		At: time.Now().UTC().Add(-40 * 24 * time.Hour).Format(time.RFC3339), Version: 1,
	})
	writeMarkerFile(t, xdgHome, testMarker{
		Project: "stale-two", PhasesFailed: []string{"resolve"}, Error: "boom",
		At: time.Now().UTC().Add(-41 * 24 * time.Hour).Format(time.RFC3339), Version: 1,
	})
	refuseDataDirWrites(t, filepath.Join(xdgHome, "ghost"))

	alert := lifecycleFailureAlert("", "")

	if alert == "" {
		t.Fatal("a marker the directory refuses to remove must still be reported")
	}
	if !strings.Contains(alert, "refuses new files") {
		t.Errorf("the directory was not named as the blocker: %q", alert)
	}
	if !strings.Contains(alert, "project stale-one") && !strings.Contains(alert, "project stale-two") {
		t.Errorf("the blocker line must name one of the two refused markers, got: %q", alert)
	}
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

// TestSessionStart_ARefusedExpiredMarkerNamesTheCurrentFailureOnAnUnwritableDir
// is the same rule on a data directory that refuses new files, where the
// expired marker cannot be removed because the directory itself is the
// blocker. The blocker line is still the line the directory earns, but the
// failure it names must be the current one: the expired marker is a dead
// file, and a session told about a 40-day-old failure instead of the current
// one is told about the wrong thing.
func TestSessionStart_ARefusedExpiredMarkerNamesTheCurrentFailureOnAnUnwritableDir(t *testing.T) {
	skipUnlessModesRestrict(t)
	isolatedHome(t)
	xdgHome := t.TempDir()
	t.Setenv("XDG_DATA_HOME", xdgHome)

	writeMarkerFile(t, xdgHome, testMarker{
		Project: "stale", PhasesFailed: []string{"reflect"}, Error: "boom",
		At: time.Now().UTC().Add(-40 * 24 * time.Hour).Format(time.RFC3339), Version: 1,
	})
	writeMarkerFile(t, xdgHome, testMarker{
		Project: "current", PhasesFailed: []string{"resolve"}, Error: "boom",
		At: time.Now().UTC().Add(time.Hour).Format(time.RFC3339), Version: 1,
	})
	refuseDataDirWrites(t, filepath.Join(xdgHome, "ghost"))

	alert := lifecycleFailureAlert("", "")

	if !strings.Contains(alert, "project current") {
		t.Errorf("the blocker line must name the current failure, got: %q", alert)
	}
	if strings.Contains(alert, "project stale") {
		t.Errorf("the blocker line must not name the expired marker, got: %q", alert)
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
