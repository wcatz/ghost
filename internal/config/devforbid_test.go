package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// devVersion is what a plain `go build` reports (main.version's default, set
// only by the release ldflags), so it is the version every test here judges a
// development build by.
const devVersion = "dev"

// mkdirAll is a test helper that fails rather than returns an error, so a test
// body stays about the rule rather than about the fixture.
func mkdirAll(t *testing.T, path string) string {
	t.Helper()
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
	return path
}

// realpath is the test's own answer to "where does this path really live", used
// to spell the path an error is expected to name. It calls the standard library
// directly rather than the code under test, so the expectation is independent of
// the implementation.
func realpath(t *testing.T, path string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatalf("resolve %s: %v", path, err)
	}
	return resolved
}

// TestCheckDevDataDirRefusesAListedDirectoryInEverySpelling pins the property
// the whole guard rests on: a directory is matched by what it IS, not by how it
// was typed. A trailing separator, a "." segment and a ".." round trip are the
// same directory spelled three ways, and a guard that compared strings would
// protect the store only for the one spelling the author happened to test.
func TestCheckDevDataDirRefusesAListedDirectoryInEverySpelling(t *testing.T) {
	root := t.TempDir()
	real := mkdirAll(t, filepath.Join(root, "real-store"))
	t.Setenv(DevForbidDataDirEnv, real)

	for _, spelling := range []string{
		real,
		real + string(filepath.Separator),
		filepath.Join(root, "real-store"),
		filepath.Join(root, ".", "real-store"),
		filepath.Join(root, "real-store", "..", "real-store"),
	} {
		if err := CheckDevDataDir(devVersion, spelling); err == nil {
			t.Errorf("CheckDevDataDir(%q) = nil, want a refusal", spelling)
		}
	}
}

// TestCheckDevDataDirRefusesAListedDirectoryReachedThroughASymlink is the
// spelling that matters most in practice: a development workflow that exports
// XDG_DATA_HOME through a symlinked home is pointing at the real store while
// naming a path nowhere near it. A guard that did not resolve symlinks would
// pass the real store through, which is the failure this whole issue exists to
// prevent.
func TestCheckDevDataDirRefusesAListedDirectoryReachedThroughASymlink(t *testing.T) {
	root := t.TempDir()
	real := mkdirAll(t, filepath.Join(root, "real-store"))
	link := filepath.Join(root, "link-to-store")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("cannot create a symlink here: %v", err)
	}

	// Both directions: the listing names the link and the store is reached
	// directly, and the listing names the store while the build was pointed at
	// the link. A one-directional check would be a guard that works only when
	// the two sides happen to be spelled alike.
	t.Setenv(DevForbidDataDirEnv, real)
	if err := CheckDevDataDir(devVersion, link); err == nil {
		t.Error("a symlinked data dir pointing at the listed directory was opened, want a refusal")
	}
	t.Setenv(DevForbidDataDirEnv, link)
	if err := CheckDevDataDir(devVersion, real); err == nil {
		t.Error("a listed directory reached through a symlink was opened, want a refusal")
	}
}

// TestCheckDevDataDirRefusesARelativeSpelling covers the spelling a shell
// export produces when a session is started from inside the tree: the same
// directory named relatively, which no string comparison could match against an
// absolute listing.
func TestCheckDevDataDirRefusesARelativeSpelling(t *testing.T) {
	root := t.TempDir()
	mkdirAll(t, filepath.Join(root, "real-store"))
	mkdirAll(t, filepath.Join(root, "elsewhere"))
	t.Setenv(DevForbidDataDirEnv, filepath.Join(root, "real-store"))
	t.Chdir(root)

	if err := CheckDevDataDir(devVersion, filepath.Join(".", "real-store")); err == nil {
		t.Error("a relative spelling of the listed directory was opened, want a refusal")
	}
	if err := CheckDevDataDir(devVersion, "elsewhere"); err != nil {
		t.Errorf("an unlisted directory was refused: %v", err)
	}
}

