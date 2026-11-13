package memory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The manifest is what makes a backup checkable: without it a restore is "put
// the file back and hope", and the only question a user can ask about a copy is
// whether it opens. These tests hold the manifest to describing the file that
// was written, and hold VerifyBackup to noticing when it does not.

// checkNamed returns the named check from a report, failing if the report does
// not run it. Every assertion below is about a specific check's verdict, so a
// report that quietly stopped running one has to fail rather than pass on the
// ones that remain.
func checkNamed(t *testing.T, rep VerifyReport, name string) VerifyCheck {
	t.Helper()
	for _, c := range rep.Checks {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("no check named %q in %+v", name, rep.Checks)
	return VerifyCheck{}
}

// readManifestAt decodes the manifest a backup wrote beside dest.
func readManifestAt(t *testing.T, dest string) BackupManifest {
	t.Helper()
	m, err := ReadBackupManifest(ManifestPath(dest))
	if err != nil {
		t.Fatalf("ReadBackupManifest(%s): %v", ManifestPath(dest), err)
	}
	return m
}

func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return info.Size()
}

func writeManifestFile(t *testing.T, path string, m BackupManifest) {
	t.Helper()
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	if err := os.WriteFile(path, append(b, '\n'), 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
}

// rewriteManifest re-reads the manifest beside a snapshot, applies mutate, and
// writes it back carrying the hash and size of the file as it is NOW. That is
// what makes the count-mismatch and newer-schema cases reachable: without
// recomputing the hash, editing the manifest would be caught by the sha256 check
// first and the check under test would never run.
func rewriteManifest(t *testing.T, dest string, mutate func(*BackupManifest)) {
	t.Helper()
	path := ManifestPath(dest)
	m, err := ReadBackupManifest(path)
	if err != nil {
		t.Fatalf("ReadBackupManifest: %v", err)
	}
	mutate(&m)
	sum, err := FileSHA256(dest)
	if err != nil {
		t.Fatalf("FileSHA256: %v", err)
	}
	m.SHA256, m.Bytes = sum, fileSize(t, dest)
	writeManifestFile(t, path, m)
}

// backedUpFixture backs up the standard fixture to a fresh path and returns it.
func backedUpFixture(t *testing.T) string {
	t.Helper()
	store := backupTestStore(t)
	seedBackupFixture(t, store)
	dest := filepath.Join(t.TempDir(), "snapshot.db")
	if _, err := store.Backup(context.Background(), dest); err != nil {
		t.Fatalf("Backup: %v", err)
	}
	return dest
}

// TestBackupWritesAManifestDescribingTheFileItWrote: the manifest has to
// describe THIS snapshot (schema version, counts, size, hash, file name), or
// `backup verify` would answer "ok" about a copy nobody checked. It is created
// without group or world bits.
func TestBackupWritesAManifestDescribingTheFileItWrote(t *testing.T) {
	store := backupTestStore(t)
	seedBackupFixture(t, store)

	dest := filepath.Join(t.TempDir(), "snapshot.db")
	res, err := store.Backup(context.Background(), dest)
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if res.ManifestPath != ManifestPath(dest) {
		t.Errorf("ManifestPath = %q, want %q", res.ManifestPath, ManifestPath(dest))
	}
	m := readManifestAt(t, dest)
	if m.ManifestVersion != ManifestVersion || m.SchemaVersion != SchemaVersion() {
		t.Errorf("manifest_version/schema_version = %d/%d, want %d/%d", m.ManifestVersion, m.SchemaVersion, ManifestVersion, SchemaVersion())
	}
	if m.Counts != res.Counts || m.Counts.Memories != 4 || m.Bytes != res.Bytes {
		t.Errorf("manifest counts/bytes = %+v/%d, the report's = %+v/%d, want 4 memories", m.Counts, m.Bytes, res.Counts, res.Bytes)
	}
	if sum, err := FileSHA256(dest); err != nil || m.SHA256 != sum {
		t.Errorf("manifest sha256 = %q, the file's = %q (%v)", m.SHA256, sum, err)
	}
	if _, err := time.Parse(time.RFC3339, m.CreatedAt); err != nil {
		t.Errorf("created_at = %q, want RFC3339: %v", m.CreatedAt, err)
	}
	raw, err := os.ReadFile(ManifestPath(dest))
	if err != nil || !strings.Contains(string(raw), filepath.Base(dest)) {
		t.Errorf("manifest does not name the file it describes: %s (%v)", raw, err)
	}
	if info, err := os.Stat(ManifestPath(dest)); err != nil || info.Mode().Perm()&0o077 != 0 {
		t.Errorf("manifest mode = %v (%v), want no group or world bits", info.Mode().Perm(), err)
	}
}

// fileChangeCounterOffset is byte 24 of a SQLite file: the 4-byte "file change
// counter" in the database header. A flipped bit there is invisible to
// integrity_check and to a row count, so only the hash can catch it.
const fileChangeCounterOffset = 24

// flipChangeCounter flips the low bit of the header's file change counter in place.
func flipChangeCounter(t *testing.T, path string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if len(raw) <= fileChangeCounterOffset {
		t.Fatalf("%s is %d bytes, too short to be a SQLite database", path, len(raw))
	}
	raw[fileChangeCounterOffset] ^= 0x01
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("write flipped snapshot: %v", err)
	}
}

