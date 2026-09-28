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
// describe THIS snapshot. A manifest carrying a schema version, counts and a
// hash that belong to some other file is worse than no manifest, because
// `backup verify` would answer "ok" about a copy nobody checked.
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
	if m.ManifestVersion != ManifestVersion {
		t.Errorf("manifest_version = %d, want %d", m.ManifestVersion, ManifestVersion)
	}
	if m.SchemaVersion != SchemaVersion() {
		t.Errorf("schema_version = %d, want this build's %d", m.SchemaVersion, SchemaVersion())
	}
	if m.Counts != res.Counts {
		t.Errorf("manifest counts = %+v, the report's = %+v — the two describe one file", m.Counts, res.Counts)
	}
	if m.Counts.Memories != 4 {
		t.Errorf("manifest memories = %d, want the 4 the fixture wrote", m.Counts.Memories)
	}
	if m.Bytes != res.Bytes {
		t.Errorf("manifest bytes = %d, the report's = %d", m.Bytes, res.Bytes)
	}
	sum, err := FileSHA256(dest)
	if err != nil {
		t.Fatalf("FileSHA256: %v", err)
	}
	if m.SHA256 != sum {
		t.Errorf("manifest sha256 = %q, the file's = %q", m.SHA256, sum)
	}
	if _, err := time.Parse(time.RFC3339, m.CreatedAt); err != nil {
		t.Errorf("created_at = %q, want RFC3339: %v", m.CreatedAt, err)
	}
	// The manifest names what the user remembers, so it is a document: a
	// directory of backups is legible without running anything against it.
	raw, err := os.ReadFile(ManifestPath(dest))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	if !strings.Contains(string(raw), filepath.Base(dest)) {
		t.Errorf("manifest does not name the file it describes: %s", raw)
	}
	// And it is created at the width the database has, never group- or
	// world-readable like a plain create would leave it.
	info, err := os.Stat(ManifestPath(dest))
	if err != nil {
		t.Fatalf("stat manifest: %v", err)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("manifest mode = %04o, want no group or world bits", perm)
	}
}

// TestVerifyAcceptsTheBackupItJustWrote: the round trip. Every check passes on
// an untouched copy, which is the only thing that makes a failing check mean
// anything.
func TestVerifyAcceptsTheBackupItJustWrote(t *testing.T) {
	dest := backedUpFixture(t)

	rep, err := VerifyBackup(context.Background(), dest)
	if err != nil {
		t.Fatalf("VerifyBackup: %v", err)
	}
	if len(rep.Problems) != 0 {
		t.Errorf("Problems = %v, want none for a fresh backup", rep.Problems)
	}
	for _, name := range []string{"sha256", "integrity check", "schema version", "row counts"} {
		if got := checkNamed(t, rep, name); got.State != VerifyOK {
			t.Errorf("check %q = %q (%s), want ok", name, got.State, got.Detail)
		}
	}
	if !rep.HasManifest {
		t.Error("HasManifest = false for a backup `ghost backup` just wrote")
	}
	if rep.Counts.Memories != 4 {
		t.Errorf("report memories = %d, want the 4 in the snapshot", rep.Counts.Memories)
	}
}

// fileChangeCounterOffset is byte 24 of a SQLite file: the start of the 4-byte
// "file change counter" in the 100-byte database header
// (https://sqlite.org/fileformat.html#the_database_header). It is the field a
// write interrupted before the header was finalised leaves wrong, and it is the
// one place a single flipped bit is invisible to everything except a hash —
// integrity_check does not read it, and neither does a row count. Flipping it
// here is therefore not a contrivance to dodge the integrity check: it is the
// cheapest instance of the case the hash exists for, and a test that flipped a
// byte inside a b-tree page would prove only that SQLite noticed.
const fileChangeCounterOffset = 24

// flipChangeCounter flips the low bit of the header's file change counter in
// place, and fails if the file is too short to have a header.
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

