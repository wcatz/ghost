package mcpinit

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// writeDeadLifecycleClaim leaves the pid file a coordinator killed outright
// leaves behind: a claim naming a process that is gone. 999999999 is above any
// platform's pid_max, so it never names a live process. The file is dated at
// the given instant, because the claim's own timestamp is what ties it to the
// start stamp beside it.
func writeDeadLifecycleClaim(t *testing.T, dataHome, id string, at time.Time) string {
	t.Helper()
	path := filepath.Join(dataHome, "ghost", "lifecycle-"+id+".pid")
	if err := os.WriteFile(path, []byte(strconv.Itoa(999999999)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, at, at); err != nil {
		t.Fatal(err)
	}
	return path
}

// writeStartStamp writes the cooldown stamp a run writes once it has work to
// do, dated at the given instant. The stamp file is never removed, so the
// tests that pin the pairing with a claim have to date both files themselves.
func writeStartStamp(t *testing.T, dataHome, id string, at time.Time) string {
	t.Helper()
	if err := TouchLifecycleStart(id); err != nil {
		t.Fatalf("TouchLifecycleStart: %v", err)
	}
	path := filepath.Join(dataHome, "ghost", lifecycleLastStartFile(id))
	if err := os.Chtimes(path, at, at); err != nil {
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
	// The real order: the claim is taken first, the stamp is written after it.
	writeDeadLifecycleClaim(t, dataHome, "proj-123", time.Now().Add(-time.Hour))
	writeStartStamp(t, dataHome, "proj-123", time.Now())
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
	writeDeadLifecycleClaim(t, dataHome, "proj-123", time.Now().Add(-time.Hour))

	if alert := lifecycleFailureAlert("proj-123", "My Project"); alert != "" {
		t.Errorf("a claim with no start stamp raised an alert: %s", alert)
	}
}

// TestUnfinishedLifecycleRunIgnoresAClaimNewerThanTheStamp pins the pairing:
// the stamp file is never removed, so it outlives the run that wrote it. A
// claim NEWER than the stamp is a run that died before it stamped — one that
// burned no cooldown window — and the old stamp beside it is not its evidence.
func TestUnfinishedLifecycleRunIgnoresAClaimNewerThanTheStamp(t *testing.T) {
	dataHome := isolatedHome(t)
	seedProject(t, dataHome, "proj-123", "/tmp/my-project", "My Project")
	// An earlier run stamped the project; this claim is newer than that stamp.
	writeStartStamp(t, dataHome, "proj-123", time.Now().Add(-time.Hour))
	writeDeadLifecycleClaim(t, dataHome, "proj-123", time.Now())

	if alert := lifecycleFailureAlert("proj-123", "My Project"); alert != "" {
		t.Errorf("a claim newer than the start stamp raised an alert: %s", alert)
	}
}

// TestLiveLifecycleClaimDoesNotAlert: a run still in progress owns a pid file
// naming a live process, and another session starting beside it must not read
// that as a failure.
func TestLiveLifecycleClaimDoesNotAlert(t *testing.T) {
	dataHome := isolatedHome(t)
	seedProject(t, dataHome, "proj-123", "/tmp/my-project", "My Project")
	writeDeadLifecycleClaim(t, dataHome, "proj-123", time.Now().Add(-time.Hour))
	writeStartStamp(t, dataHome, "proj-123", time.Now())
	pidPath := filepath.Join(dataHome, "ghost", "lifecycle-proj-123.pid")
	if err := os.WriteFile(pidPath, []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(pidPath, time.Now().Add(-time.Hour), time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}

	if alert := lifecycleFailureAlert("proj-123", "My Project"); alert != "" {
		t.Errorf("a live run raised a maintenance alert: %s", alert)
	}
}

// TestReclaimedLifecycleClaimRecordsTheUnfinishedRun covers the next run's
// side: when it reclaims the dead claim of a run that had stamped the project,
// it records that earlier run as unfinished, so the marker survives even if
// this run is killed in the same way. A later successful run heals it.
func TestReclaimedLifecycleClaimRecordsTheUnfinishedRun(t *testing.T) {
	dataHome := isolatedHome(t)
	seedProject(t, dataHome, "proj-123", "/tmp/my-project", "My Project")
	writeDeadLifecycleClaim(t, dataHome, "proj-123", time.Now().Add(-time.Hour))
	writeStartStamp(t, dataHome, "proj-123", time.Now())

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

// TestReclaimedClaimNewerThanTheStampRecordsNoMarker is the same pairing on
// the reclaim path: a claim newer than the start stamp is a run that died
// before it stamped, and the run reclaiming it must not write a marker for it.
// The same shape covers a run whose exit skipped the deferred release (an
// unlocatable binary), which never stamped either.
func TestReclaimedClaimNewerThanTheStampRecordsNoMarker(t *testing.T) {
	dataHome := isolatedHome(t)
	seedProject(t, dataHome, "proj-123", "/tmp/my-project", "My Project")
	writeStartStamp(t, dataHome, "proj-123", time.Now().Add(-time.Hour))
	writeDeadLifecycleClaim(t, dataHome, "proj-123", time.Now())

	release, ok, err := AcquireLifecycleLock("proj-123")
	if err != nil || !ok {
		t.Fatalf("AcquireLifecycleLock = ok %v, err %v; want the reclaimed claim", ok, err)
	}
	defer release()
	if marker := readMarkerAtPath(filepath.Join(dataHome, "ghost", LifecycleFailureMarkerFile("proj-123"))); marker != nil {
		t.Errorf("reclaiming a claim newer than the start stamp recorded a marker: %s", marker.Error)
	}
	if alert := lifecycleFailureAlert("proj-123", "My Project"); alert != "" {
		t.Errorf("a claim newer than the start stamp raised an alert: %s", alert)
	}
}
