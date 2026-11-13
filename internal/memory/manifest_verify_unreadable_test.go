package memory

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A check that COULD NOT RUN is still a check (#1095): the report has one row
// per check whether it ran or not, and one that cannot reports as skipped.

// checkOrder returns the names of the checks a report ran, in order.
func checkOrder(rep VerifyReport) []string {
	names := make([]string, 0, len(rep.Checks))
	for _, c := range rep.Checks {
		names = append(names, c.Name)
	}
	return names
}

// TestVerifyStillReportsEveryCheckWhenTheReadsFail: a copy cut short (an
// interrupted copy opens but has no readable schema) still prints all four
// checks, the two reads as skipped with a reason, claims no row count, and the
// run refuses the file. A sound database that is not a Ghost store has a
// readable user_version, so only its row-count row is skipped, and that row
// carries the read's own error.
func TestVerifyStillReportsEveryCheckWhenTheReadsFail(t *testing.T) {
	const all = "sha256, integrity check, schema version, row counts"
	t.Run("cut short", func(t *testing.T) {
		dest := backedUpFixture(t)
		if full := fileSize(t, dest); full <= 20000 {
			t.Skipf("the snapshot is %d bytes, too small to cut", full)
		}
		if err := os.Truncate(dest, 20000); err != nil {
			t.Fatalf("truncate: %v", err)
		}
		rep, err := VerifyBackup(context.Background(), dest)
		if err == nil {
			t.Fatalf("verify accepted a copy cut to 20000 bytes: %+v", rep)
		}
		if got := strings.Join(checkOrder(rep), ", "); got != all {
			t.Fatalf("checks = [%s], want [%s]", got, all)
		}
		for _, name := range []string{"schema version", "row counts"} {
			if c := checkNamed(t, rep, name); c.State != VerifySkipped || strings.TrimSpace(c.Detail) == "" {
				t.Errorf("%s check = %q (%s), want skipped with a reason", name, c.State, c.Detail)
			}
		}
		if rep.CountsRead || rep.Counts != (BackupCounts{}) {
			t.Errorf("CountsRead=%v Counts=%+v, want nothing claimed about rows nobody counted", rep.CountsRead, rep.Counts)
		}
	})

	t.Run("not a ghost store", func(t *testing.T) {
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
		if got := strings.Join(checkOrder(rep), ", "); got != all {
			t.Fatalf("checks = [%s], want all four", got)
		}
		counts := checkNamed(t, rep, "row counts")
		read, err := OpenReadDB(path)
		if err != nil {
			t.Fatalf("OpenReadDB: %v", err)
		}
		_, countErr := CountRows(context.Background(), read)
		_ = read.Close()
		if countErr == nil {
			t.Fatal("the fixture's rows can be counted, so this test would assert nothing")
		}
		if counts.State != VerifySkipped || !strings.Contains(counts.Detail, countErr.Error()) {
			t.Errorf("row counts check = %q (%s), want skipped carrying %q", counts.State, counts.Detail, countErr)
		}
		if schema := checkNamed(t, rep, "schema version"); schema.State == VerifySkipped {
			t.Errorf("schema version check = skipped (%s), want it judged", schema.Detail)
		}
		if rep.CountsRead {
			t.Error("CountsRead is true for a file whose rows could not be counted")
		}
	})
}
