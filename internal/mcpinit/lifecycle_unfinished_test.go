package mcpinit

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// writeDeadLifecycleClaim leaves the pid file a coordinator killed outright
// leaves behind: a claim naming a process that is gone. 999999999 is above any
// platform's pid_max, so it never names a live process.
func writeDeadLifecycleClaim(t *testing.T, dataHome, id string) string {
	t.Helper()
	path := filepath.Join(dataHome, "ghost", "lifecycle-"+id+".pid")
	if err := os.WriteFile(path, []byte(strconv.Itoa(999999999)), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestUnfinishedLifecycleRunAlertsAtSessionStart is the reported failure: a
// coordinator killed after it wrote the cooldown stamp leaves the stamp and its
// pid claim, and wrote no failure marker. The session start has to read those
// two files as a run that never finished and raise the same maintenance alert a
// recorded failure would.
func TestUnfinishedLifecycleRunAlertsAtSessionStart(t *testing.T) {
	dataHome := isolatedHome(t)
	seedProject(t, dataHome, "proj-123", "/tmp/my-project", "My Project")
	if err := TouchLifecycleStart("proj-123"); err != nil {
		t.Fatalf("TouchLifecycleStart: %v", err)
	}
	writeDeadLifecycleClaim(t, dataHome, "proj-123")
	marker := filepath.Join(dataHome, "ghost", LifecycleFailureMarkerFile("proj-123"))
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("the scenario must start with no failure marker (stat err %v)", err)
	}

	alert := lifecycleFailureAlert("proj-123", "My Project")
	if alert == "" {
		t.Fatal("a run that never finished left no maintenance alert")
	}
	if !strings.Contains(alert, "did not finish") {
		t.Errorf("alert does not say the run never finished: %s", alert)
	}
	if !strings.Contains(alert, "proj-123") {
		t.Errorf("alert does not name the project: %s", alert)
	}
}

// TestUnfinishedLifecycleRunNeedsTheCooldownStamp keeps the inference narrow: a
// claim alone does not mean a window was burned. The stamp is written after the
// claim and is what the next stop hook reads, so a run killed before it costs
// one immediate retry and is not reported as a paused project.
func TestUnfinishedLifecycleRunNeedsTheCooldownStamp(t *testing.T) {
	dataHome := isolatedHome(t)
	seedProject(t, dataHome, "proj-123", "/tmp/my-project", "My Project")
	writeDeadLifecycleClaim(t, dataHome, "proj-123")

	if alert := lifecycleFailureAlert("proj-123", "My Project"); alert != "" {
		t.Errorf("a claim with no start stamp raised an alert: %s", alert)
	}
}

// TestLiveLifecycleClaimDoesNotAlert: a run still in progress owns a pid file
// naming a live process, and another session starting beside it must not read
// that as a failure.
func TestLiveLifecycleClaimDoesNotAlert(t *testing.T) {
	dataHome := isolatedHome(t)
	seedProject(t, dataHome, "proj-123", "/tmp/my-project", "My Project")
	if err := TouchLifecycleStart("proj-123"); err != nil {
		t.Fatalf("TouchLifecycleStart: %v", err)
	}
	pidPath := filepath.Join(dataHome, "ghost", "lifecycle-proj-123.pid")
	if err := os.WriteFile(pidPath, []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		t.Fatal(err)
	}

	if alert := lifecycleFailureAlert("proj-123", "My Project"); alert != "" {
		t.Errorf("a live run raised a maintenance alert: %s", alert)
	}
}

// TestReclaimedLifecycleClaimRecordsTheUnfinishedRun covers the next run's
// side: when it reclaims the dead claim it records the earlier run as
// unfinished, so the marker survives even if this run is killed in the same
// way. A later successful run heals it.
func TestReclaimedLifecycleClaimRecordsTheUnfinishedRun(t *testing.T) {
	dataHome := isolatedHome(t)
	seedProject(t, dataHome, "proj-123", "/tmp/my-project", "My Project")
	if err := TouchLifecycleStart("proj-123"); err != nil {
		t.Fatalf("TouchLifecycleStart: %v", err)
	}
	writeDeadLifecycleClaim(t, dataHome, "proj-123")

	release, ok, err := AcquireLifecycleLock("proj-123")
	if err != nil || !ok {
		t.Fatalf("AcquireLifecycleLock = ok %v, err %v; want the reclaimed claim", ok, err)
	}
	if got := pidInFile(filepath.Join(dataHome, "ghost", "lifecycle-proj-123.pid")); got != os.Getpid() {
		t.Fatalf("pid file holds %d, want this process %d", got, os.Getpid())
	}
	marker := readMarkerAtPath(filepath.Join(dataHome, "ghost", LifecycleFailureMarkerFile("proj-123")))
	if marker == nil {
		t.Fatal("reclaiming a dead claim recorded no failure marker")
	}
	if !strings.Contains(marker.Error, "did not finish") {
		t.Errorf("recorded cause does not say the run never finished: %s", marker.Error)
	}
	if alert := lifecycleFailureAlert("proj-123", "My Project"); !strings.Contains(alert, "did not finish") {
		t.Errorf("the reclaimed run's marker did not alert: %s", alert)
	}

	release()
	if err := FinishLifecycleRun("proj-123", 1, nil, ""); err != nil {
		t.Fatalf("FinishLifecycleRun: %v", err)
	}
	if alert := lifecycleFailureAlert("proj-123", "My Project"); alert != "" {
		t.Errorf("a successful run did not heal the unfinished-run record: %s", alert)
	}
}
