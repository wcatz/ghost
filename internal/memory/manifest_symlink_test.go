package memory

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The sidecar cannot use O_EXCL because a manifest is REPLACED rather than
// refused, so the defence against a symlink at its path is a classification.

// TestWriteBackupManifestRefusesToWriteThroughASymlink: a manifest that followed
// a link would truncate, overwrite and narrow to 0600 a file the user never
// named. A dangling link (which a stat reads as absent) and a live one are both
// refused, and the live target keeps its content and mode.
func TestWriteBackupManifestRefusesToWriteThroughASymlink(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "snapshot.db")
	if err := os.WriteFile(dest, []byte("not really a database"), 0o600); err != nil {
		t.Fatalf("seed the snapshot: %v", err)
	}
	target := filepath.Join(t.TempDir(), "elsewhere.json")
	if err := os.Symlink(target, ManifestPath(dest)); err != nil {
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

	live := filepath.Join(dir, "important.json")
	if err := os.WriteFile(live, []byte("do not lose me"), 0o644); err != nil {
		t.Fatalf("seed the target: %v", err)
	}
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
	if after, err := os.Stat(live); err != nil || after.Mode().Perm() != before.Mode().Perm() {
		t.Errorf("the symlink target is now mode %v (err %v), was %v: Ghost must not narrow a file the user never named", after, err, before.Mode().Perm())
	}
}

// TestWriteBackupManifestReplacesARegularFile: the refusal above is a
// classification, not a blanket "refuse to replace": a regular file at the path
// belongs to an earlier backup, and the replacement describes the file as it is NOW.
func TestWriteBackupManifestReplacesARegularFile(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "snapshot.db")
	if err := os.WriteFile(dest, []byte("bytes"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	at := time.Date(2026, 9, 26, 15, 32, 7, 0, time.UTC)
	for _, n := range []int{3, 7} {
		if _, err := writeBackupManifest(dest, BackupCounts{Memories: n}, at); err != nil {
			t.Fatalf("write with %d memories: %v", n, err)
		}
	}
	m, err := ReadBackupManifest(ManifestPath(dest))
	if err != nil {
		t.Fatalf("ReadBackupManifest: %v", err)
	}
	if sum, err := FileSHA256(dest); err != nil || m.Counts.Memories != 7 || m.SHA256 != sum {
		t.Errorf("manifest = %+v (sha %q, err %v), want the second write's 7 memories and the file's hash", m, sum, err)
	}
}

// TestVerifyBackupNamesADamagedManifestAsPresentNotAbsent: a damaged sidecar and
// a dangling symlink at the manifest path (which Lstat finds but ReadFile reports
// as ENOENT, the same error an absent manifest returns) are both PRESENT but
// unreadable, so the report names an error rather than printing a nil one and
// the checks that need no manifest still run.
func TestVerifyBackupNamesADamagedManifestAsPresentNotAbsent(t *testing.T) {
	for _, tc := range []struct {
		name   string
		damage func(t *testing.T, link string)
	}{
		{"damaged", func(t *testing.T, link string) {
			if err := os.WriteFile(link, []byte("{ not json"), 0o600); err != nil {
				t.Fatalf("damage the manifest: %v", err)
			}
		}},
		{"dangling symlink", func(t *testing.T, link string) {
			if err := os.Remove(link); err != nil {
				t.Fatalf("remove the manifest: %v", err)
			}
			if err := os.Symlink(filepath.Join(t.TempDir(), "gone.json"), link); err != nil {
				t.Skipf("symlinks unavailable: %v", err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dest := backedUpFixture(t)
			tc.damage(t, ManifestPath(dest))
			rep, err := VerifyBackup(context.Background(), dest)
			if err == nil {
				t.Fatalf("verify accepted an unreadable manifest: %+v", rep)
			}
			if !rep.ManifestPresent || rep.HasManifest || rep.ManifestErr == nil {
				t.Errorf("ManifestPresent=%v HasManifest=%v ManifestErr=%v, want present but unreadable", rep.ManifestPresent, rep.HasManifest, rep.ManifestErr)
			}
			requireManifestFreeChecksRan(t, dest, rep, err)
		})
	}
}

// TestVerifyBackupMarksTheSizeReadOnceItHasStatEd: BytesRead stops a report
// claiming a 0-byte file it never measured, so a successful run must set it.
func TestVerifyBackupMarksTheSizeReadOnceItHasStatEd(t *testing.T) {
	dest := backedUpFixture(t)
	rep, err := VerifyBackup(context.Background(), dest)
	if err != nil {
		t.Fatalf("VerifyBackup: %v", err)
	}
	if want := fileSize(t, dest); !rep.BytesRead || rep.Bytes != want {
		t.Errorf("BytesRead=%v Bytes=%d, want the file's %d", rep.BytesRead, rep.Bytes, want)
	}
}

// TestManifestOpenErrorNamesTheSymlinkItRefused: openManifestFile maps ELOOP to a
// message naming the symlink and the path (the unix-only test beside the write
// path proves the flag refuses the open); any other error is wrapped, not
// reported as a symlink.
func TestManifestOpenErrorNamesTheSymlinkItRefused(t *testing.T) {
	got := manifestOpenError("/data/ghost.db.manifest.json", syscall.ELOOP)
	if !strings.Contains(got.Error(), "symlink") || !strings.Contains(got.Error(), "/data/ghost.db.manifest.json") {
		t.Errorf("ELOOP mapped to %q, want it to name the symlink and the path", got)
	}
	other := errors.New("permission denied")
	wrapped := manifestOpenError("/data/ghost.db.manifest.json", other)
	if !errors.Is(wrapped, other) || strings.Contains(wrapped.Error(), "symlink") {
		t.Errorf("a non-ELOOP error mapped to %q, want it wrapped with no mention of a symlink", wrapped)
	}
}
