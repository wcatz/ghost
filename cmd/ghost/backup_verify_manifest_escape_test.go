package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// TestBackupVerifyReportCarriesNoTerminalControlFromTheManifest: the printed
// report for a sidecar whose sha256 holds ESC [1F + "verified" (#1187) must
// keep "refusing" as the verdict a terminal shows, which it can only do if no
// control byte from the manifest reaches the output.
func TestBackupVerifyReportCarriesNoTerminalControlFromTheManifest(t *testing.T) {
	dir := t.TempDir()
	snap := filepath.Join(dir, "snap.db")
	if err := os.WriteFile(snap, []byte("not a database"), 0o600); err != nil {
		t.Fatal(err)
	}
	body := `{"manifest_version":1,"schema_version":1,"bytes":14,"sha256":"\u001b[1Fverified` + strings.Repeat("0", 52) + `"}`
	if err := os.WriteFile(memory.ManifestPath(snap), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	rep, runErr := memory.VerifyBackup(context.Background(), snap)
	if runErr == nil {
		t.Fatal("verify accepted a manifest whose sha256 is not a digest")
	}
	var out bytes.Buffer
	if err := printVerifyReport(&out, rep, runErr); err != nil {
		t.Fatalf("printVerifyReport: %v", err)
	}
	got := out.String()
	if !strings.HasPrefix(got, "refusing ") {
		t.Errorf("report = %q, want it to open with the refusing verdict", got)
	}
	for i := 0; i < len(got); i++ {
		if c := got[i]; (c < 0x20 && c != '\n') || c == 0x7f {
			t.Fatalf("report carries control byte %#x at %d: %q", c, i, got)
		}
	}
	if !strings.Contains(runErr.Error(), "sha256") {
		t.Errorf("error = %q, want it to name the sha256 field", runErr)
	}
}