// TestCheckDevDataDirAllowsAnUnlistedDirectory is the other half: a guard that
// refused everything would satisfy every test above while making the variable
// useless, so an ordinary second store must open normally.
func TestCheckDevDataDirAllowsAnUnlistedDirectory(t *testing.T) {
	root := t.TempDir()
	forbidden := mkdirAll(t, filepath.Join(root, "real-store"))
	other := mkdirAll(t, filepath.Join(root, "scratch-store"))
	// A DIRECTORY the listing names is not the parent of a data directory: the
	// store lives in <data home>/ghost, and naming <data home> is naming
	// something else. A parent match would refuse stores the operator never
	// listed, which is the false positive the rule must not have.
	parent := filepath.Dir(forbidden)
	nested := mkdirAll(t, filepath.Join(parent, "unlisted", "ghost"))

	for _, dataDir := range []string{other, parent, nested} {
		t.Setenv(DevForbidDataDirEnv, forbidden)
		if err := CheckDevDataDir(devVersion, dataDir); err != nil {
			t.Errorf("CheckDevDataDir(%q) = %v, want nil for a directory the listing does not name", dataDir, err)
		}
	}
}

// TestCheckDevDataDirIsIgnoredByAReleaseBuild is the half that makes the
// variable safe to export into every session's environment. The release Ghost
// those same sessions reach through their MCP integration has to keep working
// against the real store, so "release" is read off the build version and the
// variable is then never consulted — every spelling of a release version is
// tried, because the version is set by ldflags this package never sees.
func TestCheckDevDataDirIsIgnoredByAReleaseBuild(t *testing.T) {
	dataDir := mkdirAll(t, filepath.Join(t.TempDir(), "real-store"))
	t.Setenv(DevForbidDataDirEnv, dataDir)

	for _, version := range []string{
		"0.39.0",
		"v0.39.0",
		"v0.39.1",
		"1.0.0",
		"0.39.0+build.7", // build metadata takes no part in precedence
	} {
		if err := CheckDevDataDir(version, dataDir); err != nil {
			t.Errorf("CheckDevDataDir(%q, %q) = %v, want nil: a release build ignores the variable", version, dataDir, err)
		}
	}
}

// TestCheckDevDataDirRefusesEveryNonReleaseVersion is the other direction, and
// it is a table because "is this build a release" has more than one answer: the
// default a plain `go build` carries, the `git describe` string a local build
// stamps, a prerelease, a two-component version, and a tag that is not a version
// at all. A rule that recognised only the literal "dev" would leave every other
// development build unguarded, which is the majority of them.
func TestCheckDevDataDirRefusesEveryNonReleaseVersion(t *testing.T) {
	dataDir := mkdirAll(t, filepath.Join(t.TempDir(), "real-store"))
	t.Setenv(DevForbidDataDirEnv, dataDir)

	for _, version := range []string{
		"dev",
		"",                         // an unstampable build
		"nightly",                  // a tag that is not a version
		"1.0",                      // two components: ambiguous, so unparseable
		"v0.38.0-14-gabc1234",      // git describe -tags
		"0.39.0-rc.1",              // a prerelease, not a release
		"0.39.0-rc.1+build.7",      // a prerelease carrying build metadata
		"v0.38.0-0-gabc1234-dirty", // describe --dirty
	} {
		if err := CheckDevDataDir(version, dataDir); err == nil {
			t.Errorf("CheckDevDataDir(%q, %q) = nil, want a refusal: the build is not a release", version, dataDir)
		}
	}
}

