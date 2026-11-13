package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/wcatz/ghost/internal/config"
	"github.com/wcatz/ghost/internal/mcpinit"
	"github.com/wcatz/ghost/internal/selfupdate"
)

type upgradeOutcome int

const (
	upgradeProceed upgradeOutcome = iota
	upgradeCurrent
	upgradeRefuseDowngrade
	upgradeRefusePrerelease
)

var upgradeBudget = 12 * time.Minute

type upgradeStreams struct{ out, err io.Writer }

type upgradeDeps struct {
	fetch   func(context.Context) (*selfupdate.Release, error)
	install func([]byte) error
	attest  func(ctx context.Context, archive []byte, version string) selfupdate.AttestationResult
}

type upgradeOptions struct {
	allowDowngrade  bool
	allowPrerelease bool
	allowUnattested bool
}

func parseUpgradeArgs(args []string) (upgradeOptions, error) {
	var opts upgradeOptions
	for _, arg := range args {
		switch arg {
		case "--allow-downgrade":
			opts.allowDowngrade = true
		case "--allow-prerelease":
			opts.allowPrerelease = true
		case "--allow-unattested":
			opts.allowUnattested = true
		default:
			return upgradeOptions{}, fmt.Errorf("unknown argument %q (usage: ghost upgrade [--allow-downgrade] [--allow-prerelease] [--allow-unattested])", arg)
		}
	}
	return opts, nil
}

func decideUpgrade(running, latest string, opts upgradeOptions) upgradeOutcome {
	if strings.TrimPrefix(running, "v") == strings.TrimPrefix(latest, "v") {
		return upgradeCurrent
	}
	if !opts.allowPrerelease && selfupdate.IsPrerelease(latest) {
		return upgradeRefusePrerelease
	}
	cmp, err := selfupdate.CompareVersions(latest, running)
	switch {
	case err != nil:
		return upgradeProceed
	case cmp == 0:
		return upgradeCurrent
	case cmp < 0 && !opts.allowDowngrade:
		return upgradeRefuseDowngrade
	default:
		return upgradeProceed
	}
}

func isOlderRelease(running, latest string) bool {
	cmp, err := selfupdate.CompareVersions(latest, running)
	return err == nil && cmp < 0
}

func downgradeMessage(running, latest string) string {
	return fmt.Sprintf(
		"refusing to downgrade: the latest release is %s but this binary is %s. If %s was withdrawn, re-run with --allow-downgrade, or install the release archive you want from https://github.com/wcatz/ghost/releases",
		latest, running, running)
}

func prereleaseMessage(latest string) string {
	return fmt.Sprintf(
		"refusing to install the prerelease %s: it is not a release. Re-run with --allow-prerelease to install it deliberately, or wait for the release it leads to",
		latest)
}

func unattestedMessage(latest string) string {
	return fmt.Sprintf(
		"refusing to install %s: GitHub publishes no build attestation for its release archive, so nothing says which workflow built these bytes. "+
			"The published digests confirm this is the file the release is serving, not that this repository's release workflow made it. "+
			"Re-run with --allow-unattested to install it deliberately, or install the release archive you want from https://github.com/wcatz/ghost/releases",
		latest)
}

func attestationUnreachableMessage(latest, detail string) string {
	return fmt.Sprintf(
		"refusing to install %s: could not check its build attestation (%v). That is not the same as a release with no attestation — nothing was established either way. "+
			"Re-run with --allow-unattested to install without the check, or install the release archive you want from https://github.com/wcatz/ghost/releases",
		latest, detail)
}

func attestationRejectedMessage(latest, detail string) string {
	return fmt.Sprintf("refusing to install %s: %s", latest, detail)
}

func noVerifierMessage(latest string) string {
	return fmt.Sprintf(
		"refusing to install %s: this build of ghost could not check the release's build attestation at all, which is a bug in ghost rather than a property of the release — no flag changes it. "+
			"Install the release archive you want from https://github.com/wcatz/ghost/releases, and please report this at https://github.com/wcatz/ghost/issues",
		latest)
}

