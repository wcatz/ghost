package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/config"
)

// realPath is a path as the filesystem reports it, which is what a refusal names:
// the guard canonicalizes both sides before comparing, and a temp dir under a
// symlinked /var (macOS) is not spelled the way EvalSymlinks returns it.
func realPath(t *testing.T, path string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatalf("resolve %s: %v", path, err)
	}
	return resolved
}

// withVersion sets the build version the way the release ldflags do and
// restores the one the test binary was built with, so one test cannot leave the
// next one believing it is a release.
func withVersion(t *testing.T, v string) {
	t.Helper()
	prev := version
	version = v
	t.Cleanup(func() { version = prev })
}

// TestRequireDataDirRefusesAListedDirectoryOnADevBuild drives the resolver every
// cmd/ghost entry point that opens a store goes through, with the version the
// binary is actually built with: "dev", unless a test says otherwise. The
// refusal has to arrive from the RESOLVER, before config.DataDir creates the
// directory — a guard placed one line later would leave the phantom directory
// behind, and creating a data directory in the store the variable protects is
// the smaller half of the damage.
func TestRequireDataDirRefusesAListedDirectoryOnADevBuild(t *testing.T) {
	withVersion(t, "dev")
	root := t.TempDir()
	dataHome := filepath.Join(root, "data")
	dataDir := filepath.Join(dataHome, "ghost")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", root, err)
	}
	t.Setenv("XDG_DATA_HOME", dataHome)
	t.Setenv(config.DevForbidDataDirEnv, dataDir)

	_, err := requireDataDir()
	if err == nil {
		t.Fatal("requireDataDir = nil, want a refusal on a development build")
	}
	if !strings.Contains(err.Error(), config.DevForbidDataDirEnv) {
		t.Errorf("the refusal does not name %s: %v", config.DevForbidDataDirEnv, err)
	}
	// The refusal names the canonical directory, and neither that directory nor
	// its parent exists — which is the assertion two lines up — so the canonical
	// form to expect is the resolved root with the missing components
	// re-attached, exactly what the guard does with a path that is not there yet.
	// The root is the one component this test created.
	if want := filepath.Join(realPath(t, root), "data", "ghost"); !strings.Contains(err.Error(), want) {
		t.Errorf("the refusal does not name the data directory %s: %v", want, err)
	}
	if _, statErr := os.Stat(dataDir); !os.IsNotExist(statErr) {
		t.Errorf("the refusal created %s, want nothing created (stat error: %v)", dataDir, statErr)
	}
}

// TestRequireDataDirIgnoresTheVariableOnAReleaseBuild is the half that makes the
// variable exportable: the same listed directory, on the version the release
// ldflags stamp, is created and returned. This is the mechanism the test gets
// the way it does — the package's own `version` var, which is what -X
// main.version sets — rather than a second global the product does not have.
func TestRequireDataDirIgnoresTheVariableOnAReleaseBuild(t *testing.T) {
	withVersion(t, "0.39.0")
	root := t.TempDir()
	dataHome := filepath.Join(root, "data")
	dataDir := filepath.Join(dataHome, "ghost")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", root, err)
	}
	t.Setenv("XDG_DATA_HOME", dataHome)
	t.Setenv(config.DevForbidDataDirEnv, dataDir)

	got, err := requireDataDir()
	if err != nil {
		t.Fatalf("requireDataDir on a release build = %v, want the directory", err)
	}
	if got != dataDir {
		t.Errorf("requireDataDir = %q, want %q", got, dataDir)
	}
	if _, statErr := os.Stat(dataDir); statErr != nil {
		t.Errorf("requireDataDir did not create %s: %v", dataDir, statErr)
	}
}

// TestRequireDataDirPathLeavesNothingBehind covers the non-creating resolver,
// which is what the read-only paths use: a diagnostic must be able to say "there
// is no database" without making that untrue, so it resolves without creating,
// and the guard runs on the path it would have reported rather than on one the
// check made.
func TestRequireDataDirPathLeavesNothingBehind(t *testing.T) {
	withVersion(t, "dev")
	root := t.TempDir()
	dataHome := filepath.Join(root, "data")
	dataDir := filepath.Join(dataHome, "ghost")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", root, err)
	}
	t.Setenv("XDG_DATA_HOME", dataHome)
	t.Setenv(config.DevForbidDataDirEnv, dataDir)

	if _, err := requireDataDirPath(); err == nil {
		t.Fatal("requireDataDirPath = nil, want a refusal on a development build")
	}
	if _, statErr := os.Stat(dataDir); !os.IsNotExist(statErr) {
		t.Errorf("requireDataDirPath created %s, want nothing created (stat error: %v)", dataDir, statErr)
	}

	// Unlisted, the same call resolves and still creates nothing.
	t.Setenv(config.DevForbidDataDirEnv, filepath.Join(root, "somewhere-else"))
	got, err := requireDataDirPath()
	if err != nil {
		t.Fatalf("requireDataDirPath for an unlisted directory = %v, want the path", err)
	}
	if got != dataDir {
		t.Errorf("requireDataDirPath = %q, want %q", got, dataDir)
	}
	if _, statErr := os.Stat(dataDir); !os.IsNotExist(statErr) {
		t.Errorf("requireDataDirPath created %s, want nothing created (stat error: %v)", dataDir, statErr)
	}
}

// TestDataDirPathRefusesAListedDirectory covers the resolver the transfer,
// backup and history commands share, including the read-only export and
// dry-run-import paths behind it. It is a named function because those commands
// parse argv and exit the process, so the decision they all make is only
// reachable at the resolver they all share.
func TestDataDirPathRefusesAListedDirectory(t *testing.T) {
	withVersion(t, "dev")
	root := t.TempDir()
	dataHome := filepath.Join(root, "data")
	dataDir := filepath.Join(dataHome, "ghost")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", root, err)
	}
	t.Setenv("XDG_DATA_HOME", dataHome)
	t.Setenv(config.DevForbidDataDirEnv, dataDir)

	if _, err := dataDirPath(); err == nil {
		t.Fatal("dataDirPath = nil, want a refusal on a development build")
	} else if !strings.Contains(err.Error(), config.DevForbidDataDirEnv) {
		t.Errorf("the refusal does not name %s: %v", config.DevForbidDataDirEnv, err)
	}

	withVersion(t, "0.39.0")
	if got, err := dataDirPath(); err != nil {
		t.Errorf("dataDirPath on a release build = %v, want the path", err)
	} else if got != dataDir {
		t.Errorf("dataDirPath = %q, want %q", got, dataDir)
	}
}
