package memory

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A backup that takes a good snapshot and then cannot describe it has produced
// something the user still wants, and the only place they will be told so is the
// error: the CLI prints no report for a failed backup (see
// TestRunBackupCoreWritesAndReports, which holds that nothing is printed on
// failure), so an error that does not mention the surviving file leaves someone
// believing they have no copy at all — the opposite of what happened.

// TestBackupLeavesTheSnapshotWhenTheManifestWriteFails: the vacuum succeeds, the
// manifest cannot be written, and the snapshot must survive. Deleting it would
// trade a smaller problem (an uncheckable backup) for a larger one (no backup at
// all) over a sidecar — and the user would have to take a new one to find out.
func TestBackupLeavesTheSnapshotWhenTheManifestWriteFails(t *testing.T) {
	store := backupTestStore(t)
	seedBackupFixture(t, store)
	ctx := context.Background()

	dest := filepath.Join(t.TempDir(), "snapshot.db")
	// A directory where the sidecar goes. It classifies as not-a-regular-file,
	// which is the refusal openManifestFile makes — and unlike a symlink it is
	// available on every platform without a privilege, so the case is reachable
	// in the test rather than only on a developer's machine. Placed before the
	// backup because nothing looks at this path until the manifest is written.
	if err := os.Mkdir(ManifestPath(dest), 0o700); err != nil {
		t.Fatalf("occupy the manifest path with a directory: %v", err)
	}

	// bakErr, not err: the reads below reuse err, and a reader scanning this
	// function would reasonably assume the name still held the backup's failure
	// at the point the message is asserted.
	res, bakErr := store.Backup(ctx, dest)
	if bakErr == nil {
		t.Fatalf("Backup reported success with no manifest: %+v", res)
	}

	// The zero result, not a populated one with an empty path. A result carrying
	// Path and Counts beside a failed run is a caller that checks err and a
	// caller that does not, disagreeing about the same outcome.
	if res != (BackupResult{}) {
		t.Errorf("BackupResult = %+v, want the zero value: a caller that ignores the error must not be handed a backup that cannot be checked", res)
	}

	// The snapshot is there, and it is a database holding the rows the store
	// held — not merely a file at the path. "Still exists" would be satisfied by
	// the empty reservation a failed vacuum leaves behind, which is exactly the
	// file a user must NOT be told to keep.
	info, statErr := os.Stat(dest)
	if statErr != nil {
		t.Fatalf("the snapshot is gone after the manifest write failed: %v", statErr)
	}
	if info.Size() == 0 {
		t.Fatal("the surviving snapshot is empty: that is a failed vacuum's reservation, not a backup")
	}
	bdb, err := OpenReadDB(dest)
	if err != nil {
		t.Fatalf("the surviving snapshot is not a database (%v), so it is not a backup", err)
	}
	var rows int
	if err := bdb.QueryRow(`SELECT count(*) FROM memories`).Scan(&rows); err != nil {
		t.Fatalf("count the surviving snapshot: %v", err)
	}
	if err := bdb.Close(); err != nil {
		t.Fatalf("close the snapshot: %v", err)
	}
	if rows != 4 {
		t.Errorf("the surviving snapshot holds %d memories, want the 4 the fixture wrote", rows)
	}

	// The error has to do the whole job here, because the report is not printed
	// for a failed backup — runBackupCore returns before printBackupReport, and
	// TestRunBackupCoreWritesAndReports holds that nothing is printed on failure.
	// It must name the sidecar, so the reader knows the snapshot is not the thing
	// that failed, and it must name the snapshot that is still usable, or a
	// reader concludes from a failed command that they have no copy.
	msg := bakErr.Error()
	if !strings.Contains(msg, ManifestPath(dest)) {
		t.Errorf("error = %q, want it to name the manifest path: the backup is not what failed", msg)
	}
	if !strings.Contains(msg, dest) {
		t.Errorf("error = %q, want it to name %s, the snapshot that survived — the CLI prints no report for a failed backup, so the message is the only place that gets said", msg, dest)
	}
	if !strings.Contains(msg, "restorable") && !strings.Contains(msg, "still there") {
		t.Errorf("error = %q, want it to say the snapshot is still there and usable, rather than leaving a failed command to read as no backup at all", msg)
	}
}

// TestBackupStillWritesItsManifestWhenThePathIsFree: the other half, so the test
// above is not satisfied by a Backup that never writes one. A refused manifest
// and an absent manifest have to be told apart, and only the second is a
// failure.
func TestBackupStillWritesItsManifestWhenThePathIsFree(t *testing.T) {
	store := backupTestStore(t)
	seedBackupFixture(t, store)

	dest := filepath.Join(t.TempDir(), "snapshot.db")
	res, err := store.Backup(context.Background(), dest)
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if res.ManifestPath == "" {
		t.Fatal("ManifestPath is empty on a successful backup, so the refusal above proves nothing")
	}
	if _, err := ReadBackupManifest(res.ManifestPath); err != nil {
		t.Errorf("the manifest a successful backup reported was not readable: %v", err)
	}
}
