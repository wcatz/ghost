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

// bumpUserVersion writes a schema version onto a snapshot. A snapshot is opened
// writeable here even though the product never does that: OpenDB would migrate
// the version back, and these cases need a file at a version this build does
// not expect.
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

// TestVerifyReportsAHashMismatchOnATruncatedCopy: the hash check runs FIRST so a
// truncated copy (which has no schema to read, and ends in a hard error) still
// reports the size on both sides, and claims no row counts it never read.
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
	for _, want := range []string{strconv.FormatInt(full, 10), strconv.Itoa(left)} {
		if !strings.Contains(hash.Detail, want) {
			t.Errorf("detail = %q, want it to name %s", hash.Detail, want)
		}
	}
	if rep.Checks[0].Name != "sha256" {
		t.Errorf("the first check reported is %q, want sha256", rep.Checks[0].Name)
	}
	if rep.CountsRead || rep.Counts != (BackupCounts{}) {
		t.Errorf("CountsRead=%v Counts=%+v, want nothing claimed about rows nobody counted", rep.CountsRead, rep.Counts)
	}
}

// TestVerifyAcceptsAnOlderSchemaBecauseARestoreProducesOne: every upgrade leaves
// a pre-migration copy at a LOWER version, so that direction is restorable, not
// a failure, and the detail says the next open migrates it.
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
	if got.State != VerifyOK || len(rep.Problems) != 0 || !strings.Contains(got.Detail, "migrat") {
		t.Errorf("schema version check = %q (%s), problems %v, want ok and a note that the next open migrates", got.State, got.Detail, rep.Problems)
	}
}

// TestVerifyRefusesAManifestItCannotRead: a sidecar that is present but damaged
// (not JSON, or from another format version) is refused, not treated as absent,
// which would turn a damaged backup into one that merely looks unchecked.
func TestVerifyRefusesAManifestItCannotRead(t *testing.T) {
	for _, tc := range []struct {
		name   string
		damage func(t *testing.T, dest string)
		want   func(dest string) string
	}{
		{"not json", func(t *testing.T, dest string) {
			if err := os.WriteFile(ManifestPath(dest), []byte("{ this is not json"), 0o600); err != nil {
				t.Fatalf("damage the manifest: %v", err)
			}
		}, ManifestPath},
		{"another format version", func(t *testing.T, dest string) {
			rewriteManifest(t, dest, func(m *BackupManifest) { m.ManifestVersion = ManifestVersion + 1 })
		}, func(string) string { return "format v" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dest := backedUpFixture(t)
			tc.damage(t, dest)
			rep, err := VerifyBackup(context.Background(), dest)
			if err == nil || !strings.Contains(err.Error(), tc.want(dest)) {
				t.Fatalf("VerifyBackup error = %v, want a refusal naming %q (%+v)", err, tc.want(dest), rep)
			}
			if rep.HasManifest {
				t.Error("HasManifest is true for a manifest that could not be read")
			}
		})
	}
}

// TestCheckIntegrityReportsEveryProblemNotJustTheFirst: a check that reported
// only the first of SQLite's findings would describe a damaged file as having
// one, and a cap applied to rows would bound nothing (a driver may deliver the
// whole report in one row). It runs on a plain b-tree database because damaging
// a Ghost store lands on the FTS5 shadow tables and aborts the pragma. The cap
// states how many findings it dropped.
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
	if findings := strings.Count(got.Detail, "Tree "); findings < 2 {
		t.Errorf("detail names %d findings, want at least 2: %q", findings, got.Detail)
	}
	if !strings.Contains(got.Detail, "more)") {
		t.Errorf("detail = %q, want an explicit count of the findings left out", got.Detail)
	}
	if strings.Contains(got.Detail, "\n") {
		t.Errorf("detail carries a newline, which would break the reportthe report columns: %q", got.Detail)
	}
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

// TestManifestReplacedIsNarrowedToTheDatabaseWidth: the manifest is REPLACED, and
// os.OpenFile's mode only applies to a file it creates, so a sidecar left at 0644
// must come out at 0600 rather than sit wide beside the memory database.
func TestManifestReplacedIsNarrowedToTheDatabaseWidth(t *testing.T) {
	store := backupTestStore(t)
	seedBackupFixture(t, store)
	dest := filepath.Join(t.TempDir(), "snapshot.db")
	if _, err := store.Backup(context.Background(), dest); err != nil {
		t.Fatalf("Backup: %v", err)
	}
	manifest := ManifestPath(dest)
	if err := os.Chmod(manifest, 0o644); err != nil {
		t.Fatalf("widen the manifest: %v", err)
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
		t.Errorf("manifest mode = %04o after being replaced, want 0600", perm)
	}
	m, err := ReadBackupManifest(manifest)
	if err != nil || m.Counts.Memories != 4 || m.CreatedAt != at.Format(time.RFC3339) {
		t.Errorf("manifest = %+v, %v, want the counts and instant that were written", m, err)
	}
}
