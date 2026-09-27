package main

import (
	"context"
	"fmt"
	"io"
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

// upgradeOptions is the parsed `ghost upgrade` command line.
type upgradeOptions struct {
	allowDowngrade bool
}

// parseUpgradeArgs parses `ghost upgrade` arguments. Only --allow-downgrade is
// recognized; anything else is an error rather than being ignored, because a
// flag that does not parse here is a flag the user believes is in effect while
// the command does something else.
func parseUpgradeArgs(args []string) (upgradeOptions, error) {
	var opts upgradeOptions
	for _, arg := range args {
		switch arg {
		case "--allow-downgrade":
			opts.allowDowngrade = true
		default:
			return upgradeOptions{}, fmt.Errorf("unknown argument %q (usage: ghost upgrade [--allow-downgrade])", arg)
		}
	}
	return opts, nil
}

// decideUpgrade compares the running version against the latest release tag.
//
// Ordering matters: the release has to be newer than what is already
// installed. A tag that is merely *different* is not an upgrade, and installing
// it moves the user backwards — the "upgrade" that unpicks a yanked release
// whose tag no longer points at the newest build, or that reads "0.9.0" as
// newer than "0.10.0" because it compared the strings.
//
// allowDowngrade is the user's explicit override for the case where backwards
// is what they want: a release that was withdrawn, or a build that has to be
// pinned while the newer one is investigated. It changes the direction
// decision and nothing else — being on the latest version is still "already up
// to date", and an unorderable version is still unorderable below.
//
// A version that cannot be ordered — a "dev" build, a tag like
// "vscode-pre-rewrite" — keeps the pre-guard behaviour of going ahead, because
// the alternative is refusing to update every developer build, and the archive
// checksum still has to agree before anything is installed. An unorderable
// version identical to the release is still the one installed, as before.
func decideUpgrade(running, latest string, allowDowngrade bool) upgradeOutcome {
	if strings.TrimPrefix(running, "v") == strings.TrimPrefix(latest, "v") {
		return upgradeCurrent
	}
	cmp, err := selfupdate.CompareVersions(latest, running)
	switch {
	case err != nil:
		return upgradeProceed
	case cmp == 0:
		// Equal precedence, different text: "0.1.0" and "0.1.0+build.5" are
		// the same release.
		return upgradeCurrent
	case cmp < 0 && !allowDowngrade:
		return upgradeRefuseDowngrade
	default:
		return upgradeProceed
	}
}

// isOlderRelease reports whether latest is *provably* older than running. It is
// the fact decideUpgrade refuses on and the warning reports on, so both agree
// about what counts as backwards. An unorderable version is not older: nothing
// says it is.
func isOlderRelease(running, latest string) bool {
	cmp, err := selfupdate.CompareVersions(latest, running)
	return err == nil && cmp < 0
}

// downgradeMessage names both versions, so the refusal explains which release
// the guard saw and which one is installed, what to do when the installed build
// is the one that was withdrawn, and how to say so deliberately if it is not.
func downgradeMessage(running, latest string) string {
	return fmt.Sprintf(
		"refusing to downgrade: the latest release is %s but this binary is %s. If %s was withdrawn, re-run with --allow-downgrade, or install the release archive you want from https://github.com/wcatz/ghost/releases",
		latest, running, running)
}

// upgradeUsage is the help for `ghost upgrade`: stdout for -h/--help (see
// handleHelp), so a help request never checks GitHub Releases, downloads an
// archive or replaces the running binary.
const upgradeUsage = `Usage: ghost upgrade [--allow-downgrade]

Checks GitHub Releases and replaces this binary after verifying the archive
against the digest GitHub reports for that release asset and against the
published checksum manifest. A plugin-managed binary refuses this path: update
it with /plugin update in Claude Code instead.

  --allow-downgrade   Install a release older than this binary. Without it, an
                      older latest release is refused.
`

// runUpgrade downloads and installs the latest ghost release.
func runUpgrade(args []string) {
	opts, err := parseUpgradeArgs(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

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

	// One context for the whole command: the deadlines inside selfupdate bound
	// each request, and cancelling this one ends an in-flight transfer if the
	// command is ever given a reason to stop.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rel, err := selfupdate.LatestRelease(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	latest := strings.TrimPrefix(rel.TagName, "v")
	switch decideUpgrade(version, latest, opts.allowDowngrade) {
	case upgradeCurrent:
		fmt.Printf("Already up to date (%s).\n", version)
		return
	case upgradeRefuseDowngrade:
		fmt.Fprintf(os.Stderr, "error: %s\n", downgradeMessage(version, latest))
		os.Exit(1)
	}
	if opts.allowDowngrade && isOlderRelease(version, latest) {
		// The flag is what let a backwards install through, so say that where
		// a warning is visible rather than leaving a downgrade to read as an
		// upgrade in the line of output below.
		fmt.Fprintf(os.Stderr, "warning: installing %s over %s — --allow-downgrade was given\n", latest, version)
	}

	asset, err := selfupdate.FindAsset(rel)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	if err := installRelease(ctx, os.Stdout, rel, asset, func(binary []byte) error {
		fmt.Printf("Replacing %s...\n", exe)
		return selfupdate.Replace(exe, binary)
	}); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Updated: ghost %s → %s\n", version, latest)

	// A new binary can invalidate wiring the old one installed (hook flags,
	// embedded plugin sources). Surface stale integrations instead of letting
	// them fail open invisibly on every fire.
	mcpinit.ReportStaleIntegrations(os.Stdout)
}

// installRelease downloads the release's archive for asset, verifies it against
// both digests the release publishes, extracts the binary and hands it to
// install. Nothing reaches install that has not been verified: the archive is
// checked before it is parsed, so a substituted release is refused without the
// attacker-supplied bytes ever being decompressed.
//
// install is a parameter so the one property that matters — an unverified
// archive never reaches the file replacing the running binary — can be asserted
// directly, against a local server, rather than inferred from the order of the
// statements above.
func installRelease(ctx context.Context, out io.Writer, rel *selfupdate.Release, asset *selfupdate.Asset, install func([]byte) error) error {
	// Fail closed on both published digests. GitHub's is checked first
	// because it is the one that does not come from a second file in the same
	// release: checksums.txt is uploaded alongside the binary, so anyone able
	// to replace one can replace the manifest that vouches for the other. The
	// manifest is still checked, as an independent witness — and a refusal
	// naming it gives the user a digest to check by hand.
	checksumAsset, err := selfupdate.FindChecksumAsset(rel)
	if err != nil {
		return err
	}
	manifest, err := downloadCapped(ctx, checksumAsset.BrowserDownloadURL, selfupdate.ReadChecksums)
	if err != nil {
		return err
	}

	fmt.Fprintf(out, "Downloading %s...\n", asset.Name)
	archive, err := downloadCapped(ctx, asset.BrowserDownloadURL, selfupdate.ReadArchive)
	if err != nil {
		return fmt.Errorf("downloading %s: %w", asset.Name, err)
	}

	if err := selfupdate.VerifyAssetDigest(archive, asset); err != nil {
		return err
	}
	if err := selfupdate.VerifyChecksum(archive, string(manifest), asset.Name); err != nil {
		return err
	}

	binary, err := selfupdate.ExtractBinary(archive)
	if err != nil {
		return err
	}
	return install(binary)
}

// downloadCapped fetches url and reads the body through read, which is the only
// thing that decides how large a response may become.
func downloadCapped(ctx context.Context, url string, read func(io.Reader) ([]byte, error)) ([]byte, error) {
	body, err := selfupdate.Download(ctx, url)
	if err != nil {
		return nil, err
	}
	// Closed before the error is reported so a failed read still releases the
	// connection and ends the request's deadline.
	data, readErr := read(body)
	_ = body.Close()
	if readErr != nil {
		return nil, readErr
	}
	return data, nil
}
