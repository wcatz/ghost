package main

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// A manifest that is present but cannot be read is a refusal that still reports
// the checks needing nothing from it (size, integrity, schema version), with the
// two that compare against it skipped and the manifest named as the reason.

// verifyReportOf runs the command over dest and returns what it printed.
func verifyReportOf(dest string) (string, error) {
	var out strings.Builder
	err := runBackupVerifyCore(context.Background(), &out, dest)
	return out.String(), err
}

// requireUnreadableManifestReport holds a printed report over dest to the shape
// above.
func requireUnreadableManifestReport(t *testing.T, dest, text string, runErr error) {
	t.Helper()
	info, err := os.Stat(dest)
	if err != nil {
		t.Fatalf("stat snapshot: %v", err)
	}
	if runErr == nil {
		t.Fatalf("verify accepted an unreadable manifest:\n%s", text)
	}
	manifest := memory.ManifestPath(dest)
	if !strings.Contains(runErr.Error(), manifest) {
		t.Errorf("error = %v, want the manifest at %s as the diagnosis", runErr, manifest)
	}
	headline := strings.SplitN(text, "\n", 2)[0]
	if want := "refusing " + dest + ": " + strconv.FormatInt(info.Size(), 10) + " bytes"; !strings.HasPrefix(headline, want) {
		t.Errorf("headline = %q, want it to open with %q", headline, want)
	}
	testCheckLine(t, text, "integrity check", "ok")
	testCheckLine(t, text, "schema version", "ok")
	for _, name := range []string{"sha256", "row counts"} {
		want := "  " + name
		found := false
		for _, line := range strings.Split(text, "\n") {
			if strings.HasPrefix(line, want+" ") && strings.Contains(line, " skipped ") &&
				strings.Contains(line, "the manifest beside it could not be read") {
				found = true
			}
		}
		if !found {
			t.Errorf("no line reports %s as skipped for the unreadable manifest:\n%s", name, text)
		}
	}
	// The first row is the manifest problem, ahead of anything the file itself
	// turned up, and the footer still names it as damage rather than absence.
	if rows := strings.Split(text, "\n"); len(rows) < 2 || !strings.HasPrefix(rows[1], "  sha256 ") {
		t.Errorf("the first row is not the sha256 check naming the manifest:\n%s", text)
	}
	if !strings.Contains(text, "\n  the manifest at "+manifest+" is present but could not be read: ") || strings.Contains(text, "no manifest at") {
		t.Errorf("the footer does not name the sidecar as present and damaged:\n%s", text)
	}
	if strings.Contains(text, "no rows") || strings.Contains(text, "1 project and 1 memory") {
		t.Errorf("the report prints a row count for a check it skipped:\n%s", text)
	}
}

func TestRunBackupVerifyCoreReportsTheManifestFreeChecksForAnUnreadableManifest(t *testing.T) {
	for _, tc := range []struct {
		name   string
		damage func(t *testing.T, path string)
		want   string
	}{
		{"truncated", func(t *testing.T, path string) {
			if err := os.WriteFile(path, []byte("{\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}, "unexpected end of JSON input"},
		{"non-digest sha256", func(t *testing.T, path string) {
			m, err := memory.ReadBackupManifest(path)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(strings.Replace(string(raw), m.SHA256, "ABC", 1)), 0o600); err != nil {
				t.Fatal(err)
			}
		}, "sha256"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dest := snapshotForVerify(t)
			tc.damage(t, memory.ManifestPath(dest))
			text, err := verifyReportOf(dest)
			requireUnreadableManifestReport(t, dest, text, err)
			if err != nil && !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to contain %q:\n%s", err, tc.want, text)
			}
		})
	}
}
