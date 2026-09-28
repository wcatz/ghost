package mcpinit

import (
	"os"

	"github.com/wcatz/ghost/internal/config"
)

// buildVersion is this binary's version: the string cmd/ghost's release ldflags
// stamp into main.version, and "dev" for a plain `go build`. It is the same
// value the whole dispatch already has, handed in once rather than read from a
// second place — see SetBuildVersion.
//
// The default is "dev" on purpose, and it is the guarded side of the
// GHOST_DEV_FORBID_DATA_DIR rule: a caller that forgets to wire the version gets
// a development build's behaviour, which is the safe direction. A default that
// read as a release would be a rule that is off until something remembers to
// turn it on.
var buildVersion = "dev"

// SetBuildVersion tells this package which build it is part of. cmd/ghost calls
// it once, from the dispatch that covers the MCP server, the lifecycle hooks and
// every CLI subcommand, for the reason memory.SetDetectRemote is wired there:
// threading one string through every exported entry point (Run, RunOpencode,
// RunCodex, RunGoose, Status, StatusOpencode, RenderSessionContextAt,
// RunHostEvent) would make the version a parameter of the hook contract instead
// of a fact about the process.
//
// It is a setter rather than an exported var because a var would invite a write
// from a goroutine, and this value is read on every hook path.
func SetBuildVersion(v string) { buildVersion = v }

// guardedDataDir resolves the ghost data directory for a path that is about to
// open a store, and refuses the one GHOST_DEV_FORBID_DATA_DIR names when this
// build is not a release (#721).
//
// Every store-opening path in this package resolves its data directory through
// here, and each of them ALREADY returns empty on a data-dir error — that is the
// hook's fail-open contract, and it is what makes the refusal safe on a hook: no
// database is opened, and the session is never blocked. The alternative, asking
// each call site to handle a refusal of its own, is a second statement of the
// same rule at ten call sites, and the one that forgot it would be the one that
// opened a forbidden store.
func guardedDataDir() (string, error) {
	dir, err := guardedDataDirPath()
	if err != nil {
		return "", err
	}
	// Created only now that the guard has passed: a refusal must leave no
	// phantom directory behind either, and DataDirPath never creates.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

// guardedDataDirPath is guardedDataDir for the paths that must leave nothing
// behind — the marker writers and the status check, which resolve a directory
// that the store itself is what normally created.
func guardedDataDirPath() (string, error) {
	dir, err := config.DataDirPath()
	if err != nil {
		return "", err
	}
	return dir, config.CheckDevDataDir(buildVersion, dir)
}

// resolveMarkerProject is the one place a marker writer turns a directory it was
// handed into a store read, so the GHOST_DEV_FORBID_DATA_DIR refusal belongs
// here: it is the single point every marker path (WriteLifecycleFailure,
// ClearLifecycleFailure, ReadLifecycleFailure, TouchLifecycleStart) passes
// through, and "" is this function's own answer to every failure it already
// has — no store, nothing to report.
