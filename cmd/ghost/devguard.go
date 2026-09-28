package main

import (
	"github.com/wcatz/ghost/internal/config"
)

// requireDataDir resolves the ghost data directory for an entry point that is
// about to open a store, and refuses the one GHOST_DEV_FORBID_DATA_DIR names
// when this build is not a release (#721).
//
// The check runs BEFORE config.DataDir creates anything, which is the whole
// ordering: a guard one line later would leave a phantom data directory behind
// in the very store the variable exists to protect, and a guard inside
// DataDirPath would refuse the read-only paths as well as the open that
// migrates — the issue's scope is every entry point that OPENS a store, and
// this is where cmd/ghost's share of them resolve the directory.
//
// Every store-opening command goes through one of these two functions, so
// `version` is read from the one place the release ldflags stamp it and the
// rule cannot be applied with a different version than the binary reports.
func requireDataDir() (string, error) {
	if _, err := requireDataDirPath(); err != nil {
		return "", err
	}
	// Created only now that the guard has passed.
	return config.DataDir()
}

// requireDataDirPath resolves the data directory WITHOUT creating it, for the
// paths that must leave nothing behind: the read-only transfer store (an export,
// a dry-run import), the history and maintenance reports, and the diagnostic
// store behind `ghost mcp status`. The refusal is an ordinary error here, so
// each of those callers reports it the way it already reports a data dir it
// cannot resolve.
func requireDataDirPath() (string, error) {
	dir, err := config.DataDirPath()
	if err != nil {
		return "", err
	}
	return dir, config.CheckDevDataDir(version, dir)
}
