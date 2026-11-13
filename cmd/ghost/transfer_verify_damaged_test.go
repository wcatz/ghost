package main

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// TestRunBackupVerifyCoreDistinguishesADamagedManifestFromAMissingOne: a missing
// sidecar merely limits what was checked, but one that is there and cannot be
// read (garbage, or a symlink whose target is gone) is DAMAGE: a refusal that
// names the sidecar and the real error, never "no manifest at" (which sends the
// reader looking for a file beside the snapshot) and never a nil error.
func TestRunBackupVerifyCoreDistinguishesADamagedManifestFromAMissingOne(t *testing.T) {
	for _, damage := range []string{"garbage", "dangling symlink"} {
		t.Run(damage, func(t *testing.T) {
			dest := snapshotForVerify(t)
			manifest := memory.ManifestPath(dest)
			if damage == "garbage" {
				if err := os.WriteFile(manifest, []byte("{ not json"), 0o600); err != nil {
					t.Fatalf("damage the manifest: %v", err)
				}
			} else {
				if err := os.Remove(manifest); err != nil {
					t.Fatalf("remove the manifest: %v", err)
				}
				if err := os.Symlink(filepath.Join(t.TempDir(), "gone.json"), manifest); err != nil {
					t.Skipf("symlinks unavailable: %v", err)
				}
			}
			var out strings.Builder
			if err := runBackupVerifyCore(context.Background(), &out, dest); err == nil {
				t.Fatalf("verify accepted a damaged manifest:\n%s", out.String())
			}
			text := out.String()
			for _, want := range []string{"refusing ", "present but could not be read", manifest} {
				if !strings.Contains(text, want) {
					t.Errorf("the report does not say %q:\n%s", want, text)
				}
			}
			for _, unwanted := range []string{"no manifest at", "<nil>"} {
				if strings.Contains(text, unwanted) {
					t.Errorf("the report says %q for a present, damaged sidecar:\n%s", unwanted, text)
				}
			}
		})
	}
}

// TestRunBackupVerifyCoreClaimsNoSizeForAFileItCouldNotStat: a run that fails
// before the stat has measured nothing ("0 bytes" would be a claim about a
// file), while a run that got as far as the stat reports the measured size.
func TestRunBackupVerifyCoreClaimsNoSizeForAFileItCouldNotStat(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "snap.db")
	if err := os.WriteFile(memory.ManifestPath(dest), []byte("{ not json"), 0o600); err != nil {
		t.Fatalf("damage the manifest: %v", err)
	}
	var out strings.Builder
	if err := runBackupVerifyCore(context.Background(), &out, dest); err == nil {
		t.Fatalf("verify accepted a damaged manifest:\n%s", out.String())
	}
	if strings.Contains(out.String(), " bytes") || !strings.Contains(out.String(), "not checked") {
		t.Errorf("the report states a size for a file it never stat'ed, or does not say it was not measured:\n%s", out.String())
	}

	real := snapshotForVerify(t)
	info, err := os.Stat(real)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	out.Reset()
	if err := runBackupVerifyCore(context.Background(), &out, real); err != nil {
		t.Fatalf("runBackupVerifyCore: %v\n%s", err, out.String())
	}
	if want := strconv.FormatInt(info.Size(), 10) + " bytes"; !strings.Contains(out.String(), want) {
		t.Errorf("the report does not state the measured size %q:\n%s", want, out.String())
	}
}
