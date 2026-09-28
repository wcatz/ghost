package memory

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The three cases the first round of this file did not cover, and the two of them
// that changed a verdict rather than added a message: a copy that is not the file
// its manifest describes, in the shape where the hash still had to run; and a copy
// from an OLDER Ghost, which is restorable and therefore not a failure.

// bumpUserVersion writes a schema version onto a snapshot. A snapshot is opened
// writeable here even though the product never does that: OpenDB would migrate
// the version back, and the point of these cases is a file that is at a version
// this build does not expect.
func bumpUserVersion(t *testing.T, dest string, version int) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(dest))
	if err != nil {
		t.Fatalf("open %s writable: %v", dest, err)
	}
	if _, err := db.Exec(fmt.Sprintf("PRAGMA user_version = %d", version)); err != nil {
		t.Fatalf("set user_version = %d: %v", version, err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close %s: %v", dest, err)
	}
}

// TestVerifyReportsAHashMismatchOnATruncatedCopy: a copy cut short is the case
// the hash exists for, and the one where running the database checks FIRST would
// have said nothing useful — a truncated file has no schema to read, so the count
// and version checks cannot run, and the single most informative fact (the
// manifest says 245760 bytes, the file is 4096) would never have been reported.
//
// The run also ends in a hard error, because SQLite cannot read a schema out of
// half a file. That is the point: the report still has to carry the diagnosis, so
// the assertions here are on the report and the error is only required.
func TestVerifyReportsAHashMismatchOnATruncatedCopy(t *testing.T) {
	dest := backedUpFixture(t)
	full := fileSize(t, dest)
	const left = 4096
	if err := os.Truncate(dest, left); err != nil {
		t.Fatalf("truncate %s to %d: %v", dest, left, err)
	}

	rep, _ := VerifyBackup(context.Background(), dest)
	if len(rep.Problems) == 0 {
		t.Fatalf("verify accepted a truncated copy: %+v", rep)
	}
	hash := checkNamed(t, rep, "sha256")
	if hash.State != VerifyFailed {
		t.Fatalf("sha256 check = %q (%s), want failed", hash.State, hash.Detail)
	}
	// The size, in bytes on both sides, because a digest a reader has to compare
	// by hand is a worse answer to "is this the whole file" than two numbers.
	for _, want := range []string{strconv.FormatInt(full, 10), strconv.Itoa(left)} {
		if !strings.Contains(hash.Detail, want) {
			t.Errorf("detail = %q, want it to name %s", hash.Detail, want)
		}
	}
	// The hash check is reported FIRST, ahead of everything that needs the
	// database, so a reader meets the diagnosis before the checks that could not
	// run.
	if rep.Checks[0].Name != "sha256" {
		t.Errorf("the first check reported is %q, want sha256: it is the only one that can answer for a file that is not a database", rep.Checks[0].Name)
	}
	// And nothing is claimed about rows nobody counted.
	if rep.CountsRead {
		t.Error("CountsRead is true for a truncated copy, so the report would print a row count it never read")
	}
	if rep.Counts != (BackupCounts{}) {
		t.Errorf("Counts = %+v, want the zero value: nothing was read", rep.Counts)
	}
}

// TestVerifyRefusesANewerSchemaAndSaysToUpgrade: a file from a newer Ghost
// cannot be opened by this one at all — OpenDB refuses it outright — so that is
// the one direction of the schema check that is a failure, and the direction whose
// message has to name UPGRADE. "The file is newer" with the obvious reading
// ("older stores get migrated") would send a user the wrong way.
func TestVerifyRefusesANewerSchemaAndSaysToUpgrade(t *testing.T) {
	dest := backedUpFixture(t)
	newer := SchemaVersion() + 1
	bumpUserVersion(t, dest, newer)
	rewriteManifest(t, dest, func(m *BackupManifest) { m.SchemaVersion = newer })

	rep, err := VerifyBackup(context.Background(), dest)
	if err != nil {
		t.Fatalf("VerifyBackup: %v", err)
	}
	got := checkNamed(t, rep, "schema version")
	if got.State != VerifyFailed {
		t.Fatalf("schema version check = %q (%s), want failed", got.State, got.Detail)
	}
	if !strings.Contains(got.Detail, "upgrade") {
		t.Errorf("detail = %q, want it to say upgrade: a newer file is refused precisely because this build cannot open it", got.Detail)
	}
	if strings.Contains(got.Detail, "migrate") {
		t.Errorf("detail = %q, must not tell a user to migrate a file from a NEWER Ghost", got.Detail)
	}
}

