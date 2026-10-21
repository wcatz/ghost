//go:build e2e

package e2e

// A failed claim publish leaves nothing behind in the data directory.
//
// The per-project lifecycle claim is published by writing a staging file and
// renaming it onto the claim path, so a claim that cannot be published — a
// directory sitting where the claim belongs is enough — must take its staging
// file with it. The data directory is never swept, so a leftover would sit
// there for as long as the store exists, and every later run would find it.
//
// This drives the BUILT binary, so the assertion is about the data directory
// as the coordinator left it rather than about a helper's return value.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLifecycleClaimPublishLeavesNoStagingFile plants a directory where the
// project's claim belongs and runs the coordinator against it. The claim
// cannot be published over a directory, so the run continues unlocked — and
// the data directory must hold no staging file afterwards.
func TestLifecycleClaimPublishLeavesNoStagingFile(t *testing.T) {
	s := newSandbox(t)
	call(t, s.mcpSession(t), "ghost_memory_save", map[string]any{
		"project_id": e2eProject,
		"content":    "a memory so the project exists for the coordinator",
	})
	s.mustRun("project", "bind", e2eProject, s.work)

	claim := filepath.Join(s.dataDir(), "lifecycle-"+e2eProject+".pid")
	if err := os.Mkdir(claim, 0o700); err != nil {
		t.Fatalf("mkdir the claim path: %v", err)
	}

	// The exit code is not the claim: the coordinator treats an unevaluatable
	// claim as a warning and runs the phases unlocked, so the claim is the
	// filesystem, and the filesystem is what is asserted. The warning is what
	// says the run really reached the claim, so the assertion cannot pass
	// because the planted path was never the one the claim uses.
	r := s.run("lifecycle", e2eProject)
	mustContain(t, "the coordinator's warning", r.stderr, "could not take the per-project lock")

	for _, name := range namesIn(t, s.dataDir()) {
		if strings.Contains(name, ".tmp") {
			t.Errorf("the failed claim publish left %q behind in the data directory", name)
		}
	}
	// The planted directory is still the directory it was: the failed publish
	// did not replace it with a file.
	info, err := os.Stat(claim)
	if err != nil {
		t.Fatalf("stat the claim path: %v", err)
	}
	if !info.IsDir() {
		t.Errorf("the claim path is a %v, want the directory that was planted", info.Mode().Type())
	}
}
