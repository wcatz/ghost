package memory

import (
	"context"
	"os"
	"strings"
	"testing"
)

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
		t.Fatalf("checks = [%s], want [%s]", got, want)
	}
	for _, name := range []string{"integrity check", "schema version"} {
		if c := checkNamed(t, rep, name); c.State != VerifyOK {
			t.Errorf("%s check = %q (%s), want ok: it needs nothing from the manifest", name, c.State, c.Detail)
		}
	}
	for _, name := range []string{"sha256", "row counts"} {
		c := checkNamed(t, rep, name)
		if c.State != VerifySkipped || !strings.Contains(c.Detail, "the manifest beside it could not be read") {
			t.Errorf("%s check = %q (%s), want skipped with the unreadable manifest as the reason", name, c.State, c.Detail)
		}
	}
	if rep.CountsRead {
		t.Error("CountsRead is true, so the report would print a row count for a check it skipped")
	}
}

// TestVerifyRunsTheManifestFreeChecksOnAnUnreadableManifest: a manifest that is
// present but cannot be read (truncated JSON, a damaged sidecar, a dangling
// symlink) is a refusal whose error is the diagnosis, but the run goes on to the
// checks that need nothing from it, and the report says the manifest is
// present rather than absent. When the snapshot itself is not a database the
// manifest is still the diagnosis and the first row is the skipped sha256.
func TestVerifyRunsTheManifestFreeChecksOnAnUnreadableManifest(t *testing.T) {
	for _, tc := range []struct {
		name    string
		damage  func(t *testing.T, dest string)
		wantErr string
	}{
		{"truncated", func(t *testing.T, dest string) {
			if err := os.WriteFile(ManifestPath(dest), []byte("{\n"), 0o600); err != nil {
				t.Fatalf("truncate the manifest: %v", err)
			}
		}, "unexpected end of JSON input"},
		{"sha256 is not a digest", func(t *testing.T, dest string) {
			m := readManifestAt(t, dest)
			m.SHA256 = "ABC"
			writeManifestFile(t, ManifestPath(dest), m)
		}, "sha256"},
		{"dangling symlink", func(t *testing.T, dest string) {
			link := ManifestPath(dest)
			if err := os.Remove(link); err != nil {
				t.Fatalf("remove the manifest: %v", err)
			}
			if err := os.Symlink(link+".gone", link); err != nil {
				t.Skipf("symlinks unavailable: %v", err)
			}
		}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dest := backedUpFixture(t)
			tc.damage(t, dest)
			rep, err := VerifyBackup(context.Background(), dest)
			requireManifestFreeChecksRan(t, dest, rep, err)
			if !rep.ManifestPresent || rep.HasManifest {
				t.Errorf("ManifestPresent=%v HasManifest=%v, want a present sidecar that could not be read", rep.ManifestPresent, rep.HasManifest)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %v, want it to contain %q", err, tc.wantErr)
			}
		})
	}

	t.Run("snapshot is not a database", func(t *testing.T) {
		dest := backedUpFixture(t)
		if err := os.WriteFile(dest, []byte("not a database at all, just some bytes"), 0o600); err != nil {
			t.Fatalf("overwrite the snapshot: %v", err)
		}
		if err := os.WriteFile(ManifestPath(dest), []byte("{"), 0o600); err != nil {
			t.Fatalf("truncate the manifest: %v", err)
		}
		rep, err := VerifyBackup(context.Background(), dest)
		if err == nil || rep.ManifestErr == nil || err.Error() != rep.ManifestErr.Error() {
			t.Fatalf("run error = %v, want the manifest's %v", err, rep.ManifestErr)
		}
		if len(rep.Checks) == 0 || rep.Checks[0].Name != "sha256" || rep.Checks[0].State != VerifySkipped || !rep.BytesRead {
			t.Fatalf("checks = %+v BytesRead=%v, want the first row to be the skipped sha256 and the size measured", rep.Checks, rep.BytesRead)
		}
	})
}
