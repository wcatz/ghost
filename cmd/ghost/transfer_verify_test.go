package main

import (
	"context"
	"database/sql"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// writePlainDatabase creates a sound SQLite database that is not a Ghost store:
// it opens and passes an integrity check but has none of the tables a restore
// would read.
func writePlainDatabase(t *testing.T, path string) error {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(`PRAGMA user_version = 18`); err != nil {
		return err
	}
	_, err = db.Exec(`CREATE TABLE unrelated (x)`)
	return err
}

// `ghost backup verify <file>` goes from "I have a copy" to "I know this copy is
// the one". These tests drive the parser and the report; the checking itself is
// internal/memory's.

// TestParseBackupVerifyArgs: verify reads exactly one file and takes no flags.
func TestParseBackupVerifyArgs(t *testing.T) {
	if got, err := parseBackupVerifyArgs([]string{"/backups/snap.db"}); err != nil || got != "/backups/snap.db" {
		t.Errorf("parseBackupVerifyArgs = %q, %v, want the path verbatim", got, err)
	}
	for _, tc := range []struct {
		args []string
		want string
	}{
		{nil, "exactly one"},
		{[]string{"a.db", "b.db"}, "exactly one"},
		{[]string{"--deep", "a.db"}, "unknown flag"},
	} {
		if _, err := parseBackupVerifyArgs(tc.args); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("parseBackupVerifyArgs(%v) error = %v, want %q", tc.args, err, tc.want)
		}
	}
}

// TestPrintBackupReportNamesTheManifest: the report is the only place the
// manifest path is printed.
func TestPrintBackupReportNamesTheManifest(t *testing.T) {
	dest := filepath.Join("/backups", "snap.db")
	var out strings.Builder
	res := memory.BackupResult{Path: dest, Bytes: 204800, ManifestPath: memory.ManifestPath(dest), Counts: memory.BackupCounts{Projects: 1, Memories: 4}}
	if err := printBackupReport(&out, res); err != nil {
		t.Fatalf("printBackupReport: %v", err)
	}
	if !strings.Contains(out.String(), dest) || !strings.Contains(out.String(), res.ManifestPath) {
		t.Errorf("report does not name the snapshot and the manifest:\n%s", out.String())
	}
}

