package main

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// writePlainDatabase creates a sound SQLite database that is not a Ghost store:
// it opens, passes an integrity check, and has none of the tables a restore
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

// `ghost backup verify <file>` is the path from "I have a copy" to "I know this
// copy is the one". The tests here drive the argument parser and the report, the
// two things a user meets first, and leave the checking itself to
// internal/memory.

// TestParseBackupVerifyArgs: verify reads exactly one file, and nothing else.
// Two files in one run would report one verdict for a question about two
// answers, and a flag would be a flag this command has no meaning for.
func TestParseBackupVerifyArgs(t *testing.T) {
	t.Run("one file", func(t *testing.T) {
		got, err := parseBackupVerifyArgs([]string{"/backups/snap.db"})
		if err != nil {
			t.Fatalf("parseBackupVerifyArgs: %v", err)
		}
		if got != "/backups/snap.db" {
			t.Errorf("file = %q, want the path verbatim", got)
		}
	})
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{name: "no file", args: nil, want: "exactly one"},
		{name: "two files", args: []string{"a.db", "b.db"}, want: "exactly one"},
		{name: "a flag", args: []string{"--out", "a.db"}, want: "unknown flag"},
		{name: "an unknown flag with a file", args: []string{"--deep", "a.db"}, want: "unknown flag"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseBackupVerifyArgs(tc.args); err == nil {
				t.Fatalf("parseBackupVerifyArgs(%v) accepted it", tc.args)
			} else if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to say %q", err, tc.want)
			}
		})
	}
}

// TestPrintBackupReportNamesTheManifest: a backup that wrote a manifest and did
// not say where is a backup the user has to go looking for. The report is the
// only place the path is printed, so the path has to be in it.
func TestPrintBackupReportNamesTheManifest(t *testing.T) {
	dest := filepath.Join("/backups", "snap.db")
	manifest := memory.ManifestPath(dest)
	var out strings.Builder
	res := memory.BackupResult{
		Path:         dest,
		Bytes:        204800,
		ManifestPath: manifest,
		Counts:       memory.BackupCounts{Projects: 1, Memories: 4, MemoryLinks: 1, Tasks: 1, Decisions: 1},
	}
	if err := printBackupReport(&out, res); err != nil {
		t.Fatalf("printBackupReport: %v", err)
	}
	text := out.String()
	if !strings.Contains(text, dest) {
		t.Errorf("report does not name the snapshot:\n%s", text)
	}
	if !strings.Contains(text, manifest) {
		t.Errorf("report does not name the manifest %q:\n%s", manifest, text)
	}
}

// TestRunBackupVerifyCoreAcceptsAFreshBackup: the whole command over a real
// snapshot — backup, then verify, against a temp store. A verify that only ever
// says "ok" would pass a round trip too, so the next test is the other half.
func TestRunBackupVerifyCoreAcceptsAFreshBackup(t *testing.T) {
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

	var out strings.Builder
	if err := runBackupVerifyCore(ctx, &out, dest); err != nil {
		t.Fatalf("runBackupVerifyCore: %v\n%s", err, out.String())
	}
	text := out.String()
	// Every check named AND carrying the state it earned, so a reader can tell a
	// check that passed from a check that did not run — the two are not the same
	// reassurance, and a substring search for "ok" cannot tell them apart.
	for _, check := range []struct{ name, state string }{
		{"sha256", "ok"},
		{"integrity check", "ok"},
		{"schema version", "ok"},
		{"row counts", "ok"},
	} {
		want := fmt.Sprintf("%-16s %-8s", check.name, check.state)
		if !strings.Contains(text, want) {
			t.Errorf("verify report is missing the line %q:\n%s", want, text)
		}
	}
	// "verified" is the strongest word the command prints and is reserved for a
	// run where every check ran.
	if !strings.Contains(text, "verified "+dest) {
		t.Errorf("verify report does not open with the verdict for a fully checked file:\n%s", text)
	}
	// The counts, in the report, from the file — the same numbers the backup
	// printed. A verify that reported only pass/fail would leave a reader with
	// no way to tell WHICH copy they were about to restore.
	if !strings.Contains(text, "1 project and 1 memory") {
		t.Errorf("verify report does not state what the file holds:\n%s", text)
	}
}