// TestVerifyBackupChecks drives VerifyBackup over a fresh backup and each way a
// copy can be wrong, asserting the verdict of each named check: an untouched
// copy passes everything; a flipped header byte is caught by the hash alone; a
// wrong row count and a schema the file and manifest disagree about are caught
// with the hash still matching; a newer schema names both versions; a copy with
// no manifest (what the migration path writes) still gets the structural checks
// and reports which it skipped.
func TestVerifyBackupChecks(t *testing.T) {
	newer := SchemaVersion() + 1
	for _, tc := range []struct {
		name       string
		damage     func(t *testing.T, dest string)
		want       map[string]VerifyState
		problems   int // -1: at least one
		hasMan     bool
		detailName string // check whose detail must contain every string of detail
		detail     []string
	}{
		{"untouched", func(*testing.T, string) {},
			map[string]VerifyState{"sha256": VerifyOK, "integrity check": VerifyOK, "schema version": VerifyOK, "row counts": VerifyOK}, 0, true, "", nil},
		{"flipped byte", func(t *testing.T, d string) { flipChangeCounter(t, d) },
			map[string]VerifyState{"sha256": VerifyFailed, "integrity check": VerifyOK, "row counts": VerifyOK}, -1, true, "", nil},
		{"row count mismatch", func(t *testing.T, d string) {
			rewriteManifest(t, d, func(m *BackupManifest) { m.Counts.Memories = 99 })
		}, map[string]VerifyState{"row counts": VerifyFailed, "sha256": VerifyOK}, -1, true, "row counts", []string{"99"}},
		{"newer schema", func(t *testing.T, d string) {
			bumpUserVersion(t, d, newer)
			rewriteManifest(t, d, func(m *BackupManifest) { m.SchemaVersion = newer })
		}, map[string]VerifyState{"schema version": VerifyFailed}, -1, true, "schema version",
			[]string{"v" + strconv.Itoa(newer), "v" + strconv.Itoa(SchemaVersion())}},
		{"manifest disagrees with file", func(t *testing.T, d string) {
			rewriteManifest(t, d, func(m *BackupManifest) { m.SchemaVersion = SchemaVersion() - 1 })
		}, map[string]VerifyState{"schema version": VerifyFailed}, 1, true, "", nil},
		{"no manifest", func(t *testing.T, d string) {
			if err := os.Remove(ManifestPath(d)); err != nil {
				t.Fatalf("remove manifest: %v", err)
			}
		}, map[string]VerifyState{"sha256": VerifySkipped, "row counts": VerifySkipped, "integrity check": VerifyOK}, 0, false, "", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dest := backedUpFixture(t)
			tc.damage(t, dest)
			rep, err := VerifyBackup(context.Background(), dest)
			if err != nil {
				t.Fatalf("VerifyBackup: %v", err)
			}
			if rep.HasManifest != tc.hasMan {
				t.Errorf("HasManifest = %v, want %v", rep.HasManifest, tc.hasMan)
			}
			if tc.problems >= 0 && len(rep.Problems) != tc.problems || tc.problems < 0 && len(rep.Problems) == 0 {
				t.Errorf("Problems = %v, want %d", rep.Problems, tc.problems)
			}
			for name, state := range tc.want {
				if got := checkNamed(t, rep, name); got.State != state {
					t.Errorf("check %q = %q (%s), want %q", name, got.State, got.Detail, state)
				}
			}
			for _, w := range tc.detail {
				if d := checkNamed(t, rep, tc.detailName).Detail; !strings.Contains(d, w) {
					t.Errorf("%s detail = %q, want it to name %s", tc.detailName, d, w)
				}
			}
			if rep.Counts.Memories != 4 && tc.name != "row count mismatch" {
				t.Errorf("report memories = %d, want the 4 in the snapshot", rep.Counts.Memories)
			}
		})
	}
}

// TestVerifyRefusesSomethingThatIsNotADatabase: a mistyped path or a JSONL
// artifact passed where a backup was wanted; the error names the file.
func TestVerifyRefusesSomethingThatIsNotADatabase(t *testing.T) {
	dir := t.TempDir()
	notDB := filepath.Join(dir, "artifact.jsonl")
	if err := os.WriteFile(notDB, []byte(`{"type":"header","schema_version":2}`+"\n"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := VerifyBackup(context.Background(), notDB); err == nil || !strings.Contains(err.Error(), notDB) {
		t.Fatalf("error = %v, want a refusal naming the file", err)
	}
	if _, err := VerifyBackup(context.Background(), filepath.Join(dir, "no-such-file.db")); err == nil {
		t.Error("verify accepted a path that does not exist")
	}
}

// TestFileSHA256IsTheHashOfTheBytesOnDisk: the manifest's whole claim rests on
// this, and a streaming bug would produce a stable wrong answer.
func TestFileSHA256IsTheHashOfTheBytesOnDisk(t *testing.T) {
	path := filepath.Join(t.TempDir(), "blob")
	body := []byte(strings.Repeat("ghost", 5000))
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	sum := sha256.Sum256(body)
	if got, err := FileSHA256(path); err != nil || got != hex.EncodeToString(sum[:]) {
		t.Errorf("FileSHA256 = %q, %v, want %q", got, err, hex.EncodeToString(sum[:]))
	}
	if _, err := FileSHA256(filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Error("FileSHA256 of a missing file returned no error")
	}
}

// TestManifestPathSitsBesideTheSnapshot: the sidecar is found by convention, so
// its name derives from the snapshot's and is a legal filename on Windows as
// well as Linux - a backup is taken on one machine and checked on another.
func TestManifestPathSitsBesideTheSnapshot(t *testing.T) {
	if got, want := ManifestPath("/data/ghost.db.backup-20260926T153207Z"), "/data/ghost.db.backup-20260926T153207Z.manifest.json"; got != want {
		t.Errorf("ManifestPath = %q, want %q", got, want)
	}
	for _, r := range filepath.Base(ManifestPath("ghost.db")) {
		if strings.ContainsRune(`:\/:*?"<>| `, r) {
			t.Errorf("manifest name contains %q, illegal in a Windows path", r)
		}
	}
}