// TestVerifyAcceptsAnOlderSchemaBecauseARestoreProducesOne: the documented
// restore path migrates the restored file on the next open, and every upgrade a
// user has ever done left a pre-migration copy behind at a LOWER version than the
// build that wrote it. Refusing that direction would make the command reject the
// one file it exists to protect.
func TestVerifyAcceptsAnOlderSchemaBecauseARestoreProducesOne(t *testing.T) {
	dest := backedUpFixture(t)
	older := SchemaVersion() - 1
	bumpUserVersion(t, dest, older)
	rewriteManifest(t, dest, func(m *BackupManifest) { m.SchemaVersion = older })

	rep, err := VerifyBackup(context.Background(), dest)
	if err != nil {
		t.Fatalf("VerifyBackup: %v", err)
	}
	got := checkNamed(t, rep, "schema version")
	if got.State != VerifyOK {
		t.Errorf("schema version check = %q (%s), want ok: an older file is what every restore of a pre-migration copy produces", got.State, got.Detail)
	}
	if len(rep.Problems) != 0 {
		t.Errorf("Problems = %v, want none", rep.Problems)
	}
	// Reported as restorable rather than as identical, so a reader knows the
	// next open will change the file.
	if !strings.Contains(got.Detail, "migrat") {
		t.Errorf("detail = %q, want it to say the next open migrates the file", got.Detail)
	}
}

// TestVerifyRefusesAManifestItCannotRead: a sidecar that is present but damaged
// is not an absent optional file. Treating it as absent would report a snapshot
// nobody can check as merely unchecked — the exact downgrade that turns a damaged
// backup into one that still looks fine.
func TestVerifyRefusesAManifestItCannotRead(t *testing.T) {
	dest := backedUpFixture(t)
	manifest := ManifestPath(dest)
	if err := os.WriteFile(manifest, []byte("{ this is not json"), 0o600); err != nil {
		t.Fatalf("damage the manifest: %v", err)
	}

	rep, err := VerifyBackup(context.Background(), dest)
	if err == nil {
		t.Fatalf("verify accepted a damaged manifest: %+v", rep)
	}
	if !strings.Contains(err.Error(), manifest) {
		t.Errorf("error = %v, want it to name the unreadable manifest", err)
	}
	if rep.HasManifest {
		t.Error("HasManifest is true for a manifest that could not be read")
	}
}

// TestVerifyRefusesAManifestFromAnotherFormatVersion: the manifest's own version
// is a wire format, and a file this build cannot interpret is refused rather than
// read on the writer's assumptions.
func TestVerifyRefusesAManifestFromAnotherFormatVersion(t *testing.T) {
	dest := backedUpFixture(t)
	rewriteManifest(t, dest, func(m *BackupManifest) { m.ManifestVersion = ManifestVersion + 1 })

	_, err := VerifyBackup(context.Background(), dest)
	if err == nil {
		t.Fatal("verify accepted a manifest from a format version this build does not read")
	}
	if !strings.Contains(err.Error(), "format v") {
		t.Errorf("error = %v, want it to name the format versions", err)
	}
}