// TestVerifyCatchesAFlippedByte: a backup is trusted on the strength of its
// hash, and a hash's whole job is the byte that changed for no visible reason.
// The byte flipped here is in the database header, so the file still opens, still
// passes SQLite's own integrity_check and still holds every row — which is
// exactly why a hash is in the manifest: without it a damaged copy reads as
// sound.
func TestVerifyCatchesAFlippedByte(t *testing.T) {
	dest := backedUpFixture(t)
	flipChangeCounter(t, dest)

	rep, err := VerifyBackup(context.Background(), dest)
	if err != nil {
		t.Fatalf("VerifyBackup: %v", err)
	}
	if len(rep.Problems) == 0 {
		t.Fatal("verify accepted a snapshot with a flipped byte")
	}
	if got := checkNamed(t, rep, "sha256"); got.State != VerifyFailed {
		t.Errorf("sha256 check = %q (%s), want failed", got.State, got.Detail)
	}
	// The point of the test: the file is structurally fine, so nothing but the
	// hash could have caught this.
	if got := checkNamed(t, rep, "integrity check"); got.State != VerifyOK {
		t.Errorf("integrity check = %q (%s) — this test flips a byte SQLite does not read, so anything but ok means it picked the wrong byte", got.State, got.Detail)
	}
	// And so are the counts: a restore of this file would have held every
	// memory. The only thing wrong with it is that it is not provably the file
	// the manifest describes.
	if got := checkNamed(t, rep, "row counts"); got.State != VerifyOK {
		t.Errorf("row counts check = %q (%s), want ok", got.State, got.Detail)
	}
}

// TestVerifyCatchesARowCountMismatch: the manifest's numbers are a claim about
// the file. A manifest claiming 99 memories over a file holding 4 is a restore
// that would silently lose rows, and the hash cannot see it — the file is
// byte-for-byte the one the manifest describes, so only the counts can.
func TestVerifyCatchesARowCountMismatch(t *testing.T) {
	dest := backedUpFixture(t)
	rewriteManifest(t, dest, func(m *BackupManifest) { m.Counts.Memories = 99 })

	rep, err := VerifyBackup(context.Background(), dest)
	if err != nil {
		t.Fatalf("VerifyBackup: %v", err)
	}
	got := checkNamed(t, rep, "row counts")
	if got.State != VerifyFailed {
		t.Fatalf("row counts check = %q (%s), want failed", got.State, got.Detail)
	}
	if !strings.Contains(got.Detail, "99") {
		t.Errorf("detail = %q, want it to name the count the manifest claims", got.Detail)
	}
	// The hash still matches: that is what makes this the counts' check and not
	// the hash's.
	if sha := checkNamed(t, rep, "sha256"); sha.State != VerifyOK {
		t.Errorf("sha256 check = %q (%s), want ok — the test left the snapshot untouched", sha.State, sha.Detail)
	}
}

// TestVerifyRefusesABackupFromANewerSchema: a copy from a Ghost that migrated
// past this one. Restoring it would be a downgrade, so verify says so and names
// both versions rather than reporting a file it cannot vouch for.
func TestVerifyRefusesABackupFromANewerSchema(t *testing.T) {
	dest := backedUpFixture(t)

	// The file itself moves past this build, which is what a newer Ghost's
	// snapshot carries, and the manifest is brought into line so the hash check
	// still passes and the schema check is the one under test.
	newer := SchemaVersion() + 1
	bumpUserVersion(t, dest, newer)
	rewriteManifest(t, dest, func(m *BackupManifest) { m.SchemaVersion = newer })

	rep, err := VerifyBackup(context.Background(), dest)
	if err != nil {
		t.Fatalf("VerifyBackup: %v", err)
	}
	if len(rep.Problems) == 0 {
		t.Fatal("verify accepted a backup from a newer schema")
	}
	got := checkNamed(t, rep, "schema version")
	if got.State != VerifyFailed {
		t.Fatalf("schema version check = %q (%s), want failed", got.State, got.Detail)
	}
	// Both versions are named, because "the schema version is wrong" leaves the
	// reader to work out which side is which.
	for _, want := range []string{"v" + strconv.Itoa(newer), "v" + strconv.Itoa(SchemaVersion())} {
		if !strings.Contains(got.Detail, want) {
			t.Errorf("detail = %q, want it to name %s", got.Detail, want)
		}
	}
}

