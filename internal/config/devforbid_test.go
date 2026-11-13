package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// devVersion is what a plain `go build` reports, so it is the version every
// test here judges a development build by.
const devVersion = "dev"

func mkdirAll(t *testing.T, path string) string {
	t.Helper()
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
	return path
}

// realpath is the test's own answer to "where does this path really live",
// independent of the code under test.
func realpath(t *testing.T, path string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatalf("resolve %s: %v", path, err)
	}
	return resolved
}

// A directory is matched by what it IS, not by how it was typed: trailing
// separator, "." segment, ".." round trip, a relative spelling, and a symlink in
// either direction. An unlisted directory (including the parent of the listed
// one) opens normally, and the refusal names the variable and the path.
func TestCheckDevDataDirMatchesTheDirectoryNotTheSpelling(t *testing.T) {
	root := t.TempDir()
	real := mkdirAll(t, filepath.Join(root, "real-store"))
	other := mkdirAll(t, filepath.Join(root, "scratch-store"))
	nested := mkdirAll(t, filepath.Join(root, "unlisted", "ghost"))
	t.Setenv(DevForbidDataDirEnv, real)

	for _, spelling := range []string{
		real,
		real + string(filepath.Separator),
		filepath.Join(root, ".", "real-store"),
		filepath.Join(root, "real-store", "..", "real-store"),
	} {
		if err := CheckDevDataDir(devVersion, spelling); err == nil {
			t.Errorf("CheckDevDataDir(%q) = nil, want a refusal", spelling)
		}
	}
	// A listed directory is not the parent of a data directory: a parent match
	// would refuse stores the operator never listed.
	for _, dataDir := range []string{other, root, nested} {
		if err := CheckDevDataDir(devVersion, dataDir); err != nil {
			t.Errorf("CheckDevDataDir(%q) = %v, want nil for a directory the listing does not name", dataDir, err)
		}
	}

	t.Chdir(root)
	if err := CheckDevDataDir(devVersion, filepath.Join(".", "real-store")); err == nil {
		t.Error("a relative spelling of the listed directory was opened, want a refusal")
	}
	if err := CheckDevDataDir(devVersion, "scratch-store"); err != nil {
		t.Errorf("an unlisted relative directory was refused: %v", err)
	}

	link := filepath.Join(root, "link-to-store")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("cannot create a symlink here: %v", err)
	}
	// Both directions, or the guard works only when both sides are spelled alike.
	if err := CheckDevDataDir(devVersion, link); err == nil {
		t.Error("a symlink to the listed directory was opened, want a refusal")
	}
	t.Setenv(DevForbidDataDirEnv, link)
	err := CheckDevDataDir(devVersion, real)
	if err == nil {
		t.Fatal("a listed directory reached through a symlink was opened, want a refusal")
	}
	if !strings.Contains(err.Error(), DevForbidDataDirEnv) || !strings.Contains(err.Error(), realpath(t, real)) {
		t.Errorf("the refusal must name %s and the data directory: %v", DevForbidDataDirEnv, err)
	}
}

// A release build never consults the variable (so it can be exported into every
// session), while every non-release spelling of the version is guarded: the
// default, an empty version, a non-version tag, a two-component version,
// git-describe strings and prereleases.
func TestCheckDevDataDirByBuildVersion(t *testing.T) {
	dataDir := mkdirAll(t, filepath.Join(t.TempDir(), "real-store"))
	t.Setenv(DevForbidDataDirEnv, dataDir)

	for _, version := range []string{"0.39.0", "v0.39.1", "0.39.0+build.7"} {
		if err := CheckDevDataDir(version, dataDir); err != nil {
			t.Errorf("CheckDevDataDir(%q) = %v, want nil: a release build ignores the variable", version, err)
		}
	}
	for _, version := range []string{"dev", "", "nightly", "1.0", "v0.38.0-14-gabc1234", "0.39.0-rc.1", "0.39.0-rc.1+build.7", "v0.38.0-0-gabc1234-dirty"} {
		if err := CheckDevDataDir(version, dataDir); err == nil {
			t.Errorf("CheckDevDataDir(%q) = nil, want a refusal: the build is not a release", version)
		}
	}
}

// The list form: every entry is read, an empty entry is a separator artifact
// (resolving "" would refuse the working directory), and an unset or blank
// variable is a no-op.
func TestCheckDevDataDirList(t *testing.T) {
	root := t.TempDir()
	first := mkdirAll(t, filepath.Join(root, "first-store"))
	second := mkdirAll(t, filepath.Join(root, "second-store"))
	other := mkdirAll(t, filepath.Join(root, "other-store"))
	sep := string(os.PathListSeparator)

	t.Setenv(DevForbidDataDirEnv, first+sep+second)
	for _, dataDir := range []string{first, second} {
		if err := CheckDevDataDir(devVersion, dataDir); err == nil {
			t.Errorf("CheckDevDataDir(%q) = nil, want a refusal: the directory is in the list", dataDir)
		}
	}
	for _, value := range []string{first, first + sep, sep + first, first + sep + sep + second} {
		t.Setenv(DevForbidDataDirEnv, value)
		if err := CheckDevDataDir(devVersion, other); err != nil {
			t.Errorf("with %q: unlisted %q = %v, want nil", value, other, err)
		}
		if err := CheckDevDataDir(devVersion, first); err == nil {
			t.Errorf("with %q: listed %q = nil, want a refusal", value, first)
		}
	}
	for _, value := range []string{"", " ", sep} {
		t.Setenv(DevForbidDataDirEnv, value)
		if err := CheckDevDataDir(devVersion, first); err != nil {
			t.Errorf("with the variable set to %q = %v, want nil", value, err)
		}
	}
}

