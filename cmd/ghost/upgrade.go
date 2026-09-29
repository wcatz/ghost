package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

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
	upgradeRefusePrerelease
)

// upgradeBudget bounds one whole `ghost upgrade` run: the release lookup, the
// checksum manifest and the archive, together. The per-request deadlines inside
// selfupdate bound each request, so three requests in sequence bound the run at
// their sum — 30s for the lookup plus 10 minutes per transfer is twenty and a
// half minutes — which is what an interactive command can otherwise sit through
// on a link that is slow but not broken.
//
// Twelve minutes is longer than a single transfer's own deadline, so a transfer
// that begins at once and would have finished on its own is never cut short, and
// shorter than that sum, so a run that keeps making progress still stops. Which
// request gets squeezed is not specified: a slow manifest can leave the archive
// less than its own ten minutes, and that is the budget working, not a second
// rule.
//
// It is a var only so a test can shorten it below what a local server can
// answer; production never reassigns it.
var upgradeBudget = 12 * time.Minute

// upgradeStreams is where a run writes. Progress and results go to out;
// warnings go to err, where they do not interleave with what the command is
// reporting it is doing. Both are parameters so a test can read what a run said
// without capturing the process's own streams.
type upgradeStreams struct{ out, err io.Writer }

// discardStreams is the quiet pair, for the cases where what a run said is not
// the assertion.
func discardStreams() upgradeStreams { return upgradeStreams{out: io.Discard, err: io.Discard} }

// upgradeDeps is the outside world a run acts on, as parameters: the release
// lookup and the install step. Production passes selfupdate.LatestRelease and a
// selfupdate.Replace over the running binary; a test passes a local server and a
// scratch file, so the decision and verification path can be exercised end to
// end without GitHub and without the binary the test runner is executing.
type upgradeDeps struct {
	fetch   func(context.Context) (*selfupdate.Release, error)
	install func([]byte) error
}

// upgradeOptions is the parsed `ghost upgrade` command line.
type upgradeOptions struct {
	allowDowngrade  bool
	allowPrerelease bool
}

// parseUpgradeArgs parses `ghost upgrade` arguments. Only --allow-downgrade and
// --allow-prerelease are recognized; anything else is an error rather than
// being ignored, because a flag that does not parse here is a flag the user
// believes is in effect while the command does something else.
func parseUpgradeArgs(args []string) (upgradeOptions, error) {
	var opts upgradeOptions
	for _, arg := range args {
		switch arg {
		case "--allow-downgrade":
			opts.allowDowngrade = true
		case "--allow-prerelease":
			opts.allowPrerelease = true
		default:
			return upgradeOptions{}, fmt.Errorf("unknown argument %q (usage: ghost upgrade [--allow-downgrade] [--allow-prerelease])", arg)
		}
	}
	return opts, nil
}

// decideUpgrade compares the running version against the latest release tag.
//
// Ordering matters, twice. First the release has to be newer than what is
// already installed: a tag that is merely *different* is not an upgrade, and
// installing it moves the user backwards — the "upgrade" that unpicks a yanked
// release whose tag no longer points at the newest build, or that reads "0.9.0"
// as newer than "0.10.0" because it compared the strings. Second, the release
// has to be a release at all: a prerelease is unfinished, and installing one
// because its number is higher is how a candidate ends up running on machines
// that asked for a stable build. That check comes first of the two refusals
// because it is about what the release *is*, not about which direction it
// moves — an older rc is refused as a prerelease, and the flag that permits it
// is the prerelease one.
//
// The two opt-ins are independent, and each changes its own decision and nothing
// else. allowDowngrade permits a release older than the installed one, for a
// release that was withdrawn or a build that has to be pinned while the newer
// one is investigated. allowPrerelease permits a release that is not final yet.
// Being on the latest version is still "already up to date" with either flag
// set, and an unorderable version is still unorderable.
//
// A version that cannot be ordered — a "dev" build, a tag like
// "vscode-pre-rewrite" — keeps the pre-guard behaviour of going ahead, because
// the alternative is refusing to update every developer build, and it is not
// read as a prerelease either: nothing about "dev" says it is a candidate
// release. The archive digest still has to agree before anything is installed. An
// unorderable version identical to the release is still the one installed.
//
// One ORDERABLE shape is worth naming, because it reads newer than it is. A
// local `make build` stamps the binary with `git describe`, so five commits past
// v0.32.0 it reports "0.32.0-5-gabc1234" and a dirty tree adds "-dirty". That is
// a legal prerelease, and semver ranks every prerelease BELOW its release, so
// the release compares as the NEWER of the two and the guard proceeds: the run
// installs v0.32.0 over a build five commits ahead of it, which is the shape
// this function cannot see. Neither opt-in prevents it, and that is arithmetic
// rather than an oversight — --allow-prerelease asks about the RELEASE, which
// is final, and --allow-downgrade is only consulted once the release compares as
// older. The remedy is not to run this on a describe build. Reading
// "-N-g<sha>" as newer than its base would fix the direction at the cost of
// inventing an ordering semver does not define, and a dirty tree would still
// have no count to compare.
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
		// Equal precedence, different text: "0.1.0" and "0.1.0+build.5" are
		// the same release.
		return upgradeCurrent
	case cmp < 0 && !opts.allowDowngrade:
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

// prereleaseMessage names the release and the one flag that permits it. It must
// not mention a downgrade: --allow-downgrade does not reach this guard, and a
// message naming it would send the user to re-run into the same refusal.
func prereleaseMessage(latest string) string {
	return fmt.Sprintf(
		"refusing to install the prerelease %s: it is not a release. Re-run with --allow-prerelease to install it deliberately, or wait for the release it leads to",
		latest)
}

