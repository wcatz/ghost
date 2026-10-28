package mcpinit

import (
	"strings"
	"testing"
	"time"
)

// A legacy marker recording another project must not hide a killed run's
// unfinished alert for this project.
func TestLegacyMarkerNamingAnotherProjectDoesNotHideUnfinishedRun(t *testing.T) {
	dataHome := isolatedHome(t)
	seedProject(t, dataHome, "proj-123", "/tmp/my-project", "My Project")
	writeDeadLifecycleClaim(t, dataHome, "proj-123", time.Now().Add(-time.Hour))
	writeStartStamp(t, dataHome, "proj-123", time.Now())
	writeLegacyMarkerFile(t, dataHome, testMarker{Project: "other", PhasesFailed: []string{"reflect"},
		Error: "other project broke", At: time.Now().UTC().Format(time.RFC3339), Version: 1})

	alert := lifecycleFailureAlert("proj-123", "My Project")
	if alert == "" {
		t.Fatal("expected the unfinished-run alert, got \"\"")
	}
	if !strings.Contains(alert, "did not finish") {
		t.Errorf("alert does not say the run never finished: %s", alert)
	}
}

// A legacy marker recording this project is still returned (and alerts) as
// before.
func TestLegacyMarkerNamingThisProjectStillAlerts(t *testing.T) {
	dataHome := isolatedHome(t)
	seedProject(t, dataHome, "proj-123", "/tmp/my-project", "My Project")
	writeLegacyMarkerFile(t, dataHome, testMarker{Project: "proj-123", PhasesFailed: []string{"reflect"},
		Error: "this project broke", At: time.Now().UTC().Format(time.RFC3339), Version: 1})

	alert := lifecycleFailureAlert("proj-123", "My Project")
	if alert == "" {
		t.Fatal("expected the legacy marker alert, got \"\"")
	}
	if !strings.Contains(alert, "this project broke") && !strings.Contains(alert, "reflect") {
		t.Errorf("alert does not reflect the legacy marker: %s", alert)
	}
}

// A legacy marker with no project is still returned (and alerts) as before.
func TestLegacyMarkerWithNoProjectStillAlerts(t *testing.T) {
	dataHome := isolatedHome(t)
	seedProject(t, dataHome, "proj-123", "/tmp/my-project", "My Project")
	writeLegacyMarkerFile(t, dataHome, testMarker{Project: "", PhasesFailed: []string{"reflect"},
		Error: "unattributed broke", At: time.Now().UTC().Format(time.RFC3339), Version: 1})

	alert := lifecycleFailureAlert("proj-123", "My Project")
	if alert == "" {
		t.Fatal("expected the legacy marker alert, got \"\"")
	}
}

// An expired, unremovable legacy marker recording another project must not be
// reported as this session's blocked marker.
func TestLegacyMarkerNamingAnotherProjectIsNotReportedAsBlocked(t *testing.T) {
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
	if strings.Contains(alert, "boom") {
		t.Errorf("the other project's blocked legacy marker spoke for this session: %q", alert)
	}
}
