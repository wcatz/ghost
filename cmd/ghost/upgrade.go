package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/wcatz/ghost/internal/mcpinit"
	"github.com/wcatz/ghost/internal/selfupdate"
)

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
	if latest == version {
		fmt.Printf("Already up to date (%s).\n", version)
		return
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
	checksumBytes, err := io.ReadAll(checksumBody)
	_ = checksumBody.Close()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: reading checksums.txt: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Downloading %s...\n", asset.Name)
	body, err := selfupdate.Download(asset.BrowserDownloadURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	archiveBytes, err := io.ReadAll(body)
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
