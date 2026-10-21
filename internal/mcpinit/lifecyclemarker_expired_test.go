package mcpinit

import (
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

// TestSessionStart_ExpiredMarkerOnAnUnwritableDataDirNamesTheDirectory pins
// the sweep's other half: a marker the data directory refuses to remove is as
// un-clearable as a current one, so the alert names the directory as the
// blocker rather than going silent and repeating the failed removal at every
// session start. The per-project read already holds that rule; the sweep in
// the no-project read reports it the same way.
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
