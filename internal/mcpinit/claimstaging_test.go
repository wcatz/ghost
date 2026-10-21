package mcpinit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The claim is published by writing a staging file and renaming it onto the
// claim path, so a reader never sees a half-written claim. Two things about
// that staging file are the publish's own business:
//
//   - A publish that FAILS must leave nothing behind. The staging file used
//     to sit in the data directory with nothing to sweep it, and
//     claimPidFileReclaim deliberately reaches the write for a non-regular
//     entry at the claim path — so a directory where the claim belongs was
//     enough to leave one behind per failed publish.
//   - The staging file must not be a name somebody else can point at. A
//     fixed staging name is one a link can be planted at, and writing it
//     followed the link: the claim text landed in a file the caller never
//     named, and the rename then moved the link onto the claim path.

// stagingEntries lists the names in dir that name a staging file, which is
// what a failed publish leaves behind.
func stagingEntries(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	var out []string
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp") {
			out = append(out, e.Name())
		}
	}
	return out
}

// TestAFailedClaimPublishLeavesNoStagingFile: a directory where the claim
// belongs makes the rename fail — the shape claimPidFileReclaim reaches on
// purpose — and the publish must take its staging file with it.
func TestAFailedClaimPublishLeavesNoStagingFile(t *testing.T) {
	dir := t.TempDir()
	claim := filepath.Join(dir, "lifecycle-p1.pid")
	if err := os.Mkdir(claim, 0o700); err != nil {
		t.Fatal(err)
	}

	if err := atomicWritePID(claim, 4242, "token", true); err == nil {
		t.Fatal("publishing a claim onto a directory must fail")
	}

	if left := stagingEntries(t, dir); len(left) != 0 {
		t.Errorf("a failed publish left %v behind in the data directory", left)
	}
}

// TestAClaimThatCannotBePublishedLeavesNoStagingFile is the reported symptom:
// the claim could not be published, and the refusal said so — but the staging
// file the publish had already written stayed in the data directory.
func TestAClaimThatCannotBePublishedLeavesNoStagingFile(t *testing.T) {
	dir := t.TempDir()
	claim := filepath.Join(dir, "lifecycle-p1.pid")
	if err := os.Mkdir(claim, 0o700); err != nil {
		t.Fatal(err)
	}

	claimed, _, err := claimPidFileReclaim(claim)
	if claimed || err == nil {
		t.Fatalf("an unpublishable claim = claimed %v, err %v; want false and an error", claimed, err)
	}
	if !strings.Contains(err.Error(), "write the claim") {
		t.Errorf("the refusal does not name the step that failed: %v", err)
	}

	if left := stagingEntries(t, dir); len(left) != 0 {
		t.Errorf("a refused claim left %v behind in the data directory", left)
	}
}

// TestTheClaimPublishDoesNotWriteThroughAPlantedLink: the staging write used
// to open a fixed name, so a link planted there was written through — the
// claim text landed in a file the caller never named. The publish must write
// the claim into the claim path and leave every other file alone.
func TestTheClaimPublishDoesNotWriteThroughAPlantedLink(t *testing.T) {
	dir := t.TempDir()
	scratch := filepath.Join(dir, "scratch.txt")
	if err := os.WriteFile(scratch, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	claim := filepath.Join(dir, "lifecycle-p1.pid")
	if err := os.Symlink(scratch, claim+".tmp"); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	if err := atomicWritePID(claim, 4242, "token", true); err != nil {
		t.Fatalf("publish the claim: %v", err)
	}

	data, err := os.ReadFile(scratch)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "original" {
		t.Errorf("the claim text was written through a planted link: %q", data)
	}
	info, err := os.Lstat(claim)
	if err != nil {
		t.Fatalf("lstat the published claim: %v", err)
	}
	if !info.Mode().IsRegular() {
		t.Errorf("the published claim is a %v, want a regular file", info.Mode().Type())
	}
	published, err := os.ReadFile(claim)
	if err != nil {
		t.Fatalf("read the published claim: %v", err)
	}
	if string(published) != "4242:token" {
		t.Errorf("the published claim holds %q, want %q", published, "4242:token")
	}
}