func unverifiableStateMessage(latest, got string) string {
	return fmt.Sprintf(
		"refusing to install %s: the attestation check reported an unrecognised state (%s), which this build has no policy for — no flag changes it. "+
			"Install the release archive you want from https://github.com/wcatz/ghost/releases, and please report this at https://github.com/wcatz/ghost/issues",
		latest, got)
}

const upgradeUsage = `Usage: ghost upgrade [--allow-downgrade] [--allow-prerelease] [--allow-unattested]

Checks GitHub Releases and replaces this binary after verifying the archive
against the digest GitHub reports for that release asset, against the published
checksum manifest, and — for release 0.43.0 onwards — against the build
attestation GitHub publishes for it, which names this repository's release
workflow as the signer. The whole run is bounded by a 12 minute budget.
A plugin-managed binary refuses this path: update it with /plugin update in
Claude Code instead.

  --allow-downgrade   Install a release older than this binary. Without it, an
                      older latest release is refused.
  --allow-prerelease  Install a prerelease (an rc, a beta). Without it, a
                      prerelease is refused however new it is.
  --allow-unattested  Install a release whose attestation could not be checked
                      — either because the release publishes none, or because
                      the attestation service could not be reached. It never
                      permits a release whose attestation is present and did not
                      verify: that is always refused.
`

const pluginManagedRefusal = "This ghost binary is managed by the Claude Code plugin — update it with `/plugin update` in Claude Code, not `ghost upgrade`."

