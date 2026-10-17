package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// A manifest that is present but cannot be read is a refusal, and it is not a
// reason to print nothing. The report used to stop at the manifest, with no
// size and no rows; the checks that need nothing from it — the size, the
// integrity check and the schema version — must still be reported, and the two
// that compare against it shown as skipped with the manifest as the reason.

// backedUpSnapshot writes a one-memory backup and returns the snapshot's path.
func backedUpSnapshot(t *testing.T) string {
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

func TestRunBackupVerifyCoreReportsTheManifestFreeChecksForATruncatedManifest(t *testing.T) {
	dest := backedUpSnapshot(t)
	if err := os.WriteFile(memory.ManifestPath(dest), []byte("{\n"), 0o600); err != nil {
		t.Fatalf("truncate the manifest: %v", err)
	}
	text, err := verifyReportOf(dest)
	requireUnreadableManifestReport(t, dest, text, err)
	if !strings.Contains(err.Error(), "unexpected end of JSON input") {
		t.Errorf("error = %v, want the decoder's reason:\n%s", err, text)
	}
}

func TestRunBackupVerifyCoreReportsTheManifestFreeChecksForANonDigestSHA256(t *testing.T) {
	dest := backedUpSnapshot(t)
	path := memory.ManifestPath(dest)
	m, err := memory.ReadBackupManifest(path)
	if err != nil {
		t.Fatalf("ReadBackupManifest: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	if err := os.WriteFile(path, []byte(strings.Replace(string(raw), m.SHA256, "ABC", 1)), 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	text, verr := verifyReportOf(dest)
	requireUnreadableManifestReport(t, dest, text, verr)
	if !strings.Contains(verr.Error(), "sha256") {
		t.Errorf("error = %v, want it to name the sha256 field:\n%s", verr, text)
	}
}
