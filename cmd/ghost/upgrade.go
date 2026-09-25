package main

import (
	"bytes"
	"fmt"
	"os"
	"strings"

	"github.com/wcatz/ghost/internal/mcpinit"
	"github.com/wcatz/ghost/internal/selfupdate"
)

// upgradeOutcome is what `ghost upgrade` decided to do, from the running
// version and the latest release tag alone — before anything is downloaded.
type upgradeOutcome int

const (
	upgradeProceed upgradeOutcome = iota
	upgradeCurrent
	upgradeRefuseDowngrade
)

// decideUpgrade compares the running version against the latest release tag.
//
// Ordering matters: the release has to be newer than what is already
// installed. A tag that is merely *different* is not an upgrade, and installing
// it moves the user backwards — the "upgrade" that unpicks a yanked release
// whose tag no longer points at the newest build, or that reads "0.9.0" as
// newer than "0.10.0" because it compared the strings.
//
// A version that cannot be ordered — a "dev" build, a tag like
// "vscode-pre-rewrite" — keeps the pre-guard behaviour of going ahead, because
// the alternative is refusing to update every developer build, and the archive
// checksum still has to agree before anything is installed.
func decideUpgrade(running, latest string) upgradeOutcome {
	cmp, err := selfupdate.CompareVersions(latest, running)
	switch {
	case err != nil:
		return upgradeProceed
	case cmp < 0:
		return upgradeRefuseDowngrade
	case cmp == 0:
		return upgradeCurrent
	default:
		return upgradeProceed
	}
}

// downgradeMessage names both versions, so the refusal explains which release
// the guard saw and which one is installed, and what to do when the installed
// build is the one that was withdrawn.
func downgradeMessage(running, latest string) string {
	return fmt.Sprintf(
		"refusing to downgrade: the latest release is %s but this binary is %s. If %s was withdrawn, install the release archive you want from https://github.com/wcatz/ghost/releases",
		latest, running, running)
}

// runUpgrade downloads and installs the latest ghost release.
func runUpgrade() {
	// A plugin-managed binary is replaced by the plugin manager, not by us:
	// writing into the plugin cache would fight `/plugin update` and be undone
	// on the next plugin update. Refuse rather than guess.
	if mcpinit.RunningAsPlugin() {
		fmt.Println("This ghost binary is managed by the Claude Code plugin — update it with `/plugin update` in Claude Code, not `ghost upgrade`.")
		return
	}

	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: cannot determine binary path: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Current: ghost %s (%s)\n", version, exe)
	fmt.Println("Checking for updates...")

	rel, err := selfupdate.LatestRelease()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	latest := strings.TrimPrefix(rel.TagName, "v")
	switch decideUpgrade(version, latest) {
	case upgradeCurrent:
		fmt.Printf("Already up to date (%s).\n", version)
		return
	case upgradeRefuseDowngrade:
		fmt.Fprintf(os.Stderr, "error: %s\n", downgradeMessage(version, latest))
		os.Exit(1)
	}

	asset, err := selfupdate.FindAsset(rel)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	// Fail closed: the archive digest must match the release's checksums.txt
	// manifest before a single byte reaches Replace. Without this, anyone able
	// to substitute a release asset gets arbitrary code execution on every
	// machine that runs `ghost upgrade`.
	checksumAsset, err := selfupdate.FindChecksumAsset(rel)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	checksumBody, err := selfupdate.Download(checksumAsset.BrowserDownloadURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	// Capped: a manifest is a few hundred bytes, so a response larger than the
	// cap is not one, and reading it whole would just move the problem.
	checksumBytes, err := selfupdate.ReadChecksums(checksumBody)
	_ = checksumBody.Close()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Downloading %s...\n", asset.Name)
	body, err := selfupdate.Download(asset.BrowserDownloadURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	// Capped, and over the cap is an error: a truncated archive would either
	// fail to extract or extract a short binary, and neither is an answer.
	archiveBytes, err := selfupdate.ReadArchive(body)
	_ = body.Close()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: downloading %s: %v\n", asset.Name, err)
		os.Exit(1)
	}

	if err := selfupdate.VerifyChecksum(archiveBytes, string(checksumBytes), asset.Name); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	bin, err := selfupdate.ExtractBinary(bytes.NewReader(archiveBytes))
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Replacing %s...\n", exe)
	if err := selfupdate.Replace(exe, bin); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Updated: ghost %s → %s\n", version, latest)

	// A new binary can invalidate wiring the old one installed (hook flags,
	// embedded plugin sources). Surface stale integrations instead of letting
	// them fail open invisibly on every fire.
	mcpinit.ReportStaleIntegrations(os.Stdout)
}
