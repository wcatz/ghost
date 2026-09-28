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

// The cases a damaged sidecar and an unstat'ed file produce, which are the two
// ways this report used to describe something it had not measured.

// TestRunBackupVerifyCoreDistinguishesADamagedManifestFromAMissingOne: the two
// are opposites and "no manifest" is wrong for one of them. A missing sidecar is
// expected and merely limits what was checked; a sidecar that is there and cannot
// be read is DAMAGE, and telling that reader to go looking for a file sitting
// right beside the snapshot is the "treating it as absent" the command is built
// not to do.
func TestRunBackupVerifyCoreDistinguishesADamagedManifestFromAMissingOne(t *testing.T) {
	ctx := context.Background()
	store := transferTestStore(t)
	if err := store.EnsureProject(ctx, "p1", "/src/p1", "one"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	dest := filepath.Join(t.TempDir(), "snap.db")
	if err := runBackupCore(ctx, store, os.Stderr, dest); err != nil {
		t.Fatalf("runBackupCore: %v", err)
	}
	manifest := memory.ManifestPath(dest)
	if err := os.WriteFile(manifest, []byte("{ not json"), 0o600); err != nil {
		t.Fatalf("damage the manifest: %v", err)
	}

	var out strings.Builder
	if err := runBackupVerifyCore(ctx, &out, dest); err == nil {
		t.Fatalf("verify accepted a damaged manifest:\n%s", out.String())
	}
	text := out.String()
	if !strings.Contains(text, "present but could not be read") {
		t.Errorf("the report does not say the sidecar is present and damaged:\n%s", text)
	}
	if strings.Contains(text, "no manifest at") {
		t.Errorf("the report calls a present sidecar absent, sending the reader to look for a file that is there:\n%s", text)
	}
	if !strings.Contains(text, manifest) {
		t.Errorf("the report does not name the sidecar:\n%s", text)
	}
}

// TestRunBackupVerifyCoreClaimsNoSizeForAFileItCouldNotStat: a run that fails
// before the stat has measured nothing, and "0 bytes" is a claim about a file —
// the same reason CountsRead exists for row counts. A damaged sidecar is the
// reachable case: the manifest error returns before the stat, so a perfectly
// good snapshot would be reported as 0 bytes.
func TestRunBackupVerifyCoreClaimsNoSizeForAFileItCouldNotStat(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "snap.db")
	if err := os.WriteFile(dest, make([]byte, 2*1024*1024), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := os.WriteFile(memory.ManifestPath(dest), []byte("{ not json"), 0o600); err != nil {
		t.Fatalf("damage the manifest: %v", err)
	}

	var out strings.Builder
	if err := runBackupVerifyCore(context.Background(), &out, dest); err == nil {
		t.Fatalf("verify accepted a damaged manifest:\n%s", out.String())
	}
	text := out.String()
	if strings.Contains(text, "0 bytes") {
		t.Errorf("the report states a size for a file it never stat'ed:\n%s", text)
	}
	if !strings.Contains(text, "not checked") {
		t.Errorf("the report does not say the file was not measured:\n%s", text)
	}
}

// TestRunBackupVerifyCoreStillReportsAMeasuredSize: the other half, so the
// assertion above is not satisfied by simply never printing one. A run that got
// as far as the stat must report it.
func TestRunBackupVerifyCoreStillReportsAMeasuredSize(t *testing.T) {
	ctx := context.Background()
	store := transferTestStore(t)
	if err := store.EnsureProject(ctx, "p1", "/src/p1", "one"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	dest := filepath.Join(t.TempDir(), "snap.db")
	if err := runBackupCore(ctx, store, os.Stderr, dest); err != nil {
		t.Fatalf("runBackupCore: %v", err)
	}
	info, err := os.Stat(dest)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}

	var out strings.Builder
	if err := runBackupVerifyCore(ctx, &out, dest); err != nil {
		t.Fatalf("runBackupVerifyCore: %v\n%s", err, out.String())
	}
	if want := strconv.FormatInt(info.Size(), 10) + " bytes"; !strings.Contains(out.String(), want) {
		t.Errorf("the report does not state the measured size %q:\n%s", want, out.String())
	}
}
