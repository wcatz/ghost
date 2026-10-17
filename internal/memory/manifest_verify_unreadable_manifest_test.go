package memory

import (
	"context"
	"os"
	"strings"
	"testing"
)

// A manifest that is present but cannot be read is a refusal, and its error is
// the diagnosis. It is not a reason to stop checking: the size, the integrity
// check and the schema version need nothing from the manifest, and a report that
// dropped them would answer "nothing could be checked" about a file whose only
// problem may be its sidecar. VerifyBackup used to return as soon as the read
// failed, before any of them, so the report had no rows and no size.

// requireManifestFreeChecksRan holds a run against an unreadable manifest to the
// documented report: the manifest's error is the run's error, the size was
// measured, every check has its row in order, the two that need no manifest ran
// and passed on a sound backup, and the two that compare against it are skipped
// with the manifest named as the reason.
func requireManifestFreeChecksRan(t *testing.T, dest string, rep VerifyReport, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("verify accepted an unreadable manifest: %+v", rep)
	}
	if rep.ManifestErr == nil || err.Error() != rep.ManifestErr.Error() {
		t.Errorf("run error = %v, ManifestErr = %v: the manifest must stay the diagnosis", err, rep.ManifestErr)
	}
	if !rep.BytesRead || rep.Bytes != fileSize(t, dest) {
		t.Errorf("BytesRead=%v Bytes=%d, want the file's %d bytes measured", rep.BytesRead, rep.Bytes, fileSize(t, dest))
	}
	got := strings.Join(checkOrder(rep), ", ")
	if want := "sha256, integrity check, schema version, row counts"; got != want {
		t.Fatalf("checks = [%s], want [%s]: the checks that need no manifest still run", got, want)
	}
	for _, name := range []string{"integrity check", "schema version"} {
		if c := checkNamed(t, rep, name); c.State != VerifyOK {
			t.Errorf("%s check = %q (%s), want ok: it needs nothing from the manifest", name, c.State, c.Detail)
		}
	}
	for _, name := range []string{"sha256", "row counts"} {
		c := checkNamed(t, rep, name)
		if c.State != VerifySkipped {
			t.Errorf("%s check = %q (%s), want skipped: there is nothing to compare against", name, c.State, c.Detail)
		}
		if !strings.Contains(c.Detail, "the manifest beside it could not be read") {
			t.Errorf("%s detail = %q, want it to give the unreadable manifest as the reason", name, c.Detail)
		}
	}
	// The counts are a comparison against the manifest, and nothing counted
	// them, so nothing may print a row count.
	if rep.CountsRead {
		t.Error("CountsRead is true, so the report would print a row count for a check it skipped")
	}
}

func TestVerifyRunsTheManifestFreeChecksOnATruncatedManifest(t *testing.T) {
	dest := backedUpFixture(t)
	if err := os.WriteFile(ManifestPath(dest), []byte("{\n"), 0o600); err != nil {
		t.Fatalf("truncate the manifest: %v", err)
	}
	rep, err := VerifyBackup(context.Background(), dest)
	requireManifestFreeChecksRan(t, dest, rep, err)
	if !strings.Contains(err.Error(), "unexpected end of JSON input") {
		t.Errorf("error = %v, want the JSON decoder's own reason", err)
	}
}

func TestVerifyRunsTheManifestFreeChecksOnAManifestWhoseSHA256IsNotADigest(t *testing.T) {
	dest := backedUpFixture(t)
	m := readManifestAt(t, dest)
	m.SHA256 = "ABC"
	writeManifestFile(t, ManifestPath(dest), m)
	rep, err := VerifyBackup(context.Background(), dest)
	requireManifestFreeChecksRan(t, dest, rep, err)
	if !strings.Contains(err.Error(), "sha256") {
		t.Errorf("error = %v, want it to name the sha256 field", err)
	}
}

// TestVerifyLeadsWithTheManifestForAFileThatIsNotADatabase: the reason the
// manifest was read first. A file that is not SQLite, beside a sidecar that
// cannot be read, still gets the manifest as its diagnosis; what the file
// itself is goes in the rows, where integrity check names it.
func TestVerifyLeadsWithTheManifestForAFileThatIsNotADatabase(t *testing.T) {
	dest := backedUpFixture(t)
	if err := os.WriteFile(dest, []byte("not a database at all, just some bytes"), 0o600); err != nil {
		t.Fatalf("overwrite the snapshot: %v", err)
	}
	if err := os.WriteFile(ManifestPath(dest), []byte("{"), 0o600); err != nil {
		t.Fatalf("truncate the manifest: %v", err)
	}
	rep, err := VerifyBackup(context.Background(), dest)
	if err == nil {
		t.Fatalf("verify accepted a non-database beside an unreadable manifest: %+v", rep)
	}
	if rep.ManifestErr == nil || err.Error() != rep.ManifestErr.Error() {
		t.Errorf("run error = %v, want the manifest's %v", err, rep.ManifestErr)
	}
	if len(rep.Checks) == 0 || rep.Checks[0].Name != "sha256" || rep.Checks[0].State != VerifySkipped {
		t.Fatalf("checks = %+v, want the first row to be the skipped sha256 naming the manifest", rep.Checks)
	}
	if !rep.BytesRead {
		t.Error("BytesRead is false: the size needs nothing but the path")
	}
}