func runUpgrade(args []string) {
	opts, err := parseUpgradeArgs(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	if mcpinit.RunningAsPlugin() {
		fmt.Fprintln(os.Stderr, pluginManagedRefusal)
		os.Exit(1)
	}

	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: cannot determine binary path: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Current: ghost %s (%s)\n", version, exe)
	fmt.Println("Checking for updates...")

	installed, err := performUpgrade(context.Background(), upgradeStreams{out: os.Stdout, err: os.Stderr}, version, opts, upgradeDeps{
		fetch: selfupdate.LatestRelease,
		install: func(binary []byte) error {
			fmt.Printf("Replacing %s...\n", exe)
			note, err := selfupdate.Replace(exe, binary)
			if note != "" {
				fmt.Fprintf(os.Stderr, "warning: %s\n", note)
			}
			return err
		},
		attest: selfupdate.LiveAttestationCheck(config.DataDirPath),
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	if installed == "" {
		return
	}
	fmt.Printf("Updated: ghost %s → %s\n", version, installed)

	reportStaleIntegrations(exe, upgradeStreams{out: os.Stdout, err: os.Stderr})

	mcpinit.ReportStaleServers(os.Stdout, exe)
}

var wiringCheckArgv = []string{"mcp", "wiring"}

var spawnWiringCheck = func(exe string, out io.Writer) error {
	cmd := exec.Command(exe, wiringCheckArgv...)
	cmd.Stdout = out
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func reportStaleIntegrations(exe string, streams upgradeStreams) {
	if err := spawnWiringCheck(exe, streams.out); err != nil {
		_, _ = fmt.Fprintf(streams.err, "warning: the integration wiring check did not run (%v) — run `ghost mcp wiring` to check it yourself\n", err)
	}
}

func performUpgrade(parent context.Context, streams upgradeStreams, running string, opts upgradeOptions, deps upgradeDeps) (string, error) {
	ctx, cancel := context.WithTimeout(parent, upgradeBudget)
	defer cancel()

	rel, err := deps.fetch(ctx)
	if err != nil {
		return "", withBudget(parent, ctx, err)
	}

	latest := strings.TrimPrefix(rel.TagName, "v")
	switch decideUpgrade(running, latest, opts) {
	case upgradeCurrent:
		_, _ = fmt.Fprintf(streams.out, "Already up to date (%s).\n", running)
		return "", nil
	case upgradeRefusePrerelease:
		return "", errors.New(prereleaseMessage(latest))
	case upgradeRefuseDowngrade:
		return "", errors.New(downgradeMessage(running, latest))
	}

	if opts.allowPrerelease && selfupdate.IsPrerelease(latest) {
		_, _ = fmt.Fprintf(streams.err, "warning: about to install the prerelease %s over %s — --allow-prerelease was given\n", latest, running)
	}
	if opts.allowDowngrade && isOlderRelease(running, latest) {
		_, _ = fmt.Fprintf(streams.err, "warning: about to install %s over the newer %s — --allow-downgrade was given\n", latest, running)
	}

	asset, err := selfupdate.FindAsset(rel)
	if err != nil {
		return "", withBudget(parent, ctx, err)
	}
	if err := installRelease(ctx, streams, rel, asset, opts, deps); err != nil {
		return "", withBudget(parent, ctx, err)
	}
	return latest, nil
}

func withBudget(parent, ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return err
	}
	if errors.Is(parent.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("ghost upgrade stopped on the deadline its caller set: %w", err)
	}
	return fmt.Errorf("ghost upgrade exceeded its %s total budget: %w", upgradeBudget, err)
}

func installRelease(ctx context.Context, streams upgradeStreams, rel *selfupdate.Release, asset *selfupdate.Asset, opts upgradeOptions, deps upgradeDeps) error {
	version := strings.TrimPrefix(rel.TagName, "v")
	checksumAsset, err := selfupdate.FindChecksumAsset(rel)
	if err != nil {
		return err
	}
	manifest, err := downloadCapped(ctx, checksumAsset.BrowserDownloadURL, selfupdate.ReadChecksums)
	if err != nil {
		return err
	}

	_, _ = fmt.Fprintf(streams.out, "Downloading %s...\n", asset.Name)
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
	if err := checkAttestation(ctx, streams.err, archive, version, opts, deps); err != nil {
		return err
	}

	binary, err := selfupdate.ExtractBinary(archive)
	if err != nil {
		return err
	}
	return deps.install(binary)
}

func checkAttestation(ctx context.Context, warn io.Writer, archive []byte, version string, opts upgradeOptions, deps upgradeDeps) error {
	if deps.attest == nil {
		if !selfupdate.AttestationRequiredFor(version) {
			return nil
		}
		return errors.New(noVerifierMessage(version))
	}

	switch result := deps.attest(ctx, archive, version); result.State {
	case selfupdate.AttestationVerified:
		if result.Detail != "" {
			_, _ = fmt.Fprintf(warn, "warning: %s\n", result.Detail)
		}
		return nil
	case selfupdate.AttestationAbsent:
		if !opts.allowUnattested {
			return errors.New(unattestedMessage(version))
		}
		_, _ = fmt.Fprintf(warn, "warning: about to install %s, which publishes no build attestation — nothing says which workflow built it, and --allow-unattested was given\n", version)
		return nil
	case selfupdate.AttestationUnreachable:
		if !opts.allowUnattested {
			return errors.New(attestationUnreachableMessage(version, result.Detail))
		}
		_, _ = fmt.Fprintf(warn, "warning: about to install %s without checking its build attestation (%v) — --allow-unattested was given\n", version, result.Detail)
		return nil
	case selfupdate.AttestationUnverifiable:
		return errors.New(attestationRejectedMessage(version, result.Detail))
	default:
		return errors.New(unverifiableStateMessage(version, result.State.String()))
	}
}

func downloadCapped(ctx context.Context, url string, read func(io.Reader) ([]byte, error)) ([]byte, error) {
	body, err := selfupdate.Download(ctx, url)
	if err != nil {
		return nil, err
	}
	data, readErr := read(body)
	_ = body.Close()
	if readErr != nil {
		return nil, readErr
	}
	return data, nil
}