// snapshotForVerify writes a one-memory backup and returns its path.
func snapshotForVerify(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	store := transferTestStore(t)
	if err := store.EnsureProject(ctx, "p1", "/src/p1", "one"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	if _, err := store.Create(ctx, "p1", memory.Memory{Category: "fact", Content: "one", Source: "mcp"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	dest := filepath.Join(t.TempDir(), "snap.db")
	if err := runBackupCore(ctx, store, io.Discard, dest); err != nil {
		t.Fatalf("runBackupCore: %v", err)
	}
	return dest
}

// testCheckLine asserts one check's name and state appear on a line of their own
// at the columns the report prints them in; a substring search cannot tell
// "sha256 failed" from a mention of the word elsewhere.
func testCheckLine(t *testing.T, report, name, state string) {
	t.Helper()
	for _, line := range strings.Split(report, "\n") {
		if strings.HasPrefix(line, strings.TrimRight("  "+name, " ")) && strings.Contains(line, " "+state) {
			return
		}
	}
	t.Errorf("no line reports %q as %q:\n%s", name, state, report)
}

// TestRunBackupVerifyCoreAcceptsAFreshBackup: backup then verify. Every check is
// named with the state it earned (a check that passed and one that did not run
// are different reassurances), "verified" is reserved for a run where every
// check ran, and the counts come from the file.
func TestRunBackupVerifyCoreAcceptsAFreshBackup(t *testing.T) {
	dest := snapshotForVerify(t)
	var out strings.Builder
	if err := runBackupVerifyCore(context.Background(), &out, dest); err != nil {
		t.Fatalf("runBackupVerifyCore: %v\n%s", err, out.String())
	}
	for _, name := range []string{"sha256", "integrity check", "schema version", "row counts"} {
		testCheckLine(t, out.String(), name, "ok")
	}
	for _, want := range []string{"verified " + dest, "1 project and 1 memory"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("verify report does not say %q:\n%s", want, out.String())
		}
	}
}

// TestRunBackupVerifyCoreFailsOnADamagedBackup: a command that exits 0 on a
// damaged copy is worse than none. The manifest's own hash is edited to another
// well-formed digest (a malformed one would be refused as an unreadable manifest
// before the sha256 check ran), so only a verify that checks the file passes.
func TestRunBackupVerifyCoreFailsOnADamagedBackup(t *testing.T) {
	dest := snapshotForVerify(t)
	manifest := memory.ManifestPath(dest)
	raw, err := os.ReadFile(manifest)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	const field = `"sha256": "`
	at := strings.Index(string(raw), field)
	if at < 0 {
		t.Fatalf("manifest has no sha256 field to tamper with:\n%s", raw)
	}
	digit := at + len(field)
	swapped := byte('0')
	if raw[digit] == '0' {
		swapped = '1'
	}
	if err := os.WriteFile(manifest, []byte(string(raw[:digit])+string(swapped)+string(raw[digit+1:])), 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	var out strings.Builder
	if err := runBackupVerifyCore(context.Background(), &out, dest); err == nil || !strings.Contains(err.Error(), "sha256") {
		t.Fatalf("runBackupVerifyCore error = %v, want a refusal naming sha256:\n%s", err, out.String())
	}
	testCheckLine(t, out.String(), "sha256", "failed")
	testCheckLine(t, out.String(), "integrity check", "ok")
	if !strings.Contains(out.String(), "refusing "+dest) {
		t.Errorf("the report does not say it is refusing the file:\n%s", out.String())
	}
}

// TestRunBackupVerifyCoreNamesAMissingFile: a typo or a lost file; the message
// names the path.
func TestRunBackupVerifyCoreNamesAMissingFile(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "no-such-backup.db")
	if err := runBackupVerifyCore(context.Background(), &strings.Builder{}, missing); err == nil || !strings.Contains(err.Error(), missing) {
		t.Errorf("error = %v, want it to name the path", err)
	}
}

// TestRunBackupVerifyCoreDoesNotCallAManifestLessFileVerified: the sidecar is the
// part a user forgets to copy, and the one file whose hash was never checked is
// reported "checked", with the two manifest-derived checks skipped and the counts
// still shown.
func TestRunBackupVerifyCoreDoesNotCallAManifestLessFileVerified(t *testing.T) {
	dest := snapshotForVerify(t)
	if err := os.Remove(memory.ManifestPath(dest)); err != nil {
		t.Fatalf("remove the manifest: %v", err)
	}
	var out strings.Builder
	if err := runBackupVerifyCore(context.Background(), &out, dest); err != nil {
		t.Fatalf("runBackupVerifyCore refused a sound manifest-less copy: %v\n%s", err, out.String())
	}
	text := out.String()
	if strings.Contains(text, "verified ") {
		t.Errorf("the report calls a file whose hash was never checked verified:\n%s", text)
	}
	for _, want := range []string{"checked " + dest, "no manifest at " + memory.ManifestPath(dest), "1 project and 1 memory"} {
		if !strings.Contains(text, want) {
			t.Errorf("the report does not say %q:\n%s", want, text)
		}
	}
	testCheckLine(t, text, "integrity check", "ok")
	testCheckLine(t, text, "schema version", "ok")
	testCheckLine(t, text, "sha256", "skipped")
	testCheckLine(t, text, "row counts", "skipped")
}

// TestRunBackupVerifyCoreClaimsNothingAboutAFileItCouldNotRead: a file whose
// tables cannot be read (not a Ghost store, or cut short so SQLite finds no
// schema) gets a refusing report with no counts ("no rows" is a claim about a
// file) and every documented check printed, the unrunnable ones skipped.
func TestRunBackupVerifyCoreClaimsNothingAboutAFileItCouldNotRead(t *testing.T) {
	plain := filepath.Join(t.TempDir(), "notghost.db")
	if err := writePlainDatabase(t, plain); err != nil {
		t.Fatalf("seed: %v", err)
	}
	cut := snapshotForVerify(t)
	if info, err := os.Stat(cut); err != nil || info.Size() <= 20000 {
		t.Skipf("the snapshot is too small to cut and leave a half-written database: %v", err)
	}
	if err := os.Truncate(cut, 20000); err != nil {
		t.Fatalf("truncate snapshot: %v", err)
	}
	for _, path := range []string{plain, cut} {
		var out strings.Builder
		if err := runBackupVerifyCore(context.Background(), &out, path); err == nil {
			t.Fatalf("runBackupVerifyCore accepted %s:\n%s", path, out.String())
		}
		text := out.String()
		if strings.Contains(text, "no rows") || strings.Contains(text, "verified ") || !strings.Contains(text, "refusing "+path) {
			t.Errorf("the report claims rows or verification for an unreadable file, or does not refuse it:\n%s", text)
		}
	}
}