// TestCheckDevDataDirNamesTheVariableAndThePath pins the diagnostic. A refusal
// that did not name the variable would leave the reader to guess which piece of
// their environment stopped the command, and one that did not name the path
// would leave them with a store they cannot tell from any other.
func TestCheckDevDataDirNamesTheVariableAndThePath(t *testing.T) {
	root := t.TempDir()
	real := mkdirAll(t, filepath.Join(root, "real-store"))
	t.Setenv(DevForbidDataDirEnv, filepath.Join(root, "link-to-store"))
	if err := os.Symlink(real, filepath.Join(root, "link-to-store")); err != nil {
		t.Skipf("cannot create a symlink here: %v", err)
	}
	t.Setenv(DevForbidDataDirEnv, filepath.Join(root, "link-to-store"))

	err := CheckDevDataDir(devVersion, real)
	if err == nil {
		t.Fatal("CheckDevDataDir = nil, want a refusal")
	}
	if !strings.Contains(err.Error(), DevForbidDataDirEnv) {
		t.Errorf("the refusal does not name %s: %v", DevForbidDataDirEnv, err)
	}
	if want := realpath(t, real); !strings.Contains(err.Error(), want) {
		t.Errorf("the refusal does not name the data directory %s: %v", want, err)
	}
}

// TestCheckDevDataDirReadsEveryEntryInTheList covers the list form: a
// development session exports more than one store it must not touch, and an
// empty entry is a separator artifact rather than a directory named "" (which
// resolves against the working directory and would refuse everything).
func TestCheckDevDataDirReadsEveryEntryInTheList(t *testing.T) {
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
	if err := CheckDevDataDir(devVersion, other); err != nil {
		t.Errorf("CheckDevDataDir(%q) = %v, want nil", other, err)
	}

	// An empty entry is a separator, not a directory, so a list built by
	// appending to an empty variable still refuses the entries it does name —
	// and does not refuse the directories it does not. Both halves matter: a
	// splitter that kept the empty entries would resolve "" against the working
	// directory and refuse whatever the process is in.
	for _, value := range []string{
		first,
		first + sep,
		sep + first,
		first + sep + sep,
		first + sep + sep + second,
		first + sep + second + sep,
	} {
		t.Setenv(DevForbidDataDirEnv, value)
		if err := CheckDevDataDir(devVersion, other); err != nil {
			t.Errorf("CheckDevDataDir with %q = %v, want nil for the unlisted directory %q", value, err, other)
		}
		if !strings.Contains(value, first) {
			continue
		}
		if err := CheckDevDataDir(devVersion, first); err == nil {
			t.Errorf("CheckDevDataDir with %q = nil, want a refusal for the listed directory %q", value, first)
		}
	}
}

// TestCheckDevDataDirDoesNothingWithoutTheVariable is the state every user is in
// by default, and it has to be a no-op rather than a refusal: the variable is
// opt-in, and a build that required it would be a build nobody can run.
func TestCheckDevDataDirDoesNothingWithoutTheVariable(t *testing.T) {
	dataDir := mkdirAll(t, filepath.Join(t.TempDir(), "real-store"))
	for _, value := range []string{"", " ", string(os.PathListSeparator)} {
		t.Setenv(DevForbidDataDirEnv, value)
		if err := CheckDevDataDir(devVersion, dataDir); err != nil {
			t.Errorf("CheckDevDataDir with the variable set to %q = %v, want nil", value, err)
		}
	}
}