// upgradeUsage is the help for `ghost upgrade`: stdout for -h/--help (see
// handleHelp), so a help request never checks GitHub Releases, downloads an
// archive or replaces the running binary.
const upgradeUsage = `Usage: ghost upgrade [--allow-downgrade] [--allow-prerelease]

Checks GitHub Releases and replaces this binary after verifying the archive
against the digest GitHub reports for that release asset and against the
published checksum manifest. The whole run is bounded by a 12 minute budget.
A plugin-managed binary refuses this path: update it with /plugin update in
Claude Code instead.

  --allow-downgrade   Install a release older than this binary. Without it, an
                      older latest release is refused.
  --allow-prerelease  Install a prerelease (an rc, a beta). Without it, a
                      prerelease is refused however new it is.
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

	// The budget is applied inside performUpgrade, so that the bound belongs to
	// the run rather than to the one call site that starts it.
	installed, err := performUpgrade(context.Background(), upgradeStreams{out: os.Stdout, err: os.Stderr}, version, opts, upgradeDeps{
		fetch: selfupdate.LatestRelease,
		install: func(binary []byte) error {
			fmt.Printf("Replacing %s...\n", exe)
			return selfupdate.Replace(exe, binary)
		},
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	if installed == "" {
		return
	}
	fmt.Printf("Updated: ghost %s → %s\n", version, installed)

	// A new binary can invalidate wiring the old one installed (hook flags,
	// embedded plugin sources). Surface stale integrations instead of letting
	// them fail open invisibly on every fire.
	mcpinit.ReportStaleIntegrations(os.Stdout)

	// The replacement above is what makes this list matter (#746): a running
	// `ghost mcp` holds the inode of the binary it started from, so the file
	// that was just replaced is exactly the one those processes are still
	// executing. They keep serving, and keep writing, from code that is no
	// longer on disk. Reporting them HERE — right after the swap that orphaned
	// them — is the only moment the operator is told which clients to restart
	// as part of the same action.
	//
	// The comparison is against `exe`, the path just replaced, not
	// os.Executable(): this process is still running the OLD image, and
	// comparing against it would report nothing. That is also why the path is
	// passed in rather than resolved again here.
	mcpinit.ReportStaleServers(os.Stdout, exe)
}

// performUpgrade is the command with its two dependencies injected: it looks the
// release up, decides, and installs what verifies. It returns the version it
// installed, or "" when the command stopped without installing anything because
// the binary is already that release.
//
// Every refusal is returned before anything is downloaded, so a release that is
// not wanted costs one metadata request rather than a transfer and an install.
//
// The total budget is applied here rather than by the caller, so that the bound
// is a property of the run and not of the one place that happens to start it —
// and so a test can shorten it. A caller that knows better still bounds it
// further: the budget is the later of the two deadlines, never the only one.
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
		// Reporting, not a result: a stdout that has gone away (a closed pipe,
		// a redirect to a full disk) is not a reason to fail a run that has
		// already decided there is nothing to do.
		_, _ = fmt.Fprintf(streams.out, "Already up to date (%s).\n", running)
		return "", nil
	case upgradeRefusePrerelease:
		return "", errors.New(prereleaseMessage(latest))
	case upgradeRefuseDowngrade:
		return "", errors.New(downgradeMessage(running, latest))
	}

	// Both warnings say what the flag allowed, on stderr where a warning does
	// not interleave with the progress a run is reporting. The prerelease one
	// matters most: the line below it reads like an ordinary upgrade.
	//
	// Stated as what is about to happen, and written here rather than after the
	// install, because a download is the long part of the run and a user
	// watching it is exactly who a warning is for. Phrasing it as a completed
	// act would leave it claiming an install that a later refusal — a missing
	// asset, a substituted archive, the budget — never performed, sitting
	// directly above the error saying it did not happen.
	//
	// Neither write is checked: a warning that could not be printed is not a
	// reason to stop a run.
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
	if err := installRelease(ctx, streams.out, rel, asset, deps.install); err != nil {
		return "", withBudget(parent, ctx, err)
	}
	return latest, nil
}

// withBudget names the total bound in an error that came from it. "context
// deadline exceeded" on its own says which request gave up, not that the command
// as a whole ran out of time, and a user watching an upgrade stop has to be able
// to tell those apart — the first is a slow link, the second is a run that has to
// be re-run.
//
// parent and ctx are both needed, because the budget is derived from parent and
// a deadline on parent is earlier whenever it is shorter — so a parent that
// expired leaves ctx expired too, and reporting the budget there would be a
// false statement about why the run stopped. A cancelled context is not a
// deadline in either direction: that is the caller stopping the command, and
// saying so would be the one place the command explains itself wrongly.
func withBudget(parent, ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return err
	}
	if errors.Is(parent.Err(), context.DeadlineExceeded) {
		// Without the budget named, on purpose: this message exists to say the
		// budget is not what stopped the run, and quoting it here would put the
		// word in the one error that must not blame it.
		return fmt.Errorf("ghost upgrade stopped on the deadline its caller set: %w", err)
	}
	return fmt.Errorf("ghost upgrade exceeded its %s total budget: %w", upgradeBudget, err)
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

	// Progress, not a result: a stdout that has gone away (a closed pipe, a
	// redirect to a full disk) is not a reason to refuse an upgrade whose
	// verification has already passed.
	_, _ = fmt.Fprintf(out, "Downloading %s...\n", asset.Name)
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