// TestCheckIntegrityReportsEveryProblemNotJustTheFirst: PRAGMA
// integrity_check returns ONE ROW PER PROBLEM, so reading only the first — which
// is what a QueryRow does — would report a file with fifty damaged pages as
// having one. This runs against a plain database built for the purpose: a Ghost
// store has FTS shadow tables whose pages fail to load before the check can even
// start, which tests the "could not check" path and nothing about row counting.
//
// The cap keeps a catastrophic file from producing an unreadable report, and the
// count of what was dropped is stated rather than the cap being silent.
func TestCheckIntegrityReportsEveryProblemNotJustTheFirst(t *testing.T) {
	path := filepath.Join(t.TempDir(), "plain.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE blobs (id INTEGER PRIMARY KEY, body BLOB)`); err != nil {
		t.Fatalf("create: %v", err)
	}
	body := make([]byte, 400)
	for i := range body {
		body[i] = byte(i)
	}
	for i := 0; i < 400; i++ {
		if _, err := db.Exec(`INSERT INTO blobs (body) VALUES (?)`, body); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Scramble the body of every leaf page, which is what makes a page's cells
	// unreadable rather than merely stale.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	const page = 4096
	const want = 6
	damaged := 0
	for offset := 2 * page; offset+page <= len(raw) && damaged < want; offset += page {
		for i := 8; i < page-8; i += 64 {
			raw[offset+i] ^= 0xA5
		}
		damaged++
	}
	if damaged < want {
		t.Skipf("the database is only %d bytes, too small to damage %d pages", len(raw), want)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	checkDB, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path))
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer checkDB.Close() //nolint:errcheck
	got := checkIntegrity(context.Background(), checkDB, path)

	if got.State != VerifyFailed {
		t.Fatalf("integrity check = %q (%s), want failed on %d scrambled pages", got.State, got.Detail, damaged)
	}
	if !strings.Contains(got.Detail, "damaged") {
		t.Errorf("detail = %q, want SQLite's own words carried through", got.Detail)
	}
	// More than the FIRST finding, which is the whole point: a driver may hand
	// the whole report over in one row, and judging or reporting one of them is
	// what a QueryRow would do.
	if findings := strings.Count(got.Detail, "Tree "); findings < 2 {
		t.Errorf("detail names %d findings, want at least 2: %q", findings, got.Detail)
	}
	// And — the fixture is deliberately larger than the cap — an explicit count
	// of what was left out rather than silence.
	if !strings.Contains(got.Detail, "more)") {
		t.Errorf("detail = %q, want an explicit count of the findings left out", got.Detail)
	}
	// One line, because the report is a table and a newline in a detail shifts
	// every column under it.
	if strings.Contains(got.Detail, "\n") {
		t.Errorf("detail carries a newline, which would break the reportthe report columns: %q", got.Detail)
	}
	// And the same function still says "ok" about a sound file, so the failure
	// above is about this file rather than about the function always failing.
	sound := filepath.Join(t.TempDir(), "sound.db")
	soundDB, err := sql.Open("sqlite", sound)
	if err != nil {
		t.Fatalf("open sound: %v", err)
	}
	if _, err := soundDB.Exec(`CREATE TABLE t (x)`); err != nil {
		t.Fatalf("create sound: %v", err)
	}
	if err := soundDB.Close(); err != nil {
		t.Fatalf("close sound: %v", err)
	}
	reopened, err := sql.Open("sqlite", "file:"+filepath.ToSlash(sound))
	if err != nil {
		t.Fatalf("reopen sound: %v", err)
	}
	defer reopened.Close() //nolint:errcheck
	if ok := checkIntegrity(context.Background(), reopened, sound); ok.State != VerifyOK {
		t.Errorf("integrity check of a sound file = %q (%s), want ok", ok.State, ok.Detail)
	}
}

// TestManifestReplacedIsNarrowedToTheDatabaseWidth: the manifest is REPLACED
// rather than refused on (see writeBackupManifest), which means the open mode is
// the one thing that does not apply — os.OpenFile's mode only sets the width of a
// file it CREATES. A sidecar left at 0644 by anything else would otherwise be
// rewritten at 0644, and a full description of the memory database would sit in a
// directory the user chose to share.
func TestManifestReplacedIsNarrowedToTheDatabaseWidth(t *testing.T) {
	store := backupTestStore(t)
	seedBackupFixture(t, store)
	dest := filepath.Join(t.TempDir(), "snapshot.db")
	if _, err := store.Backup(context.Background(), dest); err != nil {
		t.Fatalf("Backup: %v", err)
	}
	manifest := ManifestPath(dest)
	// Stand in for a manifest something else wrote: wide, and not even JSON.
	// Chmod rather than a mode argument to the write — os.WriteFile does not
	// change the width of a file that already exists, so writing at 0644 over
	// the 0600 one this test just created would leave it at 0600 and assert
	// nothing at all.
	if err := os.Chmod(manifest, 0o644); err != nil {
		t.Fatalf("widen the manifest: %v", err)
	}
	if info, err := os.Stat(manifest); err != nil || info.Mode().Perm() != 0o644 {
		t.Fatalf("the manifest is not wide to begin with (mode %v, err %v): the rest of this test would assert nothing",
			func() os.FileMode {
				if info == nil {
					return 0
				}
				return info.Mode().Perm()
			}(), err)
	}

	at := time.Date(2026, 9, 26, 15, 32, 7, 0, time.UTC)
	if _, err := writeBackupManifest(dest, BackupCounts{Memories: 4}, at); err != nil {
		t.Fatalf("writeBackupManifest over an existing manifest: %v", err)
	}
	info, err := os.Stat(manifest)
	if err != nil {
		t.Fatalf("stat manifest: %v", err)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("manifest mode = %04o after being replaced, want 0600: an open mode does not apply to a file that already existed", perm)
	}
	m, err := ReadBackupManifest(manifest)
	if err != nil {
		t.Fatalf("ReadBackupManifest after replacement: %v", err)
	}
	if m.Counts.Memories != 4 {
		t.Errorf("manifest counts = %+v, want the 4 that were written", m.Counts)
	}
	if m.CreatedAt != at.Format(time.RFC3339) {
		t.Errorf("created_at = %q, want the instant the caller passed", m.CreatedAt)
	}
}