// TestCheckDevDataDirHandlesAPathThatDoesNotExist covers the case a real
// development run hits constantly. The store a build is pointed at may not be
// there yet — a fresh sandbox, a wiped install, a directory listed before it was
// created — and the guard cannot answer by resolving a path that is not there.
// The deepest existing ancestor is resolved instead, so a symlinked parent still
// matches while a missing tail compares as the missing tail it is.
func TestCheckDevDataDirHandlesAPathThatDoesNotExist(t *testing.T) {
	root := t.TempDir()
	real := mkdirAll(t, filepath.Join(root, "real-store"))
	link := filepath.Join(root, "link-to-store")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("cannot create a symlink here: %v", err)
	}

	// A data directory that does not exist, reached through a symlinked parent.
	// Nothing is created by the check — that is the caller's business — and the
	// comparison still has to see that it is the listed store.
	// A data directory that does not exist yet, reached through a symlinked
	// parent, against a listing naming the same directory by its resolved path.
	// The two sides need different work to compare equal — the listing resolves
	// outright, the data dir only once its missing tail is re-attached to a
	// resolved ancestor — and a guard that resolved either side differently
	// would let a store through on exactly the spelling a fresh install uses.
	t.Setenv(DevForbidDataDirEnv, filepath.Join(real, "not-created"))
	missing := filepath.Join(link, "not-created")
	if err := CheckDevDataDir(devVersion, missing); err == nil {
		t.Error("a data directory that does not exist yet, reached through a symlink, was opened, want a refusal")
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Errorf("the check created %s, want nothing created", missing)
	}

	// The other direction: a LISTED directory that does not exist yet, and a
	// data directory that does not exist either. The two compare equal, which is
	// the honest answer — they are the same directory.
	notYet := filepath.Join(root, "not-created-yet", "ghost")
	t.Setenv(DevForbidDataDirEnv, notYet)
	if err := CheckDevDataDir(devVersion, notYet); err == nil {
		t.Error("a data directory equal to a listed path that does not exist was opened, want a refusal")
	}
	// A listed path whose parent does not exist is compared on its own spelling
	// rather than refused everything.
	t.Setenv(DevForbidDataDirEnv, filepath.Join(root, "nowhere", "at-all"))
	if err := CheckDevDataDir(devVersion, real); err != nil {
		t.Errorf("CheckDevDataDir = %v, want nil: an unresolvable listing is not the data directory", err)
	}
}

// withBuildVersion sets the version the RESOLVERS judge this build by (the
// string cmd/ghost hands over from its own `version` var) and restores it, so
// one test cannot leave the next one believing it is a release.
func withBuildVersion(t *testing.T, v string) {
	t.Helper()
	prev := BuildVersion()
	SetBuildVersion(v)
	t.Cleanup(func() { SetBuildVersion(prev) })
}

// TestTheResolversRefuseAListedDirectoryOnADevBuild is the structural half of
// #721: the check lives INSIDE config.DataDirPath, so it is not the callers that
// remember to ask. A path that only writes a marker, a pid file or a log still
// resolves the data directory, and every one of those used to reach a forbidden
// directory that way — so the invariant is asserted on the resolver itself, and
// a future caller of DataDir or DataDirPath is covered by having used it.
func TestTheResolversRefuseAListedDirectoryOnADevBuild(t *testing.T) {
	withBuildVersion(t, devVersion)
	root := t.TempDir()
	dataHome := filepath.Join(root, "data")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", root, err)
	}
	t.Setenv("XDG_DATA_HOME", dataHome)
	t.Setenv(DevForbidDataDirEnv, filepath.Join(dataHome, "ghost"))

	if _, err := DataDirPath(); err == nil {
		t.Error("DataDirPath = nil, want a refusal on a development build")
	} else if !strings.Contains(err.Error(), DevForbidDataDirEnv) {
		t.Errorf("the refusal does not name %s: %v", DevForbidDataDirEnv, err)
	}

	// The creating resolver is the one that could leave a phantom directory in
	// the store the variable protects: DataDir MkdirAll's, and it has to be
	// AFTER the check, not before it.
	if _, err := DataDir(); err == nil {
		t.Error("DataDir = nil, want a refusal on a development build")
	}
	for _, path := range []string{
		filepath.Join(dataHome, "ghost"),
		filepath.Join(dataHome, "ghost", "scratch"),
	} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("the refused resolver created %s, want nothing created (stat error: %v)", path, err)
		}
	}
}

