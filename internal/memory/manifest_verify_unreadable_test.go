package memory

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A check that COULD NOT RUN is still a check, and the report says so with one
// row per check whether it ran or not (#1095). VerifyBackup used to return at the
// first read that failed, so a damaged copy printed two of the four documented
// checks and a reader could not tell those two from a build that only has two.
// The documented rule is the opposite: every check that can run does run, and one
// that cannot reports as skipped rather than being dropped.

// checkOrder returns the names of the checks a report ran, in order, so a report
// that quietly stopped running one fails on the LIST rather than only on the
// check that happened to be missing.
func checkOrder(rep VerifyReport) []string {
	names := make([]string, 0, len(rep.Checks))
	for _, c := range rep.Checks {
		names = append(names, c.Name)
	}
	return names
}

// cutShort truncates a snapshot to its first n bytes, which is what an
// interrupted copy looks like: a valid header and then nothing. The size is
// asserted rather than assumed, because a fixture that is already smaller than
// the cut would leave this file's tests asserting nothing at all.
func cutShort(t *testing.T, dest string, n int64) {
	t.Helper()
	if full := fileSize(t, dest); full <= n {
		t.Skipf("the snapshot is %d bytes, too small to cut to %d and leave a half-written database", full, n)
	}
	if err := os.Truncate(dest, n); err != nil {
		t.Fatalf("truncate %s to %d: %v", dest, n, err)
	}
}

// TestVerifyStillReportsEveryCheckWhenTheReadsFail: the issue's own reproduction,
// as a test. The copy opens — which is all a `head -c` away from a real one — and
// SQLite cannot read a schema or count rows out of it. Both of those checks must
// still be printed, as skipped, and the run must still refuse the file.
func TestVerifyStillReportsEveryCheckWhenTheReadsFail(t *testing.T) {
	dest := backedUpFixture(t)
	cutShort(t, dest, 20000)

	rep, err := VerifyBackup(context.Background(), dest)
	if err == nil {
		t.Fatalf("verify accepted a copy cut to 20000 bytes: %+v", rep)
	}
	// All four, in the order the docs say they run: the two that ran are not the
	// whole report, and a table missing two rows reads like a build with two
	// checks rather than a file the other two could not be asked about.
	got := checkOrder(rep)
	want := "sha256, integrity check, schema version, row counts"
	if strings.Join(got, ", ") != want {
		t.Fatalf("checks = [%s], want [%s]: a check that could not run is still reported", strings.Join(got, ", "), want)
	}
	for _, name := range []string{"schema version", "row counts"} {
		c := checkNamed(t, rep, name)
		if c.State != VerifySkipped {
			t.Errorf("%s check = %q (%s), want skipped: the read failed, which is not a verdict about the file", name, c.State, c.Detail)
		}
		if strings.TrimSpace(c.Detail) == "" {
			t.Errorf("%s check says nothing about the read that failed, so a reader is left to guess", name)
		}
	}
	// And nothing is claimed about rows nobody counted: the zero value of a count
	// is an empty database, and this file is not an empty database.
	if rep.CountsRead {
		t.Error("CountsRead is true for a copy nobody could count, so the report would print a row count it never read")
	}
	if rep.Counts != (BackupCounts{}) {
		t.Errorf("Counts = %+v, want the zero value: nothing was read", rep.Counts)
	}
}

// TestVerifyReportsRowCountsAsSkippedWhenTheyCannotBeCounted: the other half,
// and the one an operator hits with a real file — a sound database that is not a
// Ghost store. Its user_version reads, so the schema check RUNS and is judged;
// only the count fails, and only that row is skipped. Coupling the two would say
// a file whose version could not be read also has no countable rows, which is a
// claim about a second read nobody made.
func TestVerifyReportsRowCountsAsSkippedWhenTheyCannotBeCounted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "notghost.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE unrelated (x)`); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	rep, err := VerifyBackup(context.Background(), path)
	if err == nil {
		t.Fatalf("verify accepted a file whose rows cannot be counted: %+v", rep)
	}
	if got := strings.Join(checkOrder(rep), ", "); got != "sha256, integrity check, schema version, row counts" {
		t.Fatalf("checks = [%s], want all four", got)
	}
	counts := checkNamed(t, rep, "row counts")
	if counts.State != VerifySkipped {
		t.Fatalf("row counts check = %q (%s), want skipped", counts.State, counts.Detail)
	}
	// The detail carries the read's own error, so the report names what could
	// not be counted rather than reporting the check as merely absent. Read the
	// error off the same file rather than transcribing SQLite's wording.
	read, err := OpenReadDB(path)
	if err != nil {
		t.Fatalf("OpenReadDB: %v", err)
	}
	_, countErr := CountRows(context.Background(), read)
	if err := read.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if countErr == nil {
		t.Fatal("the fixture's rows can be counted, so this test would assert nothing")
	}
	if !strings.Contains(counts.Detail, countErr.Error()) {
		t.Errorf("row counts detail = %q, want it to carry the failed read %q", counts.Detail, countErr)
	}
	// The schema check RAN, so it is judged rather than skipped: the two are
	// separate reads, and dropping this one would leave a reader unable to tell
	// which question was asked.
	if schema := checkNamed(t, rep, "schema version"); schema.State == VerifySkipped {
		t.Errorf("schema version check = skipped (%s), want it judged: reading user_version is a different read from counting rows", schema.Detail)
	}
	if rep.CountsRead {
		t.Error("CountsRead is true for a file whose rows could not be counted")
	}
}
