package memory

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The sidecar cannot use O_EXCL, because a manifest is REPLACED rather than
// refused — so unlike the snapshot beside it, the atomic "this path is mine" is
// not available, and the defence has to be a classification. These are the cases
// that classification exists for.

// TestWriteBackupManifestRefusesToWriteThroughASymlink: the snapshot is defended
// against a symlink at its destination (see reserveBackupPath), and a manifest
// that followed one instead would be the same bug with a stranger's file as the
// victim — the TARGET of a link the user never named, truncated, overwritten with
// a manifest, and narrowed to 0600. A `--out` directory is exactly where a link
// is plausible.
func TestWriteBackupManifestRefusesToWriteThroughASymlink(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "snapshot.db")
	if err := os.WriteFile(dest, []byte("not really a database"), 0o600); err != nil {
		t.Fatalf("seed the snapshot: %v", err)
	}

	// A symlink at the manifest path whose target does not exist is the case a
	// stat-based check reads as absent, and the write then goes through to
	// wherever the link points.
	target := filepath.Join(t.TempDir(), "elsewhere.json")
	link := ManifestPath(dest)
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	if _, err := writeBackupManifest(dest, BackupCounts{Memories: 1}, time.Now()); err == nil {
		t.Fatal("writeBackupManifest wrote through a symlink at the manifest path")
	} else if !strings.Contains(err.Error(), "not a regular file") {
		t.Errorf("error = %v, want it to say the path is not a regular file", err)
	}
	if _, err := os.Lstat(target); err == nil {
		t.Error("the manifest was written through the symlink to its target")
	}

	// The same for a link that DOES resolve: the target is a real file someone is
	// relying on, and truncating it is the damage that matters.
	live := filepath.Join(dir, "important.json")
	if err := os.WriteFile(live, []byte("do not lose me"), 0o644); err != nil {
		t.Fatalf("seed the target: %v", err)
	}
	// The width the file actually landed at, which is not necessarily 0644: a
	// create applies the umask, and this process's is not necessarily 0. Asserting
	// a literal would blame Ghost for the umask.
	before, err := os.Stat(live)
	if err != nil {
		t.Fatalf("stat the target: %v", err)
	}
	second := filepath.Join(dir, "other.db")
	if err := os.WriteFile(second, []byte("bytes"), 0o600); err != nil {
		t.Fatalf("seed the second snapshot: %v", err)
	}
	if err := os.Symlink(live, ManifestPath(second)); err != nil {
		t.Fatalf("symlink at the second manifest path: %v", err)
	}
	if _, err := writeBackupManifest(second, BackupCounts{Memories: 1}, time.Now()); err == nil {
		t.Error("writeBackupManifest wrote through a symlink whose target exists")
	}
	body, err := os.ReadFile(live)
	if err != nil || string(body) != "do not lose me" {
		t.Errorf("the symlink target changed: %q (err %v)", body, err)
	}
	// And it was not narrowed either, which is the other half of the damage: a
	// user's file silently chmod'ed to 0600 is a file their group can no longer
	// read.
	after, err := os.Stat(live)
	if err != nil {
		t.Fatalf("re-stat the target: %v", err)
	}
	if after.Mode().Perm() != before.Mode().Perm() {
		t.Errorf("the symlink target is now mode %04o, was %04o: Ghost must not narrow a file the user never named",
			after.Mode().Perm(), before.Mode().Perm())
	}
}

// TestWriteBackupManifestReplacesARegularFile: the other side of the same
// decision, so the refusal above is a classification and not a blanket "refuse to
// replace". A manifest is derived from the snapshot beside it, so one found at
// this path belongs to an earlier backup of a file the user has since deleted.
func TestWriteBackupManifestReplacesARegularFile(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "snapshot.db")
	if err := os.WriteFile(dest, []byte("bytes"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	at := time.Date(2026, 9, 26, 15, 32, 7, 0, time.UTC)
	if _, err := writeBackupManifest(dest, BackupCounts{Memories: 3}, at); err != nil {
		t.Fatalf("first write: %v", err)
	}
	if _, err := writeBackupManifest(dest, BackupCounts{Memories: 7}, at); err != nil {
		t.Fatalf("replacing write: %v", err)
	}
	m, err := ReadBackupManifest(ManifestPath(dest))
	if err != nil {
		t.Fatalf("ReadBackupManifest: %v", err)
	}
	if m.Counts.Memories != 7 {
		t.Errorf("manifest memories = %d, want the 7 the second write recorded", m.Counts.Memories)
	}
	// And the replacement describes the file as it is NOW, not as it was.
	sum, err := FileSHA256(dest)
	if err != nil {
		t.Fatalf("FileSHA256: %v", err)
	}
	if m.SHA256 != sum {
		t.Errorf("manifest sha256 = %q, the file's = %q", m.SHA256, sum)
	}
}

// TestVerifyBackupNamesADamagedManifestAsPresentNotAbsent: the two ways a
// sidecar can be unusable are opposites, and a report that calls damage "no
// manifest" sends the reader to look for a file that is sitting right there.
func TestVerifyBackupNamesADamagedManifestAsPresentNotAbsent(t *testing.T) {
	dest := backedUpFixture(t)
	if err := os.WriteFile(ManifestPath(dest), []byte("{ not json"), 0o600); err != nil {
		t.Fatalf("damage the manifest: %v", err)
	}

	rep, err := VerifyBackup(context.Background(), dest)
	if err == nil {
		t.Fatalf("verify accepted a damaged manifest: %+v", rep)
	}
	if !rep.ManifestPresent {
		t.Error("ManifestPresent is false for a sidecar that is on disk")
	}
	if rep.HasManifest {
		t.Error("HasManifest is true for a sidecar that could not be read")
	}
	if rep.ManifestErr == nil {
		t.Error("ManifestErr is nil, so the report has nothing to tell the reader went wrong")
	}
	// And nothing was measured, so nothing may be claimed.
	if rep.BytesRead || rep.CountsRead {
		t.Errorf("a run that stopped at the manifest reports BytesRead=%v CountsRead=%v, want both false",
			rep.BytesRead, rep.CountsRead)
	}
}

// TestVerifyBackupMarksTheSizeReadOnceItHasStatEd: BytesRead is what stops a
// report claiming a 0-byte file it never measured, and it is only trustworthy if
// a successful run sets it.
func TestVerifyBackupMarksTheSizeReadOnceItHasStatEd(t *testing.T) {
	dest := backedUpFixture(t)
	rep, err := VerifyBackup(context.Background(), dest)
	if err != nil {
		t.Fatalf("VerifyBackup: %v", err)
	}
	if !rep.BytesRead {
		t.Error("BytesRead is false after a successful run, so the size would never be reported")
	}
	if want := fileSize(t, dest); rep.Bytes != want {
		t.Errorf("Bytes = %d, want the file's %d", rep.Bytes, want)
	}
}