// TestVerifyRefusesAManifestThatDisagreesWithTheFile: the two places a schema
// version is recorded have to agree. A manifest that understates its own file's
// version is a copy whose description cannot be trusted at all, and accepting it
// would make the schema check decorative.
func TestVerifyRefusesAManifestThatDisagreesWithTheFile(t *testing.T) {
	dest := backedUpFixture(t)
	rewriteManifest(t, dest, func(m *BackupManifest) { m.SchemaVersion = SchemaVersion() - 1 })

	rep, err := VerifyBackup(context.Background(), dest)
	if err != nil {
		t.Fatalf("VerifyBackup: %v", err)
	}
	got := checkNamed(t, rep, "schema version")
	if got.State != VerifyFailed {
		t.Fatalf("schema version check = %q (%s), want failed — the file is at v%d and the manifest claims v%d",
			got.State, got.Detail, SchemaVersion(), SchemaVersion()-1)
	}
	// The file itself is one this build opens happily, so the only problem
	// reported is the disagreement.
	if len(rep.Problems) != 1 {
		t.Errorf("Problems = %v, want just the disagreement", rep.Problems)
	}
}

// TestVerifyChecksACopyWithNoManifest: the copy a user actually restores from
// after a bad upgrade is written by the migration path, which writes no
// manifest. Refusing it would make the command useless exactly when it is
// wanted, so the structural checks run and the report says plainly which ones
// it could not.
func TestVerifyChecksACopyWithNoManifest(t *testing.T) {
	dest := backedUpFixture(t)
	if err := os.Remove(ManifestPath(dest)); err != nil {
		t.Fatalf("remove manifest: %v", err)
	}

	rep, err := VerifyBackup(context.Background(), dest)
	if err != nil {
		t.Fatalf("VerifyBackup: %v", err)
	}
	if rep.HasManifest {
		t.Error("HasManifest = true after the manifest was removed")
	}
	if len(rep.Problems) != 0 {
		t.Errorf("Problems = %v, want none: a manifest-less copy that is structurally sound is still a restorable backup", rep.Problems)
	}
	for _, name := range []string{"sha256", "row counts"} {
		if got := checkNamed(t, rep, name); got.State != VerifySkipped {
			t.Errorf("%s check = %q (%s), want skipped — there is no manifest to check against", name, got.State, got.Detail)
		}
	}
	if got := checkNamed(t, rep, "integrity check"); got.State != VerifyOK {
		t.Errorf("integrity check = %q (%s), want ok", got.State, got.Detail)
	}
	// The counts are still reported: they are what a restore would hold, and a
	// reader deciding whether this is the right copy needs them.
	if rep.Counts.Memories != 4 {
		t.Errorf("report memories = %d, want the 4 the snapshot holds", rep.Counts.Memories)
	}
}

// TestVerifyRefusesSomethingThatIsNotADatabase: a mistyped path, or a JSONL
// artifact passed where a backup was wanted. The message names the file, because
// "not a database" on its own is a statement about a file the user chose
// deliberately.
func TestVerifyRefusesSomethingThatIsNotADatabase(t *testing.T) {
	dir := t.TempDir()
	notDB := filepath.Join(dir, "artifact.jsonl")
	if err := os.WriteFile(notDB, []byte(`{"type":"header","schema_version":2}`+"\n"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	_, err := VerifyBackup(context.Background(), notDB)
	if err == nil {
		t.Fatal("verify accepted a file that is not a database")
	}
	if !strings.Contains(err.Error(), notDB) {
		t.Errorf("error = %v, want it to name the file", err)
	}

	if _, err := VerifyBackup(context.Background(), filepath.Join(dir, "no-such-file.db")); err == nil {
		t.Error("verify accepted a path that does not exist")
	}
}

// TestFileSHA256IsTheHashOfTheBytesOnDisk: the manifest's whole claim rests on
// this, and a streaming bug that read the file twice, or hashed only the header
// it had already consumed, would produce a stable wrong answer that no other
// check contradicts.
func TestFileSHA256IsTheHashOfTheBytesOnDisk(t *testing.T) {
	path := filepath.Join(t.TempDir(), "blob")
	body := []byte(strings.Repeat("ghost", 5000))
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	sum := sha256.Sum256(body)
	got, err := FileSHA256(path)
	if err != nil {
		t.Fatalf("FileSHA256: %v", err)
	}
	if want := hex.EncodeToString(sum[:]); got != want {
		t.Errorf("FileSHA256 = %q, want %q", got, want)
	}
	if _, err := FileSHA256(filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Error("FileSHA256 of a missing file returned no error")
	}
}

// TestManifestPathSitsBesideTheSnapshot: the sidecar is found by convention, so
// its name derives from the snapshot's and is a legal filename on Windows as
// well as Linux — a backup is taken on one machine and checked on another.
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