// A store that does not exist yet (a fresh sandbox) is compared by resolving
// its deepest existing ancestor, so a symlinked parent still matches, nothing is
// created by the check, and an unresolvable listing is not the data directory.
func TestCheckDevDataDirHandlesAPathThatDoesNotExist(t *testing.T) {
	root := t.TempDir()
	real := mkdirAll(t, filepath.Join(root, "real-store"))
	link := filepath.Join(root, "link-to-store")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("cannot create a symlink here: %v", err)
	}

	t.Setenv(DevForbidDataDirEnv, filepath.Join(real, "not-created"))
	missing := filepath.Join(link, "not-created")
	if err := CheckDevDataDir(devVersion, missing); err == nil {
		t.Error("a missing data directory reached through a symlink was opened, want a refusal")
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Errorf("the check created %s, want nothing created", missing)
	}

	notYet := filepath.Join(root, "not-created-yet", "ghost")
	t.Setenv(DevForbidDataDirEnv, notYet)
	if err := CheckDevDataDir(devVersion, notYet); err == nil {
		t.Error("a data directory equal to a listed path that does not exist was opened, want a refusal")
	}
	t.Setenv(DevForbidDataDirEnv, filepath.Join(root, "nowhere", "at-all"))
	if err := CheckDevDataDir(devVersion, real); err != nil {
		t.Errorf("CheckDevDataDir = %v, want nil: an unresolvable listing is not the data directory", err)
	}
}

// withBuildVersion sets the version the RESOLVERS judge this build by and
// restores it, so one test cannot leave the next one believing it is a release.
func withBuildVersion(t *testing.T, v string) {
	t.Helper()
	prev := BuildVersion()
	SetBuildVersion(v)
	t.Cleanup(func() { SetBuildVersion(prev) })
}

// #721: the check lives INSIDE the resolvers, so a future caller of DataDir or
// DataDirPath is covered by having used it. A dev build (set or left at its
// default, which must be the GUARDED side) refuses a listed directory and the
// creating resolver leaves nothing behind; a release build and an unset variable
// resolve and create it as they always did.
func TestTheResolversGuardTheListedDirectory(t *testing.T) {
	root := t.TempDir()
	dataHome := filepath.Join(root, "data")
	want := filepath.Join(dataHome, "ghost")
	t.Setenv("XDG_DATA_HOME", dataHome)

	t.Setenv(DevForbidDataDirEnv, want)
	if _, err := DataDir(); err == nil {
		t.Error("DataDir = nil with the default build version, want a refusal")
	}
	withBuildVersion(t, devVersion)
	if _, err := DataDirPath(); err == nil || !strings.Contains(err.Error(), DevForbidDataDirEnv) {
		t.Errorf("DataDirPath on a dev build = %v, want a refusal naming %s", err, DevForbidDataDirEnv)
	}
	if _, err := DataDir(); err == nil {
		t.Error("DataDir = nil on a dev build, want a refusal")
	}
	if _, err := os.Stat(want); !os.IsNotExist(err) {
		t.Errorf("the refused resolver created %s (stat error: %v)", want, err)
	}

	t.Setenv(DevForbidDataDirEnv, "")
	if got, err := DataDir(); err != nil || got != want {
		t.Errorf("DataDir without the variable = %q, %v, want %q", got, err, want)
	}
	if err := os.Remove(want); err != nil {
		t.Fatal(err)
	}

	t.Setenv(DevForbidDataDirEnv, want)
	withBuildVersion(t, "0.39.0")
	if got, err := DataDir(); err != nil || got != want {
		t.Errorf("DataDir on a release build = %q, %v, want %q", got, err, want)
	}
	if _, err := os.Stat(want); err != nil {
		t.Errorf("DataDir did not create %s: %v", want, err)
	}
	t.Setenv(DevForbidDataDirEnv, filepath.Join(root, "somewhere-else"))
	if got, err := DataDirPath(); err != nil || got != want {
		t.Errorf("DataDirPath on a release build = %q, %v, want %q", got, err, want)
	}
}

// An unwired build must fall on the development side, or the rule is off until
// something remembers to turn it on.
func TestTheDefaultBuildVersionIsADevelopmentBuild(t *testing.T) {
	if got := BuildVersion(); got != "dev" {
		t.Errorf("the default build version is %q, want \"dev\"", got)
	}
}