// TestTheResolversRefuseWithoutAWiredBuildVersion is the unwired case, which is
// the one a default decides. Every other test here names the version it is
// testing; this one calls no setter at all, so it is what a binary whose
// dispatch has not run (and a caller that has not wired the version) actually
// gets. A default that read as a release would leave the rule off for exactly
// that binary, silently.
func TestTheResolversRefuseWithoutAWiredBuildVersion(t *testing.T) {
	root := t.TempDir()
	dataHome := filepath.Join(root, "data")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", root, err)
	}
	t.Setenv("XDG_DATA_HOME", dataHome)
	t.Setenv(DevForbidDataDirEnv, filepath.Join(dataHome, "ghost"))

	if _, err := DataDir(); err == nil {
		t.Errorf("DataDir = nil with the default build version, want a refusal: the default must be the GUARDED side")
	}
	if _, err := DataDirPath(); err == nil {
		t.Errorf("DataDirPath = nil with the default build version, want a refusal: the default must be the GUARDED side")
	}
}

// TestTheResolversIgnoreTheVariableOnAReleaseBuild is why the variable can be
// exported into a developer's shell: the same listed directory, on the version
// the release ldflags stamp, is created and returned.
func TestTheResolversIgnoreTheVariableOnAReleaseBuild(t *testing.T) {
	withBuildVersion(t, "0.39.0")
	root := t.TempDir()
	dataHome := filepath.Join(root, "data")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", root, err)
	}
	t.Setenv("XDG_DATA_HOME", dataHome)
	t.Setenv(DevForbidDataDirEnv, filepath.Join(dataHome, "ghost"))

	want := filepath.Join(dataHome, "ghost")
	got, err := DataDir()
	if err != nil {
		t.Fatalf("DataDir on a release build = %v, want the directory", err)
	}
	if got != want {
		t.Errorf("DataDir = %q, want %q", got, want)
	}
	if _, err := os.Stat(want); err != nil {
		t.Errorf("DataDir did not create %s: %v", want, err)
	}
	// And the non-creating one, which is what the read-only paths use.
	t.Setenv(DevForbidDataDirEnv, filepath.Join(root, "somewhere-else"))
	if got, err := DataDirPath(); err != nil {
		t.Errorf("DataDirPath on a release build = %v, want the path", err)
	} else if got != want {
		t.Errorf("DataDirPath = %q, want %q", got, want)
	}
}

// TestTheResolversDoNothingWithoutTheVariable is the state every user is in by
// default, and the reason this change is safe for the rest of the tree: with the
// variable unset the resolvers are exactly what they always were.
func TestTheResolversDoNothingWithoutTheVariable(t *testing.T) {
	withBuildVersion(t, devVersion)
	root := t.TempDir()
	dataHome := filepath.Join(root, "data")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", root, err)
	}
	t.Setenv("XDG_DATA_HOME", dataHome)
	t.Setenv(DevForbidDataDirEnv, "")

	want := filepath.Join(dataHome, "ghost")
	got, err := DataDir()
	if err != nil {
		t.Fatalf("DataDir = %v, want the directory", err)
	}
	if got != want {
		t.Errorf("DataDir = %q, want %q", got, want)
	}
	if _, err := os.Stat(want); err != nil {
		t.Errorf("DataDir did not create %s: %v", want, err)
	}
}

// TestTheDefaultBuildVersionIsADevelopmentBuild pins the direction an unwired
// build falls. `SetBuildVersion` is called by cmd/ghost's dispatch, and a
// default that read as a RELEASE would leave the guard off for every caller that
// has not wired it — a rule that is off until something remembers to turn it on.
func TestTheDefaultBuildVersionIsADevelopmentBuild(t *testing.T) {
	prev := BuildVersion()
	SetBuildVersion("0.39.0")
	if got := BuildVersion(); got != "0.39.0" {
		t.Fatalf("BuildVersion() = %q after setting 0.39.0", got)
	}
	SetBuildVersion(prev)
	if prev != "dev" {
		t.Errorf("the default build version is %q, want \"dev\"", prev)
	}
}
