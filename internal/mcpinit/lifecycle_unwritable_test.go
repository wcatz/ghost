package mcpinit

// Every lifecycle bookkeeping file — the cooldown stamp, the pid claim, the
// failure marker, the log — lives in the data directory, so a directory that
// refuses new entries used to turn each of its failures into silence: the hook
// ran a full chain every turn (the cooldown stamp never persisted), a marker
// already on disk could be neither cleared nor re-recorded, and session start
// kept presenting the frozen marker as the current cause. These tests pin the
// three reported cases against a chmod'd data directory: the spawn refused and
// reported with the lifecycle log present and absent, the marker writes that
// report their failure, and the alert that names the directory instead of the
// recorded phase.

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/audit"
	"github.com/wcatz/ghost/internal/config"
	"github.com/wcatz/ghost/internal/memory"
)

// skipUnlessModesRestrict skips a test that needs mode bits to actually deny
// write access: on Windows chmod does not, and root ignores the mode.
func skipUnlessModesRestrict(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("mode bits do not deny write access on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("running as root: mode 0o500 still allows writes")
	}
}

// refuseDataDirWrites makes the data dir refuse new entries and restores
// write access when the test ends.
func refuseDataDirWrites(t *testing.T, dataDir string) {
	t.Helper()
	if err := os.Chmod(dataDir, 0o500); err != nil {
		t.Fatalf("chmod the data dir read-only: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dataDir, 0o755) })
}

// warmReadOnlyStore resolves the project once through a read-only handle
// before the directory is locked, so the store's own side files exist and the
// hook's read-only open still succeeds afterwards: the test is about the
// bookkeeping writes, not about SQLite's reaction to a locked directory.
func warmReadOnlyStore(t *testing.T, dataDir, projDir string) {
	t.Helper()
	db, err := sql.Open("sqlite", roDSN(filepath.Join(dataDir, "ghost.db")))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close() //nolint:errcheck
	store := memory.NewStore(db, nil)
	if id, _ := resolveSessionProject(context.Background(), store, projDir); id == "" {
		t.Fatal("the seeded project did not resolve")
	}
}

// TestLifecycleBookkeepingWritable pins the writability check itself: a
// directory that accepts a file reports nothing and is left as it was, a
// refused directory reports the OS reason.
func TestLifecycleBookkeepingWritable(t *testing.T) {
	skipUnlessModesRestrict(t)
	dir := t.TempDir()

	if err := lifecycleBookkeepingWritable(dir); err != nil {
		t.Fatalf("a writable directory reported %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("the check left %d file(s) behind", len(entries))
	}

	refuseDataDirWrites(t, dir)
	if err := lifecycleBookkeepingWritable(dir); err == nil {
		t.Fatal("a refused directory reported no error")
	}
}

// TestRecordSpawnFailure_ReportsTheMarkerItCouldNotWrite pins the report that
// replaced the discarded WriteLifecycleFailure error: a spawn that failed for
// its own reason still says so when the marker recording it cannot be written.
func TestRecordSpawnFailure_ReportsTheMarkerItCouldNotWrite(t *testing.T) {
	skipUnlessModesRestrict(t)
	dataHome := isolatedHome(t)
	seedProject(t, dataHome, "proj-marker", t.TempDir(), "proj")
	refuseDataDirWrites(t, filepath.Join(dataHome, "ghost"))

	var stderr bytes.Buffer
	recordSpawnFailure("proj-marker", &config.Config{Reflection: config.ReflectionConfig{AutoResolve: true}}, errors.New("the detached process never started"), &stderr)

	if got := stderr.String(); !strings.Contains(got, "cannot record the lifecycle failure marker") {
		t.Errorf("the failed marker write was not reported: %q", got)
	}
}

// TestRecordReflectSkipMarker_ReportsTheMarkerItCouldNotWrite pins the same
// report on the no-LLM guard path, which runs before the spawn's own
// writability check.
func TestRecordReflectSkipMarker_ReportsTheMarkerItCouldNotWrite(t *testing.T) {
	skipUnlessModesRestrict(t)
	dataHome := isolatedHome(t)
	projDir := t.TempDir()
	seedProject(t, dataHome, "proj-skip", projDir, "proj")
	warmReadOnlyStore(t, filepath.Join(dataHome, "ghost"), projDir)
	refuseDataDirWrites(t, filepath.Join(dataHome, "ghost"))

	var stderr bytes.Buffer
	recordReflectSkipMarker(projDir, &stderr)

	if got := stderr.String(); !strings.Contains(got, "cannot record the reflect-skip marker") {
		t.Errorf("the failed marker write was not reported: %q", got)
	}
}

// TestLifecycleFailureAlert_UnwritableDataDirIsTheBlockerNotTheMarker pins the
// session-start half: a marker that cannot be cleared reads the same as a
// fresh failure — a cause already fixed looks identical to one still live —
// so the alert names the directory as the blocker and does not present the
// recorded cause or the paused claim as the current one.
func TestLifecycleFailureAlert_UnwritableDataDirIsTheBlockerNotTheMarker(t *testing.T) {
	skipUnlessModesRestrict(t)
	dataHome := isolatedHome(t)
	seedProject(t, dataHome, "proj-alert", t.TempDir(), "proj")
	if err := WriteLifecycleFailure("proj-alert", []string{"resolve"}, "consolidation halted: the harness exited 7"); err != nil {
		t.Fatal(err)
	}
	refuseDataDirWrites(t, filepath.Join(dataHome, "ghost"))

	alert := lifecycleFailureAlert("proj-alert", "proj")
	if !strings.Contains(alert, "refuses new files") {
		t.Errorf("the directory was not named as the blocker: %q", alert)
	}
	if strings.Contains(alert, "Memory consolidation is paused") {
		t.Errorf("an un-clearable marker still claims consolidation is paused: %q", alert)
	}
	if strings.Contains(alert, "the harness exited 7") {
		t.Errorf("the recorded cause is still presented as current: %q", alert)
	}
}

// TestSpawnLifecycle_UnwritableDataDirRefusesAndReports pins the spawn half
// in the three states a data directory can be in when it stops accepting new
// entries: the lifecycle log already present (opening it for append still
// succeeds, so only the writability check stops the chain), the log absent,
// and a failure marker from an earlier run on disk. In each the child is not
// started and the refusal is on the hook's stderr; the old marker is left as
// it was.
func TestSpawnLifecycle_UnwritableDataDirRefusesAndReports(t *testing.T) {
	cases := []struct {
		name      string
		logExists bool
		marker    bool
	}{
		{name: "log present", logExists: true},
		{name: "log absent"},
		{name: "old failure marker", marker: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			skipUnlessModesRestrict(t)
			dataHome := isolatedHome(t)
			writeGhostConfigFile(t, "reflection:\n  auto_resolve: true\n")
			projDir := t.TempDir()
			seedProject(t, dataHome, "proj-unwritable", projDir, "proj")
			dataDir := filepath.Join(dataHome, "ghost")
			seen := captureLifecycleChild(t)

			logPath := filepath.Join(dataDir, "lifecycle.log")
			if tc.logExists {
				if err := os.WriteFile(logPath, []byte("earlier run\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if tc.marker {
				if err := WriteLifecycleFailure("proj-unwritable", []string{"resolve"}, "an earlier run failed"); err != nil {
					t.Fatal(err)
				}
			}
			markerPath := filepath.Join(dataDir, projectMarkerFile("proj-unwritable"))
			warmReadOnlyStore(t, dataDir, projDir)
			refuseDataDirWrites(t, dataDir)

			var stderr bytes.Buffer
			spawnLifecycleIfConfigured(projDir, "claude-code", func() *audit.Signals { return nil }, &stderr)

			if len(*seen) != 0 {
				t.Errorf("the lifecycle child was started despite the refused directory: %v", *seen)
			}
			if got := stderr.String(); !strings.Contains(got, "cannot write lifecycle bookkeeping files") {
				t.Errorf("the refusal was not reported on stderr: %q", got)
			}
			if !tc.logExists {
				if _, err := os.Stat(logPath); !os.IsNotExist(err) {
					t.Errorf("the refused spawn created the lifecycle log, stat err = %v", err)
				}
			}
			if tc.marker {
				if _, err := os.Stat(markerPath); err != nil {
					t.Errorf("the old marker was disturbed: %v", err)
				}
			}
		})
	}
}
