package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/wcatz/ghost/internal/selfupdate"
)

// DevForbidDataDirEnv names a data directory a build that is not a release
// refuses to open. Its value is a list of directories separated by the
// platform's list separator (`:` on Unix, `;` on Windows, so a Windows drive
// letter is a path rather than a separator).
//
// What it is for: a development build opened against a real store migrates it
// to the branch's schema, and the installed release then refuses the store as
// "newer than this build" until a release catches up. The protection used to be
// an instruction to set the data paths on every command, and an instruction is
// not a mechanism — the one that broke was a malformed environment export that
// left XDG_DATA_HOME pointing at the real store.
//
// A RELEASE build ignores the variable entirely, which is what makes it safe to
// export into a developer's shell: the same environment reaches the release
// Ghost those sessions use through their MCP integration, and that one must keep
// working against the real store.
const DevForbidDataDirEnv = "GHOST_DEV_FORBID_DATA_DIR"

// CheckDevDataDir reports an error when dataDir is one of the directories
// DevForbidDataDirEnv names and version is not a release version. It is a pure
// query: it reads the variable, resolves paths and creates nothing, so a caller
// can run it before anything has touched the filesystem.
//
// version is the BUILD's version — the string the release ldflags stamp into
// main.version, and "dev" for a plain `go build`. It is a parameter rather than
// a package variable because that ldflags var is the one value in the tree that
// already says what this build is, and a second copy of it would be a second
// answer to the same question. `selfupdate.IsRelease` is what decides, so the
// rule is the release parser's and not a second spelling of "looks like a
// version": `dev`, a `git describe` stamp, a prerelease, and anything that does
// not parse are all development builds, and none of them may open a listed
// directory.
//
// Both sides of the comparison are canonicalized (absolute, symlinks resolved,
// and a path that does not exist yet handled), because the whole failure this
// guards against is a spelling difference: a symlinked home, a relative export, a
// trailing separator. Equality is the rule and a listed directory is a DATA
// directory (`<data home>/ghost`) rather than its parent, so a store nobody
// named is never refused.
//
// Callers: every entry point that opens a store, before the open. The hook paths
// are the exception in what they do with the error rather than in whether they
// ask — they fail open, so a session is never blocked and no database is
// opened.
func CheckDevDataDir(version, dataDir string) error {
	// A release build ignores the variable, and returns before resolving
	// anything: the check is not merely satisfied by a release, it is absent
	// from one.
	if selfupdate.IsRelease(version) {
		return nil
	}
	listed := os.Getenv(DevForbidDataDirEnv)
	if listed == "" {
		return nil
	}
	want, err := canonicalDataDir(dataDir)
	if err != nil {
		return err
	}
	for _, entry := range strings.Split(listed, string(os.PathListSeparator)) {
		if entry == "" {
			// A separator artifact, not a directory named "": the empty string
			// resolves against the working directory, so reading one as a path
			// would refuse whatever the process happens to be in.
			continue
		}
		got, err := canonicalDataDir(entry)
		if err != nil {
			// A listing nothing on this filesystem can resolve is not this data
			// directory. Skipping it is the only safe answer: there is nothing
			// to compare, and treating an unresolvable path as a match would
			// refuse every store.
			continue
		}
		if got == want {
			return fmt.Errorf("%s refuses to open the data directory %s: this build is %q, which is not a release, and a development build migrates a store it opens. Run a released ghost against this directory, or unset %s",
				DevForbidDataDirEnv, want, version, DevForbidDataDirEnv)
		}
	}
	return nil
}

// canonicalDataDir is the one spelling of a data directory both sides of the
// comparison are reduced to: absolute, and with symlinks resolved.
//
// A path that does not exist is the case that needs the work. `EvalSymlinks`
// fails on it, and a store a development build is pointed at may well not be
// there yet — a fresh sandbox, an install that has not run, a directory listed
// before it was created. So the deepest ancestor that DOES exist is resolved and
// the remaining components are re-attached: a symlinked parent still matches,
// and a missing tail compares as the missing tail it is rather than being
// dropped.
func canonicalDataDir(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	dir, rest := abs, ""
	for {
		if resolved, err := filepath.EvalSymlinks(dir); err == nil {
			return filepath.Join(resolved, rest), nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			// The root, or a Windows volume that resolves to nothing: no
			// component of this path exists, so there is nothing to resolve and
			// the cleaned absolute path is the whole answer.
			return abs, nil
		}
		rest = filepath.Join(filepath.Base(dir), rest)
		dir = parent
	}
}
