package scratch

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wcatz/ghost/internal/config"
)

// TestRootRefusesAFollowedDataDirInsteadOfCreatingAScratchRootInIt is the
// first of four paths an independent review found reaching a forbidden data
// directory: `ghost lifecycle` calls scratch.Reap before anything else, Reap
// calls Root, and Root's default root is <dataDir>/scratch — so a dev build
// created a directory inside the store it was told not to touch, before any
// store check could run.
//
// The override is unset deliberately: GHOST_SCRATCH_DIR is the sandbox's own
// guard, and with it set Root never resolves the data directory at all, which is
// exactly why a reproduction that left it set saw nothing here.
func TestRootRefusesAFollowedDataDirInsteadOfCreatingAScratchRootInIt(t *testing.T) {
	prev := config.BuildVersion()
	config.SetBuildVersion("dev")
	t.Cleanup(func() { config.SetBuildVersion(prev) })

	root := t.TempDir()
	dataHome := filepath.Join(root, "data")
	dataDir := filepath.Join(dataHome, "ghost")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", dataDir, err)
	}
	t.Setenv("XDG_DATA_HOME", dataHome)
	t.Setenv(envDir, "")
	t.Setenv(config.DevForbidDataDirEnv, dataDir)

	if _, err := Root(); err == nil {
		t.Fatal("Root = nil, want a refusal: the data directory is forbidden")
	} else if !strings.Contains(err.Error(), config.DevForbidDataDirEnv) {
		t.Errorf("the refusal does not name %s: %v", config.DevForbidDataDirEnv, err)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "scratch")); !os.IsNotExist(err) {
		t.Errorf("Root created a scratch root inside the forbidden data dir (stat error: %v)", err)
	}

	// Reap is what the lifecycle calls, and it must fail the same way rather
	// than report a clean sweep of a root it never had.
	if _, err := Reap(time.Hour); err == nil {
		t.Error("Reap = nil, want the refusal Root reports")
	}
	entries, err := os.ReadDir(dataDir)
	if err != nil {
		t.Fatalf("read %s: %v", dataDir, err)
	}
	if len(entries) != 0 {
		t.Errorf("the forbidden data directory now holds %d entries, want none", len(entries))
	}

	// The control: the same call with the variable unset creates the root it
	// has always created, so the assertions above are about the guard rather
	// than about a sandbox that cannot hold a scratch dir.
	t.Setenv(config.DevForbidDataDirEnv, "")
	got, err := Root()
	if err != nil {
		t.Fatalf("Root with the variable unset = %v, want the scratch root", err)
	}
	if want := filepath.Join(dataDir, "scratch"); got != want {
		t.Errorf("Root = %q, want %q", got, want)
	}
	if _, err := os.Stat(got); err != nil {
		t.Errorf("Root did not create %s: %v", got, err)
	}
}

// TestRootIgnoresAFollowedDataDirOnAReleaseBuild is the other half, and the
// reason a refusal here is a development-only behaviour: a released ghost's
// lifecycle reaps its scratch root in the real store exactly as it always has.
func TestRootIgnoresAFollowedDataDirOnAReleaseBuild(t *testing.T) {
	prev := config.BuildVersion()
	config.SetBuildVersion("0.39.0")
	t.Cleanup(func() { config.SetBuildVersion(prev) })

	root := t.TempDir()
	dataHome := filepath.Join(root, "data")
	dataDir := filepath.Join(dataHome, "ghost")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", dataDir, err)
	}
	t.Setenv("XDG_DATA_HOME", dataHome)
	t.Setenv(envDir, "")
	t.Setenv(config.DevForbidDataDirEnv, dataDir)

	got, err := Root()
	if err != nil {
		t.Fatalf("Root on a release build = %v, want the scratch root", err)
	}
	if want := filepath.Join(dataDir, "scratch"); got != want {
		t.Errorf("Root = %q, want %q", got, want)
	}
}