// TestRunBackupVerifyCoreFailsOnADamagedBackup: a command that exits 0 on a
// damaged copy is worse than no command, because it is the one a script would
// ask. The report is still printed, so the reader sees which check said so.
func TestRunBackupVerifyCoreFailsOnADamagedBackup(t *testing.T) {
	ctx := context.Background()
	store := transferTestStore(t)
	if err := store.EnsureProject(ctx, "p1", "/src/p1", "one"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	dest := filepath.Join(t.TempDir(), "snap.db")
	if err := runBackupCore(ctx, store, io.Discard, dest); err != nil {
		t.Fatalf("runBackupCore: %v", err)
	}

	// The manifest's own hash, edited to a wrong value: the file is untouched
	// and perfectly restorable, so the only thing wrong is the description of
	// it. A verify that trusted the file because it opens would pass this.
	manifest := memory.ManifestPath(dest)
	raw, err := os.ReadFile(manifest)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	tampered := strings.Replace(string(raw), `"sha256": "`, `"sha256": "0`, 1)
	if tampered == string(raw) {
		t.Fatalf("manifest has no sha256 field to tamper with:\n%s", raw)
	}
	if err := os.WriteFile(manifest, []byte(tampered), 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	var out strings.Builder
	if err := runBackupVerifyCore(ctx, &out, dest); err == nil {
		t.Fatalf("runBackupVerifyCore accepted a snapshot whose hash does not match:\n%s", out.String())
	} else if !strings.Contains(err.Error(), "sha256") {
		t.Errorf("error = %v, want it to name the check that failed", err)
	}
	testCheckLine(t, out.String(), "sha256", "failed")
	testCheckLine(t, out.String(), "integrity check", "ok")
	if !strings.Contains(out.String(), "refusing "+dest) {
		t.Errorf("the report does not say it is refusing the file:\n%s", out.String())
	}
}

// TestRunBackupVerifyCoreNamesAMissingFile: `ghost backup verify` on a path that
// is not there is a typo or a lost file, and the difference decides what the
// reader does next, so the message has to make it.
func TestRunBackupVerifyCoreNamesAMissingFile(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "no-such-backup.db")
	var out strings.Builder
	err := runBackupVerifyCore(context.Background(), &out, missing)
	if err == nil {
		t.Fatal("verify accepted a path that does not exist")
	}
	if !strings.Contains(err.Error(), missing) {
		t.Errorf("error = %v, want it to name the path", err)
	}
}

// testCheckLine asserts one check's name and state appear on a line of their
// own in a report, at the columns the report prints them in. A bare substring
// search cannot tell "sha256 failed" from a mention of the word elsewhere, and
// the difference between those is the whole report.
func testCheckLine(t *testing.T, report, name, state string) {
	t.Helper()
	for _, line := range strings.Split(report, "\n") {
		if strings.HasPrefix(line, strings.TrimRight("  "+name, " ")) &&
			strings.Contains(line, " "+state) {
			return
		}
	}
	t.Errorf("no line reports %q as %q:\n%s", name, state, report)
}

// TestRunBackupVerifyCoreDoesNotCallAManifestLessFileVerified: the sidecar is the
// part of a backup a user forgets to copy, so a file without one is the single
// most likely thing to arrive here — and it is the one file whose hash was never
// checked. It is reported as "checked" with the two manifest-derived checks
// marked skipped: the same sound advice as "verified", minus the claim.
func TestRunBackupVerifyCoreDoesNotCallAManifestLessFileVerified(t *testing.T) {
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
	if err := os.Remove(memory.ManifestPath(dest)); err != nil {
		t.Fatalf("remove the manifest: %v", err)
	}

	var out strings.Builder
	if err := runBackupVerifyCore(ctx, &out, dest); err != nil {
		t.Fatalf("runBackupVerifyCore refused a manifest-less copy that is structurally sound: %v\n%s", err, out.String())
	}
	text := out.String()
	if strings.Contains(text, "verified ") {
		t.Errorf("the report calls a file whose hash was never checked verified:\n%s", text)
	}
	if !strings.Contains(text, "checked "+dest) {
		t.Errorf("the report does not say it checked the file rather than verifying it:\n%s", text)
	}
	testCheckLine(t, text, "integrity check", "ok")
	testCheckLine(t, text, "schema version", "ok")
	testCheckLine(t, text, "sha256", "skipped")
	testCheckLine(t, text, "row counts", "skipped")
	if !strings.Contains(text, "no manifest at "+memory.ManifestPath(dest)) {
		t.Errorf("the report does not say where the manifest it could not find should have been:\n%s", text)
	}
	// The counts are still reported: they are what a restore would hold, and a
	// reader deciding whether this is the right copy needs them.
	if !strings.Contains(text, "1 project and 1 memory") {
		t.Errorf("the report does not state what the file holds:\n%s", text)
	}
}

// TestRunBackupVerifyCoreClaimsNothingAboutAFileItCouldNotRead: a file that
// opens but whose tables cannot be read gets a report with no counts, and the
// counts clause is dropped rather than filled in with zeroes. "no rows" is a
// claim about a file; it is exactly the wrong thing to say about one nobody
// managed to read.
func TestRunBackupVerifyCoreClaimsNothingAboutAFileItCouldNotRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "notghost.db")
	// A real, sound SQLite database that is not a Ghost store: it opens, and it
	// has no projects table.
	if err := writePlainDatabase(t, path); err != nil {
		t.Fatalf("seed: %v", err)
	}

	var out strings.Builder
	if err := runBackupVerifyCore(context.Background(), &out, path); err == nil {
		t.Fatalf("runBackupVerifyCore accepted a file that is not a Ghost store:\n%s", out.String())
	}
	text := out.String()
	if strings.Contains(text, "no rows") {
		t.Errorf("the report claims a row count for a file it could not read:\n%s", text)
	}
	if strings.Contains(text, "verified ") {
		t.Errorf("the report verifies a file whose checks could not finish:\n%s", text)
	}
	if !strings.Contains(text, "refusing "+path) {
		t.Errorf("the report does not say it is refusing the file:\n%s", text)
	}
}
